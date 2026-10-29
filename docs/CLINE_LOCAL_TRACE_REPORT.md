# OmniCoder traces and local Cline

**Date:** 2026-10-29  
**Question:** Where are the trace records, and how is OmniCoder doing with local Cline models?

**Shipped the same day.** Interactive clients now get an 8 KB cap on read, diff, list, and search results, with the next action in `next`. `omnicoder_doctor` defaults to the cheap profile for Cline, Cursor, and Antigravity, and an identical repeat returns `UNCHANGED` plus `git_commit`. `omnicoder_git_commit` resolves to `git_commit` without adding a listed tool. `{path, old, new}` is the advertised patch shape, and a JSON string in `chunks` is parsed. Trace rows include `client` and `model`, and a workspace switch also appends to that workspace's `.omnicoder/mcp_trace.jsonl`. `.clinerules` only says not to call `omnicoder_run` or `omnicoder_swarm` from chat.

## Where the traces are

This checkout (`omnicoder-icx`) has the tracer (`pkg/mcp/trace.go`) and no trace file. There is no `.omnicoder/mcp_trace.jsonl` here.

Day 10 has the early log, and it is not the log of the local Cline sessions:

| Log | Events | Span | What it is |
|---|---:|---|---|
| `Vibe Code Streams/Day 10/.omnicoder/mcp_trace.jsonl` | 166 | 2026-10-06 to 2026-10-08 | Bring-up of OmniCoder itself. 64 KB. |
| `2027 Projects/.omnicoder/mcp_trace.jsonl` | 3,113 | through 2026-10-27 | Shared server log. 1,082 of those events have workspace `omnicoder-icx` (40 on Oct 23, 1,032 on Oct 26). |
| `StreamGoblin/.omnicoder/mcp_trace.jsonl` | tail of file | 2026-10-28 18:44–20:39 local | The local Cline session. The MCP process was started in StreamGoblin, then `omnicoder_set_workspace` moved the workspace to Zero Cost Projects. Later calls stayed in this file. |

The tracer does not record the client or the model. A row is a tool name, arguments, duration, status, result size, and workspace. Cline versus Cursor versus Antigravity has to be joined by timestamp from the VS Code Cline log.

The join for the evening of 2026-10-28 is clean. Cline’s log (`1-Cline.log`, session `20261028T182812`) and the StreamGoblin trace share the same call times, down to the millisecond on the doctor loop.

## The local model that actually ran

Cline on this machine is not using Cline’s cloud. Auth is unset. Configured providers are Ollama, Spark Lama, SAP AI Core, LM Studio, and an OpenAI-compatible endpoint. At startup the last-used provider was Spark Lama. The user then saved LM Studio, and both tasks resolved as:

- provider: `lmstudio`
- model: `qwen/qwen3.8-27b`
- API key: none
- cost: `$0`

Older Cline task history on this machine (March 2026, Fast_API, not OmniCoder) used the same pattern: `qwen3-coder:30b` and `gemma3:27b-it-q4_K_M` through local servers, plus one `gpt-oss:120b` Ollama run. Those tasks also show a huge input and a small output. The October 28 session is the one that actually drove OmniCoder.

## What the local session did

Two tasks, both on Zero Cost Projects, both docs work. Times are local (UTC−5).

**Task 1, 18:36–19:51.** “make a new update in the timeline.” Cline’s system prompt was 5,997 characters, then 4,287 after a mode rebuild. Input tokens grew from 8,977 to 92,529. Most replies were 70–500 tokens. One reply was 1,694. Turns took about 2–8 minutes. One turn, from 19:04 to 19:30, took 26 minutes and ended in `omnicoder_set_workspace`.

Before that switch the model used Cline’s own tools (`read_files`, `run_commands`, `search_codebase`, `editor`). After the switch, OmniCoder did the writes:

