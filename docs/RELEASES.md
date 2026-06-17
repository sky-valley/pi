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
| [`v0.2.4`](#v024) | 2026-06-17 | `a9b7e5c` | `29c1504c` | pi-ai 0.79.6 | GLM-5.2 reasoning_effort; null Responses content; provider-scoped env overrides; deepseek gate live |
| [`v0.2.3`](#v023) | 2026-06-16 | `c655c5a` | `f8a77f47` | pi-ai 0.79.4 | Docs-only: disclose default provider-attribution headers (no code change vs v0.2.2) |
| [`v0.2.2`](#v022) | 2026-06-16 | `39b3879` | `f8a77f47` | pi-ai 0.79.4 | 1h cache-write 2×; bash stdout drain; deepseek/gemini thinking gates; provider-attribution headers |
| [`v0.2.1`](#v021) | 2026-06-13 | `ca0d684` | `6f29450e` | pi-ai 0.79.3 | Catalog 0.79.3; Anthropic refusal details; fallback thinking flip; late-tool-update guard; Fable-5 gate live |
| [`v0.2.0`](#v020) | 2026-06-12 | `a2f0471` | `3f44d3e2` | pi-ai 0.79.1 | First synced catalog (Fable 5, zai payload, anthropic off:null gate, PI_EXPERIMENTAL); UPSTREAM.md pipeline |
| [`v0.1.1`](#v011) | 2026-06-11 | `b09cb46` | — | — | Built-in providers register on import (init()) |
| [`v0.1.0`](#v010) | 2026-06-10 | `1210b0a` | — | — | Initial tagged baseline |

## Notes

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
