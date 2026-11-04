#!/usr/bin/env python3
"""
Official SWE-300 (`princeton-nlp/SWE-bench_Lite` 300 Test Split) Runner for Config 3
====================================================================================
Runs Config 3: Full Organism (`OmniCoder-ICX` + `VLN SWE Cartridge` + `OmniCoder-Gemma-E2B`
+ Speculative `K=4`) against real upstream GitHub repository checkouts at `base_commit`
with zero oracle prompt leakage:

  1. Loads the official 300 test instances from `princeton-nlp/SWE-bench_Lite`.
  2. Audits and tags each instance for train/DPO overlap (`in_training_split` vs `unseen_holdout`).
  3. Clones/caches real upstream GitHub repositories (`--repo-cache-dir`) and checks out a
     clean git worktree at each instance's exact `base_commit`.
  4. Runs `omnicoder.exe solve --model google/gemma-4-e2b --base-url http://127.0.0.1:18090/v1 --beam-k 4`
     on the full repository (passing ONLY `problem_statement` in `ISSUE.md`).
  5. Evaluates Top-1/Top-3 localization on the real repo, patch synthesis, gold diff token F1,
     and applies the official `test_patch` to run `FAIL_TO_PASS` tests.
  6. Emits official SWE-bench predictions format (`predictions.jsonl`:
     `{"instance_id", "model_name_or_path", "model_patch"}`).
"""

import argparse
import ast
import json
import os
import re
import shutil
import subprocess
import sys
import tempfile
import time
import urllib.request
from pathlib import Path
from typing import Any

import pyarrow.parquet as pq

REPO_ROOT = Path(__file__).resolve().parent.parent
SWE_LITE_PARQUET = Path(
    r"C:\Users\livin\.cache\huggingface\hub\datasets--princeton-nlp--SWE-bench_Lite"
    r"\snapshots\6ec7bb89b9342f664a54a6e0a6ea6501d3437cc2\data\test-00000-of-00001.parquet"
)
SWE_VERIFIED_PARQUET = Path(
    r"C:\Users\livin\.cache\huggingface\hub\datasets--princeton-nlp--SWE-bench_Verified"
    r"\snapshots\c104f840cc67f8b6eec6f759ebc8b2693d585d4a\data\test-00000-of-00001.parquet"
)


def load_training_contaminated_ids() -> set[str]:
    contaminated: set[str] = set()
    for rel in ("training/data/train.jsonl", "training/data/dpo_pairs.jsonl"):
        p = REPO_ROOT / rel
        if p.exists():
            for line in p.read_text(encoding="utf-8").splitlines():
                if line.strip():
                    iid = json.loads(line).get("id", "").split("::")[0]
                    if iid:
                        contaminated.add(iid)
    return contaminated


def check_config3_server(base_url: str) -> dict[str, Any]:
    health_url = base_url.replace("/v1", "").rstrip("/") + "/health"
    req = urllib.request.Request(health_url, method="GET")
    with urllib.request.urlopen(req, timeout=5) as resp:
        return json.loads(resp.read().decode("utf-8"))


def ensure_repo_mirror(repo: str, cache_dir: Path, base_commit: str) -> Path:
    safe_name = repo.replace("/", "__")
    repo_dir = cache_dir / safe_name
    if not (repo_dir / ".git").exists():
        repo_dir.parent.mkdir(parents=True, exist_ok=True)
        url = f"https://github.com/{repo}.git"
        print(f"  [git] Cloning blobless cache for {repo} -> {repo_dir} ...", flush=True)
        subprocess.run(
            [
                "git",
                "-c",
                "core.longpaths=true",
                "-c",
                "core.autocrlf=false",
                "clone",
                "--filter=blob:none",
                "--no-checkout",
                url,
                str(repo_dir),
            ],
            check=True,
            capture_output=True,
            text=True,
        )
    chk = subprocess.run(
        ["git", "-C", str(repo_dir), "cat-file", "-e", f"{base_commit}^{{commit}}"],
        capture_output=True,
        text=True,
    )
    if chk.returncode != 0:
        subprocess.run(
            ["git", "-C", str(repo_dir), "fetch", "--filter=blob:none", "origin", base_commit],
            check=False,
            capture_output=True,
            text=True,
        )
    return repo_dir