| Time | Tool | Result | Latency | Result size |
|---|---|---|---:|---:|
| 19:30 | `omnicoder_set_workspace` → Zero Cost Projects | OK | 160 ms | 273 B |
| 19:31 | `omnicoder_patch` `docs/paper.md` (6 chunks) | OK | 2 ms | 288 B |
| 19:32 | `omnicoder_patch` `UPDATES.md` (3 chunks) | OK | 1 ms | 285 B |
| 19:32 | `omnicoder_heal` `go build/vet/gofmt/test` | OK | 7.8 s | 320 B |
| 19:33 | `omnicoder_patch` Aider block on `UPDATES.md` | OK | 2 ms | 236 B |
| 19:44 | `omnicoder_doctor` (full gates) | OK | 2.9 s | 1,339 B |
| 19:44–19:45 | `omnicoder_doctor` ×5, gates and lint and git off | OK, identical | 0 ms | 453 B |
| 19:50 | two more patches (`paper.md`, `UPDATES.md`) | OK | 1 ms | ~240 B |

The five stripped doctor calls were the same arguments and the same 453-byte body. Cline stopped the loop: “Detected 5 consecutive identical calls to `omnicoder__omnicoder_doctor`.” The recovery text then told the model to call `omnicoder_git_commit`. That name does not exist. The registered tool is `git_commit`, which Cline displays as `omnicoder__git_commit`. Task 1 finished the roster update in the docs and did not commit.

**Task 2, 20:19–20:39.** “add an external project tracker.” A cancel was logged at 20:23. The run continued. Input climbed from about 9,000 to 78,610 tokens. One reply was 3,935 tokens, then `write_file` created `docs/projects.md` (about 5.7 KB of content). Patches to `paper.md`, `README.md`, and `UPDATES.md` succeeded. `omnicoder_heal` passed in 2.9 s. `git_commit` of the four doc files succeeded in 108 ms at 20:39. `git_status` afterward was clean.

Tool failures in this session: none. The tools that ran returned OK. The session still burned an hour and ~90k tokens on a docs edit, and the commit in task 1 never happened.

## What the other logs say about the same tools

These rows are not labeled Cline. They are the same MCP server. They show the failure modes a smaller model will hit, because the Oct 26 burst on this repo is large enough to see them.

**Day 10 bring-up, 166 events, 152 OK (91.6%), 9 validation errors, 5 errors.**

- 9 of 25 `omnicoder_set_workspace` calls omitted `path`. That is the whole validation pile.
- `omnicoder_doctor` averaged 45 s (one call 101 s) and returned a small body. The model waited; the payload was not the cost.
- One `git_diff` returned 3.8 MB. A 27B local model cannot take that back into the next turn.
- Patch errors were “file does not exist yet” and a bad line index, not fuzzy-match failures.

**`omnicoder-icx` workspace inside the shared log, 1,082 events, 1,059 OK (97.9%).** Almost all of it is 2026-10-26.

| Tool | Calls | Typical latency | Notes |
|---|---:|---|---|
| `read_file` | 554 | 1 ms | 2.5 MB returned. 15 reads were over 20 KB. |
| `omnicoder_patch` | 366 | 1–2 ms | 20 failures (5.5%). |
| `omnicoder_search` | 47 | p50 120 ms | Fine on this repo. The shared log also contains one 47-minute search on 2026-10-06. |
| `omnicoder_doctor` | 10 | p50 77 s, max 155 s | Gates dominate. The body is ~1.6 KB. |
| `omnicoder_map` | 1 | 1 ms | Almost unused. |

Patch failures on this repo, 20 of 366:

- 11 search-block mismatches (fuzzy match exhausted on `pkg/ui` tests, `plan.go`, the product paper).
- 5 times `chunks` was a string instead of an array.
- 3 Aider `SEARCH` / `=======` parse errors.
- 1 syntax-guard rejection, which is the guard working.

The local Cline session did not hit these, because it patched markdown with either a real chunk array or a well-formed Aider block. The schema still has three dialects (`chunks`, a `patch` string, and inline Aider text). A 27B model will pick the wrong one as soon as the edit is code rather than a short markdown line.

## Why local Cline is slow even when the tools are fast

OmniCoder’s own paper describes the failure mode the session measured. Decision cards are supposed to keep a model under ~300 tokens per step and reset the window. That path runs when OmniCoder owns the loop (`omnicoder run` / heal’s inner loop). Cline does not use it.

In the interactive session OmniCoder is a stdio tool server inside Cline’s cumulative transcript. Every tool result is appended, and the next LM Studio request resends the whole transcript. Measured growth on task 1 was 9k → 92k input tokens, with replies of ~100–500 tokens, on a 27B model served locally. The MCP calls themselves were 0–3 seconds, except heal at 3–8 seconds. The minutes were the model re-reading Cline’s history.

