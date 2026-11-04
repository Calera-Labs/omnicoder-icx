---
base_model: google/gemma-4-e2b
library_name: peft
pipeline_tag: text-generation
license: apache-2.0
tags:
- gemma-4
- omnicoder
- icx
- vln
- swe-bench
- lora
- code-repair
- transformers
---

# OmniCoder-Gemma-E2B (`Config 3 v4` Surgical LoRA + VLN Cartridge Organism)

**`OmniCoder-Gemma-E2B`** is a specialized 2-billion-parameter model co-tuned to operate as the neural micro-decision actuator inside the **`OmniCoder-ICX + VLN`** autonomous software engineering organism (`Config 3 v4`).

Rather than asking a 2B model to hallucinate repository paths or emit raw multi-hundred-line unified diffs in an unbounded chat loop, **OmniCoder-ICX** splits the cognitive workload:
1. **Deterministic Structural Engine (`pkg/devops/localizer.go`, `pkg/vln/cartridge.go`, `pkg/devops/editblock.go`):** Handles AST symbol extraction, 1-hop relative import-graph resolution, code-structured signal extraction, 2048-bit SDR Hopfield VLN associative memory routing, 16-line dual-symbol focal windowing (`w1` $\cup$ `w2`), and 5-tier + $\Delta\text{indent}$ AST-verified patch splicing.
2. **Co-Tuned `OmniCoder-Gemma-E2B` Actuator (`[OC:LOCATE]`, `[OC:REPRO]`, `[OC:HEAL]`):** Emits bounded `<300`-token structured JSON micro-decisions for candidate file ranking (`[OC:LOCATE]`), minimal Python AST bug reproduction (`[OC:REPRO]`), and surgical `search_text` / `replace_text` block synthesis (`[OC:HEAL]`).

---

## Official Zero-Leakage `SWE-300` (`princeton-nlp/SWE-bench_Lite`) Benchmark (`v1` $\to$ `v4`)

- **Evaluation Dataset:** `princeton-nlp/SWE-bench_Lite` Test Split (`N = 300` real GitHub issues across 12 Python repositories)
- **Zero-Leakage Verification:** **`0 / 300` (`0.0%`)** instance overlap with the `407` `SWE-bench_Verified` co-tuning training set (`zero_leakage_training_split: true`)
- **Hardware:** Single NVIDIA A100-SXM4-80GB (`torch.bfloat16`, Peak VRAM `14,977.0 MB`)

| Metric (`N = 300` Unseen `SWE-bench_Lite` Issues, `0%` Overlap) | `v1` Baseline | `v2` Upgraded | `v3` (Laws 1–4) | **`v4` (Import-Graph + Dual-Window)** | Total Gain (`v1` $\to$ `v4`) |
| :--- | ---: | ---: | ---: | ---: | :--- |
| **Wall-Clock Runtime (`NVIDIA A100-80GB`)** | `24.16 min` | `20.55 min` | `9.65 min` (`578.9s`) | **`8.46 min` (`507.8s`)** | **`-15.70 min` (`2.86x` faster, `~1.69s`/issue)** |
| **`[OC:LOCATE]` JSON Schema Validity** | `100.0%` (`300/300`) | `100.0%` (`300/300`) | `100.0%` (`300/300`) | **`100.0%` (`300/300`)** | `0.0%` |
| **`[OC:LOCATE]` Real-Repo Top-1 File Accuracy (`Loc@1`)** | `35.3%` (`106/300`) | `40.3%` (`121/300`) | `45.0%` (`135/300`) | **`50.3%` (`151/300`)** | **`+15.0%` (`+45` exact files, `+42.5%` rel.)** |
| **`[OC:LOCATE]` Real-Repo Top-3 File Recall (`Loc@3`)** | `60.0%` (`180/300`) | `57.3%` (`172/300`) | `59.3%` (`178/300`) | **`66.0%` (`198/300`)** | **`+6.0%` (`+18` instances)** |
| **Stage-1 Top-6 Candidate Recall** | `64.3%` (`193/300`) | `64.7%` (`194/300`) | `65.3%` (`196/300`) | **`75.0%` (`225/300`)** | **`+10.7%` (`+32` instances)** |
| **Law 2 Gold-Hunk Focal Window Overlap (`w1` $\cup$ `w2`)** | `—` | `—` | `16.3%` (`49/300`) | **`35.7%` (`107/300`)** | **`+19.4%` (`+58` instances, `2.18x`)** |
| **`[OC:REPRO]` Valid Python AST Reproducer Rate** | `99.7%` (`299/300`) | `99.0%` (`297/300`) | `99.7%` (`299/300`) | **`100.0%` (`300/300`)** | **`+0.3%` (`300/300` perfect)** |
| **`[OC:HEAL]` Valid Non-Noop `EditBlock` Rate (`K=4`)** | `93.7%` (`281/300`) | `98.0%` (`294/300`) | `99.3%` (`298/300`) | **`99.7%` (`299/300`)** | **`+6.0%` (`+18` instances)** |
| **`[OC:HEAL]` Real-Source `search_text` Anchor Match** | `87.7%` (`263/300`) | `95.7%` (`287/300`) | `98.7%` (`296/300`) | **`99.3%` (`298/300`)** | **`+11.6%` (`+35` instances)** |
| **`[OC:HEAL]` AST-Verified Unified Diff Rate (`Patched`)** | `86.0%` (`258/300`) | `93.7%` (`281/300`) | `95.3%` (`286/300`) | **`96.7%` (`290/300`)** | **`+10.7%` (`+32` instances)** |
| **End-to-End `GoldFile` AST-Verified Patch Rate** | `29.3%` (`88/300`) | `34.7%` (`104/300`) | `37.7%` (`113/300`) | **`42.7%` (`128/300`)** | **`+13.4%` (`+40` gold patches, `+45.5%` rel.)** |
| **High-Similarity Patches (`DiffTokenF1 >= 25%`)** | `97 / 300` | `113 / 300` | `136 / 300` | **`142 / 300` (`47.3%`)** | **`+45` instances (`+46.4%` rel.)** |
| **Very-High-Similarity Patches (`DiffTokenF1 >= 40%`)** | `35 / 300` | `36 / 300` | `44 / 300` | **`49 / 300` (`16.3%`)** | **`+14` instances (`+40.0%` rel.)** |

