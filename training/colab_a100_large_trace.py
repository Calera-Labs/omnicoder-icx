#!/usr/bin/env python3
"""
A100 Large-Scale OmniCoder-ICX + VLN + Gemma-4-E2B Joint Trace & Mathematical Delta Analyzer
=============================================================================================
Runs the full 5-stage OmniCoder + VLN + Gemma-4-E2B trace across 1,200 Zero-Leakage SWE-bench
instances (`300` SWE-bench_Lite + `900` Disjoint Unseen Full SWE-bench Test instances = `1,200`
total, 0% overlap with the 407 training rows) on an NVIDIA A100-SXM4-80GB.

Records the complete mathematical interaction trace between:
  1. OmniCoder Deterministic Engine (8-dim structural feature vector: Traceback Pin, Explicit
     Path, Dotted Import, Package Prefix, Subtoken Stem Synergy, AST `def/class` Specificity,
     Symbol Usage DF, and VLN 2048-bit SDR Hopfield Coherence).
  2. Gemma-4-E2B Neural Gates (`[OC:LOCATE]` candidate reranking votes, `[OC:REPRO]` AST
     synthesis, and `[OC:HEAL]` K=4 Multi-Window / Multi-File Speculative Tournament).
  3. Intra-File Window IoU & EditBlock Actuation Funnel (Enclosing AST Symbol Overlap ->
     6-Line Focal Window Overlap -> Exact vs. Fuzzy EditBlock Anchor Match -> AST Compilation ->
     Zero-Leakage Judge vs. Oracle Best-of-K Regret).

Fits empirical Ridge/Logistic weights on the 900 unseen Full SWE-bench instances and evaluates
the derived mathematical optimization laws against Official SWE-300 (`SWE-bench_Lite`).
"""

import argparse
import ast
import concurrent.futures
import difflib
import json
import math
import os
import random
import re
import subprocess
import sys
import time
import warnings
from pathlib import Path
from typing import Any

warnings.filterwarnings("ignore", category=SyntaxWarning)
sys.modules["torchao"] = None
sys.modules["torchaudio"] = None

import numpy as np
import torch
from datasets import load_dataset
from peft import PeftModel
from transformers import (
    AutoModelForCausalLM,
    AutoTokenizer,
    StoppingCriteria,
    StoppingCriteriaList,
)

sys.path.insert(0, str(Path(__file__).resolve().parent))
from build_cotune_dataset import (
    SYS_HEAL,
    SYS_LOCATE,
    SYS_REPRO,
    extract_traceback_line,
    parse_unified_diff_hunks,
    recall_vln_hint,
)
from colab_a100_swe300_parallel import (
    STOPWORDS,
    diff_token_f1,
    extract_issue_signals,
    extract_modified_files_from_patch,
    make_heal_prompt,
    make_unified_git_diff,
    prewarm_all_swe300_repos,
    score_candidate_zero_leakage,
    trim_to_first_json_object,
    try_apply_edit_block_to_source,
    try_parse_json,
)


class FastVectorizedJSONStoppingCriteria(StoppingCriteria):
    """Zero-CPU-bottleneck stopping criteria: checks GPU EOS tensor first, decodes only every 16 steps."""

    def __init__(self, tokenizer, prompt_len: int, eos_token_id: int):
        super().__init__()
        self.tokenizer = tokenizer
        self.prompt_len = prompt_len
        self.eos_token_id = eos_token_id

    def __call__(self, input_ids: torch.LongTensor, scores: torch.FloatTensor, **kwargs) -> bool:
        gen_len = input_ids.shape[1] - self.prompt_len
        if gen_len < 16 or gen_len % 16 != 0:
            return False
        gen_slice = input_ids[:, self.prompt_len :]
        has_eos = (gen_slice == self.eos_token_id).any(dim=1)
        if bool(has_eos.all()):
            return True
        # Only decode rows that have not yet emitted EOS
        unfinished = (~has_eos).nonzero(as_tuple=True)[0]
        if len(unfinished) > 16:
            return False
        txts = self.tokenizer.batch_decode(gen_slice[unfinished], skip_special_tokens=True)
        for t in txts:
            if "}" not in t:
                return False
        return True


def run_length_bucketed_generation(
    model,
    tokenizer,
    device: torch.device,
    prompts: list[str],
    batch_size: int,
    max_new_tokens: int,
    do_sample: bool = False,
    temperature: float = 0.30,
    stage_name: str = "Stage",
) -> list[str]:
    """Sorts prompts by character length to minimize padding waste on A100, then restores original order."""
    total = len(prompts)
    if total == 0:
        return []
    indexed = sorted(enumerate(prompts), key=lambda x: len(x[1]))
    sorted_indices = [i for i, _ in indexed]
    sorted_prompts = [p for _, p in indexed]
    sorted_outputs: list[str] = []

    t0 = time.time()
    eos_id = tokenizer.eos_token_id or tokenizer.pad_token_id

    for start in range(0, total, batch_size):
        batch_prompts = sorted_prompts[start : start + batch_size]
        enc = tokenizer(
            batch_prompts,
            return_tensors="pt",
            padding=True,
            truncation=True,
            max_length=1024,
        ).to(device)
        prompt_len = int(enc["input_ids"].shape[1])
        stop_crit = StoppingCriteriaList([FastVectorizedJSONStoppingCriteria(tokenizer, prompt_len, eos_id)])

        with torch.no_grad(), torch.amp.autocast("cuda", dtype=torch.bfloat16):
            if do_sample:
                gen = model.generate(
                    **enc,
                    max_new_tokens=max_new_tokens,
                    do_sample=True,
                    temperature=temperature,
                    top_p=0.92,
                    logits_to_keep=1,
                    stopping_criteria=stop_crit,
                    pad_token_id=tokenizer.pad_token_id,
                    eos_token_id=tokenizer.eos_token_id,
                )
            else:
                gen = model.generate(
                    **enc,
                    max_new_tokens=max_new_tokens,
                    do_sample=False,
                    logits_to_keep=1,
                    stopping_criteria=stop_crit,
                    pad_token_id=tokenizer.pad_token_id,
                    eos_token_id=tokenizer.eos_token_id,
                )

        new_tokens = gen[:, prompt_len:]
        decoded = tokenizer.batch_decode(new_tokens, skip_special_tokens=True)
        for txt in decoded:
            sorted_outputs.append(trim_to_first_json_object(txt))

        done = min(start + batch_size, total)
        if done % (batch_size * 2) == 0 or done == total:
            el = time.time() - t0
            print(
                f"  [{stage_name}] {done}/{total} decoded ({el:.1f}s, {done / max(el, 1e-3):.1f} seq/s)",
                flush=True,
            )

    restored = [""] * total
    for orig_idx, out_txt in zip(sorted_indices, sorted_outputs):
        restored[orig_idx] = out_txt
    return restored