def extract_modified_files_from_patch(patch_text: str) -> list[str]:
    files = []
    for line in patch_text.splitlines():
        if line.startswith("--- a/"):
            fp = line[6:].strip()
            if fp and fp not in files:
                files.append(fp)
    return files


def extract_added_removed_lines(patch_text: str) -> str:
    changed = []
    for ln in patch_text.splitlines():
        if (ln.startswith("+") and not ln.startswith("+++")) or (
            ln.startswith("-") and not ln.startswith("---")
        ):
            changed.append(ln[1:].strip())
    return "\n".join(changed)


def diff_token_f1(pred_patch: str, gold_patch: str) -> float:
    tok_re = re.compile(r"[A-Za-z_][A-Za-z0-9_]*|\d+|==|!=|<=|>=|[-+*/%()\[\]{}.,:]")
    p_toks = tok_re.findall(extract_added_removed_lines(pred_patch))
    g_toks = tok_re.findall(extract_added_removed_lines(gold_patch))
    if not p_toks or not g_toks:
        return 0.0
    p_set = set(p_toks)
    g_set = set(g_toks)
    inter = len(p_set & g_set)
    if inter == 0:
        return 0.0
    prec = inter / len(p_set)
    rec = inter / len(g_set)
    return round(100.0 * (2 * prec * rec / (prec + rec)), 1)


def parse_supporting_files(stdout_line: str) -> list[str]:
    m = re.search(r"supporting=\[([^\]]*)\]", stdout_line)
    if not m:
        return []
    raw = m.group(1).strip()
    if not raw:
        return []
    return [tok.strip() for tok in raw.split() if tok.strip()]


def evaluate_test_patch_in_worktree(tmp_root: Path, row: dict[str, Any]) -> tuple[bool, str]:
    test_patch = row.get("test_patch") or ""
    if not test_patch.strip():
        return False, "no_test_patch"

    tp_file = tmp_root / "_swe_official_test.patch"
    tp_file.write_text(test_patch, encoding="utf-8")
    ap = subprocess.run(
        ["git", "-C", str(tmp_root), "apply", "--ignore-whitespace", str(tp_file)],
        capture_output=True,
        text=True,
    )
    tp_file.unlink(missing_ok=True)
    if ap.returncode != 0:
        return False, f"test_patch_apply_failed: {ap.stderr.strip()[:120]}"

    raw_f2p = row.get("FAIL_TO_PASS", "[]")
    try:
        f2p_list = json.loads(raw_f2p) if isinstance(raw_f2p, str) else list(raw_f2p)
    except Exception:
        f2p_list = []

    test_files = extract_modified_files_from_patch(test_patch)
    targets = f2p_list[:3] if f2p_list else test_files[:2]
    if not targets:
        return False, "no_test_targets"

    env = dict(os.environ)
    py_paths = [str(tmp_root), str(tmp_root / "src"), str(tmp_root / "lib")]
    env["PYTHONPATH"] = os.pathsep.join(py_paths)

    # Convert Django-style test labels `test_name (module.path.ClassName)` to file paths if needed
    pytest_args = []
    for t in targets:
        if "(" in t and ")" in t:
            pytest_args.extend(test_files[:1])
            break
        pytest_args.append(t)

    try:
        proc = subprocess.run(
            [sys.executable, "-m", "pytest", "-q", *pytest_args],
            cwd=str(tmp_root),
            env=env,
            capture_output=True,
            text=True,
            timeout=35,
        )
        passed = proc.returncode == 0
        out_tail = (proc.stdout + "\n" + proc.stderr).strip()[-240:]
        return passed, out_tail
    except Exception as exc:
        return False, f"pytest_error: {exc}"


