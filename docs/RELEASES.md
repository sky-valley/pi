# Release tracker

Release-level view of the pi Go port: each tag mapped to the upstream pi pin it
syncs to and the `@earendil-works/pi-ai` npm catalog the byte-goldens were
captured from. The commit-by-commit triage/port ledger lives in
[`UPSTREAM.md`](UPSTREAM.md); this file is the per-release summary.

- Tags are **annotated, unsigned** (`git tag -a`). Tagger identity:
  `Noam Y. Tenne <noam@10ne.org>`.
- A release tag points at the cycle's ledger/pin-advance commit (the tip of the
  sync), so the catalog + ledger are included.
- Versioning is git-tag-only — there is no `VERSION` file or in-source version
  constant. The npm number below is the upstream catalog version, not the port's.

## Releases

| Version | Date | Commit | Upstream pin | npm catalog | Headline |
|---|---|---|---|---|---|
| [`v0.2.7`](#v027) | 2026-06-24 | `b75f5da` | `a2e3e9d8` | pi-ai 0.80.2 | Catalog 0.79.10→0.80.2; models-runtime migration complete (auth substrate + Models runtime + request-scoped auth + api_key/env credential); OpenAI Responses terminal events; anthropic compat→catalog; header-only client auth |
| [`v0.2.6`](#v026) | 2026-06-22 | `d91f8b7` | `2417adb4` | pi-ai 0.79.9 | Catalog 0.79.9; chat-template thinking compat (latent); fuzzy-edit untouched-line preservation; legacy WSL bash stdin; session-branch linearization |
| [`v0.2.5`](#v025) | 2026-06-19 | `d5f2c73` | `56b22768` | pi-ai 0.79.8 | Catalog 0.79.8 (GLM-5.2 opencode-go, openrouter/fusion, Mistral prompt-caching data); no behavior change vs v0.2.4 |
| [`v0.2.4`](#v024) | 2026-06-17 | `a9b7e5c` | `29c1504c` | pi-ai 0.79.6 | GLM-5.2 reasoning_effort; null Responses content; provider-scoped env overrides; deepseek gate live |
| [`v0.2.3`](#v023) | 2026-06-16 | `c655c5a` | `f8a77f47` | pi-ai 0.79.4 | Docs-only: disclose default provider-attribution headers (no code change vs v0.2.2) |
| [`v0.2.2`](#v022) | 2026-06-16 | `39b3879` | `f8a77f47` | pi-ai 0.79.4 | 1h cache-write 2×; bash stdout drain; deepseek/gemini thinking gates; provider-attribution headers |
| [`v0.2.1`](#v021) | 2026-06-13 | `ca0d684` | `6f29450e` | pi-ai 0.79.3 | Catalog 0.79.3; Anthropic refusal details; fallback thinking flip; late-tool-update guard; Fable-5 gate live |
| [`v0.2.0`](#v020) | 2026-06-12 | `a2f0471` | `3f44d3e2` | pi-ai 0.79.1 | First synced catalog (Fable 5, zai payload, anthropic off:null gate, PI_EXPERIMENTAL); UPSTREAM.md pipeline |
| [`v0.1.1`](#v011) | 2026-06-11 | `b09cb46` | — | — | Built-in providers register on import (init()) |
| [`v0.1.0`](#v010) | 2026-06-10 | `1210b0a` | — | — | Initial tagged baseline |

## Notes

### v0.2.7
Rolls up **three upstream cycles** untagged since v0.2.6 (`2417adb4 → a2e3e9d8`):
the v0.79.10 catalog cycle, the 2026-06-23 model-registry adopt (`732bb161`
auth substrate + Models runtime + BuiltinModels), and the 2026-06-24 migration
completion. npm reference build advanced 0.79.9 → **0.80.2** (via 0.79.10,
0.80.0, 0.80.1, 0.80.2 — each regen supersedes the prior). The model-registry
migration is now **complete**. Per-cycle triage/port detail is in
[`UPSTREAM.md`](UPSTREAM.md); headline ports below.

- **Catalog → 0.80.2** — endpoint-pinned, re-derived byte-identical (386,548 B),
  integrity-verified `sha512-5GNKfdrR…uy9RQ==`. huggingface registration
  provider + glm-5.2/glm-5v-turbo; `off:null` tripwires intact.
- **Models-runtime migration** (`732bb161` + the 2026-06-24 follow-through) —
  auth substrate (credential store, provider auth, OAuth-under-lock), Models
  runtime (Provider iface, CreateModels/CreateProvider, BuiltinModels),
  request-scoped auth resolution (`ef231c49`), and the `api-key`→`api_key` /
  credential `metadata`→`env` alignment (`49fbe683`). Globals remain the compat
  consumer surface.
- **OpenAI Responses terminal events** (`cd95c274`) — `response.incomplete`
  finalizes like `response.completed`; the stream fails when it ends with no
  terminal event.
- **anthropic compat → catalog** (`6184307c`) — provider/baseUrl auto-detection
  removed; fireworks / cloudflare-ai-gateway-anthropic values come from the
  catalog. Byte-identical for catalog models.
- **header-only client auth + vercel routing ungate** (`129eb460`) — an
  authorization / cf-aig-authorization header satisfies auth without an api key;
  vercel gateway routing is no longer baseUrl-gated.
- **Deliberate divergences** (2026-06-24 ruling): `ProviderHeaders`
  null-suppression, cloudflare base-URL relocation, and compat
  `shouldUseBuiltinModels` routing are not transliterated — confirmed
  observably byte-identical through the Go compat-globals consumer path.

Reviewed via independent go-review (ship) + adversarial parity review (all
faithful; catalog re-derived byte-identical; 6/6 differential request diff).

### v0.2.6
Upstream sync `56b22768 → 2417adb4` — 22 main-line changes (**4 behavior/perf
ports + 1 catalog regen, 17 n/a, 0 decides**). npm reference build advanced
0.79.8 → **0.79.9**.

- **Catalog → 0.79.9** (`615bf2f8`) — endpoint-pinned byte-identical both ends
  (old ≡ 0.79.8 build, new ≡ 0.79.9, integrity-verified). 0 added, **2 removed**
  (`google/gemma-4-E2B-it`, `gemma-4-E4B-it`; no Go refs), 20 changed (cost/
  metadata churn + the folded-in data commits). `off:null` gates intact.
- **chat-template thinking compat** (`8b97e75c`) — new openai-completions
  `thinkingFormat:"chat-template"` emits configurable `chat_template_kwargs`
  ($var/omitWhenOff/scalar), with insertion-order-preserving output for
  byte-exact request bodies. **Latent**: no 0.79.9 catalog model sets it
  (reachable only via custom model config).
- **Fuzzy edit preserves untouched lines** (`128330e3`) — a fuzzy edit now
  rewrites only the touched line-blocks and copies every other line back
  verbatim, instead of globally normalizing the file.
- **Legacy WSL bash via stdin** (`1287b69f`) — the Windows-bundled
  System32/Sysnative `bash.exe` (which mishandles `-c "<cmd>"` quoting) is run
  as `bash -s` with the command on stdin.
- **Session-branch linearization** (`a1da88ae`) — O(n²) prepend replaced with
  append+reverse; behavior-neutral.

Reviewed via an independent idiomatic go-review (ship) + adversarial parity
review (5/5 faithful; catalog endpoint-pinned byte-identical, tripwire +
orphaned-id checks passed); `-race` suite green.

### v0.2.5
Upstream sync `29c1504c → 56b22768` — 32 main-line changes, **0 behavior
ports, 32 n/a, 0 decides**. npm reference build advanced 0.79.6 → **0.79.8**
(two release tags crossed; v0.79.7 superseded by v0.79.8).

- **Catalog → 0.79.8** (`8eb9704b`) — endpoint-pinned byte-identical both ends
  (old ≡ 0.79.6 build, new ≡ 0.79.8 build, integrity-verified). +9/−3 ids
  (opencode-go GLM-5.2, openrouter/fusion alias, fireworks glm-5p2,
  poolside/qwen/cohere/gemini-3-pro-image/liquid; pruned glm-5/raptor-mini/
  xiaomi-mimo); 44 changed entries are data churn (Mistral prompt-caching cost
  fields, fireworks/openrouter/vercel metadata). `off:null` gates intact.
- **No behavior change vs v0.2.4.** The substantive upstream changes landed on
  unported surface: the compaction trio (overflow-retry recovery, empty-summary
  guard, post-compaction token estimates) edits the agent-session-runtime
  auto-compaction orchestration + event lifecycle the Go port doesn't have;
  RPC unknown-command id (`modes/rpc`), Mistral prompt-caching (Mistral
  provider), and the `CONFIG_DIR_NAME`/edit-diff SDK exports are all out of
  scope.

Reviewed via an independent adversarial parity review (catalog endpoint-pinned;
schema-drift, tripwire, and orphaned-id checks passed); `-race` suite green.

### v0.2.4
Upstream sync `f8a77f47 → 29c1504c` — 20 main-line changes (3 ported, 16 n/a,
1 decide ruled). npm reference build advanced 0.79.4 → 0.79.6.

- **Z.AI GLM-5.2 native reasoning_effort** (`75b0d723`) — emits the
  `thinkingLevelMap`-mapped effort alongside `thinking:{type}`; `minimal:null`
  omits the field.
- **Null Responses message content** (`2d597f02`) — no code change; Go ranges a
  nil slice safely, matching pi's `?? ""`. Locked with a regression test.
- **Provider-scoped env overrides** (`7f29e7a3`, owner-ruled) — `StreamOptions.Env`
  consulted ahead of `os.Getenv` for `PI_CACHE_RETENTION` + Cloudflare base-URL.
  Bun `/proc` fallback omitted (no Go analog); host-side population unported.
- **Deepseek disabled-thinking gate went live** — 0.79.6 ships Kimi K2.7 Code
  `off:null`; tripwire converted to `TestDeepseekDisabledThinkingGateLive`.

Reviewed via independent go-review + adversarial parity (request diff 12/12).

### v0.2.3
Docs-only release: README disclosure that the SDK sends pi's attribution headers
by default and how to disable (`PI_TELEMETRY=0`) or override
(`model.Headers`/`opts.Headers`). No code change vs v0.2.2.

### v0.2.2
Upstream sync to `f8a77f47` (pi 0.79.4). Anthropic 1h cache-write priced at
2×input; bash stdout drained past child exit; deepseek `off:null` +
`gemini-flash-latest` thinking gates; provider-attribution headers
(OpenRouter/NVIDIA/Cloudflare/Vercel/OpenCode, `PI_TELEMETRY`-gated). Review
caught an attribution header-precedence divergence, fixed and re-verified.

### v0.2.1
Upstream sync `3f44d3e2 → 6f29450e` (pi 0.79.3). Catalog at 0.79.3; Anthropic
refusal-detail error messages; custom-fallback thinking reasoning flip; agent
ignores late tool-progress updates. Claude Fable 5 disabled-thinking gate went
live (0.79.3 ships `off:null`). Parity 4/4, request diff 6/6.

### v0.2.0
First catalog-synced release (pi 0.79.1): Claude Fable 5 + moonshot/opencode
compat, zai thinking payload, anthropic `off:null` thinking gate, `:thinking`
suffix in custom-model fallback, `PI_EXPERIMENTAL` guard. Introduced
`docs/UPSTREAM.md` and the daily sync pipeline.

### v0.1.1
Fixes the SDK trap where importing only the coding package raised
"No API provider registered" on the first live call — providers now register via
`init()` (pi's module-load side effect), wired through coding's import.

### v0.1.0
Initial tagged baseline of the Go port.
