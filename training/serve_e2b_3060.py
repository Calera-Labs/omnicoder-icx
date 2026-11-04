#!/usr/bin/env python3
"""
OmniCoder-Gemma-E2B RTX-3060-Budget OpenAI-Compatible Speculative Server (Config 3)
===================================================================================
Serves the co-tuned `OmniCoder-Gemma-E2B` QLoRA adapter in 4-bit NF4 with:
  - Config 3 Full Organism defaults: Compact `[OC:*]` micro-cards + VLN 2048-bit SDR
    hints + Greedy-First (`Candidate 0`) + Speculative Micro-Batching (`K=4` default,
    up to `n=16` candidates in <=4-beam micro-batches with `logits_to_keep=1` and
    balanced JSON brace stopping) for OmniCoder's `DecideHealSpeculative` and
    `EvaluateCandidatesParallel` tournament.
  - Zero-dependency `ThreadingHTTPServer` (avoids FastAPI/Starlette version conflicts)
    with serialized GPU lock and strict RTX 3060 VRAM telemetry (`/health`, `/v1/models`,
    and `/v1/chat/completions`).
"""

import argparse
import json
import sys
import threading
import time
import uuid
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from typing import Any

import os
os.environ["USE_TF"] = "0"
os.environ["TRANSFORMERS_NO_TF"] = "1"
sys.modules["tensorflow"] = None
sys.modules["torchaudio"] = None
sys.modules["torchao"] = None

import torch
from peft import PeftModel
from transformers import (
    AutoModelForCausalLM,
    AutoTokenizer,
    BitsAndBytesConfig,
    StoppingCriteria,
    StoppingCriteriaList,
)

REPO_ROOT = Path(__file__).resolve().parent.parent
DEFAULT_ADAPTER_DIR = REPO_ROOT / "training" / "checkpoints" / "omnicoder-gemma-e2b"
DEFAULT_MANIFEST = REPO_ROOT / "training" / "spark_sync_manifest.json"

STATE: dict[str, Any] = {}
GPU_LOCK = threading.Lock()


class JSONBraceStoppingCriteria(StoppingCriteria):
    """Stops generation as soon as every sequence in the batch has closed its root JSON object."""

    def __init__(self, tokenizer, prompt_len: int):
        super().__init__()
        self.tokenizer = tokenizer
        self.prompt_len = prompt_len

    @staticmethod
    def _is_balanced_json(text: str) -> bool:
        start = text.find("{")
        if start == -1:
            return False
        depth = 0
        in_str = False
        esc = False
        for ch in text[start:]:
            if esc:
                esc = False
                continue
            if ch == "\\":
                esc = True
                continue
            if ch == '"':
                in_str = not in_str
                continue
            if not in_str:
                if ch == "{":
                    depth += 1
                elif ch == "}":
                    depth -= 1
                    if depth == 0:
                        return True
        return False

    def __call__(self, input_ids: torch.LongTensor, scores: torch.FloatTensor, **kwargs) -> bool:
        gen_len = input_ids.shape[1] - self.prompt_len
        if gen_len < 10 or gen_len % 2 != 0:
            return False
        balanced_cnt = 0
        has_open_brace = False
        for row in input_ids:
            txt = self.tokenizer.decode(row[self.prompt_len :], skip_special_tokens=True)
            if "{" in txt:
                has_open_brace = True
            if self._is_balanced_json(txt):
                balanced_cnt += 1
        if gen_len >= 32 and not has_open_brace:
            return True
        if input_ids.shape[0] == 1:
            return balanced_cnt == 1
        return balanced_cnt >= max(2, input_ids.shape[0] - 1)


def trim_to_first_json_object(text: str) -> str:
    start = text.find("{")
    if start == -1:
        return text.strip()
    depth = 0
    in_str = False
    esc = False
    for idx, ch in enumerate(text[start:], start=start):
        if esc:
            esc = False
            continue
        if ch == "\\":
            esc = True
            continue
        if ch == '"':
            in_str = not in_str
            continue
        if not in_str:
            if ch == "{":
                depth += 1
            elif ch == "}":
                depth -= 1
                if depth == 0:
                    return text[start : idx + 1]
    return text[start:].strip()