Two naming details make that transcript harder for a small model:

- Cline prefixes every tool with the server name. `omnicoder_patch` is shown as `omnicoder__omnicoder_patch`. `git_commit` is shown as `omnicoder__git_commit`. The model, and Cline’s own loop-recovery text, invented `omnicoder_git_commit`.
- Tool descriptions in `pkg/mcp/omnicoder_tools.go` are registered twice and run about 8,700 characters before the JSON schemas. Day 10’s `.clinerules` adds another long routing essay. This checkout has no `.clinerules` at all.

Day 10’s rules also tell the agent to prefer native reads, native edits, and native shell, and to treat `omnicoder_doctor` as the health check. The local model followed that. It read through Cline, then called doctor until Cline cut it off. The rule and the tool result both say “this succeeded,” so a small model calls it again.

## What to change for Cline and local deployments

Ordered by what the traces show, not by the architecture paper.

1. **Stamp every trace row with client and model.** On `initialize`, persist `clientInfo.name` (Cline already sends this in the e2e test shape). Add an optional argument or header for provider and model id. Without that, the next local run is again only recoverable by joining a VS Code log.

2. **Write the trace into the active workspace.** After `omnicoder_set_workspace`, append to `<workspace>/.omnicoder/mcp_trace.jsonl`, and keep a user-level mirror. The Cline session’s Zero Cost Projects work is sitting in StreamGoblin’s log. That is why “is the trace here or in Day 10?” does not have a single answer.

3. **Make a repeated doctor call a short refusal.** If tool, arguments, and workspace match the previous call and the tree has not changed, return a one-line body: unchanged, do not call again, next step is `git_commit` or stop. The five identical 453-byte OK results were counted as progress. A 0 ms success is the wrong signal.

4. **Default doctor to the cheap profile for interactive clients.** Full gates took 45–155 seconds in the Day 10 and Oct 26 logs and 2.9 seconds even on the small Go repo. The calls the model actually repeated had already turned gates, lint, and git off. Interactive default: git plus version plus artifact presence. Gates only when `check_gates` is explicitly true.

5. **Cap what goes back to the model.** Hard-cap `read_file`, `git_diff`, `list_directory`, and search results (8 KB is enough for a 27B turn) and return a path plus a range for the rest. One Day 10 diff was 3.8 MB. The Oct 26 reads on this repo totaled 2.5 MB. Those bytes are re-sent on every later Cline turn.

6. **One patch shape for the local profile.** Accept `{path, old, new}` and keep Aider blocks as a fallback inside the server. Stop advertising `chunks` as either an array or a string. Five of the twenty patch failures on this repo were exactly that type error. The successful Cline edits were short unique markdown lines; code edits in `pkg/ui` were the fuzzy-match failures.

7. **One visible name for commit.** Register `git_commit` and accept `omnicoder_git_commit` as an alias. In a local/Cline profile, prefer short names (`status`, `map`, `search`, `patch`, `heal`, `doctor`, `commit`) so Cline’s `omnicoder__` prefix does not produce `omnicoder__omnicoder_*`.

8. **Ship a short `.clinerules` for local models, and do not ship the Day 10 essay to them.** Ten lines: call `omnicoder_map` once before reading files; do not `read_file` a file over a few hundred lines; call `omnicoder_doctor` at most once per task; the commit tool is `git_commit`; do not call `omnicoder_run` or `omnicoder_swarm` from chat. The anti-nesting rule already in the tool description is right. The “prefer native read/edit/shell” rule fights the product on a local model, because those native results are what filled the 90k-token transcript.

9. **Give Cline a card-sized tool result.** `omnicoder_patch`, `omnicoder_heal`, and `omnicoder_doctor` already return a few hundred bytes, and that part is healthy. Extend the same budget to map and search, and put the next action in the first line (`patched docs/paper.md`, `tests passed`, `commit with git_commit`). A local model will follow the first line and skip the rest.

The tools are fast enough for local use. Patch, status, search, and commit on the Cline session were milliseconds to a couple of seconds, and the docs task did land, including a real commit. The cost is the outer transcript: unnamed traces, a doctor result that invites a retry, a commit tool the model cannot find by the name it is given, and file payloads large enough to stall a 27B model for the rest of the hour.