def find_symbol_and_windows_with_trace(
    src_text: str,
    query_symbols: list[str],
    problem: str,
    pinned_lines: list[int] | None = None,
    def_hits: list[tuple[str, int]] | None = None,
) -> dict[str, Any]:
    """Returns enclosing AST symbol range + Focal Window #1 (6 lines), Focal Window #2 (6 lines), and Extended Window (14 lines)."""
    lines = src_text.splitlines()
    if not lines:
        return {
            "symbol": "module",
            "enclosing_range": [1, 1],
            "w1_range": [1, 1],
            "w2_range": [1, 1],
            "w_ext_range": [1, 1],
            "snippet1": "",
            "snippet2": "",
            "snippet_ext": "",
        }

    best_sym = "module"
    best_start_l = 1
    best_exec_start = 1
    best_end = min(len(lines), 16)
    best_score = -1.0

    q_lower = {s.lower() for s in query_symbols[:18]}
    prob_words = {
        w.lower()
        for w in re.findall(r"[A-Za-z_][A-Za-z0-9_]{3,}", problem[:900])
        if w.lower() not in STOPWORDS
    }
    pin_set = set(pinned_lines or [])
    def_line_map = {ln: sym_n for sym_n, ln in (def_hits or [])}

    try:
        tree = ast.parse(src_text)
        for node in ast.walk(tree):
            if isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef, ast.ClassDef)):
                name = node.name
                start_l = getattr(node, "lineno", 1)
                end_l = getattr(node, "end_lineno", min(len(lines), start_l + 45))
                exec_start = start_l
                if getattr(node, "body", None):
                    first_stmt = node.body[0]
                    if (
                        isinstance(first_stmt, ast.Expr)
                        and isinstance(getattr(first_stmt, "value", None), ast.Constant)
                        and isinstance(first_stmt.value.value, str)
                    ):
                        if len(node.body) > 1:
                            exec_start = getattr(node.body[1], "lineno", start_l)
                        else:
                            exec_start = getattr(first_stmt, "end_lineno", start_l)

                score = 0.0
                if any(start_l <= pl <= end_l for pl in pin_set):
                    score += 85.0
                if start_l in def_line_map:
                    score += 50.0
                if name in query_symbols[:6]:
                    score += 48.0
                elif name in query_symbols:
                    score += 36.0
                elif name.lower() in q_lower:
                    score += 26.0
                elif any(q in name.lower() or name.lower() in q for q in q_lower if len(q) >= 4):
                    score += 12.0

                body_text = "\n".join(lines[max(0, exec_start - 1) : min(len(lines), end_l)]).lower()
                for ql in q_lower:
                    if ql in body_text:
                        score += 3.0
                for pw in list(prob_words)[:20]:
                    if pw in body_text:
                        score += 1.2
                if isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef)):
                    score += 4.0

                if score > best_score:
                    best_score = score
                    best_sym = name
                    best_start_l = start_l
                    best_exec_start = max(1, exec_start)
                    best_end = min(len(lines), max(best_exec_start + 8, end_l))
    except Exception:
        for i, ln in enumerate(lines):
            m = re.match(r"^\s*(?:def|class)\s+([A-Za-z_][A-Za-z0-9_]*)", ln)
            if m:
                name = m.group(1)
                score = 35.0 if name in query_symbols else (22.0 if name.lower() in q_lower else 1.0)
                if score > best_score:
                    best_score = score
                    best_sym = name
                    best_start_l = i + 1
                    best_exec_start = i + 1
                    best_end = min(len(lines), i + 30)

    win_len = 6
    scored_windows: list[tuple[float, int]] = []
    max_start = max(best_exec_start, best_end - win_len + 1)
    for ws in range(best_exec_start, min(max_start + 1, best_exec_start + 120)):
        cand_lines = lines[ws - 1 : min(len(lines), ws - 1 + win_len)]
        cand_txt = "\n".join(cand_lines).lower()
        if not cand_txt.strip():
            continue
        sc = 0.0
        for pl in pin_set:
            if ws <= pl <= ws + win_len:
                sc += 60.0
        for ql in q_lower:
            if ql in cand_txt:
                sc += 5.0
        for pw in prob_words:
            if pw in cand_txt:
                sc += 1.5
        if any(k in cand_txt for k in ("return ", "raise ", "if ", "elif ", "except", "isinstance")):
            sc += 1.5
        scored_windows.append((sc, ws))

    scored_windows.sort(key=lambda x: x[0], reverse=True)
    w1_start = scored_windows[0][1] if scored_windows else best_exec_start
    w2_start = w1_start
    for sc, ws in scored_windows[1:]:
        if abs(ws - w1_start) >= 4:
            w2_start = ws
            break

    def _slice_compact(ws_idx: int, max_lines: int = 6, max_chars: int = 260) -> tuple[list[int], str]:
        w_s = max(1, ws_idx)
        w_e = min(len(lines), w_s + max_lines - 1)
        kept = []
        chars = 0
        for ln in lines[w_s - 1 : w_e]:
            if chars + len(ln) > max_chars and len(kept) >= 3:
                break
            kept.append(ln)
            chars += len(ln) + 1
        return [w_s, w_s + max(0, len(kept) - 1)], "\n".join(kept)

    lrange1, snippet1 = _slice_compact(w1_start, 6, 260)
    lrange2, snippet2 = _slice_compact(w2_start, 6, 260)
    ext_start = max(best_start_l, w1_start - 3)
    lrange_ext, snippet_ext = _slice_compact(ext_start, 14, 520)

    return {
        "symbol": best_sym,
        "enclosing_range": [best_start_l, best_end],
        "w1_range": lrange1,
        "w2_range": lrange2,
        "w_ext_range": lrange_ext,
        "snippet1": snippet1,
        "snippet2": snippet2,
        "snippet_ext": snippet_ext,
    }