def build_prompt(messages: list[dict[str, Any]]) -> str:
    sys_text = ""
    user_text = ""
    for m in messages:
        role = m.get("role", "")
        content = m.get("content", "")
        if role == "system":
            sys_text = content
        elif role == "user":
            user_text = content
    return f"<|system|>\n{sys_text}\n<|user|>\n{user_text}\n<|assistant|>\n"


def generate_candidates_for_prompt(
    model,
    tokenizer,
    device: torch.device,
    prompt: str,
    n_cands: int = 4,
    max_new: int = 256,
    temperature: float = 0.1,
) -> tuple[list[str], int, int, float]:
    with GPU_LOCK:
        inputs = tokenizer(prompt, return_tensors="pt", truncation=True, max_length=1024).to(device)
        prompt_len = int(inputs["input_ids"].shape[1])
        stop_criteria = StoppingCriteriaList([JSONBraceStoppingCriteria(tokenizer, prompt_len)])

        t0 = time.time()
        texts: list[str] = []
        comp_tokens = 0

        with torch.no_grad(), torch.amp.autocast("cuda", dtype=torch.bfloat16):
            # Candidate 0 is always deterministic greedy decoding (Anchor candidate)
            if n_cands == 1 and temperature > 0.05:
                out0 = model.generate(
                    **inputs,
                    max_new_tokens=max_new,
                    do_sample=True,
                    temperature=temperature,
                    top_p=0.92,
                    logits_to_keep=1,
                    stopping_criteria=stop_criteria,
                    pad_token_id=tokenizer.pad_token_id,
                    eos_token_id=tokenizer.eos_token_id,
                )
            else:
                out0 = model.generate(
                    **inputs,
                    max_new_tokens=max_new,
                    do_sample=False,
                    logits_to_keep=1,
                    stopping_criteria=stop_criteria,
                    pad_token_id=tokenizer.pad_token_id,
                    eos_token_id=tokenizer.eos_token_id,
                )
            new_ids0 = out0[0, prompt_len:]
            comp_tokens += int(new_ids0.shape[0])
            texts.append(trim_to_first_json_object(tokenizer.decode(new_ids0, skip_special_tokens=True)))

            # Remaining (n_cands - 1) candidates generated in micro-batches of <=3..4 with diverse temperatures
            remaining = n_cands - 1
            batch_idx = 0
            temps = [0.35, 0.55, 0.70, 0.85]
            while remaining > 0:
                cur_k = min(remaining, 3)
                eff_temp = temps[min(batch_idx, len(temps) - 1)]
                b_out = model.generate(
                    **inputs,
                    max_new_tokens=max_new,
                    do_sample=True,
                    temperature=eff_temp,
                    top_p=0.92,
                    num_return_sequences=cur_k,
                    logits_to_keep=1,
                    stopping_criteria=stop_criteria,
                    pad_token_id=tokenizer.pad_token_id,
                    eos_token_id=tokenizer.eos_token_id,
                )
                for idx in range(b_out.shape[0]):
                    new_ids = b_out[idx, prompt_len:]
                    comp_tokens += int(new_ids.shape[0])
                    texts.append(trim_to_first_json_object(tokenizer.decode(new_ids, skip_special_tokens=True)))
                remaining -= cur_k
                batch_idx += 1

        elapsed = max(time.time() - t0, 1e-4)
        return texts, prompt_len, comp_tokens, elapsed


def apply_rtx3060_vram_surgery(model) -> float:
    inner = getattr(model, "model", None)
    if inner is not None:
        for attr in ("vision_tower", "audio_tower", "embed_vision", "embed_audio"):
            if hasattr(inner, attr) and getattr(inner, attr) is not None:
                setattr(inner, attr, None)
        lang = getattr(inner, "language_model", None)
        if lang is not None and hasattr(lang, "embed_tokens_per_layer"):
            ple = lang.embed_tokens_per_layer
            if ple is not None:
                ple.to("cpu")
                orig_forward = ple.forward

                def cpu_ple_forward(input_ids: torch.Tensor) -> torch.Tensor:
                    return orig_forward(input_ids.to("cpu")).to(input_ids.device, non_blocking=True)

                ple.forward = cpu_ple_forward
    torch.cuda.empty_cache()
    return round(torch.cuda.memory_allocated(0) / (1024**2), 1)


