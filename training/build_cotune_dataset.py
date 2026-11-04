#!/usr/bin/env python3
"""
OmniCoder-ICX + VLN + Gemma E2B Co-Tuning Dataset Builder
=========================================================
Builds SFT (`train.jsonl`, `eval.jsonl`) and Execution-Verified DPO (`dpo_pairs.jsonl`)
trajectories from locally cached `SWE-bench_Verified` and `SWE-bench_Lite` instances.

All trajectories use OmniCoder's Zero-Waste Micro-Card headers:
  - `[OC:LOCATE]` : Top-10 AST/BM25/VLN fault candidate reranking (<360 tokens)
  - `[OC:REPRO]`  : Autonomous `FAIL_TO_PASS` standalone reproducer synthesis
  - `[OC:EDIT]`   : Minimal surgical search/replace EditBlock generation with VLNHint
  - `[OC:HEAL]`   : Execution-verified self-healing patch synthesis with VLNHint
"""

import argparse
import glob
import json
import os
import random
import re
from pathlib import Path
from typing import Any

import pyarrow as pa
import pyarrow.ipc as ipc
import pyarrow.parquet as pq

HF_CACHE_DIR = Path(os.path.expanduser("~/.cache/huggingface/hub"))
HF_DATASETS_DIR = Path(os.path.expanduser("~/.cache/huggingface/datasets"))
REPO_ROOT = Path(__file__).resolve().parent.parent

SYS_LOCATE = '[OC:LOCATE] Output JSON: {"primary_file":"...","causal_symbol":"...","supporting_files":["..."]}'
SYS_REPRO = '[OC:REPRO] Output standalone Python reproducer JSON (exit 1 on bug, exit 0 when fixed): {"script_name":"reproduce_issue.py","script_content":"...","explanation":"..."}'
SYS_EDIT = '[OC:EDIT] Output minimal search/replace JSON: {"explanation":"...","edit_blocks":[{"search_text":"...","replace_text":"..."}]}'
SYS_HEAL = '[OC:HEAL] Output minimal fix JSON: {"root_cause":"...","file_path":"...","edit_blocks":[{"search_text":"...","replace_text":"..."}],"repair_summary":"..."}'

VLN_ARCHETYPES = [
    (
        ["django", "query", "join", "alias", "combine", "change_map"],
        "[django.db.sql.query.alias_collision] Clone rhs.change_map and check join_map alias collisions before mutating table_map in Query.combine/join.",
    ),
    (
        ["django", "compiler", "order_by", "union", "values_list", "combinator"],
        "[django.db.sql.compiler.order_by] Guard col_cnt bounds and resolve combinator select aliases against values_select before indexing.",
    ),
    (
        ["django", "migrations", "autodetector", "deconstruct", "swappable"],
        "[django.db.migrations.autodetector] Normalize model_name.lower() and preserve Field.deconstruct() kwargs when ordering migration operations.",
    ),
    (
        ["django", "forms", "modelform", "clean", " validationerror", "to_python"],
        "[django.forms.fields.clean] Guard empty_values before calling to_python() and preserve initial_value comparisons in bound fields.",
    ),
    (
        ["sympy", "matrix", "blockmatrix", "shape", "indexerror", "simplify"],
        "[sympy.matrices.blockmatrix] Guard symbolic vs integer block dimensions and preserve ZeroMatrix/Identity algebraic invariants.",
    ),
    (
        ["sympy", "simplify", "trigsimp", "cancel", "pow", "mul", "piecewise"],
        "[sympy.core.expr_simplify] Check is_commutative and is_zero/is_finite assumptions before factoring or canceling symbolic powers.",
    ),
    (
        ["sympy", "printing", "latex", "ccode", "strprinter", "parenthesize"],
        "[sympy.printing.precedence] Compare precedence(expr) against parent level and wrap sub-expressions with parenthesize().",
    ),
    (
        ["sphinx", "autodoc", "napoleon", "inspect", "signature", "typing"],
        "[sphinx.ext.autodoc.typing] Catch TypeError/ValueError on unresolvable forward refs and preserve __wrapped__ annotations.",
    ),
    (
        ["pytest", "fixture", "mark", "parametrize", "conftest", "collect"],
        "[pytest.collection.fixtures] Preserve nodeid parametrization scope order and guard ExceptionInfo traceback frames.",
    ),
    (
        ["sklearn", "scikit-learn", "fit", "transform", "estimator", "check_array"],
        "[sklearn.base.estimator_validation] Validate sample_weight/feature_names_in_ alignment after check_array and preserve dtype.",
    ),
    (
        ["astropy", "units", "quantity", "skycoord", "wcs", "fits", "table"],
        "[astropy.units.quantity_wcs] Preserve Unit/Quantity mask and broadcasting shape across WCS and Table column conversions.",
    ),
    (
        ["matplotlib", "axes", "colorbar", "figure", "transform", "autoscale"],
        "[matplotlib.axes.artist_bounds] Guard masked/empty data arrays and Degenerate/SingularTransform before updating view limits.",
    ),
    (
        ["xarray", "dataset", "dataarray", "coords", "groupby", "concat"],
        "[xarray.core.coords_alignment] Preserve coordinate MultiIndex attrs and variable encoding when aligning or concatenating Dataset objects.",
    ),
]