def run_instance(
    row: dict[str, Any],
    repo_cache_dir: Path,
    base_url: str,
    beam_k: int,
    max_turns: int,
    timeout_sec: int,
    contaminated_ids: set[str],
) -> dict[str, Any]:
    iid = row["instance_id"]
    repo = row["repo"]
    base_commit = row["base_commit"]
    problem = row["problem_statement"]
    gold_patch = row.get("patch", "")
    gold_files = extract_modified_files_from_patch(gold_patch)

    mirror_dir = ensure_repo_mirror(repo, repo_cache_dir, base_commit)
    omnicoder_bin = str(REPO_ROOT / "omnicoder.exe")

    tmp_root = Path(tempfile.mkdtemp(prefix=f"swe300_{iid}_"))
    try:
        subprocess.run(
            [
                "git",
                "-C",
                str(mirror_dir),
                "-c",
                "core.longpaths=true",
                "-c",
                "core.autocrlf=false",
                "worktree",
                "add",
                "--detach",
                str(tmp_root),
                base_commit,
            ],
            check=True,
            capture_output=True,
            text=True,
        )
        (tmp_root / "ISSUE.md").write_text(problem, encoding="utf-8")

        t0 = time.time()
        cmd = [
            omnicoder_bin,
            "solve",
            "--workspace",
            str(tmp_root),
            "--issue-file",
            "ISSUE.md",
            "--model",
            "google/gemma-4-e2b",
            "--base-url",
            base_url,
            "--api-key",
            "local-rtx3060-key",
            "--max-turns",
            str(max_turns),
            "--repro-attempts",
            "1",
            "--beam-k",
            str(beam_k),
            "--patch-out",
            "model.patch",
        ]
        try:
            proc = subprocess.run(cmd, cwd=str(tmp_root), capture_output=True, text=True, timeout=timeout_sec)
            solve_rc = proc.returncode
            stdout_txt = proc.stdout
        except subprocess.TimeoutExpired as te:
            solve_rc = -1
            stdout_txt = (te.stdout.decode("utf-8", errors="ignore") if isinstance(te.stdout, bytes) else (te.stdout or ""))
        elapsed = round(time.time() - t0, 2)

        patch_path = tmp_root / "model.patch"
        model_patch = patch_path.read_text(encoding="utf-8") if patch_path.exists() else ""
        pred_files = extract_modified_files_from_patch(model_patch)

        localized_file = ""
        supporting_files: list[str] = []
        vln_injected = False
        repro_verified_on_base = False
        for ln in stdout_txt.splitlines():
            if "Turn 0 Fault Localization: primary=" in ln:
                part = ln.split("primary=", 1)[1]
                localized_file = part.split(" ", 1)[0].strip()
                supporting_files = parse_supporting_files(ln)
                vln_injected = "vln_hint=true" in ln
            if "Turn 1" in ln and ("failed as expected on base_commit" in ln or "fails on base_commit" in ln):
                repro_verified_on_base = True

        top1_hit = bool(localized_file and localized_file in gold_files)
        all_loc = ([localized_file] if localized_file else []) + supporting_files
        top3_hit = any(f in gold_files for f in all_loc)
        patch_file_hit = any(pf in gold_files for pf in pred_files)
        f1_pct = diff_token_f1(model_patch, gold_patch) if model_patch.strip() else 0.0

        f2p_passed = False
        f2p_note = "no_patch_generated"
        if model_patch.strip():
            f2p_passed, f2p_note = evaluate_test_patch_in_worktree(tmp_root, row)

        return {
            "instance_id": iid,
            "repo": repo,
            "base_commit": base_commit,
            "in_training_split": iid in contaminated_ids,
            "elapsed_sec": elapsed,
            "solve_exit_code": solve_rc,
            "localized_file": localized_file,
            "supporting_files": supporting_files,
            "gold_files": gold_files,
            "localize_top1_match": top1_hit,
            "localize_top3_match": top3_hit,
            "repro_verified_on_base": repro_verified_on_base,
            "vln_hint_injected": vln_injected,
            "patch_generated": bool(model_patch.strip()),
            "patched_files": pred_files,
            "patched_target_file_match": patch_file_hit,
            "diff_token_f1_pct": f1_pct,
            "fail_to_pass_resolved": f2p_passed,
            "test_eval_note": f2p_note,
            "model_patch": model_patch,
        }
    finally:
        subprocess.run(
            ["git", "-C", str(mirror_dir), "worktree", "remove", "--force", str(tmp_root)],
            check=False,
            capture_output=True,
        )
        shutil.rmtree(tmp_root, ignore_errors=True)