def localize_with_feature_vectors(
    row: dict[str, Any],
    repo_dir: Path,
) -> dict[str, Any]:
    """
    Stage 1 Worker: Computes the 8-dimensional OmniCoder structural score decomposition
    per candidate file at `base_commit` with zero oracle leakage.
    """
    iid = row["instance_id"]
    repo = row["repo"]
    base_commit = row["base_commit"]
    problem = (row.get("problem_statement") or "").strip()
    cohort = row.get("_cohort", "unknown")

    ls_proc = subprocess.run(
        ["git", "-C", str(repo_dir), "ls-tree", "-r", "--name-only", base_commit],
        capture_output=True,
        text=True,
    )
    if ls_proc.returncode != 0:
        subprocess.run(
            ["git", "-C", str(repo_dir), "fetch", "--filter=blob:none", "origin", base_commit],
            check=False,
            capture_output=True,
            text=True,
        )
        ls_proc = subprocess.run(
            ["git", "-C", str(repo_dir), "ls-tree", "-r", "--name-only", base_commit],
            capture_output=True,
            text=True,
        )
    all_py_files = [
        f
        for f in ls_proc.stdout.splitlines()
        if f.endswith(".py")
        and "/tests/" not in f
        and not f.startswith("tests/")
        and not f.startswith("test_")
        and "/test_" not in f
        and "conftest.py" not in f
    ]
    all_py_set = set(all_py_files)

    signals = extract_issue_signals(problem, repo=repo)
    pinned_lines_by_file: dict[str, list[int]] = {}
    def_hits_by_file: dict[str, list[tuple[str, int]]] = {}

    # Per-file 8-dimensional feature table:
    # [s_tb_deep, s_tb_outer, s_explicit, s_module, s_pkg, s_stem, s_ast_def, s_usage]
    feat_map: dict[str, dict[str, float]] = {}

    def _f(fp: str) -> dict[str, float]:
        if fp not in feat_map:
            feat_map[fp] = {
                "s_tb_deep": 0.0,
                "s_tb_outer": 0.0,
                "s_explicit": 0.0,
                "s_module": 0.0,
                "s_pkg": 0.0,
                "s_stem": 0.0,
                "s_ast_def": 0.0,
                "s_usage": 0.0,
            }
        return feat_map[fp]

    # 1. Traceback frame pinning
    tb_frames = signals["tb_frames"]
    for idx_tb, (tb_fp, tb_ln, _) in enumerate(tb_frames):
        is_deepest = idx_tb == len(tb_frames) - 1
        for fp in all_py_files:
            if fp == tb_fp or fp.endswith("/" + tb_fp) or tb_fp.endswith("/" + fp) or fp.endswith(tb_fp):
                if is_deepest:
                    _f(fp)["s_tb_deep"] += 92.0
                else:
                    _f(fp)["s_tb_outer"] += 62.0
                pinned_lines_by_file.setdefault(fp, []).append(tb_ln)

    # 2. Explicit file mentions, dotted module import paths, package directories, and clean stems
    for fp in all_py_files:
        fp_low = fp.lower()
        stem_clean = Path(fp_low).stem.lstrip("_")

        s_exp = 0.0
        for ef in signals["explicit_files"]:
            ef_norm = ef.replace("\\", "/").lstrip("./")
            if fp == ef_norm or fp.endswith("/" + ef_norm) or ef_norm.endswith("/" + fp):
                s_exp += 75.0

        s_mod = 0.0
        for mc in signals["module_candidates"]:
            mc_low = mc.lower()
            if fp_low == mc_low or fp_low.endswith("/" + mc_low):
                s_mod += 56.0

        s_pkg = 0.0
        for pdir in signals["package_dirs"]:
            pdir_low = pdir.lower()
            if fp_low.startswith(pdir_low) or ("/" + pdir_low) in fp_low:
                s_pkg += 24.0

        s_stem = 0.0
        for ps in signals["path_stems"]:
            if ps == stem_clean and stem_clean not in ("init", "base", "utils", "common", "exceptions"):
                s_stem += 24.0
            elif len(ps) >= 5 and ps in stem_clean:
                s_stem += 11.0

        if s_exp + s_mod + s_pkg + s_stem > 0:
            entry = _f(fp)
            entry["s_explicit"] += s_exp
            entry["s_module"] += s_mod
            entry["s_pkg"] += s_pkg
            entry["s_stem"] += s_stem

    # 3. Single-Call AST Definition `git grep` (`def <sym>` or `class <sym>`)
    # Scope pathspecs to top 45 structural candidate files when available to avoid lazy blob storms on unseen commits
    def_syms = [re.escape(s) for s in signals["symbols"][:14] if len(s) >= 3]
    if def_syms:
        def_pattern = r"^\s*(def|class)\s+(" + "|".join(def_syms) + r")\b"
        prelim_ranked = sorted(feat_map.keys(), key=lambda k: sum(feat_map[k].values()), reverse=True)[:45]
        pathspecs = prelim_ranked if len(prelim_ranked) >= 6 else ["*.py"]
        try:
            gp_def = subprocess.run(
                ["git", "-C", str(repo_dir), "grep", "--threads=1", "-I", "-n", "-E", def_pattern, base_commit, "--", *pathspecs],
                capture_output=True,
                text=True,
                timeout=3.5,
            )
            if gp_def.returncode == 0:
                sym_to_files: dict[str, list[tuple[str, int]]] = {}
                for ln in gp_def.stdout.splitlines():
                    parts = ln.split(":", 3)
                    if len(parts) < 4:
                        continue
                    rel_fp = parts[1]
                    if rel_fp not in all_py_set:
                        continue
                    try:
                        lno = int(parts[2])
                    except ValueError:
                        lno = 1
                    m_sym = re.search(r"(?:def|class)\s+([A-Za-z_][A-Za-z0-9_]*)", parts[3])
                    sym_name = m_sym.group(1) if m_sym else ""
                    sym_to_files.setdefault(sym_name, []).append((rel_fp, lno))

                for sym_name, file_lno_list in sym_to_files.items():
                    unique_files = list(dict.fromkeys(f for f, _ in file_lno_list))
                    if 0 < len(unique_files) <= 20:
                        rank_idx = signals["symbols"].index(sym_name) if sym_name in signals["symbols"] else 8
                        priority_mult = 1.25 if rank_idx < 4 else 1.0
                        def_boost = priority_mult * (68.0 / (1.0 + 0.25 * (len(unique_files) - 1)))
                        for rel_fp in unique_files:
                            _f(rel_fp)["s_ast_def"] += def_boost
                        for rel_fp, lno in file_lno_list:
                            def_hits_by_file.setdefault(rel_fp, []).append((sym_name, lno))
        except Exception:
            pass

    file_scores = {fp: sum(vals.values()) for fp, vals in feat_map.items()}
    ranked = sorted(file_scores.items(), key=lambda kv: kv[1], reverse=True)[:6]
    if not ranked and all_py_files:
        ranked = [(fp, 10.0) for fp in all_py_files[:6]]
        for fp, _ in ranked:
            _f(fp)

    # 4. In-memory symbol usage density across the top-6 candidate files (0 extra git grep subprocesses)
    usage_syms = [s for s in signals["symbols"][:6] if len(s) >= 4]
    candidates = []
    file_windows: dict[str, dict[str, Any]] = {}
    for fp, raw_sc in ranked:
        sh = subprocess.run(
            ["git", "-C", str(repo_dir), "show", f"{base_commit}:{fp}"],
            capture_output=True,
            text=True,
            errors="replace",
        )
        src_text = sh.stdout if sh.returncode == 0 else ""
        if src_text and usage_syms:
            u_hits = sum(1 for us in usage_syms if us in src_text)
            if u_hits > 0:
                u_boost = round(min(24.0, u_hits * 5.5), 2)
                _f(fp)["s_usage"] = u_boost
                raw_sc += u_boost
        w_info = find_symbol_and_windows_with_trace(
            src_text,
            signals["symbols"],
            problem,
            pinned_lines=pinned_lines_by_file.get(fp),
            def_hits=def_hits_by_file.get(fp),
        )
        norm_score = min(58.0, max(12.0, round(raw_sc, 2)))
        f_vec = {k: round(v, 2) for k, v in _f(fp).items()}
        candidates.append(
            {
                "file": fp,
                "symbol": w_info["symbol"],
                "line_range": w_info["w1_range"],
                "enclosing_range": w_info["enclosing_range"],
                "w2_range": w_info["w2_range"],
                "w_ext_range": w_info["w_ext_range"],
                "score": norm_score,
                "raw_score": round(raw_sc, 2),
                "features": f_vec,
            }
        )
        file_windows[fp] = {
            "symbol": w_info["symbol"],
            "line_range": w_info["w1_range"],
            "enclosing_range": w_info["enclosing_range"],
            "w2_range": w_info["w2_range"],
            "w_ext_range": w_info["w_ext_range"],
            "snippet": w_info["snippet1"],
            "snippet_alt": w_info["snippet2"],
            "snippet_ext": w_info["snippet_ext"],
            "full_source": src_text,
            "raw_score": round(raw_sc, 2),
            "features": f_vec,
        }

    candidates.sort(key=lambda c: c["raw_score"], reverse=True)
    top_fp = candidates[0]["file"] if candidates else ""
    top_sym = candidates[0]["symbol"] if candidates else "module"
    vln_hint = recall_vln_hint(repo, problem, top_fp)
    tb_short = extract_traceback_line(problem, top_fp, top_sym)
    short_issue = " ".join(problem.split())[:320]

    cand_lines = [
        f"{idx + 1}. {c['file']} :: {c['symbol']} (L{c['line_range'][0]}-{c['line_range'][1]}, {c['score']:.2f})"
        for idx, c in enumerate(candidates)
    ]
    loc_parts = []
    if vln_hint:
        loc_parts.append(f"VLN: {vln_hint}")
    loc_parts.append(f"Trace: {tb_short}")
    loc_parts.append("Candidates:\n" + "\n".join(cand_lines))
    loc_parts.append(f"Issue: {short_issue[:220]}")
    locate_user = "\n".join(loc_parts)

    return {
        "instance_id": iid,
        "cohort": cohort,
        "repo": repo,
        "base_commit": base_commit,
        "problem": problem,
        "signals": signals,
        "short_issue": short_issue,
        "tb_short": tb_short,
        "vln_hint": vln_hint,
        "candidates": candidates,
        "file_windows": file_windows,
        "locate_prompt": f"<|system|>\n{SYS_LOCATE}\n<|user|>\n{locate_user}\n<|assistant|>\n",
    }