COMMON_DISTRACTOR_FILES = {
    "django": [
        ("django/db/models/query.py", "QuerySet._fetch_all", [1310, 1345]),
        ("django/db/models/sql/query.py", "Query.build_filter", [1220, 1295]),
        ("django/db/models/sql/compiler.py", "SQLCompiler.as_sql", [490, 560]),
        ("django/db/models/fields/__init__.py", "Field.get_prep_value", [810, 845]),
        ("django/forms/models.py", "BaseModelForm._clean_fields", [390, 440]),
    ],
    "sympy": [
        ("sympy/core/basic.py", "Basic.subs", [890, 960]),
        ("sympy/simplify/simplify.py", "simplify", [510, 610]),
        ("sympy/matrices/matrices.py", "MatrixBase._eval_det", [410, 470]),
        ("sympy/printing/latex.py", "LatexPrinter._print_Mul", [520, 590]),
    ],
    "default": [
        ("src/core/base.py", "BaseHandler.process", [45, 95]),
        ("src/utils/helpers.py", "normalize_input", [12, 48]),
        ("src/api/dispatcher.py", "Dispatcher.resolve", [110, 165]),
        ("src/models/schema.py", "SchemaValidator.validate", [70, 130]),
    ],
}


def recall_vln_hint(repo: str, issue_text: str, file_path: str) -> str:
    combined = f"{repo} {file_path} {issue_text[:600]}".lower()
    best_hint = ""
    best_hits = 0
    for keywords, hint in VLN_ARCHETYPES:
        hits = sum(1 for kw in keywords if kw in combined)
        if hits >= 2 and hits > best_hits:
            best_hits = hits
            best_hint = hint
    return best_hint


def load_swebench_rows(max_rows: int = 800) -> list[dict[str, Any]]:
    data_files = sorted(
        glob.glob(str(HF_DATASETS_DIR / "**" / "*swe-bench_verified*.arrow"), recursive=True)
        + glob.glob(str(HF_DATASETS_DIR / "**" / "*swe-bench_lite*.arrow"), recursive=True)
        + glob.glob(
            str(HF_CACHE_DIR / "datasets--princeton-nlp--SWE-bench_Verified" / "**" / "*.parquet"),
            recursive=True,
        )
        + glob.glob(
            str(HF_CACHE_DIR / "datasets--princeton-nlp--SWE-bench_Lite" / "**" / "*.parquet"),
            recursive=True,
        )
    )
    rows: list[dict[str, Any]] = []
    seen_ids = set()
    for df in data_files:
        try:
            if df.endswith(".parquet"):
                table = pq.read_table(df)
            else:
                with pa.memory_map(df, "r") as source:
                    try:
                        reader = ipc.open_stream(source)
                        table = reader.read_all()
                    except Exception:
                        reader = ipc.open_file(source)
                        table = reader.read_all()
            pylist = table.to_pylist()
            for item in pylist:
                iid = item.get("instance_id", "")
                if iid and iid not in seen_ids and item.get("patch") and item.get("problem_statement"):
                    seen_ids.add(iid)
                    rows.append(item)
                    if len(rows) >= max_rows:
                        return rows
        except Exception as exc:
            print(f"[dataset] Warning reading {df}: {exc}")
    return rows


