#!/usr/bin/env python3
"""
Merge OmniCoder-Gemma-E2B Surgical LoRA into google/gemma-4-e2b and Export Native GGUF for LM Studio
=====================================================================================================
1. Merges the co-tuned `OmniCoder-Gemma-E2B` LoRA adapter (`r=64, alpha=128`) directly into the
   base `google/gemma-4-e2b` language model (`bfloat16` / `float32`).
2. Exports a native `gemma4` GGUF model (`Q8_0` or `BF16`) compatible with LM Studio's `llama.cpp`
   Gemma 4 runtime (`general.architecture = 'gemma4'`).
3. Installs the GGUF into both:
   - `training/checkpoints/omnicoder-gemma-e2b-gguf/OmniCoder-Gemma-E2B-Q8_0.gguf`
   - `C:/Users/livin/.lmstudio/models/Calera-Labs/OmniCoder-Gemma-E2B-GGUF/OmniCoder-Gemma-E2B-Q8_0.gguf`
"""

import argparse
import json
import os
import shutil
import time
from pathlib import Path

import gguf
import numpy as np
import torch
from gguf import GGUFReader, GGUFValueType, GGUFWriter
from gguf.quants import quantize
from safetensors import safe_open

REPO_ROOT = Path(__file__).resolve().parent.parent
DEFAULT_BASE_SNAPSHOT = Path(
    os.path.expanduser(
        r"~\.cache\huggingface\hub\models--google--gemma-4-e2b\snapshots\63db66a33dc06d58c02b1e887446e103c202602c"
    )
)
DEFAULT_ADAPTER_DIR = REPO_ROOT / "training" / "checkpoints" / "omnicoder-gemma-e2b"
DEFAULT_DONOR_GGUF = Path(
    os.path.expanduser(
        r"~\.lmstudio\models\lmstudio-community\gemma-4-12B-it-QAT-GGUF\gemma-4-12B-it-QAT-Q4_0.gguf"
    )
)
DEFAULT_LOCAL_GGUF_DIR = REPO_ROOT / "training" / "checkpoints" / "omnicoder-gemma-e2b-gguf"
DEFAULT_LMSTUDIO_DIR = Path(
    os.path.expanduser(r"~\.lmstudio\models\Calera-Labs\OmniCoder-Gemma-E2B-GGUF")
)


def copy_tokenizer_kv_from_donor(writer: GGUFWriter, donor_path: Path) -> None:
    print(f"[GGUF] Copying Gemma 4 262k tokenizer metadata from {donor_path.name}...", flush=True)
    reader = GGUFReader(str(donor_path))
    for k, field in reader.fields.items():
        if not k.startswith("tokenizer."):
            continue
        val = field.contents()
        t0 = field.types[0]
        if t0 == GGUFValueType.STRING:
            writer.add_string(k, str(val))
        elif t0 == GGUFValueType.UINT32:
            writer.add_uint32(k, int(val))
        elif t0 == GGUFValueType.INT32:
            writer.add_int32(k, int(val))
        elif t0 == GGUFValueType.BOOL:
            writer.add_bool(k, bool(val))
        elif t0 == GGUFValueType.FLOAT32:
            writer.add_float32(k, float(val))
        elif t0 == GGUFValueType.ARRAY:
            writer.add_array(k, val)


def merge_lora_if_present(
    base_tensor: torch.Tensor,
    hf_key: str,
    adapter_sf,
    adapter_keys: set[str],
    lora_scale: float,
    device: torch.device,
) -> torch.Tensor:
    # hf_key looks like: model.language_model.layers.0.self_attn.q_proj.weight
    prefix = "base_model.model." + hf_key[: -len(".weight")]
    k_a = prefix + ".lora_A.weight"
    k_b = prefix + ".lora_B.weight"
    if k_a in adapter_keys and k_b in adapter_keys:
        t_base = base_tensor.to(device=device, dtype=torch.float32)
        t_a = adapter_sf.get_tensor(k_a).to(device=device, dtype=torch.float32)
        t_b = adapter_sf.get_tensor(k_b).to(device=device, dtype=torch.float32)
        merged = t_base + (t_b @ t_a) * lora_scale
        return merged.cpu()
    return base_tensor.to(dtype=torch.float32).cpu()