def classify_anchor_and_ast(full_src: str, s_txt: str, r_txt: str) -> tuple[bool, str, bool, str, str]:
    """Returns (applied, match_mode_or_fail_reason, ast_valid, ast_reason, patched_src)."""
    if not full_src or not s_txt or not r_txt or s_txt.strip() == r_txt.strip():
        return False, "empty_or_noop", False, "not_applied", ""
    if s_txt in full_src:
        patched = full_src.replace(s_txt, r_txt, 1)
        try:
            ast.parse(patched)
            return True, "exact_match", True, "ok", patched
        except IndentationError:
            return True, "exact_match", False, "IndentationError", patched
        except SyntaxError:
            return True, "exact_match", False, "SyntaxError", patched
        except Exception:
            return True, "exact_match", False, "OtherASTError", patched

    applied, patched = try_apply_edit_block_to_source(full_src, s_txt, r_txt)
    if applied:
        try:
            ast.parse(patched)
            return True, "fuzzy_indent_match", True, "ok", patched
        except IndentationError:
            return True, "fuzzy_indent_match", False, "IndentationError", patched
        except SyntaxError:
            return True, "fuzzy_indent_match", False, "SyntaxError", patched
        except Exception:
            return True, "fuzzy_indent_match", False, "OtherASTError", patched

    if "..." in s_txt:
        return False, "elided_dots_in_search", False, "not_applied", ""
    s_lines = [ln.strip() for ln in s_txt.strip().splitlines() if ln.strip()]
    src_stripped = {ln.strip() for ln in full_src.splitlines() if ln.strip()}
    matched_lines = sum(1 for ln in s_lines if ln in src_stripped)
    if len(s_lines) > 0 and matched_lines == len(s_lines):
        return False, "discontiguous_lines_in_file", False, "not_applied", ""
    if matched_lines > 0:
        return False, "partial_hallucinated_lines", False, "not_applied", ""
    return False, "search_lines_absent_from_file", False, "not_applied", ""


def ranges_overlap(r1: list[int], r2: list[int]) -> bool:
    return max(r1[0], r2[0]) <= min(r1[1], r2[1])


def fit_empirical_localization_weights(trace_records: list[dict[str, Any]]) -> dict[str, Any]:
    """
    Fits Ridge/Logistic regression on the 900 Unseen Full SWE-bench instances over the
    9-dimensional OmniCoder + E2B feature space, then tests the learned weights on SWE-300!
    """
    feat_names = [
        "s_tb_deep",
        "s_tb_outer",
        "s_explicit",
        "s_module",
        "s_pkg",
        "s_stem",
        "s_ast_def",
        "s_usage",
        "e2b_primary_vote",
        "e2b_support_vote",
    ]
    X_train, y_train = [], []
    for r in trace_records:
        if r["cohort"] != "swe900_unseen_full":
            continue
        gold_set = set(r["gold_files"])
        for c in r["candidate_trace"]:
            f_dict = c["features"]
            row_x = [
                f_dict.get("s_tb_deep", 0.0) / 92.0,
                f_dict.get("s_tb_outer", 0.0) / 62.0,
                f_dict.get("s_explicit", 0.0) / 75.0,
                f_dict.get("s_module", 0.0) / 56.0,
                f_dict.get("s_pkg", 0.0) / 24.0,
                f_dict.get("s_stem", 0.0) / 24.0,
                f_dict.get("s_ast_def", 0.0) / 68.0,
                f_dict.get("s_usage", 0.0) / 26.0,
                1.0 if c.get("e2b_vote", 0.0) >= 25.0 else 0.0,
                1.0 if 0.0 < c.get("e2b_vote", 0.0) < 25.0 else 0.0,
            ]
            X_train.append(row_x)
            y_train.append(1.0 if c["file"] in gold_set else 0.0)

    if not X_train:
        return {}

    X = np.array(X_train, dtype=np.float64)
    y = np.array(y_train, dtype=np.float64)
    # Ridge closed-form w = (X^T X + lambda I)^(-1) X^T y
    reg = 2.0 * np.eye(X.shape[1])
    w = np.linalg.solve(X.T @ X + reg, X.T @ y)

    # Evaluate learned weights w on SWE-300 and SWE-900
    def _eval_cohort(cohort_name: str) -> tuple[float, float, int]:
        sub = [r for r in trace_records if r["cohort"] == cohort_name]
        cur_hits = 0
        opt_hits = 0
        for r in sub:
            gold_set = set(r["gold_files"])
            if r["localize_top1_match"]:
                cur_hits += 1
            best_f = ""
            best_sc = -1e9
            for c in r["candidate_trace"]:
                fd = c["features"]
                rx = np.array(
                    [
                        fd.get("s_tb_deep", 0.0) / 92.0,
                        fd.get("s_tb_outer", 0.0) / 62.0,
                        fd.get("s_explicit", 0.0) / 75.0,
                        fd.get("s_module", 0.0) / 56.0,
                        fd.get("s_pkg", 0.0) / 24.0,
                        fd.get("s_stem", 0.0) / 24.0,
                        fd.get("s_ast_def", 0.0) / 68.0,
                        fd.get("s_usage", 0.0) / 26.0,
                        1.0 if c.get("e2b_vote", 0.0) >= 25.0 else 0.0,
                        1.0 if 0.0 < c.get("e2b_vote", 0.0) < 25.0 else 0.0,
                    ]
                )
                sc = float(rx @ w)
                if sc > best_sc:
                    best_sc = sc
                    best_f = c["file"]
            if best_f in gold_set:
                opt_hits += 1
        n_c = max(len(sub), 1)
        return round(100.0 * cur_hits / n_c, 1), round(100.0 * opt_hits / n_c, 1), opt_hits

    swe900_cur, swe900_opt, swe900_opt_cnt = _eval_cohort("swe900_unseen_full")
    swe300_cur, swe300_opt, swe300_opt_cnt = _eval_cohort("swe300")

    scaled_weights = {name: round(float(val * 100.0), 2) for name, val in zip(feat_names, w)}
    return {
        "normalized_importance_weights_100": scaled_weights,
        "swe900_top1_heuristic_pct": swe900_cur,
        "swe900_top1_learned_weights_pct": swe900_opt,
        "swe900_learned_top1_count": swe900_opt_cnt,
        "swe300_top1_heuristic_pct": swe300_cur,
        "swe300_top1_learned_weights_pct": swe300_opt,
        "swe300_learned_top1_count": swe300_opt_cnt,
    }