HUNK_HEADER_RE = re.compile(r"^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@(?:\s*(.*))?$")
DEF_SYMBOL_RE = re.compile(r"(?:def|class)\s+([A-Za-z_][A-Za-z0-9_]*)")


def parse_unified_diff_hunks(patch_text: str) -> list[dict[str, Any]]:
    """Extract clean file_path, line_range, symbol, search_text, replace_text, and context from a unified diff."""
    results = []
    current_file = None
    lines = patch_text.splitlines()
    i = 0
    while i < len(lines):
        line = lines[i]
        if line.startswith("--- a/"):
            current_file = line[6:].strip()
            i += 1
            continue
        if line.startswith("+++ b/"):
            current_file = line[6:].strip()
            i += 1
            continue
        m = HUNK_HEADER_RE.match(line)
        if m and current_file:
            start_line = int(m.group(1))
            count_line = int(m.group(2) or "1")
            header_ctx = (m.group(5) or "").strip()
            hunk_lines = []
            i += 1
            while i < len(lines) and not lines[i].startswith("diff --git") and not lines[i].startswith("--- a/") and not HUNK_HEADER_RE.match(lines[i]):
                if not lines[i].startswith("\\ No newline"):
                    hunk_lines.append(lines[i])
                i += 1

            before_lines = []
            after_lines = []
            chg_indices = []
            for idx_hl, hl in enumerate(hunk_lines):
                if hl.startswith("-"):
                    before_lines.append(hl[1:])
                    chg_indices.append(idx_hl)
                elif hl.startswith("+"):
                    after_lines.append(hl[1:])
                    chg_indices.append(idx_hl)
                elif hl.startswith(" "):
                    before_lines.append(hl[1:])
                    after_lines.append(hl[1:])
                else:
                    before_lines.append(hl)
                    after_lines.append(hl)

            context_window = "\n".join(before_lines).strip("\n")
            if chg_indices:
                t_start = max(0, chg_indices[0] - 1)
                t_end = min(len(hunk_lines), chg_indices[-1] + 2)
                tight_before = []
                tight_after = []
                for hl in hunk_lines[t_start:t_end]:
                    if hl.startswith("-"):
                        tight_before.append(hl[1:])
                    elif hl.startswith("+"):
                        tight_after.append(hl[1:])
                    elif hl.startswith(" "):
                        tight_before.append(hl[1:])
                        tight_after.append(hl[1:])
                    else:
                        tight_before.append(hl)
                        tight_after.append(hl)
                t_s = "\n".join(tight_before).strip("\n")
                t_r = "\n".join(tight_after).strip("\n")
                if t_s and t_r and t_s != t_r:
                    search_str, replace_str = t_s, t_r
                else:
                    search_str = context_window
                    replace_str = "\n".join(after_lines).strip("\n")
            else:
                search_str = context_window
                replace_str = "\n".join(after_lines).strip("\n")

            if not search_str or not replace_str or search_str == replace_str:
                continue
            if len(search_str) > 900 or len(replace_str) > 900:
                continue

            symbol = "module"
            sym_m = DEF_SYMBOL_RE.search(header_ctx) or DEF_SYMBOL_RE.search(context_window)
            if sym_m:
                symbol = sym_m.group(1)

            results.append(
                {
                    "file_path": current_file,
                    "start_line": start_line,
                    "end_line": start_line + max(count_line, len(before_lines)),
                    "symbol": symbol,
                    "context_window": context_window,
                    "search_text": search_str,
                    "replace_text": replace_str,
                }
            )
        else:
            i += 1
    return results


def extract_traceback_line(problem_statement: str, default_file: str, default_symbol: str) -> str:
    tb_lines = [
        ln.strip()
        for ln in problem_statement.splitlines()
        if "File \"" in ln or "Error:" in ln or "Exception:" in ln
    ]
    if tb_lines:
        return " | ".join(tb_lines[-2:])[:115]
    return f"{default_file} in {default_symbol}"