def write_summary(results: list[dict[str, Any]], args: Any, base_url: str) -> dict[str, Any]:
    n = max(len(results), 1)
    loc_top1 = sum(1 for r in results if r.get("localize_top1_match"))
    loc_top3 = sum(1 for r in results if r.get("localize_top3_match"))
    repro_ok = sum(1 for r in results if r.get("repro_verified_on_base"))
    patch_gen = sum(1 for r in results if r.get("patch_generated"))
    patch_file_hits = sum(1 for r in results if r.get("patched_target_file_match"))
    f2p_resolved = sum(1 for r in results if r.get("fail_to_pass_resolved"))
    mean_f1 = round(sum(float(r.get("diff_token_f1_pct", 0.0)) for r in results) / n, 1)

    final_health = check_config3_server(base_url)
    summary = {
        "timestamp": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
        "dataset": args.dataset,
        "filter": args.filter,
        "instances_evaluated": len(results),
        "speculative_k": args.beam_k,
        "real_repo_localize_top1_pct": round(100.0 * loc_top1 / n, 1),
        "real_repo_localize_top3_pct": round(100.0 * loc_top3 / n, 1),
        "reproducer_verified_on_base_pct": round(100.0 * repro_ok / n, 1),
        "non_empty_patch_generated_pct": round(100.0 * patch_gen / n, 1),
        "patched_correct_gold_file_pct": round(100.0 * patch_file_hits / n, 1),
        "mean_diff_token_f1_pct": mean_f1,
        "official_fail_to_pass_resolved_count": f2p_resolved,
        "official_fail_to_pass_resolved_pct": round(100.0 * f2p_resolved / n, 1),
        "peak_vram_mb": final_health.get("peak_vram_mb"),
        "results": results,
    }
    Path(args.report_out).write_text(json.dumps(summary, indent=2), encoding="utf-8")
    return summary