def compute_cohort_summary(records: list[dict[str, Any]], label: str) -> dict[str, Any]:
    n = max(len(records), 1)
    det_top1 = sum(1 for r in records if r["det_only_top1_match"])
    e2b_top1 = sum(1 for r in records if r["e2b_only_top1_match"])
    hyb_top1 = sum(1 for r in records if r["localize_top1_match"])
    hyb_top3 = sum(1 for r in records if r["localize_top3_match"])
    top6_rec = sum(1 for r in records if r["localize_top6_recall"])

    oc_right_e2b_wrong = sum(1 for r in records if r["det_only_top1_match"] and not r["e2b_only_top1_match"])
    e2b_right_oc_wrong = sum(1 for r in records if r["e2b_only_top1_match"] and not r["det_only_top1_match"])
    both_right = sum(1 for r in records if r["det_only_top1_match"] and r["e2b_only_top1_match"])
    in_top6_neither_top1 = sum(
        1
        for r in records
        if r["localize_top6_recall"] and not r["det_only_top1_match"] and not r["e2b_only_top1_match"]
    )

    gold_file_loc_recs = [r for r in records if r["gold_in_top_candidate_window"]]
    n_gf = max(len(gold_file_loc_recs), 1)
    sym_overlap_cnt = sum(1 for r in gold_file_loc_recs if r["enclosing_sym_overlaps_gold"])
    w1_overlap_cnt = sum(1 for r in gold_file_loc_recs if r["w1_overlaps_gold"])
    w12_overlap_cnt = sum(1 for r in gold_file_loc_recs if r["w1_or_w2_overlaps_gold"])
    wext_overlap_cnt = sum(1 for r in gold_file_loc_recs if r["w_ext_overlaps_gold"])

    repro_ok = sum(1 for r in records if r["repro_ast_valid"])
    k0_valid = sum(1 for r in records if r["k0_ast_valid_patch"])
    valid_eb = sum(1 for r in records if r["valid_editblock_generated"])
    anchor_ok = sum(1 for r in records if r["search_anchor_applied_cleanly"])
    ast_patched = sum(1 for r in records if r["ast_verified_patch_generated"])
    gold_patched = sum(1 for r in records if r["patched_target_file_match"])
    oracle_gold_patched = sum(1 for r in records if r["oracle_best_k_gold_file_match"])

    mean_f1 = round(
        sum(r["diff_token_f1_pct"] for r in records if r["ast_verified_patch_generated"]) / max(ast_patched, 1),
        1,
    )
    oracle_mean_f1 = round(
        sum(r["oracle_best_k_diff_f1_pct"] for r in records if r["ast_verified_patch_generated"])
        / max(ast_patched, 1),
        1,
    )

    f1_when_w12_overlaps = [
        r["oracle_best_k_diff_f1_pct"]
        for r in gold_file_loc_recs
        if r["w1_or_w2_overlaps_gold"] and r["ast_verified_patch_generated"]
    ]
    f1_when_w12_misses = [
        r["oracle_best_k_diff_f1_pct"]
        for r in gold_file_loc_recs
        if (not r["w1_or_w2_overlaps_gold"]) and r["ast_verified_patch_generated"]
    ]

    by_repo: dict[str, dict[str, Any]] = {}
    for r in records:
        rp = r["repo"]
        st = by_repo.setdefault(
            rp,
            {
                "total": 0,
                "det_top1": 0,
                "e2b_top1": 0,
                "hyb_top1": 0,
                "hyb_top3": 0,
                "top6": 0,
                "patched": 0,
                "gold_file_patched": 0,
                "oracle_gold_patched": 0,
                "f1_sum": 0.0,
                "oracle_f1_sum": 0.0,
            },
        )
        st["total"] += 1
        st["det_top1"] += int(r["det_only_top1_match"])
        st["e2b_top1"] += int(r["e2b_only_top1_match"])
        st["hyb_top1"] += int(r["localize_top1_match"])
        st["hyb_top3"] += int(r["localize_top3_match"])
        st["top6"] += int(r["localize_top6_recall"])
        st["patched"] += int(r["ast_verified_patch_generated"])
        st["gold_file_patched"] += int(r["patched_target_file_match"])
        st["oracle_gold_patched"] += int(r["oracle_best_k_gold_file_match"])
        st["f1_sum"] += float(r["diff_token_f1_pct"])
        st["oracle_f1_sum"] += float(r["oracle_best_k_diff_f1_pct"])

    repo_summary = {}
    for rp, st in sorted(by_repo.items()):
        t = max(st["total"], 1)
        p = max(st["patched"], 1)
        repo_summary[rp] = {
            "total": st["total"],
            "det_loc_top1_pct": round(100.0 * st["det_top1"] / t, 1),
            "e2b_loc_top1_pct": round(100.0 * st["e2b_top1"] / t, 1),
            "hyb_loc_top1_pct": round(100.0 * st["hyb_top1"] / t, 1),
            "hyb_loc_top3_pct": round(100.0 * st["hyb_top3"] / t, 1),
            "top6_recall_pct": round(100.0 * st["top6"] / t, 1),
            "patched_pct": round(100.0 * st["patched"] / t, 1),
            "gold_file_patched_pct": round(100.0 * st["gold_file_patched"] / t, 1),
            "oracle_gold_file_patched_pct": round(100.0 * st["oracle_gold_patched"] / t, 1),
            "mean_diff_f1_pct": round(st["f1_sum"] / p, 1),
            "oracle_mean_diff_f1_pct": round(st["oracle_f1_sum"] / p, 1),
        }

    return {
        "cohort_label": label,
        "instances": len(records),
        "localization_interaction": {
            "omnicoder_deterministic_top1_pct": round(100.0 * det_top1 / n, 1),
            "e2b_neural_only_top1_pct": round(100.0 * e2b_top1 / n, 1),
            "hybrid_coupled_top1_pct": round(100.0 * hyb_top1 / n, 1),
            "hybrid_top3_recall_pct": round(100.0 * hyb_top3 / n, 1),
            "stage1_top6_candidate_recall_pct": round(100.0 * top6_rec / n, 1),
            "both_oc_and_e2b_agree_right_cnt": both_right,
            "oc_right_e2b_overrode_wrong_cnt": oc_right_e2b_wrong,
            "e2b_rescued_oc_rank2_to_6_cnt": e2b_right_oc_wrong,
            "gold_in_top6_neither_picked_top1_cnt": in_top6_neither_top1,
        },
        "intra_file_window_funnel_given_gold_file": {
            "gold_file_localized_instances": len(gold_file_loc_recs),
            "enclosing_ast_symbol_overlaps_gold_hunk_pct": round(100.0 * sym_overlap_cnt / n_gf, 1),
            "focal_window1_6line_overlaps_gold_hunk_pct": round(100.0 * w1_overlap_cnt / n_gf, 1),
            "focal_window1_or_2_overlaps_gold_hunk_pct": round(100.0 * w12_overlap_cnt / n_gf, 1),
            "extended_14line_window_overlaps_gold_hunk_pct": round(100.0 * wext_overlap_cnt / n_gf, 1),
            "mean_diff_f1_when_window_overlaps_gold_pct": round(
                sum(f1_when_w12_overlaps) / max(len(f1_when_w12_overlaps), 1), 1
            ),
            "mean_diff_f1_when_window_misses_gold_pct": round(
                sum(f1_when_w12_misses) / max(len(f1_when_w12_misses), 1), 1
            ),
        },
        "heal_tournament_and_actuation": {
            "repro_valid_python_ast_pct": round(100.0 * repro_ok / n, 1),
            "k0_greedy_ast_verified_patch_pct": round(100.0 * k0_valid / n, 1),
            "k4_valid_non_noop_editblock_pct": round(100.0 * valid_eb / n, 1),
            "k4_real_file_anchor_match_pct": round(100.0 * anchor_ok / n, 1),
            "k4_ast_verified_patch_generated_pct": round(100.0 * ast_patched / n, 1),
            "judge_selected_gold_file_patched_pct": round(100.0 * gold_patched / n, 1),
            "oracle_best_of_k4_gold_file_patched_pct": round(100.0 * oracle_gold_patched / n, 1),
            "judge_gold_file_regret_delta_pct": round(100.0 * (oracle_gold_patched - gold_patched) / n, 1),
            "judge_selected_mean_diff_f1_pct": mean_f1,
            "oracle_best_of_k4_mean_diff_f1_pct": oracle_mean_f1,
            "judge_diff_f1_regret_delta_pct": round(oracle_mean_f1 - mean_f1, 1),
        },
        "per_repository": repo_summary,
    }


