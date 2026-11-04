#!/usr/bin/env python3
"""
OmniCoder-ICX + VLN + Gemma 4 E2B Asset Synchronizer
====================================================
Checks and syncs `google/gemma-4-e2b` from `calera-spark@192.168.0.57` (LAN)
or HuggingFace Hub / local HF cache (`C:\\Users\\livin\\.cache\\huggingface\\hub`),
and verifies local `SWE-bench_Verified`, `SWE-bench_Lite`, and `models/vln_swe_code.gob`.
"""

import argparse
import glob
import json
import os
import subprocess
import sys
import time
from pathlib import Path

HF_CACHE_DIR = Path(os.path.expanduser("~/.cache/huggingface/hub"))
REPO_ROOT = Path(__file__).resolve().parent.parent


def find_snapshot_dir(model_cache_dir: Path) -> str | None:
    snaps = sorted(glob.glob(str(model_cache_dir / "snapshots" / "*")))
    for snap in reversed(snaps):
        snap_path = Path(snap)
        if (snap_path / "config.json").exists() and (
            list(snap_path.glob("*.safetensors")) or list(snap_path.glob("*.bin"))
        ):
            return str(snap_path)
    return None


def try_lan_sync_from_spark(spark_host: str, target_hub_dir: Path) -> str | None:
    """Attempt quick non-blocking SSH/SCP check against calera-spark@192.168.0.57."""
    try:
        cmd = [
            "ssh",
            "-o", "BatchMode=yes",
            "-o", "ConnectTimeout=3",
            spark_host,
            "ls -d ~/.cache/huggingface/hub/models--google--gemma-4-e2b* 2>/dev/null",
        ]
        res = subprocess.run(cmd, capture_output=True, text=True, timeout=5)
        if res.returncode == 0 and res.stdout.strip():
            remote_path = res.stdout.strip().splitlines()[0].strip()
            print(f"[sync] Found remote Gemma-4-E2B on {spark_host}: {remote_path}")
            scp_cmd = [
                "scp",
                "-r",
                "-o", "BatchMode=yes",
                "-o", "ConnectTimeout=5",
                f"{spark_host}:{remote_path}",
                str(target_hub_dir),
            ]
            subprocess.run(scp_cmd, check=False, timeout=180)
            folder_name = Path(remote_path).name
            snap = find_snapshot_dir(target_hub_dir / folder_name)
            if snap:
                return snap
    except Exception as exc:
        print(f"[sync] LAN check on {spark_host} skipped/unavailable: {exc}")
    return None


def resolve_local_gemma_model(spark_host: str) -> dict:
    """
    Resolve the best available local Gemma 4 / Gemma 3 / Qwen Coder model in HF cache,
    trying LAN sync for `models--google--gemma-4-e2b` first.
    """
    candidates = [
        ("google/gemma-4-e2b", "models--google--gemma-4-e2b"),
        ("google/gemma-4-e2b-it", "models--google--gemma-4-e2b-it"),
        ("google/gemma-4-e4b-it", "models--google--gemma-4-e4b-it"),
        ("google/gemma-3-4b-it", "models--google--gemma-3-4b-it"),
        ("Qwen/Qwen2.5-Coder-7B-Instruct", "models--Qwen--Qwen2.5-Coder-7B-Instruct"),
    ]

    # 1. Check if gemma-4-e2b is already in local cache
    for model_id, folder in candidates[:2]:
        snap = find_snapshot_dir(HF_CACHE_DIR / folder)
        if snap:
            return {
                "requested_model": "google/gemma-4-e2b",
                "resolved_model_id": model_id,
                "snapshot_path": snap,
                "source": "local_hf_cache_e2b",
            }

    # 2. Attempt LAN pull from calera-spark@192.168.0.57
    lan_snap = try_lan_sync_from_spark(spark_host, HF_CACHE_DIR)
    if lan_snap:
        return {
            "requested_model": "google/gemma-4-e2b",
            "resolved_model_id": "google/gemma-4-e2b",
            "snapshot_path": lan_snap,
            "source": f"lan_sync:{spark_host}",
        }

    # 3. Resolve from local cached Gemma-4 / Gemma-3 / Qwen models
    # Note: gemma-3-4b-it has standard causal LM layers compatible with fast 4-bit QLoRA
    # and gemma-4-e4b-it is the local Gemma-4 checkpoint in HF cache.
    preferred_local = [
        ("google/gemma-3-4b-it", "models--google--gemma-3-4b-it"),
        ("google/gemma-4-e4b-it", "models--google--gemma-4-e4b-it"),
        ("Qwen/Qwen2.5-Coder-7B-Instruct", "models--Qwen--Qwen2.5-Coder-7B-Instruct"),
    ]
    for model_id, folder in preferred_local:
        snap = find_snapshot_dir(HF_CACHE_DIR / folder)
        if snap:
            return {
                "requested_model": "google/gemma-4-e2b",
                "resolved_model_id": model_id,
                "snapshot_path": snap,
                "source": "local_hf_cache_gemma",
            }

    raise FileNotFoundError(f"No cached Gemma or Qwen models found in {HF_CACHE_DIR}")