def add_gguf_tensor(
    writer: GGUFWriter,
    gguf_name: str,
    tensor_f32: torch.Tensor,
    quant_2d: gguf.GGMLQuantizationType,
) -> None:
    arr = tensor_f32.detach().to(dtype=torch.float32).contiguous().numpy()
    if arr.ndim == 1 or gguf_name.endswith("_norm.weight") or gguf_name.endswith("scale.weight") or gguf_name == "rope_freqs.weight":
        writer.add_tensor(gguf_name, arr.astype(np.float32), raw_dtype=gguf.GGMLQuantizationType.F32)
    else:
        q_arr = quantize(arr, quant_2d)
        writer.add_tensor(gguf_name, q_arr, raw_dtype=quant_2d)


def main() -> None:
    parser = argparse.ArgumentParser(description="Export OmniCoder-Gemma-E2B to GGUF for LM Studio")
    parser.add_argument("--base-snapshot", default=str(DEFAULT_BASE_SNAPSHOT))
    parser.add_argument("--adapter-dir", default=str(DEFAULT_ADAPTER_DIR))
    parser.add_argument("--donor-gguf", default=str(DEFAULT_DONOR_GGUF))
    parser.add_argument("--quant", choices=["Q8_0", "BF16"], default="Q8_0")
    args = parser.parse_args()

    t0 = time.time()
    base_dir = Path(args.base_snapshot)
    adapter_dir = Path(args.adapter_dir)
    donor_gguf = Path(args.donor_gguf)

    cfg = json.loads((base_dir / "config.json").read_text(encoding="utf-8"))
    tcfg = cfg["text_config"]
    adapter_cfg = json.loads((adapter_dir / "adapter_config.json").read_text(encoding="utf-8"))
    lora_r = float(adapter_cfg.get("r", 64))
    lora_alpha = float(adapter_cfg.get("lora_alpha", 128))
    lora_scale = lora_alpha / lora_r

    n_layers = int(tcfg["num_hidden_layers"])  # 35
    n_kv_shared = int(tcfg.get("num_kv_shared_layers", 20))  # 20
    first_kv_shared = n_layers - n_kv_shared  # 15

    quant_type = (
        gguf.GGMLQuantizationType.Q8_0
        if args.quant == "Q8_0"
        else gguf.GGMLQuantizationType.BF16
    )
    ftype_int = 7 if args.quant == "Q8_0" else 32

    DEFAULT_LOCAL_GGUF_DIR.mkdir(parents=True, exist_ok=True)
    DEFAULT_LMSTUDIO_DIR.mkdir(parents=True, exist_ok=True)

    gguf_filename = f"OmniCoder-Gemma-4-E2B-{args.quant}.gguf"
    out_local = DEFAULT_LOCAL_GGUF_DIR / gguf_filename
    out_lmstudio = DEFAULT_LMSTUDIO_DIR / gguf_filename

    print("=" * 78)
    print(f"  Exporting OmniCoder-Gemma-E2B ({args.quant}) GGUF for LM Studio")
    print(f"  Base Snapshot: {base_dir}")
    print(f"  LoRA Adapter:  {adapter_dir} (r={int(lora_r)}, alpha={int(lora_alpha)}, scale={lora_scale:.2f})")
    print(f"  Output Local:  {out_local}")
    print(f"  Output LMS:    {out_lmstudio}")
    print("=" * 78, flush=True)

    writer = GGUFWriter(str(out_local), arch="gemma4")

    # General metadata
    writer.add_string("general.type", "model")
    writer.add_int32("general.sampling.top_k", 64)
    writer.add_float32("general.sampling.top_p", 0.95)
    writer.add_float32("general.sampling.temp", 0.2)
    writer.add_string("general.name", f"OmniCoder Gemma 4 E2B ({args.quant})")
    writer.add_string("general.finetune", "omnicoder-icx-vln-v4")
    writer.add_string("general.basename", "google_gemma-4")
    writer.add_string("general.size_label", "E2B")

    # Gemma 4 architecture metadata
    writer.add_uint32("gemma4.block_count", n_layers)
    writer.add_uint32("gemma4.context_length", int(tcfg["max_position_embeddings"]))
    writer.add_uint32("gemma4.embedding_length", int(tcfg["hidden_size"]))

    ff_lengths = [
        int(tcfg["intermediate_size"]) * (2 if (i >= first_kv_shared and tcfg.get("use_double_wide_mlp")) else 1)
        for i in range(n_layers)
    ]
    writer.add_array("gemma4.feed_forward_length", ff_lengths)
    writer.add_uint32("gemma4.attention.head_count", int(tcfg["num_attention_heads"]))
    writer.add_array("gemma4.attention.head_count_kv", [int(tcfg["num_key_value_heads"])] * n_layers)
    writer.add_float32("gemma4.rope.freq_base", float(tcfg["rope_parameters"]["full_attention"]["rope_theta"]))
    writer.add_float32("gemma4.rope.freq_base_swa", float(tcfg["rope_parameters"]["sliding_attention"]["rope_theta"]))
    writer.add_float32("gemma4.attention.layer_norm_rms_epsilon", float(tcfg["rms_norm_eps"]))
    writer.add_uint32("gemma4.attention.key_length", int(tcfg["global_head_dim"]))
    writer.add_uint32("gemma4.attention.value_length", int(tcfg["global_head_dim"]))
    writer.add_float32("gemma4.final_logit_softcapping", float(tcfg["final_logit_softcapping"]))
    writer.add_uint32("gemma4.attention.sliding_window", int(tcfg["sliding_window"]))
    writer.add_uint32("gemma4.attention.shared_kv_layers", n_kv_shared)
    writer.add_uint32("gemma4.embedding_length_per_layer_input", int(tcfg["hidden_size_per_layer_input"]))

    swa_pattern = [lt == "sliding_attention" for lt in tcfg["layer_types"]]
    writer.add_array("gemma4.attention.sliding_window_pattern", swa_pattern)
    writer.add_uint32("gemma4.attention.key_length_swa", int(tcfg["head_dim"]))
    writer.add_uint32("gemma4.attention.value_length_swa", int(tcfg["head_dim"]))
    writer.add_uint32("gemma4.rope.dimension_count", int(tcfg["global_head_dim"]))
    writer.add_uint32("gemma4.rope.dimension_count_swa", int(tcfg["head_dim"]))

    # Copy tokenizer KV fields from donor Gemma 4 GGUF
    copy_tokenizer_kv_from_donor(writer, donor_gguf)
    writer.add_uint32("general.quantization_version", 2)
    writer.add_uint32("general.file_type", ftype_int)

    device = torch.device("cuda:0" if torch.cuda.is_available() else "cpu")
    base_sf_path = base_dir / "model.safetensors"
    adapter_sf_path = adapter_dir / "adapter_model.safetensors"

    with safe_open(str(base_sf_path), framework="pt") as base_sf, safe_open(
        str(adapter_sf_path), framework="pt"
    ) as adapter_sf:
        adapter_keys = set(adapter_sf.keys())
        merged_count = 0

        # Top-level tensors
        add_gguf_tensor(
            writer,
            "output_norm.weight",
            base_sf.get_tensor("model.language_model.norm.weight"),
            quant_type,
        )

        # Proportional RoPE frequency divisors: 64 active rotary dims (1.0), 192 non-rotary dims (1e30)
        rope_freqs = np.array([1.0] * 64 + [1e30] * 192, dtype=np.float32)
        writer.add_tensor("rope_freqs.weight", rope_freqs, raw_dtype=gguf.GGMLQuantizationType.F32)

        print("[GGUF] Quantizing token_embd.weight & per_layer_token_embd.weight...", flush=True)
        add_gguf_tensor(
            writer,
            "token_embd.weight",
            base_sf.get_tensor("model.language_model.embed_tokens.weight"),
            quant_type,
        )
        add_gguf_tensor(
            writer,
            "per_layer_token_embd.weight",
            base_sf.get_tensor("model.language_model.embed_tokens_per_layer.weight"),
            quant_type,
        )
        add_gguf_tensor(
            writer,
            "per_layer_model_proj.weight",
            base_sf.get_tensor("model.language_model.per_layer_model_projection.weight"),
            quant_type,
        )
        add_gguf_tensor(
            writer,
            "per_layer_proj_norm.weight",
            base_sf.get_tensor("model.language_model.per_layer_projection_norm.weight"),
            quant_type,
        )

        for i in range(n_layers):
            p = f"model.language_model.layers.{i}."
            b = f"blk.{i}."

            layer_map = [
                ("input_layernorm.weight", "attn_norm.weight"),
                ("self_attn.q_proj.weight", "attn_q.weight"),
                ("self_attn.q_norm.weight", "attn_q_norm.weight"),
                ("self_attn.o_proj.weight", "attn_output.weight"),
                ("post_attention_layernorm.weight", "post_attention_norm.weight"),
                ("pre_feedforward_layernorm.weight", "ffn_norm.weight"),
                ("mlp.gate_proj.weight", "ffn_gate.weight"),
                ("mlp.up_proj.weight", "ffn_up.weight"),
                ("mlp.down_proj.weight", "ffn_down.weight"),
                ("post_feedforward_layernorm.weight", "post_ffw_norm.weight"),
                ("per_layer_input_gate.weight", "inp_gate.weight"),
                ("per_layer_projection.weight", "proj.weight"),
                ("post_per_layer_input_norm.weight", "post_norm.weight"),
                ("layer_scalar", "layer_output_scale.weight"),
            ]
            if i < first_kv_shared:
                layer_map.extend(
                    [
                        ("self_attn.k_proj.weight", "attn_k.weight"),
                        ("self_attn.k_norm.weight", "attn_k_norm.weight"),
                        ("self_attn.v_proj.weight", "attn_v.weight"),
                    ]
                )

            for hf_suffix, gguf_suffix in layer_map:
                hf_key = p + hf_suffix
                base_t = base_sf.get_tensor(hf_key)
                prefix = "base_model.model." + (hf_key[: -len(".weight")] if hf_key.endswith(".weight") else hf_key)
                if (prefix + ".lora_A.weight") in adapter_keys:
                    merged_count += 1
                merged_t = merge_lora_if_present(
                    base_t, hf_key, adapter_sf, adapter_keys, lora_scale, device
                )
                add_gguf_tensor(writer, b + gguf_suffix, merged_t, quant_type)

            if (i + 1) % 5 == 0 or i == n_layers - 1:
                print(
                    f"[GGUF] Processed layer {i + 1}/{n_layers} (merged {merged_count} LoRA projections so far)...",
                    flush=True,
                )

    print("[GGUF] Writing GGUF header, metadata, and tensor data to disk...", flush=True)
    writer.write_header_to_file()
    writer.write_kv_data_to_file()
    writer.write_tensors_to_file()
    writer.close()

    size_gb = out_local.stat().st_size / (1024**3)
    print(f"[GGUF] Copying {out_local.name} ({size_gb:.2f} GB) -> {out_lmstudio}...", flush=True)
    shutil.copy2(out_local, out_lmstudio)

    elapsed = time.time() - t0
    print("=" * 78)
    print(f"  DONE in {elapsed:.1f}s! Merged {merged_count} LoRA projection matrices.")
    print(f"  Local GGUF:     {out_local} ({size_gb:.2f} GB)")
    print(f"  LM Studio GGUF: {out_lmstudio} ({size_gb:.2f} GB)")
    print("=" * 78, flush=True)


if __name__ == "__main__":
    main()