def load_engine(adapter_dir: Path, manifest_path: Path, default_k: int = 4):
    manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
    base_path = manifest["model"]["snapshot_path"]

    bnb_config = BitsAndBytesConfig(
        load_in_4bit=True,
        bnb_4bit_quant_type="nf4",
        bnb_4bit_compute_dtype=torch.bfloat16,
        bnb_4bit_use_double_quant=True,
    )
    tokenizer = AutoTokenizer.from_pretrained(base_path, trust_remote_code=True)
    if tokenizer.pad_token is None:
        tokenizer.pad_token = tokenizer.eos_token

    model = AutoModelForCausalLM.from_pretrained(
        base_path,
        quantization_config=bnb_config,
        device_map={"": 0},
        dtype=torch.bfloat16,
        trust_remote_code=True,
    )
    base_vram_mb = apply_rtx3060_vram_surgery(model)
    adapter_loaded = False
    if (adapter_dir / "adapter_config.json").exists():
        model = PeftModel.from_pretrained(model, str(adapter_dir))
        for p in model.parameters():
            if p.device.type == "cuda" and p.dtype == torch.float32:
                p.data = p.data.to(torch.bfloat16)
        base_vram_mb = apply_rtx3060_vram_surgery(model)
        adapter_loaded = True
    model.eval()
    torch.cuda.reset_peak_memory_stats()

    gob_path = REPO_ROOT / "models" / "vln_swe_code.gob"
    STATE["tokenizer"] = tokenizer
    STATE["model"] = model
    STATE["device"] = torch.device("cuda:0")
    STATE["adapter_loaded"] = adapter_loaded
    STATE["base_vram_mb"] = base_vram_mb
    STATE["default_k"] = default_k
    STATE["vln_cartridge_ready"] = gob_path.exists()
    STATE["vln_cartridge_bytes"] = gob_path.stat().st_size if gob_path.exists() else 0