def check_swebench_cache() -> dict:
    hf_root = HF_CACHE_DIR.parent
    verified_arrows = glob.glob(
        str(hf_root / "datasets" / "**" / "*swe-bench_verified*.arrow"),
        recursive=True,
    ) + glob.glob(
        str(HF_CACHE_DIR / "datasets--princeton-nlp--SWE-bench_Verified" / "**" / "*.parquet"),
        recursive=True,
    )
    lite_arrows = glob.glob(
        str(hf_root / "datasets" / "**" / "*swe-bench_lite*.arrow"),
        recursive=True,
    ) + glob.glob(
        str(HF_CACHE_DIR / "datasets--princeton-nlp--SWE-bench_Lite" / "**" / "*.parquet"),
        recursive=True,
    )
    return {
        "swebench_verified_arrows": verified_arrows,
        "swebench_lite_arrows": lite_arrows,
        "verified_ready": len(verified_arrows) > 0,
        "lite_ready": len(lite_arrows) > 0,
    }


def main():
    parser = argparse.ArgumentParser(description="Sync & verify OmniCoder + VLN + Gemma E2B assets")
    parser.add_argument("--spark-host", default="calera-spark@192.168.0.57")
    parser.add_argument(
        "--manifest-out",
        default=str(REPO_ROOT / "training" / "spark_sync_manifest.json"),
    )
    args = parser.parse_args()

    t0 = time.time()
    model_info = resolve_local_gemma_model(args.spark_host)
    swe_info = check_swebench_cache()
    vln_gob = REPO_ROOT / "models" / "vln_swe_code.gob"
    vln_src = Path(r"c:\Users\livin\OneDrive\Desktop\2027 Projects\VLN-v22.1\calera-volume-net")

    manifest = {
        "timestamp": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
        "elapsed_sec": round(time.time() - t0, 3),
        "model": model_info,
        "swebench_cache": swe_info,
        "vln_cartridge": {
            "gob_path": str(vln_gob),
            "exists": vln_gob.exists(),
            "size_bytes": vln_gob.stat().st_size if vln_gob.exists() else 0,
            "vln_repo_path": str(vln_src),
            "vln_repo_exists": vln_src.exists(),
        },
        "rtx3060_target_envelope": {
            "max_vram_mb": 2500,
            "max_host_ram_mb": 250,
            "micro_card_token_budget": 400,
            "speculative_beam_k": 8,
        },
    }

    out_path = Path(args.manifest_out)
    out_path.parent.mkdir(parents=True, exist_ok=True)
    out_path.write_text(json.dumps(manifest, indent=2), encoding="utf-8")

    print("=" * 76)
    print("  OmniCoder-ICX + VLN + Gemma E2B Asset Verification Complete")
    print(f"  Target Model ID:   {model_info['requested_model']}")
    print(f"  Resolved Model:    {model_info['resolved_model_id']} ({model_info['source']})")
    print(f"  Snapshot Path:     {model_info['snapshot_path']}")
    print(f"  SWE-bench Verified:{len(swe_info['swebench_verified_arrows'])} arrow shard(s)")
    print(f"  SWE-bench Lite:    {len(swe_info['swebench_lite_arrows'])} arrow shard(s)")
    print(f"  VLN SWE Cartridge: {vln_gob} ({manifest['vln_cartridge']['size_bytes']} bytes)")
    print("=" * 76)


if __name__ == "__main__":
    main()