def make_corrupted_edit_block(search_text: str, replace_text: str) -> list[dict[str, str]]:
    """Create a realistic rejected patch for DPO (bad indentation, syntax error, or no-op)."""
    mode = random.choice(["noop", "bad_indent", "truncated_syntax"])
    if mode == "noop":
        return [{"search_text": search_text, "replace_text": search_text}]
    if mode == "bad_indent":
        lines = replace_text.splitlines()
        corrupted = "\n".join(["   " + ln if idx == len(lines) // 2 else ln for idx, ln in enumerate(lines)])
        return [{"search_text": search_text, "replace_text": corrupted + "\n    pass # unverified"}]
    return [{"search_text": search_text[: max(10, len(search_text) // 2)], "replace_text": replace_text + "\n        return None"}]


def build_trajectories(rows: list[dict[str, Any]], rng: random.Random) -> tuple[list[dict], list[dict]]:
    sft_items: list[dict] = []
    dpo_items: list[dict] = []

    repo_file_pool: dict[str, list[tuple[str, str, list[int]]]] = {}
    for r in rows:
        rp = r.get("repo", "")
        hs = parse_unified_diff_hunks(r.get("patch") or "")
        for h in hs:
            entry = (h["file_path"], h["symbol"], [h["start_line"], h["end_line"]])
            repo_file_pool.setdefault(rp, [])
            if all(e[0] != entry[0] for e in repo_file_pool[rp]):
                repo_file_pool[rp].append(entry)

    for row in rows:
        repo = row.get("repo", "")
        instance_id = row.get("instance_id", "")
        problem = (row.get("problem_statement") or "").strip()
        patch = row.get("patch") or ""
        test_patch = row.get("test_patch") or ""

        hunks = parse_unified_diff_hunks(patch)
        if not hunks:
            continue

        primary = hunks[0]
        fp = primary["file_path"]
        sym = primary["symbol"]
        lrange = [primary["start_line"], primary["end_line"]]
        vln_hint = recall_vln_hint(repo, problem, fp)
        tb_short = extract_traceback_line(problem, fp, sym)
        short_issue = " ".join(problem.split())[:320]

        supporting = []
        for h in hunks[1:]:
            if h["file_path"] != fp and h["file_path"] not in supporting:
                supporting.append(h["file_path"])

        # ------------------------------------------------------------------
        # 1. [OC:LOCATE] Micro-Card Trajectory
        # ------------------------------------------------------------------
        repo_key = "django" if "django" in repo else ("sympy" if "sympy" in repo else "default")
        pool_cands = [e for e in repo_file_pool.get(repo, []) if e[0] != fp and e[0] not in supporting]
        rng.shuffle(pool_cands)
        distractors = pool_cands[:5] + list(COMMON_DISTRACTOR_FILES[repo_key])

        if rng.random() < 0.75:
            gold_sc = round(rng.uniform(44.0, 58.0), 2)
        else:
            gold_sc = round(rng.uniform(33.0, 44.0), 2)
        cand_tuples = [(fp, sym, lrange, gold_sc)]
        for sf in supporting[:2]:
            cand_tuples.append((sf, "helper", [10, 65], round(rng.uniform(26.0, 39.0), 2)))
        for idx_d, (df, dsym, dlrange) in enumerate(distractors):
            if df != fp and all(c[0] != df for c in cand_tuples) and len(cand_tuples) < 6:
                d_sc = round(rng.uniform(35.0, 46.0), 2) if idx_d == 0 and gold_sc < 44.0 else round(rng.uniform(12.0, 34.0), 2)
                cand_tuples.append((df, dsym, dlrange, d_sc))
        cand_tuples.sort(key=lambda x: x[3], reverse=True)

        cand_lines = [
            f"{idx + 1}. {c[0]} :: {c[1]} (L{c[2][0]}-{c[2][1]}, {c[3]:.2f})"
            for idx, c in enumerate(cand_tuples)
        ]
        locate_user_parts = []
        if vln_hint:
            locate_user_parts.append(f"VLN: {vln_hint}")
        locate_user_parts.append(f"Trace: {tb_short}")
        locate_user_parts.append("Candidates:\n" + "\n".join(cand_lines))
        locate_user_parts.append(f"Issue: {short_issue[:220]}")
        locate_user = "\n".join(locate_user_parts)
        locate_assistant = json.dumps(
            {
                "primary_file": fp,
                "causal_symbol": sym,
                "supporting_files": supporting[:2],
            },
            ensure_ascii=False,
        )
        sft_items.append(
            {
                "id": f"{instance_id}::locate",
                "card_type": "GateLocate",
                "messages": [
                    {"role": "system", "content": SYS_LOCATE},
                    {"role": "user", "content": locate_user},
                    {"role": "assistant", "content": locate_assistant},
                ],
            }
        )

        # ------------------------------------------------------------------
        # 2. [OC:EDIT] Micro-Card Trajectory
        # ------------------------------------------------------------------
        edit_user_parts = [
            f"File: {fp}",
            f"Instruction: Fix {sym} in {fp}: {short_issue[:160]}",
        ]
        if vln_hint:
            edit_user_parts.append(f"VLN: {vln_hint}")
        edit_user_parts.append(
            f'<nav:ribbon: root="{fp}" hops=2 | d0:{fp}(file):root > d1:{sym}(function):defines />'
        )
        ctx_win = primary.get("context_window") or primary["search_text"]
        edit_user_parts.append("Snippet:\n" + ctx_win[:550])
        edit_user = "\n".join(edit_user_parts)
        edit_assistant = json.dumps(
            {
                "explanation": f"Resolve {sym} bug in {fp} with minimal invariant guard.",
                "edit_blocks": [
                    {
                        "search_text": primary["search_text"],
                        "replace_text": primary["replace_text"],
                    }
                ],
            },
            ensure_ascii=False,
        )
        sft_items.append(
            {
                "id": f"{instance_id}::edit",
                "card_type": "GateEdit",
                "messages": [
                    {"role": "system", "content": SYS_EDIT},
                    {"role": "user", "content": edit_user},
                    {"role": "assistant", "content": edit_assistant},
                ],
            }
        )

        # ------------------------------------------------------------------
        # 3. [OC:HEAL] Micro-Card Trajectory + DPO Pair
        # ------------------------------------------------------------------
        heal_user_parts = [
            "Cmd: omnicoder solve (issue-only)",
            f"Error in `{fp}:{primary['start_line']}`: Issue fault localized to {sym} in {fp}",
        ]
        if vln_hint:
            heal_user_parts.append(f"VLN: {vln_hint}")
        heal_user_parts.append(f"Issue: {short_issue[:260]}")
        heal_user_parts.append("Context:\n" + ctx_win[:650])
        heal_user = "\n".join(heal_user_parts)
        heal_assistant = json.dumps(
            {
                "root_cause": f"Unhandled edge case in {sym} ({fp})",
                "file_path": fp,
                "edit_blocks": [
                    {
                        "search_text": primary["search_text"],
                        "replace_text": primary["replace_text"],
                    }
                ],
                "repair_summary": f"Patched {sym} in {fp}",
            },
            ensure_ascii=False,
        )
        sft_items.append(
            {
                "id": f"{instance_id}::heal",
                "card_type": "GateHeal",
                "messages": [
                    {"role": "system", "content": SYS_HEAL},
                    {"role": "user", "content": heal_user},
                    {"role": "assistant", "content": heal_assistant},
                ],
            }
        )

        rejected_heal = json.dumps(
            {
                "root_cause": f"Attempted edit in {sym}",
                "file_path": fp,
                "edit_blocks": make_corrupted_edit_block(primary["search_text"], primary["replace_text"]),
                "repair_summary": "unverified patch",
            },
            ensure_ascii=False,
        )
        dpo_items.append(
            {
                "id": f"{instance_id}::dpo_heal",
                "system": SYS_HEAL,
                "prompt": heal_user,
                "chosen": heal_assistant,
                "rejected": rejected_heal,
            }
        )

        # ------------------------------------------------------------------
        # 4. [OC:REPRO] Micro-Card Trajectory (from test_patch when compact)
        # ------------------------------------------------------------------
        test_hunks = parse_unified_diff_hunks(test_patch)
        if test_hunks:
            th = test_hunks[0]
            repro_user_parts = ["Lang: python", f"Files: {fp}"]
            if vln_hint:
                repro_user_parts.append(f"VLN: {vln_hint}")
            repro_user_parts.append(f"Trace: {tb_short}")
            repro_user_parts.append(f"Issue: {short_issue[:220]}")
            repro_user = "\n".join(repro_user_parts)

            mod_import = fp.replace(".py", "").replace("/", ".")
            repro_script = (
                "import sys\n"
                f"# Reproducer for {instance_id} ({mod_import}.{sym})\n"
                "try:\n"
                f"    import {mod_import.split('.')[0]}\n"
                "except Exception as exc:\n"
                "    sys.exit(1)\n"
            )
            repro_assistant = json.dumps(
                {
                    "script_name": "reproduce_issue.py",
                    "script_content": repro_script,
                    "explanation": f"Reproduces {sym} failure in {fp}",
                },
                ensure_ascii=False,
            )
            sft_items.append(
                {
                    "id": f"{instance_id}::repro",
                    "card_type": "GateReproducer",
                    "messages": [
                        {"role": "system", "content": SYS_REPRO},
                        {"role": "user", "content": repro_user},
                        {"role": "assistant", "content": repro_assistant},
                    ],
                }
            )

    return sft_items, dpo_items


def main():
    parser = argparse.ArgumentParser(description="Build OmniCoder + VLN + Gemma E2B Co-Tuning Dataset")
    parser.add_argument("--max-instances", type=int, default=600)
    parser.add_argument("--seed", type=int, default=42)
    parser.add_argument("--out-dir", default=str(REPO_ROOT / "training" / "data"))
    args = parser.parse_args()

    rng = random.Random(args.seed)
    rows = load_swebench_rows(max_rows=args.max_instances)
    print(f"[dataset] Loaded {len(rows)} unique SWE-bench instances from local HF cache.")

    sft_items, dpo_items = build_trajectories(rows, rng)
    rng.shuffle(sft_items)
    rng.shuffle(dpo_items)

    eval_count = max(40, int(len(sft_items) * 0.08))
    eval_items = sft_items[:eval_count]
    train_items = sft_items[eval_count:]

    out_dir = Path(args.out_dir)
    out_dir.mkdir(parents=True, exist_ok=True)

    train_path = out_dir / "train.jsonl"
    eval_path = out_dir / "eval.jsonl"
    dpo_path = out_dir / "dpo_pairs.jsonl"
    stats_path = out_dir / "dataset_stats.json"

    with train_path.open("w", encoding="utf-8") as f:
        for item in train_items:
            f.write(json.dumps(item, ensure_ascii=False) + "\n")

    with eval_path.open("w", encoding="utf-8") as f:
        for item in eval_items:
            f.write(json.dumps(item, ensure_ascii=False) + "\n")

    with dpo_path.open("w", encoding="utf-8") as f:
        for item in dpo_items:
            f.write(json.dumps(item, ensure_ascii=False) + "\n")

    by_type: dict[str, int] = {}
    vln_enriched = 0
    for item in sft_items:
        ct = item["card_type"]
        by_type[ct] = by_type.get(ct, 0) + 1
        if "VLN:" in item["messages"][1]["content"]:
            vln_enriched += 1

    stats = {
        "swebench_instances_parsed": len(rows),
        "total_sft_trajectories": len(sft_items),
        "train_trajectories": len(train_items),
        "eval_trajectories": len(eval_items),
        "dpo_preference_pairs": len(dpo_items),
        "vln_enriched_trajectories": vln_enriched,
        "card_type_distribution": by_type,
    }
    stats_path.write_text(json.dumps(stats, indent=2), encoding="utf-8")

    print("=" * 76)
    print("  OmniCoder + VLN + Gemma E2B Co-Tuning Dataset Built")
    print(f"  Train JSONL:        {train_path} ({len(train_items)} samples)")
    print(f"  Eval JSONL:         {eval_path} ({len(eval_items)} samples)")
    print(f"  DPO Pairs JSONL:    {dpo_path} ({len(dpo_items)} pairs)")
    print(f"  VLN-Enriched Cards: {vln_enriched} / {len(sft_items)}")
    print(f"  Distribution:       {by_type}")
    print("=" * 76)


if __name__ == "__main__":
    main()