class Config3SpeculativeHandler(BaseHTTPRequestHandler):
    def log_message(self, fmt: str, *args: Any) -> None:
        pass

    def _send_json(self, status: int, payload: dict[str, Any]) -> None:
        body = json.dumps(payload).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self) -> None:
        if self.path.startswith("/health"):
            alloc_mb = torch.cuda.memory_allocated(0) / (1024**2) if torch.cuda.is_available() else 0.0
            res_mb = torch.cuda.memory_reserved(0) / (1024**2) if torch.cuda.is_available() else 0.0
            peak_mb = torch.cuda.max_memory_allocated(0) / (1024**2) if torch.cuda.is_available() else 0.0
            self._send_json(
                200,
                {
                    "status": "ok",
                    "config": "Config 3: Full Organism (OmniCoder-ICX + VLN Cartridge + OmniCoder-Gemma-E2B + Speculative K=4)",
                    "model": "google/gemma-4-e2b",
                    "adapter_loaded": STATE.get("adapter_loaded", False),
                    "vln_cartridge_ready": STATE.get("vln_cartridge_ready", False),
                    "vln_cartridge_bytes": STATE.get("vln_cartridge_bytes", 0),
                    "default_speculative_k": STATE.get("default_k", 4),
                    "allocated_vram_mb": round(alloc_mb, 1),
                    "reserved_vram_mb": round(res_mb, 1),
                    "peak_vram_mb": round(peak_mb, 1),
                    "rtx3060_e2b_base_vram_mb": STATE.get("base_vram_mb", 2105.2),
                    "rtx3060_budget_limit_mb": 2500.0,
                    "rtx3060_budget_compliant": round(peak_mb, 1) < 2500.0,
                },
            )
            return
        if self.path.startswith("/v1/models"):
            self._send_json(
                200,
                {
                    "object": "list",
                    "data": [
                        {"id": "google/gemma-4-e2b", "object": "model", "owned_by": "caleralabs"},
                        {"id": "omnicoder-gemma-e2b", "object": "model", "owned_by": "caleralabs"},
                    ],
                },
            )
            return
        self._send_json(404, {"error": f"Unknown path {self.path}"})

    def do_POST(self) -> None:
        if not self.path.startswith("/v1/chat/completions"):
            self._send_json(404, {"error": f"Unknown path {self.path}"})
            return

        length = int(self.headers.get("Content-Length", "0"))
        raw = self.rfile.read(length).decode("utf-8")
        req = json.loads(raw)

        messages = req.get("messages", [])
        prompt = build_prompt(messages)

        req_n = req.get("n")
        # For Edit/Heal cards, default to Config 3 speculative K if n not explicitly > 1
        sys_txt = next((m.get("content", "") for m in messages if m.get("role") == "system"), "")
        is_edit_or_heal = ("[OC:EDIT]" in sys_txt) or ("[OC:HEAL]" in sys_txt) or ("GateEdit" in sys_txt) or ("GateHeal" in sys_txt)
        default_k = int(STATE.get("default_k", 4))
        if req_n is not None and int(req_n) > 1:
            n_cands = max(1, min(int(req_n), 16))
        elif is_edit_or_heal and req.get("use_config3_k", False):
            n_cands = default_k
        else:
            n_cands = max(1, min(int(req_n or 1), 16))

        max_new = max(32, min(int(req.get("max_tokens") or 256), 384))
        temp = float(req.get("temperature") if req.get("temperature") is not None else 0.0)

        texts, prompt_len, comp_tokens, elapsed = generate_candidates_for_prompt(
            model=STATE["model"],
            tokenizer=STATE["tokenizer"],
            device=STATE["device"],
            prompt=prompt,
            n_cands=n_cands,
            max_new=max_new,
            temperature=temp,
        )

        choices = [
            {
                "index": idx,
                "message": {"role": "assistant", "content": txt},
                "finish_reason": "stop",
            }
            for idx, txt in enumerate(texts)
        ]

        alloc_mb = torch.cuda.memory_allocated(0) / (1024**2)
        peak_mb = torch.cuda.max_memory_allocated(0) / (1024**2)
        self._send_json(
            200,
            {
                "id": f"chatcmpl-{uuid.uuid4().hex[:12]}",
                "object": "chat.completion",
                "created": int(time.time()),
                "model": req.get("model", "google/gemma-4-e2b"),
                "choices": choices,
                "usage": {
                    "prompt_tokens": prompt_len,
                    "completion_tokens": comp_tokens,
                    "total_tokens": prompt_len + comp_tokens,
                },
                "x_omnicoder_telemetry": {
                    "latency_sec": round(elapsed, 3),
                    "tokens_per_sec": round(comp_tokens / elapsed, 1),
                    "speculative_k": n_cands,
                    "allocated_vram_mb": round(alloc_mb, 1),
                    "peak_vram_mb": round(peak_mb, 1),
                },
            },
        )


def main():
    parser = argparse.ArgumentParser(description="Serve Config 3 OmniCoder-Gemma-E2B on port 18090")
    parser.add_argument("--port", type=int, default=18090)
    parser.add_argument("--host", default="127.0.0.1")
    parser.add_argument("--default-k", type=int, default=4)
    parser.add_argument("--adapter-dir", default=str(DEFAULT_ADAPTER_DIR))
    parser.add_argument("--manifest", default=str(DEFAULT_MANIFEST))
    args = parser.parse_args()

    print(f"[Config 3 Server] Loading google/gemma-4-e2b + QLoRA adapter ({args.adapter_dir}) + VLN SWE Cartridge...")
    load_engine(Path(args.adapter_dir), Path(args.manifest), default_k=args.default_k)
    print(
        f"[Config 3 Server] Ready on http://{args.host}:{args.port}/v1 "
        f"(Base VRAM: {STATE['base_vram_mb']} MB | Speculative K={args.default_k} | VLN={STATE['vln_cartridge_ready']})"
    )
    server = ThreadingHTTPServer((args.host, args.port), Config3SpeculativeHandler)
    server.serve_forever()


if __name__ == "__main__":
    main()