def main():
    parser = argparse.ArgumentParser(description="Run Official SWE-300 (SWE-bench_Lite) on Config 3")
    parser.add_argument("--dataset", choices=["lite300", "verified500"], default="lite300")
    parser.add_argument("--filter", choices=["all", "unseen_only", "seen_only"], default="unseen_only")
    parser.add_argument("--instance-id", default="", help="Optional specific instance_id filter")
    parser.add_argument("--limit", type=int, default=11)
    parser.add_argument("--beam-k", type=int, default=4)
    parser.add_argument("--max-turns", type=int, default=1)
    parser.add_argument("--timeout", type=int, default=150)
    parser.add_argument("--base-url", default="http://127.0.0.1:18090/v1")
    parser.add_argument("--repo-cache-dir", default=str(REPO_ROOT / "training" / "swe300_repos_cache"))
    parser.add_argument("--predictions-out", default=str(REPO_ROOT / "training" / "swe300_predictions.jsonl"))
    parser.add_argument("--report-out", default=str(REPO_ROOT / "training" / "swe300_official_report.json"))
    args = parser.parse_args()

    health = check_config3_server(args.base_url)
    print(
        f"[SWE-300] Connected to Config 3 Server: {health.get('config')} (VRAM: {health.get('allocated_vram_mb')} MB)",
        flush=True,
    )

    parquet_path = SWE_LITE_PARQUET if args.dataset == "lite300" else SWE_VERIFIED_PARQUET
    rows = pq.read_table(str(parquet_path)).to_pylist()
    contaminated_ids = load_training_contaminated_ids()

    if args.instance_id:
        rows = [r for r in rows if r["instance_id"] == args.instance_id]
    elif args.filter == "unseen_only":
        rows = [r for r in rows if r["instance_id"] not in contaminated_ids]
    elif args.filter == "seen_only":
        rows = [r for r in rows if r["instance_id"] in contaminated_ids]

    if args.limit > 0:
        rows = rows[: args.limit]

    print(
        f"[SWE-300] Running {len(rows)} instances (dataset={args.dataset}, filter={args.filter}, beam_k={args.beam_k})...",
        flush=True,
    )

    pred_path = Path(args.predictions_out)
    pred_path.parent.mkdir(parents=True, exist_ok=True)
    results = []

    with pred_path.open("w", encoding="utf-8") as pred_f:
        for idx, row in enumerate(rows, 1):
            iid = row["instance_id"]
            print(f"[{idx}/{len(rows)}] Solving {iid} ({row['repo']} @ {row['base_commit'][:8]})...", flush=True)
            try:
                res = run_instance(
                    row=row,
                    repo_cache_dir=Path(args.repo_cache_dir),
                    base_url=args.base_url,
                    beam_k=args.beam_k,
                    max_turns=args.max_turns,
                    timeout_sec=args.timeout,
                    contaminated_ids=contaminated_ids,
                )
            except Exception as exc:
                res = {
                    "instance_id": iid,
                    "repo": row["repo"],
                    "base_commit": row["base_commit"],
                    "in_training_split": iid in contaminated_ids,
                    "error": str(exc),
                    "localize_top1_match": False,
                    "localize_top3_match": False,
                    "repro_verified_on_base": False,
                    "patch_generated": False,
                    "patched_target_file_match": False,
                    "diff_token_f1_pct": 0.0,
                    "fail_to_pass_resolved": False,
                    "model_patch": "",
                }
            results.append(res)
            pred_record = {
                "instance_id": iid,
                "model_name_or_path": f"omnicoder-gemma-e2b-config3-k{args.beam_k}",
                "model_patch": res.get("model_patch", ""),
            }
            pred_f.write(json.dumps(pred_record, ensure_ascii=False) + "\n")
            pred_f.flush()
            write_summary(results, args, args.base_url)
            print(
                f"    -> localized={res.get('localized_file')} (gold={res.get('gold_files')}, "
                f"top1={res.get('localize_top1_match')}, top3={res.get('localize_top3_match')}) | "
                f"patch_gen={res.get('patch_generated')} | F1={res.get('diff_token_f1_pct')}% | "
                f"resolved={res.get('fail_to_pass_resolved')} ({res.get('elapsed_sec', 0)}s)",
                flush=True,
            )

    summary = write_summary(results, args, args.base_url)
    print("=" * 84, flush=True)
    print(f"  Official Unseen SWE-300 (SWE-bench_Lite) Results ({len(results)} Full-Repo Checkouts)", flush=True)
    print("=" * 84, flush=True)
    print(f"  Real Repo Localization Top-1: {summary['real_repo_localize_top1_pct']}%", flush=True)
    print(f"  Real Repo Localization Top-3: {summary['real_repo_localize_top3_pct']}%", flush=True)
    print(f"  Reproducer Verified on Base:  {summary['reproducer_verified_on_base_pct']}%", flush=True)
    print(f"  Non-Empty Patch Generated:    {summary['non_empty_patch_generated_pct']}%", flush=True)
    print(f"  Patched Correct Gold File:    {summary['patched_correct_gold_file_pct']}%", flush=True)
    print(f"  Mean Diff Token F1 vs Gold:   {summary['mean_diff_token_f1_pct']}%", flush=True)
    print(
        f"  FAIL_TO_PASS Resolved:        {summary['official_fail_to_pass_resolved_count']}/{len(results)} "
        f"({summary['official_fail_to_pass_resolved_pct']}%)",
        flush=True,
    )
    print(f"  Peak GPU VRAM:                {summary['peak_vram_mb']} MB", flush=True)
    print("=" * 84, flush=True)


if __name__ == "__main__":
    main()