### Per-Repository `v4` Breakdown (`N = 300`)

| Repository | Instances | `Loc@1` | `Loc@3` | `Top-6 Recall` | `AST-Verified Patch` | `GoldFile Patch` | `MeanDiffF1` |
| :--- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| `astropy/astropy` | 6 | `66.7%` | `83.3%` | `83.3%` | `100.0%` | **`66.7%`** | `28.2%` |
| `django/django` | 114 | `54.4%` | `68.4%` | `74.6%` | `95.6%` | **`44.7%`** | `27.0%` |
| `matplotlib/matplotlib` | 23 | `34.8%` | `65.2%` | `82.6%` | `100.0%` | **`34.8%`** | `24.9%` |
| `mwaskom/seaborn` | 4 | `50.0%` | `75.0%` | `100.0%` | `100.0%` | **`50.0%`** | `25.2%` |
| `pallets/flask` | 3 | `33.3%` | `33.3%` | `100.0%` | `100.0%` | **`33.3%`** | `12.1%` |
| `psf/requests` | 6 | `66.7%` | `100.0%` | `100.0%` | `100.0%` | **`66.7%`** | `35.8%` |
| `pydata/xarray` | 5 | `20.0%` | `60.0%` | `80.0%` | `100.0%` | **`20.0%`** | `29.8%` |
| `pylint-dev/pylint` | 6 | `50.0%` | `50.0%` | `50.0%` | `100.0%` | **`50.0%`** | `28.9%` |
| `pytest-dev/pytest` | 17 | `35.3%` | `41.2%` | `76.5%` | `100.0%` | **`23.5%`** | `21.2%` |
| `scikit-learn/scikit-learn` | 23 | `73.9%` | `87.0%` | `95.7%` | `91.3%` | **`60.9%`** | `20.0%` |
| `sphinx-doc/sphinx` | 16 | `62.5%` | `62.5%` | `68.8%` | `100.0%` | **`50.0%`** | `25.6%` |
| `sympy/sympy` | 77 | `42.9%` | `61.0%` | `64.9%` | `96.1%` | **`36.4%`** | `22.1%` |
| **Total / Weighted Mean** | **300** | **`50.3%`** | **`66.0%`** | **`75.0%`** | **`96.7%`** | **`42.7%`** | **`24.8%`** |

---

## Using `OmniCoder-Gemma-E2B` Locally in LM Studio or Python

### Option A: Load Directly in LM Studio (`GGUF`)
The merged and quantized GGUF model is installed directly into your local LM Studio catalog under:
- `C:\Users\livin\.lmstudio\models\Calera-Labs\OmniCoder-Gemma-E2B-GGUF\`

To run with `omnicoder`:
```bash
export OPENAI_BASE_URL="http://localhost:1234/v1"
export OMNICODER_MODEL="omnicoder-gemma-e2b"
omnicoder solve --issue-file ISSUE.md --workspace ./repo
```

### Option B: Load via HuggingFace `transformers` + `peft`
```python
import torch
from transformers import AutoTokenizer, AutoModelForCausalLM
from peft import PeftModel

base_id = "google/gemma-4-e2b"
adapter_dir = "training/checkpoints/omnicoder-gemma-e2b"

tokenizer = AutoTokenizer.from_pretrained(adapter_dir)
base_model = AutoModelForCausalLM.from_pretrained(
    base_id, torch_dtype=torch.bfloat16, device_map="auto"
)
model = PeftModel.from_pretrained(base_model, adapter_dir)
model.eval()
```