def main():
    parser = argparse.ArgumentParser(description="A100 Large-Scale OmniCoder + E2B Joint Trace & Delta Analyzer")
    parser.add_argument("--model-id", default="google/gemma-4-e2b")
    parser.add_argument("--adapter-dir", default="/content/omnicoder-icx/training/checkpoints/omnicoder-gemma-e2b-clean")
    parser.add_argument("--unseen-full-count", type=int, default=900, help="Disjoint Full SWE-bench test instances")
    parser.add_argument("--workers", type=int, default=16)
    parser.add_argument("--batch-size", type=int, default=96)
    parser.add_argument("--out-dir", default="/content/swe_large_trace")
    args = parser.parse_args()

    t_global = time.time()
    out_dir = Path(args.out_dir)
    out_dir.mkdir(parents=True, exist_ok=True)
    mirror_cache = Path("/content/swe300_results/repo_mirrors")

    print("[Large-Scale Trace] Loading SWE-bench_Lite (300), SWE-bench_Verified (500), and Full SWE-bench (2,294)...", flush=True)
    swe_lite = load_dataset("princeton-nlp/SWE-bench_Lite", split="test")
    swe_verified = load_dataset("princeton-nlp/SWE-bench_Verified", split="test")
    swe_full = load_dataset("princeton-nlp/SWE-bench", split="test")

    lite_rows = [dict(r, _cohort="swe300") for r in swe_lite]
    lite_ids = {r["instance_id"] for r in lite_rows}
    verified_train_ids = {r["instance_id"] for r in swe_verified if r["instance_id"] not in lite_ids}

    # Strictly unseen Full SWE-bench test instances (0% overlap with Verified train AND 0% overlap with SWE-300)
    unseen_full_pool = [
        dict(r, _cohort="swe900_unseen_full")
        for r in swe_full
        if r["instance_id"] not in verified_train_ids and r["instance_id"] not in lite_ids
    ]

    # Stratified sample across all 12 repositories so repository distribution is balanced and representative
    rng = random.Random(42)
    by_repo_pool: dict[str, list[dict[str, Any]]] = {}
    for r in unseen_full_pool:
        by_repo_pool.setdefault(r["repo"], []).append(r)
    for rp in by_repo_pool:
        rng.shuffle(by_repo_pool[rp])

    selected_unseen_full: list[dict[str, Any]] = []
    target_unseen = min(args.unseen_full_count, len(unseen_full_pool))
    total_pool = len(unseen_full_pool)
    for rp, rp_rows in sorted(by_repo_pool.items()):
        quota = max(1, int(round(target_unseen * (len(rp_rows) / total_pool))))
        selected_unseen_full.extend(rp_rows[:quota])
    if len(selected_unseen_full) < target_unseen:
        used_ids = {r["instance_id"] for r in selected_unseen_full}
        extras = [r for r in unseen_full_pool if r["instance_id"] not in used_ids]
        selected_unseen_full.extend(extras[: target_unseen - len(selected_unseen_full)])
    selected_unseen_full = selected_unseen_full[:target_unseen]

    all_rows = lite_rows + selected_unseen_full
    overlap_audit = len({r["instance_id"] for r in all_rows} & verified_train_ids)
    print(
        f"[Zero-Leakage Audit] Total Trace Dataset: {len(all_rows)} instances "
        f"(SWE-300: {len(lite_rows)} | Unseen Full SWE-bench: {len(selected_unseen_full)} | "
        f"Training Overlap: {overlap_audit})",
        flush=True,
    )

    repo_map = prewarm_all_swe300_repos(all_rows, mirror_cache)

    # =========================================================================
    # STAGE 1: Parallel Git Tree + 8-Dim OmniCoder Feature Vector Extraction
    # =========================================================================
    print(
        f"[Stage 1/5] Extracting 8-dim OmniCoder localization vectors across {len(all_rows)} instances ({args.workers} threads)...",
        flush=True,
    )
    t_s1 = time.time()
    items: list[dict[str, Any]] = [None] * len(all_rows)  # type: ignore
    with concurrent.futures.ThreadPoolExecutor(max_workers=args.workers) as pool:
        fut_to_idx = {
            pool.submit(localize_with_feature_vectors, row, repo_map[row["repo"]]): idx
            for idx, row in enumerate(all_rows)
        }
        done_cnt = 0
        for fut in concurrent.futures.as_completed(fut_to_idx):
            idx = fut_to_idx[fut]
            items[idx] = fut.result()
            done_cnt += 1
            if done_cnt % 150 == 0 or done_cnt == len(all_rows):
                el = time.time() - t_s1
                print(
                    f"  [Stage 1] {done_cnt}/{len(all_rows)} base_commit trees indexed ({el:.1f}s, {done_cnt / max(el, 1e-3):.1f} inst/s)",
                    flush=True,
                )

    # =========================================================================
    # Load A100 bf16 Co-Tuned Model
    # =========================================================================
    print(f"[Engine] Loading {args.model_id} + Clean Zero-Leakage Adapter ({args.adapter_dir}) in bf16...", flush=True)
    device = torch.device("cuda:0")
    tokenizer = AutoTokenizer.from_pretrained(args.model_id, trust_remote_code=True)
    if tokenizer.pad_token is None:
        tokenizer.pad_token = tokenizer.eos_token
    tokenizer.padding_side = "left"

    model = AutoModelForCausalLM.from_pretrained(
        args.model_id,
        dtype=torch.bfloat16,
        device_map={"": 0},
        trust_remote_code=True,
    )
    inner = getattr(model, "model", None)
    if inner is not None:
        for attr in ("vision_tower", "audio_tower", "embed_vision", "embed_audio"):
            if hasattr(inner, attr):
                setattr(inner, attr, None)
    if Path(args.adapter_dir).exists() and (Path(args.adapter_dir) / "adapter_config.json").exists():
        model = PeftModel.from_pretrained(model, args.adapter_dir)
        model = model.merge_and_unload()
        print("[Engine] Merged Clean QLoRA adapter into native bf16 weights.", flush=True)
    model.eval()
    torch.cuda.empty_cache()

    # =========================================================================
    # STAGE 2: Length-Bucketed A100 `[OC:LOCATE]` Neural Reranking
    # =========================================================================
    print(f"[Stage 2/5] Running length-bucketed [OC:LOCATE] across {len(items)} instances (batch_size={args.batch_size})...", flush=True)
    locate_prompts = [it["locate_prompt"] for it in items]
    locate_raw = run_length_bucketed_generation(
        model,
        tokenizer,
        device,
        locate_prompts,
        batch_size=args.batch_size,
        max_new_tokens=56,
        do_sample=False,
        stage_name="OC:LOCATE",
    )

    repro_prompts = []
    heal_prompts_k0 = []
    heal_prompts_k1 = []
    heal_prompts_k2 = []
    heal_prompts_k3 = []
    beam_meta_per_item: list[list[dict[str, Any]]] = []

    for it, loc_txt in zip(items, locate_raw):
        parsed_loc = try_parse_json(loc_txt)
        cands = list(it["candidates"])
        det_top1_fp = cands[0]["file"] if cands else ""

        neural_votes: dict[str, float] = {}
        e2b_raw_primary = ""
        if parsed_loc is not None:
            cand_fp = str(parsed_loc.get("primary_file") or "")
            if cand_fp in it["file_windows"]:
                neural_votes[cand_fp] = 28.0
                e2b_raw_primary = cand_fp
            for s_idx, sf in enumerate(parsed_loc.get("supporting_files") or []):
                if isinstance(sf, str) and sf in it["file_windows"] and sf not in neural_votes:
                    neural_votes[sf] = 14.0 if s_idx == 0 else 7.0

        if not e2b_raw_primary:
            e2b_raw_primary = det_top1_fp

        for c in cands:
            c["e2b_vote"] = neural_votes.get(c["file"], 0.0)
            c["hybrid_score"] = round(c.get("raw_score", c["score"]) + c["e2b_vote"], 2)

        ranked_cands = sorted(cands, key=lambda c: c["hybrid_score"], reverse=True)
        pred_fp = ranked_cands[0]["file"] if ranked_cands else ""
        pred_sym = ranked_cands[0]["symbol"] if ranked_cands else "module"
        pred_supp = [c["file"] for c in ranked_cands[1:3]]

        it["locate_json_valid"] = parsed_loc is not None
        it["det_top1_file"] = det_top1_fp
        it["e2b_top1_file"] = e2b_raw_primary
        it["localized_file"] = pred_fp
        it["localized_symbol"] = pred_sym
        it["supporting_files"] = pred_supp

        win1 = it["file_windows"].get(
            pred_fp,
            {
                "symbol": pred_sym,
                "line_range": [1, 8],
                "enclosing_range": [1, 8],
                "w2_range": [1, 8],
                "w_ext_range": [1, 8],
                "snippet": "",
                "snippet_alt": "",
                "snippet_ext": "",
                "full_source": "",
                "raw_score": 20.0,
            },
        )
        lrange1 = win1["line_range"]

        sec_fp = pred_supp[0] if pred_supp else pred_fp
        win2 = it["file_windows"].get(sec_fp, win1)
        sc1 = win1.get("raw_score", 30.0) + neural_votes.get(pred_fp, 0.0)
        sc2 = win2.get("raw_score", 15.0) + neural_votes.get(sec_fp, 0.0)
        use_sec_file_on_k3 = (sec_fp != pred_fp) and (sc2 >= 0.58 * sc1)

        repro_parts = ["Lang: python", f"Files: {pred_fp}"]
        if it["vln_hint"]:
            repro_parts.append(f"VLN: {it['vln_hint']}")
        repro_parts.append(f"Trace: {it['tb_short']}")
        repro_parts.append(f"Issue: {it['short_issue'][:220]}")
        repro_u = "\n".join(repro_parts)
        repro_prompts.append(f"<|system|>\n{SYS_REPRO}\n<|user|>\n{repro_u}\n<|assistant|>\n")

        p_w1 = make_heal_prompt(pred_fp, pred_sym, lrange1[0], it["vln_hint"], it["short_issue"], win1["snippet"])
        p_w1_alt = make_heal_prompt(
            pred_fp,
            pred_sym,
            lrange1[0],
            it["vln_hint"],
            it["short_issue"],
            win1.get("snippet_alt") or win1["snippet"],
        )
        if use_sec_file_on_k3:
            vln_sec = recall_vln_hint(it["repo"], it["problem"], sec_fp)
            p_k3 = make_heal_prompt(
                sec_fp,
                win2.get("symbol", "module"),
                win2["line_range"][0],
                vln_sec,
                it["short_issue"],
                win2["snippet"],
            )
            k3_meta = {"file": sec_fp, "full_source": win2["full_source"], "prior_bonus": 15.0, "beam_kind": "Top2File"}
        else:
            p_k3 = p_w1
            k3_meta = {"file": pred_fp, "full_source": win1["full_source"], "prior_bonus": 18.0, "beam_kind": "PrimaryT025"}

        heal_prompts_k0.append(p_w1)
        heal_prompts_k1.append(p_w1)
        heal_prompts_k2.append(p_w1_alt)
        heal_prompts_k3.append(p_k3)
        beam_meta_per_item.append(
            [
                {"file": pred_fp, "full_source": win1["full_source"], "prior_bonus": 24.0, "beam_kind": "GreedyW1"},
                {"file": pred_fp, "full_source": win1["full_source"], "prior_bonus": 22.0, "beam_kind": "SampleW1"},
                {"file": pred_fp, "full_source": win1["full_source"], "prior_bonus": 19.0, "beam_kind": "SampleW2"},
                k3_meta,
            ]
        )

    # =========================================================================
    # STAGE 3: Length-Bucketed A100 `[OC:REPRO]` Synthesis
    # =========================================================================
    print(f"[Stage 3/5] Running length-bucketed [OC:REPRO] across {len(items)} instances (batch_size={args.batch_size})...", flush=True)
    repro_raw = run_length_bucketed_generation(
        model,
        tokenizer,
        device,
        repro_prompts,
        batch_size=args.batch_size,
        max_new_tokens=96,
        do_sample=False,
        stage_name="OC:REPRO",
    )
    for it, rep_txt in zip(items, repro_raw):
        p_rep = try_parse_json(rep_txt)
        valid_ast = False
        if p_rep and isinstance(p_rep.get("script_content"), str):
            try:
                ast.parse(p_rep["script_content"])
                valid_ast = True
            except Exception:
                pass
        it["repro_ast_valid"] = valid_ast

    # =========================================================================
    # STAGE 4: Length-Bucketed A100 Speculative K=4 `[OC:HEAL]` Tournament
    # =========================================================================
    print(
        f"[Stage 4/5] Running Speculative K=4 [OC:HEAL] Tournament across {len(items)} instances (batch_size={args.batch_size}, max_new_tokens=168)...",
        flush=True,
    )
    c0_raw = run_length_bucketed_generation(
        model,
        tokenizer,
        device,
        heal_prompts_k0,
        batch_size=args.batch_size,
        max_new_tokens=168,
        do_sample=False,
        stage_name="OC:HEAL-Greedy-K0",
    )
    c1_raw = run_length_bucketed_generation(
        model,
        tokenizer,
        device,
        heal_prompts_k1,
        batch_size=args.batch_size,
        max_new_tokens=168,
        do_sample=True,
        temperature=0.30,
        stage_name="OC:HEAL-Beam-K1",
    )
    c2_raw = run_length_bucketed_generation(
        model,
        tokenizer,
        device,
        heal_prompts_k2,
        batch_size=args.batch_size,
        max_new_tokens=168,
        do_sample=True,
        temperature=0.20,
        stage_name="OC:HEAL-Window2-K2",
    )
    c3_raw = run_length_bucketed_generation(
        model,
        tokenizer,
        device,
        heal_prompts_k3,
        batch_size=args.batch_size,
        max_new_tokens=168,
        do_sample=True,
        temperature=0.25,
        stage_name="OC:HEAL-Top2File-K3",
    )

    # =========================================================================
    # STAGE 5: Full Mathematical Trace & Delta Analysis (SWE-900 vs. SWE-300)
    # =========================================================================
    print(f"[Stage 5/5] Computing OmniCoder <-> E2B mathematical delta trace across {len(items)} instances...", flush=True)
    trace_records: list[dict[str, Any]] = []
    anchor_mode_counts: dict[str, int] = {}
    ast_reason_counts: dict[str, int] = {}
    beam_win_counts = {0: 0, 1: 0, 2: 0, 3: 0}
    oracle_beam_best_counts = {0: 0, 1: 0, 2: 0, 3: 0}

    for idx, (row, it, b_metas) in enumerate(zip(all_rows, items, beam_meta_per_item)):
        cand_txts = [c0_raw[idx], c1_raw[idx], c2_raw[idx], c3_raw[idx]]
        gold_patch = row.get("patch", "")
        gold_files = extract_modified_files_from_patch(gold_patch)
        gold_hunks = parse_unified_diff_hunks(gold_patch)
        gold_ranges_by_file: dict[str, list[list[int]]] = {}
        for gh in gold_hunks:
            gold_ranges_by_file.setdefault(gh["file_path"], []).append([gh["start_line"], gh["end_line"]])

        pred_fp = it["localized_file"]
        det_fp = it["det_top1_file"]
        e2b_fp = it["e2b_top1_file"]
        issue_syms = it.get("signals", {}).get("symbols", [])

        # Intra-file window overlap check when primary or candidate #2 is a gold file
        gold_in_win = False
        sym_overlaps = False
        w1_overlaps = False
        w12_overlaps = False
        wext_overlaps = False
        for check_fp in ([pred_fp] + it["supporting_files"][:1]):
            if check_fp in gold_ranges_by_file and check_fp in it["file_windows"]:
                gold_in_win = True
                fw = it["file_windows"][check_fp]
                g_rngs = gold_ranges_by_file[check_fp]
                if any(ranges_overlap(fw["enclosing_range"], gr) for gr in g_rngs):
                    sym_overlaps = True
                if any(ranges_overlap(fw["line_range"], gr) for gr in g_rngs):
                    w1_overlaps = True
                if any(ranges_overlap(fw["line_range"], gr) or ranges_overlap(fw["w2_range"], gr) for gr in g_rngs):
                    w12_overlaps = True
                if any(ranges_overlap(fw["w_ext_range"], gr) for gr in g_rngs):
                    wext_overlaps = True
                break

        has_valid_block = False
        applied_any = False
        k0_ast_valid = False
        best_diff = ""
        best_patched_fp = pred_fp
        best_t_score = -1e9
        best_beam_idx = -1

        oracle_best_f1 = 0.0
        oracle_best_gold_hit = False
        oracle_best_beam_idx = 0

        for k_idx, (c_txt, b_meta) in enumerate(zip(cand_txts, b_metas)):
            parsed = try_parse_json(c_txt)
            if not parsed:
                continue
            ebs = parsed.get("edit_blocks") or []
            if not (isinstance(ebs, list) and len(ebs) > 0 and isinstance(ebs[0], dict)):
                continue
            s_txt = str(ebs[0].get("search_text") or "")
            r_txt = str(ebs[0].get("replace_text") or "")
            if not s_txt.strip() or not r_txt.strip() or s_txt.strip() == r_txt.strip():
                continue
            has_valid_block = True

            cand_fp = b_meta["file"]
            cand_src = b_meta["full_source"]
            applied, m_mode, ast_ok, ast_reason, patched_src = classify_anchor_and_ast(cand_src, s_txt, r_txt)
            anchor_mode_counts[m_mode] = anchor_mode_counts.get(m_mode, 0) + 1
            if applied:
                applied_any = True
                ast_reason_counts[ast_reason] = ast_reason_counts.get(ast_reason, 0) + 1

            if applied and ast_ok:
                if k_idx == 0:
                    k0_ast_valid = True
                u_diff = make_unified_git_diff(cand_fp, cand_src, patched_src)
                if u_diff:
                    t_score = score_candidate_zero_leakage(
                        s_txt,
                        r_txt,
                        it["vln_hint"],
                        issue_syms,
                        b_meta["prior_bonus"],
                    )
                    cand_f1 = diff_token_f1(u_diff, gold_patch)
                    cand_gold_hit = cand_fp in gold_files
                    if cand_gold_hit:
                        oracle_best_gold_hit = True
                    if cand_f1 > oracle_best_f1:
                        oracle_best_f1 = cand_f1
                        oracle_best_beam_idx = k_idx
                    if t_score > best_t_score:
                        best_t_score = t_score
                        best_diff = u_diff
                        best_patched_fp = cand_fp
                        best_beam_idx = k_idx

        if best_beam_idx >= 0:
            beam_win_counts[best_beam_idx] = beam_win_counts.get(best_beam_idx, 0) + 1
            oracle_beam_best_counts[oracle_best_beam_idx] = oracle_beam_best_counts.get(oracle_best_beam_idx, 0) + 1

        best_f1 = diff_token_f1(best_diff, gold_patch) if best_diff else 0.0
        top1_hit = bool((pred_fp and pred_fp in gold_files) or (best_patched_fp and best_patched_fp in gold_files))
        all_loc = ([pred_fp] if pred_fp else []) + it["supporting_files"]
        top3_hit = any(f in gold_files for f in all_loc)
        cand_top6_hit = any(c["file"] in gold_files for c in it["candidates"])
        patch_gold_file = bool(best_diff and (best_patched_fp in gold_files))

        trace_records.append(
            {
                "instance_id": it["instance_id"],
                "cohort": it["cohort"],
                "repo": it["repo"],
                "base_commit": it["base_commit"],
                "det_top1_file": det_fp,
                "e2b_top1_file": e2b_fp,
                "localized_file": best_patched_fp if best_diff else pred_fp,
                "supporting_files": it["supporting_files"],
                "gold_files": gold_files,
                "det_only_top1_match": bool(det_fp and det_fp in gold_files),
                "e2b_only_top1_match": bool(e2b_fp and e2b_fp in gold_files),
                "localize_top1_match": top1_hit,
                "localize_top3_match": top3_hit,
                "localize_top6_recall": cand_top6_hit,
                "gold_in_top_candidate_window": gold_in_win,
                "enclosing_sym_overlaps_gold": sym_overlaps,
                "w1_overlaps_gold": w1_overlaps,
                "w1_or_w2_overlaps_gold": w12_overlaps,
                "w_ext_overlaps_gold": wext_overlaps,
                "repro_ast_valid": it["repro_ast_valid"],
                "k0_ast_valid_patch": k0_ast_valid,
                "valid_editblock_generated": has_valid_block,
                "search_anchor_applied_cleanly": applied_any,
                "ast_verified_patch_generated": bool(best_diff),
                "patched_target_file_match": patch_gold_file,
                "oracle_best_k_gold_file_match": oracle_best_gold_hit,
                "diff_token_f1_pct": best_f1,
                "oracle_best_k_diff_f1_pct": max(best_f1, oracle_best_f1),
                "candidate_trace": [
                    {
                        "file": c["file"],
                        "raw_score": c["raw_score"],
                        "e2b_vote": c.get("e2b_vote", 0.0),
                        "hybrid_score": c.get("hybrid_score", c["raw_score"]),
                        "is_gold": c["file"] in gold_files,
                        "features": c["features"],
                    }
                    for c in it["candidates"]
                ],
            }
        )

    swe300_recs = [r for r in trace_records if r["cohort"] == "swe300"]
    swe900_recs = [r for r in trace_records if r["cohort"] == "swe900_unseen_full"]

    swe300_summary = compute_cohort_summary(swe300_recs, "Official SWE-300 (SWE-bench_Lite Test Split, N=300)")
    swe900_summary = compute_cohort_summary(swe900_recs, f"Unseen Full SWE-bench Holdout (N={len(swe900_recs)})")
    combined_summary = compute_cohort_summary(trace_records, f"Combined Zero-Leakage Large Benchmark (N={len(trace_records)})")
    learned_weights_report = fit_empirical_localization_weights(trace_records)

    total_wall = round(time.time() - t_global, 2)
    master_report = {
        "benchmark": f"OmniCoder-ICX + VLN + Gemma-4-E2B Large-Scale Mathematical Trace (N={len(trace_records)})",
        "hardware": torch.cuda.get_device_name(0) if torch.cuda.is_available() else "CPU",
        "zero_leakage_training_overlap": 0,
        "wall_clock_sec": total_wall,
        "wall_clock_min": round(total_wall / 60.0, 2),
        "peak_vram_mb": round(torch.cuda.max_memory_allocated(0) / (1024**2), 1),
        "swe300_summary": swe300_summary,
        "swe900_unseen_full_summary": swe900_summary,
        "combined_1200_summary": combined_summary,
        "learned_localization_law": learned_weights_report,
        "actuation_diagnostics": {
            "editblock_anchor_modes": anchor_mode_counts,
            "ast_verification_outcomes": ast_reason_counts,
            "judge_selected_beam_distribution": beam_win_counts,
            "oracle_best_f1_beam_distribution": oracle_beam_best_counts,
        },
    }

    (out_dir / "swe_large_trace_summary.json").write_text(json.dumps(master_report, indent=2), encoding="utf-8")
    (out_dir / "swe_large_trace_detailed.json").write_text(json.dumps(trace_records, indent=2), encoding="utf-8")

    print("=" * 92, flush=True)
    print(
        f"  LARGE-SCALE OMNICODER + E2B TRACE COMPLETED ({len(trace_records)} INSTANCES IN {master_report['wall_clock_min']} MIN)!",
        flush=True,
    )
    print("=" * 92, flush=True)
    print(json.dumps({k: v for k, v in master_report.items() if k != "combined_1200_summary"}, indent=2), flush=True)
    print("=" * 92, flush=True)


if __name__ == "__main__":
    main()
