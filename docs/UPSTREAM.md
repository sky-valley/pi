# Upstream provenance & sync ledger

Tracks exactly which upstream pi the Go port corresponds to, and the
commit-by-commit sync pipeline that keeps it current.

- **Upstream**: https://github.com/earendil-works/pi (TypeScript, by Mario Zechner)
- **This port started**: 2026-06-08 (cloned upstream `main` HEAD of the day)

## Current pin

| What | Value |
|---|---|
| TS source fully reviewed/ported | `541d11f7` — "chore: approve contributor skhoroshavin" (2026-06-29; the substantive change this cycle is `b91bdd5a` Z.AI thinking content); previous pins `5a073885` (06-27), `622eca76` (06-26), `1d486163` (06-25), `09f10595` (06-25), `a2e3e9d8` (06-24), `470a4736` (06-23), `3b561346` (06-22), `2417adb4` (06-21), `56b22768` (06-19), `29c1504c` (06-17). The models-runtime migration is now **complete**: the `732bb161` substrate (06-23) plus the 06-24 follow-through (catalog-data reorg landed via the 0.80.2 regen; request-scoped auth `ef231c49`; api_key/env credential `49fbe683`; OpenAI Responses terminal events `cd95c274`; anthropic compat→catalog `6184307c`; header-only client auth + vercel ungate `129eb460`). |
| npm build the byte-goldens were captured from | `@earendil-works/pi-ai` **0.80.2** (catalog endpoint-pinned, re-derived byte-identical from `dist/models.generated.js`, lock integrity verified against the registry — `sha512-5GNKfdrR…uy9RQ==`; subsumes v0.80.0/v0.80.1); `pi-coding-agent` 0.78.1 (session/image goldens — unaffected by 0.80.x) |
| Parity proofs at the pin | catalog regen endpoint-pinned byte-identical (386,548 B, independently re-derived) · session tree 8/8 · image decisions 8/8 (unchanged this cycle) · differential request diff 6/6 (re-derived from the 0.80.2 build) · in-repo differential parity 36/36 · fireworks/cf anthropic compat coupling 0 mismatches (14 fireworks + 17 cf-anthropic models carry the fields the removed auto-detect synthesized) |
| Reviewed via | initial port + parity sweeps 1–2 (`3be3911`), registration fix (`b09cb46`); 2026-06-22 v0.79.10 cycle; 2026-06-24 v0.80.2 cycle independent go-review (ship, 3 optional LOW nits) + adversarial parity review (all 7 commits faithful, 6/6 differential, all 3 deliberate divergences confirmed observably-faithful); 2026-06-25 cycle (5 ports, no release) independent go-review (ship; one LOW `strings.Join` cleanup applied) + adversarial parity review (all 5 faithful; responses test-change mutation-verified non-vacuous; `reasoning,omitempty` confirmed acceptable-latent); 2026-06-26 cycle (1 port, no release) independent go-review (ship, no findings) + adversarial parity review (faithful; openai default-model lock mutation-verified non-vacuous); 2026-06-29 cycle (1 port, no release) independent go-review (ship, no findings) + adversarial parity review (faithful; zai `clear_thinking:false` mutation-verified non-vacuous; confirmed no 0.80.2-derived golden pins the zai request shape, so no latent divergence) |

Deliberately not ported (out of scope for the ledger unless a commit changes
that decision): TUI, extensions runtime, OAuth token acquisition, project-trust
gating, Bedrock/Vertex/Mistral/Azure/Codex providers, image generation, bun/CLI
packaging, prompt-templates, settings-manager, config migrations,
agent-session-runtime (session reload + /new flow), and the host-side machinery
that *populates* provider-scoped env overrides (resolve-config-value,
model-registry, settings) — the SDK `StreamOptions.Env` field is ported but
stays latent until a host sets it (see the 2026-06-17 ruling).

### Rulings (answers to `decide` escalations — triage must not re-ask)

- **2026-06-25 — adopt the relocated SDK retry classifier as a latent export**
  (re: `371adcf3` "retry explicit provider retry errors", #6019). Upstream moved
  `isRetryableAssistantError` out of host code (`coding-agent/src/core/agent-session.ts`)
  into the in-scope SDK package (`packages/ai/src/utils/retry.ts`) and added three
  new retryable patterns (`"you can retry your request"`, `"try your request
  again"`, `"please retry your request"`). Owner call (noam): **port it** — mirror
  pi's SDK structure even though the Go port currently has **no consumer** (Go's
  `MaxRetries`/`ai/providers/retry.go` is provider-HTTP-level backoff *within* one
  request; the assistant-turn auto-retry loop that calls this classifier lives in
  the unported agent-session-runtime). Consistent with the 2026-06-23 "maximum
  parity with the SDK package" adopt ruling: port the classifier + its
  non-retryable/retryable pattern sets as idiomatic Go in package `ai` (latent
  until an auto-retry loop consumes it). The `isContextOverflow` pre-check stays
  on the (unported) consumer side, as upstream keeps it in `agent-session.ts`.
  Future commits to `packages/ai/src/utils/retry.ts` (new patterns, etc.) are
  `port` under this ruling. This makes `371adcf3` a `port`, no longer a `decide`.

- **2026-06-24 — models-runtime migration completed under the "globals stay as
  compat" divergence** (re: the `129eb460` "complete models runtime migration"
  consolidation). The 06-23 adopt ruling stands (maximum parity + Go idioms);
  this records WHERE the Go port deliberately diverges in *structure* while
  staying byte-faithful through its actual consumer path. The Go coding agent
  streams via the **compat globals** (`ai.Stream` → `withEnvAPIKey` → raw
  provider), NOT the Models runtime — the on-record "globals stay as compat"
  divergence. Three pieces of `129eb460` are therefore **not transliterated**;
  the 2026-06-24 parity review confirmed each is observably byte-identical
  through the compat path (they "compensate precisely"):
  1. **`ProviderHeaders` null-suppression** (`Record<string,string|null>`) — NOT
     ported. `Headers` stays `map[string]string`. Porting it would change the
     public Go API (`StreamOptions.Headers`/`Model.Headers`) for a **latent**
     capability: zero 0.80.2 catalog models set a null header, and pi's only
     real null use (cloudflare-ai-gateway suppressing `Authorization`) is
     already handled in Go by a conditional skip, not a null marker. Revisit
     only if a consumer needs to suppress a default header.
  2. **Cloudflare base-URL placeholder resolution + `cf-aig-authorization`** —
     kept **inline** in the openai providers (`resolveCloudflareBaseURL`) rather
     than relocated to a `cloudflare-auth` layer. Verified byte-identical
     baseURL + headers vs pi's relocated version for gateway + workers-ai.
  3. **compat `shouldUseBuiltinModels` routing** — NOT ported. pi routes catalog
     models through the Models runtime (empty credential store + env-only auth +
     cloudflare-auth baseURL); Go's "raw provider + `withEnvAPIKey` + inline
     cloudflare" path resolves to the same bytes. Divergences (1)+(3) cancel.
  In scope and ported this cycle (all faithful): `ef231c49` (request-scoped auth
  overrides — the named `auth/resolve.ts` boundary edge), `49fbe683`
  (`api-key`→`api_key`, credential `metadata`→`env`), `cd95c274` (OpenAI
  Responses terminal-event requirement + compaction zero-usage guard),
  `6184307c` (anthropic compat now from catalog — byte-safe; the 0.80.2 catalog
  carries the fields the removed auto-detect synthesized), `129eb460`'s
  `getClientApiKey` "unused" sentinel + vercel routing ungate (byte-safe for the
  catalog). The catalog-data reorg (per-provider `*.models.ts` + huggingface
  registration provider) landed via the 0.80.2 regen. Future commits to the
  null-`ProviderHeaders` plumbing or compat-routing in `packages/ai/src` re-open
  this — re-judge against the compat-path equivalence above.

- **2026-06-23 — adopt the SDK-side model-registry / env-resolution overhaul**
  (re: the `732bb161` "Merge model-registry into main" merge + rider
  `2cbce395` "pass provider-resolved env to APIs"). Owner call: **maximum
  parity with the source + maximum use of Go idioms** — port the new
  `packages/ai/src/auth/` resolution layer (`context`/`credential-store`/
  `helpers`/`resolve`/`types`), give `AuthResult` its `env` (`ProviderEnv`),
  and populate `StreamOptions.Env` from `resolution.env` merged with explicit
  `options.env` inside the Go `models.ts`/`Stream` resolution path. This
  **supersedes the latency clause** of the 2026-06-17 ruling: `StreamOptions.Env`
  no longer stays latent-until-a-host-sets-it — the ported SDK resolution now
  populates it itself, as upstream moved that machinery out of host-side
  coding-agent and into `packages/ai/src`. The earlier-named "host-side
  population machinery (resolve-config-value, model-registry, settings)" is
  re-scoped accordingly: the part that now lives in `packages/ai/src` (the
  model-registry + auth resolution) is **in scope**; whatever remains in
  `coding-agent` host wiring stays out. Idiomatic Go, not transliterated TS
  (the `pi-go-review` bar applies). Consequences: `732bb161` and `2cbce395`
  are `port` (no longer `decide`); the catalog *data* reorg
  (`models.generated.ts` → per-provider `*.models.ts`, new providers) is still
  deferred to the next release regen since 0.79.10 was not re-published; new
  providers are catalog-data/registration and land with that regen unless they
  introduce genuinely new provider *behavior* (judge per provider at port
  time). `8eeaa2bc` (compat scoped-env API-key injection) remains a `port`
  under the 2026-06-17 plumbing clause and now composes with the populated
  `Env`. The `auth/resolve.ts` credential→env resolution is the new boundary
  edge: future commits to it in `packages/ai/src` are `port`.

- **2026-06-17 — provider-scoped env overrides ported faithfully** (re:
  `7f29e7a3`). Owner call: maximum parity. `StreamOptions.Env`
  (`map[string]string`) is consulted ahead of `os.Getenv` (helper
  `getProviderEnvValue`: non-empty scoped value wins, empty falls through —
  pi's `||`) for the two ported consumers pi touches: `PI_CACHE_RETENTION` and
  Cloudflare base-URL placeholders, across anthropic/openai-completions/
  openai-responses. pi's `getBunSandboxEnvValue` `/proc/self/environ` fallback
  is DELIBERATELY OMITTED — it works around oven-sh/bun#27802 (Bun compiled
  binaries expose an empty `process.env` in Linux sandboxes), a runtime defect
  Go does not have. The host-side population machinery stays unported (field
  latent, matching pi SDK consumers that don't populate it). Future commits to
  the env-override *plumbing in ported providers* are `port`; commits only to
  the unported host-side population are `n/a`.

- **2026-06-16 — provider-attribution ported faithfully** (port-it ruling); SDK
  sends pi's default attribution headers (http-referer/x-title/...) on the
  providers pi does.

- **2026-06-12 — project trust stays excluded** (re: `718215bd`, `d8aef0fe`,
  and the wider upstream trust push). Criteria set by the owner: not an SDK
  use case (host apps control what loads), postponable (purely additive
  subsystem), and verified not to change behavior of ported surface (the only
  ported-adjacent diff was a behavior-neutral refactor inside the unported
  extension resource-loader; `skills.ts` untouched). Future trust commits are
  `n/a` under this ruling UNLESS they change behavior of surface we ported —
  that re-escalates.

## Drift at last sync check (2026-06-29) — pin advanced to 541d11f7

**Caught up to `541d11f7`.** Delta `5a073885 → 541d11f7` fully processed: 6
main-line changes — **1 port (→ 1 Go commit), 5 n/a, 0 decides**. **No release
tag crossed** — the Z.AI fix's CHANGELOG entry lands in upstream `[Unreleased]`,
no `package.json` bump; `pi-ai` stays **0.80.2** and `pi-coding-agent` stays
**0.78.1**, so every byte-golden (catalog, session tree, image decisions,
differential request diff) is untouched. Reviewed via independent go-review
(ship, no findings) + adversarial parity review (faithful). gofmt clean;
build/vet/`-race` green.

- **preserve Z.AI thinking content** (`b91bdd5a`, Go `692984a`): the zai
  `thinkingFormat` enabled payload in `applyReasoningFormat`
  (`ai/providers/openai.go:914-924`) now carries `clear_thinking:false` alongside
  `type:"enabled"` (#6083); the disabled payload stays bare `{type:"disabled"}`.
  Mirrors `openai-completions.ts buildParams`'s ternary on
  `options?.reasoningEffort` (Go's `enabled := level != ""`). **Request-body
  golden surface** (zai-format models with effort), but **no latent divergence**:
  the published 0.80.2 build still emits the bare shape, yet no zai-with-effort
  request body is pinned in any 0.80.2-derived golden or differential scenario
  (the in-repo `TestDiffZaiGLM52ReasoningEffort` is now aligned to the
  `[Unreleased]` shape). Test: `TestDiffZaiGLM52ReasoningEffort` tightened
  (`clear_thinking==false` when enabled, key absent when disabled;
  mutation-verified non-vacuous).

n/a (5): `234c2ad5` (**get_entries/get_tree RPC commands**, #6078 —
`modes/rpc/*` + docs + test, plus a one-line `index.ts` re-export of the
`SessionTreeNode` type; RPC mode is host/CLI surface, same class as the 06-27
orchestrator `rpc-entry.ts` ruling, no ported behavior); `a8c692c7` (**avoid
pre-prompt compaction continue**, #6074 — only `core/agent-session.ts`, removes a
pre-prompt `agent.continue()`/`_handlePostAgentRun` loop; agent-session-runtime,
deliberately not ported, consistent with the compaction-trio rulings);
`54113731` (**HTTP timeout for Codex SSE headers**, #4945 —
`openai-codex-responses.ts` only, Codex provider unported); `8f64353e`
(**restrict bot gate bypasses**, #6127 — `.github/workflows/*`); `541d11f7`
(**approve contributor skhoroshavin** — `.github/APPROVED_CONTRIBUTORS`,
contributor-approval meta). No new boundary questions.

## Drift at last sync check (2026-06-28) — pin advanced to 5a073885

**Caught up to `5a073885`.** Delta `622eca76 → 5a073885` fully processed: 2
main-line changes — **0 port, 2 n/a, 0 decides**. **No release tag crossed** —
`pi-ai` stays **0.80.2** and `pi-coding-agent` stays **0.78.1** (both CHANGELOG
entries land in `[Unreleased]`, no `package.json` version bump); no
`models.generated` regen, so every byte-golden (catalog, session tree, image
decisions, differential request diff) is untouched. Report-only triage; no Go
code changed (pin advance only).

n/a (2): `f2e9d753` (**preserve backslash escapes in user messages**, #6105 —
TUI-only: a markdown-rendering escape fix in `tui/src/components/markdown.ts` +
its test, plus the `modes/interactive/components/user-message.ts` caller; modes/
TUI surface, no SDK/agent/coding-agent core, no golden surface);
`5a073885` (**add external editor setting**, #6122 — host/TUI config feature:
`core/settings-manager.ts` (settings-manager — on the deliberately-not-ported
list) gains an external-editor preference consumed only by the interactive mode
(`modes/interactive/extension-editor.ts` + `interactive-mode.ts`), plus docs. No
ported behavior, no provider/tool/request-body change). No new boundary
questions.

## Drift at last sync check (2026-06-27) — pin advanced to 622eca76

**Caught up to `622eca76`.** Delta `1d486163 → 622eca76` fully processed: 2
main-line changes — **0 port, 2 n/a, 0 decides**. **No release tag crossed** —
`pi-ai` stays **0.80.2** and `pi-coding-agent` stays **0.78.1**; no
`models.generated` regen, so every byte-golden (catalog, session tree, image
decisions, differential request diff) is untouched. Report-only triage; no Go
code changed (pin advance only).

n/a (2): `87ad8243` (**pi orchestrator** — new `packages/orchestrator/`
experimental package: a host-side multi-process supervisor with IPC/RPC/
supervisor/radius/storage, entirely additive. The only `coding-agent/src` touch
is the brand-new `rpc-entry.ts`, a 12-line `--mode rpc` process entrypoint —
host/main/CLI surface, same class as the unported `main.ts`/modes wiring. The
`"version": "0.80.2"` is just the new package declaring itself; no `pi-ai` bump,
no catalog regen, no tag. Not a boundary question: it's a separate supervisor
process that doesn't make any non-ported area load-bearing for the SDK and adds
no new provider/tool on the ported `ai`/`agent`/`coding-agent` core);
`622eca76` (**installer lock generation** — packaging/CI/release tooling: new
`coding-agent/install-lock/` package, a `generate-coding-agent-install-lock.mjs`
script, a `build-binaries.yml` change, and one line in `scripts/release.mjs`
adding `install-lock:coding-agent` to the release-artifact regen. No ported
behavior, no version bump, no tag). No new boundary questions.

## Drift at last sync check (2026-06-26) — pin advanced to 1d486163

**Caught up to `1d486163`.** Delta `09f10595 → 1d486163` fully processed: 6
main-line changes — **1 port (→ 1 Go commit), 5 n/a, 0 decides**. **No release
tag crossed** — all 6 land on upstream `[Unreleased]`; `pi-ai` stays **0.80.2**
and `pi-coding-agent` stays **0.78.1**, so every byte-golden (catalog, session
tree, image decisions, differential request diff) is untouched. Reviewed via
independent go-review (ship, no findings) + adversarial parity review (faithful;
the default-model lock mutation-verified non-vacuous). gofmt clean;
build/vet/`-race` green.

- **OpenAI default model** (`77428858`, Go `c83f84f`): `defaultModelPerProvider`
  in `coding/resolve.go` advances openai `gpt-5.4 → gpt-5.5`, matching pi's
  `model-resolver.ts`. **Only openai moves** — `azure-openai-responses` and
  `github-copilot` stay `gpt-5.4`, and `openai-codex` was already `gpt-5.5`. This
  is the per-provider template id `buildFallbackModel` clones when synthesizing a
  custom-id model under a known provider; it lives in pi-coding-agent surface (not
  the pi-ai catalog), so no npm catalog regen is involved. Test:
  `TestDefaultModelPerProviderOpenAI` (locks the four openai-family ids).

n/a (5): `e454f50b` (allow session id for no-session runs — `SessionManager.inMemory`
gains an id option + `main.ts` `--no-session`/`--session-id` flag plumbing; host/CLI
surface, and Go's `Session.SessionID` field already accepts an id independent of
persistence — no `validateSessionIdFlags` consumer in the port); `543710f6` +
`0d145e89` (reject invalid session files / shorten the error — pi's `setSessionFile`
now throws on a non-empty unparseable file instead of truncating, but Go's
`ResumeSession` already rejects headerless files without modifying them, so the
fix is already-faithful; the `main.ts openSessionOrExit` console-error+exit and the
error-string wording are host/CLI); `f14b3594` (show length stop errors —
`interactive/components/assistant-message.ts`, TUI); `1d486163` (fix examples +
undici vuln bump — `examples/extensions/*` + `package-lock.json`, packaging). No
new boundary questions.

## Drift at last sync check (2026-06-25) — pin advanced to 09f10595

**Caught up to `09f10595`.** Delta `a2e3e9d8 → 09f10595` fully processed: 13
main-line changes — **5 ports (→ 5 Go commits), 7 n/a, 1 decide (ruled: port)**.
**No release tag crossed** — all 5 ports land against upstream `[Unreleased]`;
`pi-ai` stays **0.80.2**, the npm reference build is unchanged and every existing
byte-golden (catalog, session tree, image decisions, 14/14 differential) stays
valid. Reviewed via independent go-review (ship; one LOW `strings.Join` cleanup
applied) + adversarial parity review (5/5 faithful). Build/vet/`-race` green;
14/14 differential request-body diff byte-identical (the clamp is a no-op for
those scenarios).

- **retry classifier** (`371adcf3`, Go `23d15ef`): new `ai/retry_classify.go`
  exporting `IsRetryableAssistantError` (the two pattern sets byte-faithful incl.
  the 3 new #6019 strings `you can retry your request` / `try your request again`
  / `please retry your request`). **Latent SDK export** — no Go consumer yet
  (Go's `ai/providers/retry.go` is provider-HTTP backoff *within* a stream; the
  assistant-turn auto-retry loop that consumes this lives in the unported
  agent-session-runtime). Adopted per the 2026-06-25 ruling. `isContextOverflow`
  pre-check deliberately left on the consumer side, matching pi. Test:
  `TestIsRetryableAssistantError`.
- **reasoning token counts on Usage** (`d7868b09`, Go `339cb48`): `Usage.Reasoning`
  (`int`, `json:"reasoning,omitempty"`); anthropic sets it only when
  `output_tokens_details.thinking_tokens` present, openai-completions/
  openai-responses/google set it unconditionally (`|| 0`). Tests:
  `TestAnthropicReasoningTokens`, `TestOpenAICompletionsReasoningTokens`,
  `TestResponsesReasoningTokens`.
  **LATENT DIVERGENCE (must reconcile on the next pi release that publishes
  `d7868b09`):** `omitempty` drops `reasoning:0` from session JSON, whereas pi
  emits `reasoning:0` unconditionally for openai-completions/openai-responses/
  google. Acceptable *now* — the published build (≤0.80.2) has no `reasoning`
  field and the existing sessparity goldens (e.g.
  `coding/testdata/sessparity/8_agent_message_roles.json`) carry no `reasoning`
  key, so `omitempty` keeps the port byte-faithful to what real pi currently
  emits. When d7868b09 ships in an npm build: regenerate the sessparity goldens
  from that build AND drop `omitempty` for the reasoning field (or split
  anthropic-optional vs others-always-present), then re-verify the goldens.
- **responses out-of-order reasoning** (`8c9dbffa`, Go `f546acc`): rewrote the
  Responses stream parser from a single `current` pointer to a
  `map[int]*responsesOutputSlot` keyed by `output_index`, with
  `getSlot`/`createSlot`/`getOrCreateSlot`; emitted events now carry the slot's
  stable `contentIndex` (#6009). Response-parse only (request bytes unchanged).
  Faithful behavior shift confirmed against pi: a `function_call` `output_item.done`
  with no prior `added` now create-on-dones (block lands in content, `toolcall_start`
  fires, stop→toolUse) — `TestResponsesFunctionCallDoneWithoutAdded` updated to
  pi's behavior (mutation-verified non-vacuous). New regression:
  `TestResponsesOutOfOrderItemsPreserveReasoning`.
- **BMP read-tool support** (`4cc339f5`, Go `5f9c464`): `isBmp` magic+DIB
  validation (byte-exact offsets), BMP→PNG conversion (Go `x/image/bmp` +
  `png.Encode`) wired into the read tool's `processImage`; tool-description string
  → `(jpg, png, gif, webp, bmp)` (byte-golden, exact); hint/omit strings
  byte-faithful. No golden pins converted PNG bytes. Tests: `TestDetectMimeBMP`,
  `TestDetectMimeInvalidBMPRejected`, `TestReadBMPConvertsToPNG`.
- **clamp streamSimple max tokens** (`09f10595`, Go `4fed697`): new `ai/estimate.go`
  (`estimateContextTokens`; constants `charsPerToken=4`, `estimatedImageChars=4800`;
  `jsStringLength` via `utf16.Encode` = JS `.length`) + `ai/simple_options.go`
  (`ClampMaxTokensToContext`, `contextSafetyTokens=4096`, `minMaxTokens=1`) wired
  into all 4 Go streamSimple providers; anthropic thinkingBudget re-clamp
  `min(budget, max(0, maxTokens-1024))`. **Request-body golden:** streamSimple now
  always sends a clamped `max_tokens = clamp(model.maxTokens)` where it could
  previously omit it — faithful to pi, which flipped its own
  `openai-completions-empty-tools` test the same way. No-op for the existing
  differential scenarios (low-level builders untouched; 14/14 byte-identical).
  Tests: estimate/clamp units + streamSimple `3904`-clamp assertions.

**Catalog regen DEFERRED** (`9cd2c81a`): per-provider `*.models.ts` churn
(huggingface, vercel-ai-gateway, openrouter, minimax) lands in `[Unreleased]` —
no npm publish, so it folds into the next release regen (advance only when a tag
crosses). The `b940c52e` MiniMax shared-budget clamp was net-reverted by
`f78b1637` (source diff = 0); its minimax `maxTokensSharesContextWindow` churn
also folds into the deferred regen.

n/a (7): `3e551faf` (interactive-mode resource/notify ordering — TUI);
`5c76ae40` (extension-stats — startup-timing instrumentation + extensions loader,
no Go analog); `b940c52e` + `f78b1637` (MiniMax clamp add + full revert, net-zero
source); `c29bbc09` (docs/models.md); `6ca7ba7c` (`.github` contributor);
`49956a7c` (`.pi/prompts`). The lone `decide` (`371adcf3`) was ruled **port** on
2026-06-25 (see Rulings). No new boundary questions.

## Drift at last sync check (2026-06-24) — pin advanced to a2e3e9d8

**Caught up to `a2e3e9d8`.** Delta `470a4736 → a2e3e9d8` fully processed: 28
main-line changes — **9 port-tagged (→ 7 Go commits), 19 n/a, 0 decides**.
Three release tags crossed (v0.80.0/v0.80.1/v0.80.2); npm reference build
advanced 0.79.10 → **0.80.2** (each regen supersedes the prior). This cycle
**completes the models-runtime migration** (the `732bb161` follow-through);
much of it cancels intra-cycle (`detectCompat` removed in `129eb460` then
restored in `e1a2dc04` → net unchanged; anthropic compat toggled by `828493b3`
then `6184307c` → net auto-detect removed). Reviewed via independent go-review
(ship, 3 optional LOW nits) + adversarial parity review (all 7 commits
faithful; catalog re-derived byte-identical; 6/6 differential request diff;
all 3 deliberate divergences confirmed observably-faithful — see the 2026-06-24
ruling). Build/vet/`-race` green.

- **Catalog → npm 0.80.2** (`f08e968c`/`1c4a9ba7`/`0201806a`, Go `d2f937d`):
  endpoint-pinned, re-derived byte-identical (386,548 B). +24 (huggingface
  MiniMax/Qwen/GLM via the registration-only `huggingface` provider; opencode
  glm-5.2; openrouter z-ai/glm-5v-turbo), −4 openrouter (no Go refs), 357
  cost/metadata churn. `off:null` tripwires intact (110→111 in `ThinkingLevelMap`).
- **OpenAI Responses terminal events** (`cd95c274`, Go `e7c69ca`):
  `response.incomplete` finalizes like `response.completed`; stream fails with
  "OpenAI Responses stream ended before a terminal response event" if no
  terminal event. Response-parse only. Tests: `TestResponsesIncomplete…`,
  `TestResponsesNoTerminalEventFailsStream`.
- **api-key credentials → auth.json shape** (`49fbe683`, Go `fad8247`):
  `CredentialAPIKey` "api-key"→"api_key"; `Credential.Metadata`→`Env`
  (json `metadata`→`env`). On-disk breaking change (no shim, mirrors pi). Test:
  `TestCredentialAPIKeyJSON`.
- **compaction zero-usage guard** (`cd95c274`, Go `5c6c777`): usage-anchor loop
  already enforced `>0`; comment aligned + `TestUsageEstimateSkipsAllZeroUsage`.
  The agent-session.ts threshold/post-compaction halves are unported
  agent-session-runtime surface (N/A).
- **request-scoped auth overrides** (`ef231c49`, Go `b53482b`):
  `AuthResolutionOverrides{apiKey,env}` + `overlayEnvAuthContext` into
  `resolveProviderAuth`; `applyAuth` resolves through it; `GetAuth` stays
  override-free. The named `auth/resolve.ts` boundary edge. Test:
  `TestResolveProviderAuthRequestOverrides`.
- **anthropic compat → catalog** (`6184307c`, Go `64e5022`): removed
  fireworks/cloudflare auto-detect; defaults `?? true/true/false/true`, catalog
  supplies the rest. Byte-identical for catalog (0 mismatches across 14
  fireworks + 17 cf-anthropic models). `TestAnthropicSessionAffinityRetention`
  re-pinned to supply compat explicitly.
- **header-only client auth + vercel ungate** (`129eb460`, Go `54a254e`):
  `clientAPIKey` "unused" sentinel for authorization/cf-aig-authorization
  header-only auth; `vercelGatewayRouting` no longer baseUrl-gated (byte-safe —
  no catalog model sets routing). Tests: `TestClientAPIKeySentinel`,
  `TestDiffVercelGatewayRouting` (re-pinned).

**Deliberate divergences (2026-06-24 ruling):** `ProviderHeaders`
null-suppression not ported (latent + public-API change), cloudflare base-URL/
cf-aig auth kept inline (not relocated), compat `shouldUseBuiltinModels` routing
not ported (globals-stay-compat). All observably byte-identical through the Go
compat-globals consumer path.

n/a (19): docs/CHANGELOG (`15f92260`, `12ace0ba`, `2be6e670`, `526351d9`,
`86528dd9`, `e0007435`, `9096d5f9`, `8277bd68`); CI/packaging (`2285f879`
removed API subpath exports, `c3cfeac0`, `954ec998`, `97820276`, `ec6311be`);
`192fcccd` (extensions-load hint, main.ts); `b3776234` (type rename
`ExecutionEnvExecOptions`→`ShellExecOptions`, behavior-neutral); `828493b3`
(generator/data folds to 0.80.2; bedrock unported; anthropic intermediate);
`63386614` (TUI/benchmark timing); `a2e3e9d8` (**Azure** foundry — provider
excluded). `e1a2dc04` (restore detectCompat) is net-neutral with `129eb460`'s
removal → no Go change. No new boundary questions.

## Drift at last sync check (2026-06-23) — pin advanced to 470a4736

**Caught up to `470a4736`.** Delta `3b561346 → 470a4736` fully processed: 9
main-line changes — **3 ports (incl. the `732bb161` model-registry merge), 6
n/a, 0 decides** (the lone decide resolved to adopt, 2026-06-23 Rulings). No
release tag crossed (`pi-ai` stays 0.79.10; npm reference build unchanged, all
goldens unaffected, no catalog regen). Reviewed via independent pi-go-review
(ship) + pi-parity-review (faithful) on both the env slice and the substrate.

- **`8eeaa2bc` + `2cbce395` — scoped provider env through API-key resolution**
  (Go `1577144`). `GetEnvApiKey`/`FindEnvKeys` thread a scoped `env
  map[string]string` (canonical `ai.ProviderEnvValue`; providers' helper
  delegates), vertex-ADC + bedrock branches included; `withEnvAPIKey`/`Simple`
  pass `opts.Env`. `2cbce395` is a no-code-change passthrough (its
  `resolution.env` is latent upstream — no catalog provider's `resolve()`
  returns env — and Go's `opts.Env` already flows to providers; locked by
  `TestStreamEnvReachesProvider`). Byte-identical requests when `Env` unset.
- **`732bb161` "Merge model-registry into main" — Models runtime + auth
  substrate** (Go `bf7e7bd` + `37dcff5` + `2b164b3`), per noam's 2026-06-23
  **adopt** ruling (maximum parity for the SDK package/structure). Ported pi's
  `packages/ai/src/auth/*` as `auth_*.go` in package `ai` (CredentialStore +
  InMemoryCredentialStore, ProviderAuth/ApiKeyAuth/OAuthAuth, AuthContext,
  EnvAPIKeyAuth/LazyOAuth, resolveProviderAuth with OAuth refresh-under-lock),
  and `packages/ai/src/models.ts` as `models_runtime.go` (`Provider` interface,
  `CreateProvider`, `Models`/`CreateModels`, `GetAuth`/`applyAuth` incl. the
  `2cbce395` env merge, `HasApi`) + `builtins_models.go` (`BuiltinModels` wiring
  catalog + ProviderAuth + ApiProvider streams). Renamed the `Provider` string
  alias → `ProviderId` (pi's Provider→ProviderId), freeing `Provider` for the
  runtime interface. The pre-existing global free functions remain the **compat
  surface** (pi's `/compat`) — consumers unchanged. Deliberate divergences
  (documented): auth as files in package `ai` not a subpackage (import cycle);
  async→synchronous `(T,error)`; errors via `errorStream` not `lazyStream` (G3);
  OAuth *login acquisition* out of scope (interfaces ported, flows not); images
  excluded. Catalog-data reorg (per-provider `*.models.ts`, new providers)
  **deferred to the next release regen** (0.79.10 not re-published). Request
  bytes unaffected (no `openai*.go` request-builder changed → 6-scenario diff not
  required). **No new boundary questions.**

n/a (6): `d2677a63`/`02540acd` docs; `5a8ea0bc` Bedrock scoped AWS profile
(Bedrock provider unported); `6a4813a7` merge (only ai/src file is
`openai-codex-responses.ts` = Codex, unported; rest theme/startup-ui/
session-picker/settings-manager/main.ts = TUI/CLI/host); `7fedc332` session-name
`\r\n` sanitization (write path `appendSessionName`/`appendSessionInfo` is
host/TUI-driven — Go reads `SessionInfo` but has no name-write/rename path;
low-confidence n/a, re-confirm at substrate port time); `470a4736` threaded
session-selector sort (TUI).

## Drift at last sync check (2026-06-22, v0.79.10 cycle)

**Caught up to `3b561346`.** Ledger 2417adb4 → 3b561346 fully processed (14
main-line changes: **2 behavior ports + 1 catalog regen, 11 n/a, 0 decides**).
One release tag crossed (v0.79.10); npm reference build advanced 0.79.9 →
**0.79.10**. Reviewed via an independent idiomatic go-review (ship, three LOW
nits, no action) + adversarial parity review — which caught a real divergence:
the reasoning-details port adopted the buffering but not the same commit's
validation tightening; fixed in `62981f1` and re-verified faithful. Catalog
endpoint-pinned byte-identical both ends; build/vet/`-race` green; differential
request diff 14/14.

- **Catalog → npm 0.79.10** (`8e190066`, Go `c50acfc`): endpoint-pinned
  byte-identical (old ≡ 0.79.9 build, new ≡ 0.79.10, integrity-verified
  `sha512-9jR23…ORuew==`). **+1** (`vercel-ai-gateway/sakana/fugu-ultra`),
  **−1** (`openrouter/anthropic/claude-3.5-haiku`), 17 openrouter entries churn
  cost/maxTokens/contextWindow. `off:null` tripwires intact (fable-5 across
  anthropic/bedrock/cloudflare; moonshotai[-cn]/kimi-k2.7-code[-highspeed]).
  The dropped openrouter id was a resolve-fallback fixture; it now lives only
  under vercel-ai-gateway, so `TestResolveModelProviderPrefixFallsBackToFullID`
  updated to that provider (resolution logic unchanged — pi's registry `.find()`
  lands on the same sole remaining copy).
- **preserve early reasoning details** (`7d0497fd`, Go `4e60155`+`62981f1`):
  openai-completions buffers an encrypted `reasoning_detail` arriving before its
  tool-call block (`pendingReasoningDetails` keyed by id, drained in
  `ensureToolCallBlock` via `applyPendingReasoningDetail`), matching the tool
  call by the byID map instead of an order scan — no longer dropped (#5114).
  `62981f1` ports the same commit's `isEncryptedReasoningDetail` tightening
  (data must be a non-empty string). Response-parse only; request bytes
  unchanged. Golden surface: request body (reasoning_details round-trip,
  unexercised since no request change). Tests: early-arrival + non-string-data.
- **respect nested repo ignore boundaries in find** (`756a4e8f`, Go `46302ad`):
  the pure-Go fd reimplementation now stops outer repo-specific ignore sources
  (.git/info/exclude, ancestor + per-dir .gitignore) at a nested `.git`
  boundary, while the nested repo's own rules still apply and global
  core.excludesFile carries across (boundaryExempt); active only when the
  search root is inside a repo (preserving --no-require-git outside) (#5960).
  grep/rg path unchanged (respectNestedRepos=false). Golden surface: find-tool
  output. Test: TestFindRespectsNestedRepoBoundaries. **Known minor under-reach**
  (pre-existing, flagged by parity review, out of this commit's scope): a
  *nested* repo's own `.git/info/exclude` is not re-rooted (only the outer
  repoRoot's is read) — the nested repo's `.gitignore` IS honored; worth a
  follow-up.
- **n/a (11):** docs (`a61137a6`, `b7908b49`, `5df5a1ce`); changelog cycle
  headers (`329dceb5`); `.github` (`08457404` contributor approval, `5641d6ba`
  issue-triage workflow); `5b9b70d2` adds `reason`/`willRetry` to
  SessionBeforeCompact/SessionCompact **extension events** (agent-session-runtime
  + extensions/types.ts — unported event lifecycle, per the compaction-trio
  rulings); `717a8f95` reverts the selective pi-ai base entrypoints (packaging/
  test, reverting the n/a `0d89a333`); `4f71b2d3` ZAI "Coding Plan (Global)"
  label in provider-display-names (no Go equivalent — display/TUI) + cli/args
  help text; `71ca9b2b` OpenCode-Go GLM-5.2 xhigh effort (data-only, lands
  **post-0.79.10** so deferred to the next regen); `3b561346` tui ctrl+j newline
  default (TUI). No new boundary questions.

## Drift at last sync check (2026-06-22)

**Caught up to `2417adb4`.** Ledger 56b22768 → 2417adb4 fully processed (22
main-line changes: **4 behavior/perf ports + 1 catalog regen, 17 n/a, 0
decides**). One release tag crossed (v0.79.9); npm reference build advanced
0.79.8 → **0.79.9**. Reviewed via an independent idiomatic go-review (ship, one
cosmetic nit applied) + adversarial parity review (5/5 faithful; catalog
endpoint-pinned byte-identical on both ends; tripwire + orphaned-id checks
passed). Build/vet/`-race` green.

- **Catalog → npm 0.79.9** (`615bf2f8`, Go `5d8b72d`): endpoint-pinned
  byte-identical (old ≡ 0.79.8 build, new ≡ 0.79.9, integrity-verified). 0
  added, **2 removed** (`google/gemma-4-E2B-it`, `gemma-4-E4B-it`; no Go refs),
  20 changed. Subsumes the two data-only commits `8597ebaf` (openrouter
  z-ai/glm-5.2 `xhigh:xhigh`) and `500b568b` (fireworks glm-5p2 →
  api openai-completions, `/inference/v1`, compat, thinkingLevelMap); rest is
  cost/metadata churn. `off:null` tripwires intact (fable-5; moonshotai/-cn
  kimi-k2.7-code[-highspeed]).
- **chat-template thinking compat** (`8b97e75c`, Go `3c30dd2`+`56c73b7`): new
  openai-completions `thinkingFormat:"chat-template"` emitting configurable
  `chat_template_kwargs` ($var/omitWhenOff/scalar). **Latent** — no 0.79.9
  catalog model sets it (reachable only via custom model config); key order
  preserved for byte-exact request bodies. Golden surface: request body
  (unexercised by the 6-scenario diff until a model adopts it). Host-side
  model-registry schema + mergeCompat stay unported.
- **fuzzy edit preserves untouched lines** (`128330e3`, Go `18ef9eb`): fuzzy
  edits no longer globally normalize the file — only touched line-blocks are
  rewritten, other lines copied back verbatim. Golden surface: edit-tool file
  output.
- **legacy WSL bash via stdin** (`1287b69f`, Go `9f452a1`): System32/Sysnative
  `bash.exe` → `bash -s` + command on stdin (mishandles `-c` quoting). Windows
  legacy-WSL only; resolve-config-value half is host-side (n/a).
- **session branch traversal linear** (`a1da88ae`, Go `a88ef3b`): O(n²) prepend
  → append+reverse. Behavior-neutral.
- **n/a (17):** issue-triage automation/.github (`783571a6`, `47d1d90a`,
  `226a3168`, `416c673d`, `350ac3f3`); TUI (`3095977d`, `373cd6ae`,
  `d93b92ba`); changelog (`1aa79b9b`, `b4f31408`); examples (`542683b2`);
  catalog data folded into the 0.79.9 regen (`8597ebaf`, `500b568b`); OAuth +
  host model-registry (`6e6ce70c` Copilot account-availability filtering);
  extensions runtime (`5505316e`); packaging/self-update (`bc0db643`);
  agent-session-runtime reload + TUI (`2417adb4`). No new boundary questions.

## Drift at last sync check (2026-06-19)

**Caught up to `56b22768`.** Ledger 29c1504c → 56b22768 fully processed (32
main-line changes: **0 behavior ports, 32 n/a** for code — the only ported
surface touched is the catalog, advanced via the release regen below; 0
decides). Two release tags crossed (v0.79.7, v0.79.8); npm reference build
advanced 0.79.6 → **0.79.8** (v0.79.7 superseded — each regen supersedes the
prior). Reviewed via an independent adversarial parity review (catalog
endpoint-pinned byte-identical on both ends; authenticity, schema-drift,
tripwire, and orphaned-id checks all passed). Build/vet/`-race` green.

- **Catalog → npm 0.79.8** (`8eb9704b`, Go commit `5164314`): subsumes v0.79.7
  + the data-only generator commits `58dd2f59` (opencode-go GLM-5.2),
  `b09fbde0` (openrouter/fusion alias), and the Mistral prompt-caching data
  from `651d10d9`. Net +9/−3 ids; 44 changed entries are data churn (Mistral
  cost fields, fireworks/openrouter/vercel metadata). `off:null` gates intact
  (claude-fable-5, kimi-k2.7-code) → `TestFable5DisabledThinkingGateLive` and
  `TestDeepseekDisabledThinkingGateLive` green.
- **No behavior ports.** The substantive non-release changes all landed on
  unported surface: the compaction trio (`6b9f3f49` overflow-retry recovery,
  `7d08c81a` empty-summary guard / event reordering, `c60f6a8a`
  `estimatedTokensAfter`) edits the agent-session-runtime auto-compaction
  orchestration + `CompactionResult`/`compaction_*` event lifecycle, none of
  which the Go port has (it compacts inline via `shouldCompact`/`compact` with
  no overflow recovery or event emission); RPC unknown-command id
  (`51f75235`) → `modes/rpc` unported; Mistral prompt caching (`651d10d9`,
  provider code) → Mistral provider unported; `CONFIG_DIR_NAME` / edit-diff
  SDK exports (`008c76f9`, `2b46f388`) → no behavior change; selective pi-ai
  entrypoints (`0d89a333`) → packaging. No new boundary questions.

## Drift at last sync check (2026-06-17)

**Caught up to `29c1504c`.** Ledger f8a77f47 → 29c1504c fully processed (20
main-line changes: 3 ported, 16 n/a, 1 decide resolved). Two release tags
crossed (v0.79.5, v0.79.6); npm reference build advanced 0.79.4 → 0.79.6.
Reviewed via independent go-review (ship) + adversarial parity review
(4/4 faithful; request diff regenerated from the 0.79.6 build, 12/12 — 6
standard + the 4 new GLM-5.2 scenarios; null-content regression test
mutation-verified non-vacuous).

- Ports: `75b0d723` (Z.AI GLM-5.2 native reasoning_effort — `788c832`),
  `2d597f02` (null Responses content — `e8f7511`, no code change: Go ranges a
  nil slice safely; locked with a regression test), and `31bfb2f1` (catalog →
  npm 0.79.6 — `c2221a7`, subsumes the v0.79.5 catalog + the deepseek-v4 compat
  / cost / maxTokens data churn `2431491c`/`bd9f8773`/`7da475db`).
- **Deferred data landed:** the 0.79.6 catalog ships `off:null` for Kimi K2.7
  Code (`moonshotai`/`moonshotai-cn`, incl. `-highspeed`), activating the
  deepseek disabled-thinking gate ported on 2026-06-15 (`62fa1e3`). The
  `TestDeepseekCatalogNoOffNull` tripwire is now `TestDeepseekDisabledThinkingGateLive`,
  driving the omit end-to-end through the catalog-resolved model.
- **Decide → ruling:** provider-scoped env overrides **ported faithfully**
  (`7f29e7a3` → `872c303`; owner call 2026-06-17, recorded in Rulings).
  `StreamOptions.Env` consulted ahead of `os.Getenv`; Bun `/proc` fallback
  omitted (no Go analog); host-side population machinery stays unported.

### Prior — 93b3b7c1 → f8a77f47 (2026-06-16)

**Caught up to `f8a77f47`.** Ledger 93b3b7c1 → f8a77f47 fully processed (20
main-line changes: 6 ported, 14 n/a, 1 decide resolved). Reviewed via an
adversarial multi-agent workflow: 5/6 parity-faithful + go-review pass +
request diff 6/6 against the 0.79.4 build; the review caught a real attribution
header-precedence divergence (model.Headers must override the attribution
defaults — pi merges them at the bottom of the stack) which was fixed and
re-verified. Shipped as v0.2.2.

- Ports: `0be5bb6c` (anthropic 1h cache-write cost = 2× input), `3fa40956`
  (bash stdout drain past exit — re-armed idle grace), `0369bdb8` (deepseek
  off:null thinking gate — logic; catalog data deferred), `b0c8f65f`
  (gemini-flash-latest alias → MINIMAL thinking — logic; data deferred),
  `bba6af2c` (catalog → npm 0.79.4), and `f8a77f47` + the `provider-attribution`
  module (see ruling).
- **Decide → ruling:** provider-attribution **ported faithfully** (owner call,
  2026-06-16). The SDK now sends pi's attribution headers on the providers pi
  does, gated on `PI_TELEMETRY` (default enabled), at the bottom of the header
  precedence so `model.Headers`/`opts.Headers` override them. Closed a
  pre-existing header parity gap the body-diff never covered.

### Prior — 6f29450e → 93b3b7c1 (2026-06-14)

**Caught up to `93b3b7c1` — no-op cycle (pin advance only, no version bump).**
Ledger 6f29450e → 93b3b7c1: 12 main-line changes, **0 ported, 12 n/a, 0
escalations**. Nothing touched ported behavior code (TUI, packaging/self-update,
extension package-manager, config-migrations/settings, docs/meta/examples). No
release tag in the window, so the npm reference build stays at `pi-ai` 0.79.3
and all goldens are unaffected. **Deferred catalog note:** `21a904f4` flips
`supportsLongCacheRetention:false` for 6 opencode models in generate-models —
the *behavior* is already ported (`openai_compat.go` + `openai.go`), so it's
pure catalog data that will land with the next release regen (no npm build ships
it yet). No code change → no tag; v0.2.1 remains the latest release.

### Prior — 3f44d3e2 → 6f29450e (2026-06-13)

**Caught up to `6f29450e`.** Ledger 3f44d3e2 → 6f29450e fully processed (22
main-line changes: 4 ported, 18 n/a, 0 escalations). Reviewed via an adversarial
multi-agent workflow: 4/4 parity-faithful, idiomatic go-review pass, request
diff 6/6 against the 0.79.3 build, completeness critic mutation-tested every
ported change (each fix is load-bearing). Shipped as v0.2.1.

- Ports: `a455f62f` (anthropic refusal details), `1fc80f4f` (resolve fallback
  Reasoning flip), `daab056a` (agent late-tool-update guard), `f2585c4c`
  (catalog → npm 0.79.3).
- **Fable-5 disabled-thinking gate went LIVE.** The 0.79.3 catalog ships
  `off:null` for `claude-fable-5` (anthropic + cloudflare-ai-gateway + bedrock
  variants), activating the latent 9ccfcd7c gate ported on 2026-06-12. The
  former latency tripwire is now `TestFable5DisabledThinkingGateLive`, which
  drives the omit behavior end-to-end through the catalog-resolved model and
  fails loudly if a future regen drops `off:null`.

### Prior — 130ae577 → 3f44d3e2 (2026-06-12)

**Caught up.** Ledger 130ae577 → 3f44d3e2 fully processed (52 main-line
changes: 9 ported, 43 n/a, 0 open). Releases v0.79.0 + v0.79.1 ingested; the
9ccfcd7c disabled-thinking gate is latent pending upstream's next catalog
regen (tripwire: TestFable5DisabledThinkingGateLive).

## Sync pipeline

Runs as a daily job — the `/pi-sync` skill (`.claude/skills/pi-sync/`)
orchestrates one cycle over everything upstream added since the pin. Each
ledger row is one main-line change (a PR merge carries its full PR diff,
`git diff <sha>^1..<sha>`). Stages, each owned by a dedicated skill:

1. **Triage** (`/pi-triage`) — WHY/WHAT from the real diff, then a SCOPE
   verdict: `port` / `n/a` (with reason) / `decide` (boundary changes are
   escalated to a human, never decided silently).
2. **Port** — each in-scope change lands as an individual Go commit
   referencing the upstream sha (`port(<area>): <subject> (upstream <sha>)`).
3. **Idiomatic review** (`/pi-go-review`) — independent agent verifies the
   port is real Go, not transliterated TypeScript.
4. **Parity review** (`/pi-parity-review`) — independent adversarial agent
   verifies faithfulness against the TS source AND the published npm build
   (build wins on drift); regenerates goldens from the build, never by hand;
   re-runs the differential request diff when provider request code changed.
5. **Gate + record** — full build/vet/`-race` suite green; ledger row filled
   (status, Go commit, notes); pin advanced; pushed.

The reviewers are independent of the porter by design: every parity bug this
project has shipped was pinned in place by the author's own tests and caught
only by comparison against real pi.

Upstream reference clone: `$PI_UPSTREAM_DIR`, default `~/.cache/pi-upstream`.
When the delta crosses a release tag, the npm reference build is refreshed to
that version before parity review.

## Ledger — 470a4736 → a2e3e9d8

| Upstream | Date | Subject | Hint | Status | Go commit | Notes |
|---|---|---|---|---|---|---|
| `129eb460` | 2026-06-23 | feat(ai): complete models runtime migration | review | **ported** | `54a254e` (+catalog `d2f937d`) | The migration consolidation. Most lands via the 0.80.2 catalog regen (per-provider `*.models.ts` reorg, huggingface registration provider). Observable Go slices: `getClientApiKey` "unused" sentinel (`clientAPIKey`, both openai providers) + vercel routing ungate. `detectCompat` removal here is reverted by `e1a2dc04` (net unchanged). ProviderHeaders null-suppression / cloudflare-auth relocation / compat builtin-routing NOT ported — deliberate divergences (2026-06-24 ruling), observably byte-identical through the compat-globals path. |
| `15f92260` | 2026-06-23 | docs(ai): expand models migration guide | likely-n/a | n/a | — | ai/CHANGELOG.md |
| `12ace0ba` | 2026-06-23 | docs(ai): reference README in migration guide | likely-n/a | n/a | — | ai/CHANGELOG.md |
| `2285f879` | 2026-06-23 | fix(ai): remove legacy raw API subpaths | review | n/a | — | package.json export subpaths only (packaging) |
| `cd95c274` | 2026-06-23 | fix(ai): require OpenAI Responses terminal events | review | **ported** | `e7c69ca` (+`5c6c777`) | openai-responses-shared: response.incomplete finalizes like completed; throw on no terminal event (Go `e7c69ca`, response-parse only). Compaction zero-usage guard (Go `5c6c777`); agent-session-runtime halves N/A. |
| `2be6e670` | 2026-06-23 | docs(ai): document bundling behavior | likely-n/a | n/a | — | ai/README.md |
| `192fcccd` | 2026-06-23 | fix(coding-agent): hint when extensions fail to load | review | n/a | — | main.ts extension-load-failure hint — extensions runtime unported |
| `526351d9` | 2026-06-23 | docs: audit unreleased changelogs | likely-n/a | n/a | — | changelogs |
| `f08e968c` | 2026-06-23 | Release v0.80.0 | review | ported (superseded) | `d2f937d` | catalog regen; superseded by 0.80.2 (final build subsumes it) |
| `86528dd9` | 2026-06-23 | Add [Unreleased] section for next cycle | likely-n/a | n/a | — | changelog cycle header |
| `828493b3` | 2026-06-23 | fix(ai): unblock release provider tests | review | n/a | — | generate-models `isTogetherReasoningOnly` (data → 0.80.2 regen); bedrock scoped-profile revert (unported); anthropic compat intermediate (net via `6184307c`) |
| `1c4a9ba7` | 2026-06-23 | Release v0.80.1 | review | ported (superseded) | `d2f937d` | catalog regen; superseded by 0.80.2 |
| `e0007435` | 2026-06-23 | Add [Unreleased] section for next cycle | likely-n/a | n/a | — | changelog cycle header |
| `6184307c` | 2026-06-23 | fix(ai): require explicit anthropic compat metadata | review | **ported** | `64e5022` | getAnthropicCompat drops fireworks/cf auto-detect → OpenAI-standard defaults; catalog supplies the rest. Byte-identical for catalog (0 mismatches). Test re-pinned to explicit compat. |
| `c3cfeac0` | 2026-06-23 | fix(coding-agent): make release publication transactional | review | n/a | — | .github/workflows + scripts/publish.mjs (CI) |
| `b3776234` | 2026-06-23 | Type name change | review | n/a | — | packages/agent harness `ExecutionEnvExecOptions`→`ShellExecOptions` rename — behavior-neutral |
| `49fbe683` | 2026-06-23 | fix(ai): align api key credentials with auth json | review | **ported** | `fad8247` | Credential type "api-key"→"api_key"; `Metadata`→`Env` (json metadata→env). On-disk breaking change (no shim, mirrors pi). Test: TestCredentialAPIKeyJSON. |
| `04fce809` | 2026-06-23 | Merge remote-tracking branch 'origin/main' | review | n/a | — | new `legacy-api-aliases.ts` = deprecated TS re-export shims for the removed subpaths (Go compat globals already cover); compat.ts one-liner |
| `ef231c49` | 2026-06-23 | fix(ai): resolve request-scoped auth before provider calls | review | **ported** | `b53482b` | `AuthResolutionOverrides{apiKey,env}` + `overlayEnvAuthContext` into resolveProviderAuth; applyAuth resolves through it; GetAuth override-free. The named auth/resolve.ts boundary edge. Test: TestResolveProviderAuthRequestOverrides. |
| `e1a2dc04` | 2026-06-23 | fix(ai): restore detectCompat runtime fallback in openai-completions | review | n/a (net-neutral) | — | restores `detectCompat` removed by `129eb460` → net unchanged; Go's detectCompat stays as-is |
| `9096d5f9` | 2026-06-23 | docs: update changelog entries | likely-n/a | n/a | — | changelogs |
| `0201806a` | 2026-06-23 | Release v0.80.2 | review | **ported** | `d2f937d` | final catalog regen (reference build); endpoint-pinned, integrity-verified `sha512-5GNKfdrR…uy9RQ==` |
| `8277bd68` | 2026-06-23 | Add [Unreleased] section for next cycle | likely-n/a | n/a | — | changelog cycle header |
| `954ec998` | 2026-06-23 | fix: upload release assets from visible directory | likely-n/a | n/a | — | .github workflow (CI) |
| `97820276` | 2026-06-23 | fix: remove OpenClaw gate | likely-n/a | n/a | — | .github workflow (CI) |
| `ec6311be` | 2026-06-23 | fix: skip dirty check before npm publish | likely-n/a | n/a | — | .github workflow (CI) |
| `63386614` | 2026-06-24 | fix(coding-agent): print benchmark timings after TUI stop (#6030) | review | n/a | — | main.ts startup-benchmark timing order (TUI) |
| `a2e3e9d8` | 2026-06-24 | Merge #6004 support-azure-foundry-endpoints | review | n/a | — | azure-openai-responses.ts — Azure provider excluded |

## Ledger — 3b561346 → 470a4736

| Upstream | Date | Subject | Hint | Status | Go commit | Notes |
|---|---|---|---|---|---|---|
| `732bb161` | 2026-06-22 | Merge model-registry into main | decide→adopt | **ported** | `bf7e7bd`+`37dcff5`+`2b164b3` | pi's `packages/ai` object-model overhaul ported per the 2026-06-23 adopt ruling. auth_*.go = `auth/*` substrate (CredentialStore/InMemoryCredentialStore, ProviderAuth/ApiKeyAuth/OAuthAuth, AuthContext, EnvAPIKeyAuth/LazyOAuth, resolveProviderAuth w/ OAuth refresh-under-lock). models_runtime.go = `models.ts` (`Provider` iface, CreateProvider, Models/CreateModels, GetAuth/applyAuth incl. 2cbce395 env merge, HasApi). builtins_models.go = BuiltinModels (catalog+ProviderAuth+ApiProvider streams). Provider→ProviderId rename (alias). Globals stay as compat (pi `/compat`). Divergences (documented): auth as files-in-package not subpackage (cycle); sync `(T,error)`; errorStream not lazyStream (G3); OAuth login out of scope; images excluded. Catalog-data reorg deferred to next regen. Request bytes unaffected. pi-go-review ship + pi-parity-review faithful. Tests: auth_test, models_runtime_test, builtins_models_test |
| `d2677a63` | 2026-06-22 | docs(agent): mark sync models API complete | likely-n/a | n/a | — | packages/agent/docs/models.md |
| `02540acd` | 2026-06-22 | docs(ai): update provider README | likely-n/a | n/a | — | packages/ai/README.md |
| `5a8ea0bc` | 2026-06-23 | fix(ai): honor scoped AWS profile in Bedrock endpoint resolution | review | n/a | — | bedrock-converse-stream.ts only — Bedrock provider unported |
| `2cbce395` | 2026-06-23 | feat(ai): pass provider-resolved env to APIs | review | ported | `1577144` | No Go code change: `resolution.env` latent upstream (no catalog provider's `resolve()` returns env), Go's `opts.Env` already flows to providers (withEnvAPIKey clones preserve Env). images-models half = images (n/a). Locked by `TestStreamEnvReachesProvider`. |
| `8eeaa2bc` | 2026-06-23 | fix(ai): honor scoped env in compat API key injection | review | ported | `1577144` | `GetEnvApiKey`/`FindEnvKeys` thread scoped `env`; new canonical `ai.ProviderEnvValue` (providers' `getProviderEnvValue` delegates); vertex-ADC + bedrock branches consult scoped env; `withEnvAPIKey`/`Simple` pass `opts.Env`, host/example call sites pass nil. Golden: API-key selection — byte-identical when Env unset. Tests: `TestGetEnvApiKeyScopedEnv`, `TestWithEnvAPIKeyUsesScopedEnv`. |
| `6a4813a7` | 2026-06-23 | Merge remote-tracking branch 'origin/main' | review | n/a | — | only ai/src file is `openai-codex-responses.ts` (Codex, unported); rest theme/startup-ui/session-picker/settings-manager/main.ts (TUI/CLI/host) |
| `7fedc332` | 2026-06-23 | fix(coding-agent): normalize session names (#5999) | review | n/a | — | `\r\n`→space sanitize in `appendSessionName`/`appendSessionInfo` write path — host/TUI-driven; Go reads `SessionInfo` but has no name-write/rename path. Low-confidence n/a — re-confirm at substrate port time |
| `470a4736` | 2026-06-23 | fix(coding-agent): sort threaded sessions by latest activity in subtree (#5784) | review | n/a | — | interactive/components/session-selector.ts (TUI) |

## Ledger — 2417adb4 → 3b561346

| Upstream | Date | Subject | Hint | Status | Go commit | Notes |
|---|---|---|---|---|---|---|
| `a61137a6` | 2026-06-22 | docs(coding-agent): fix plan-mode docs links | likely-n/a | n/a | — | docs/tui.md + changelog |
| `08457404` | 2026-06-22 | chore: approve contributor any-victor | n/a | n/a | — | .github contributor meta |
| `7d0497fd` | 2026-06-22 | fix(ai): preserve early reasoning details | review | ported | `4e60155`+`62981f1` | openai.go: encrypted reasoning_details arriving before their tool-call block are buffered (pendingReasoningDetails by id) and drained in ensureToolCallBlock (applyPendingReasoningDetail); match via toolBuildersByID not order-scan; no longer dropped (#5114). `62981f1` ports the same commit's isEncryptedReasoningDetail tightening (data must be a non-empty string), replacing the old jsonValueTruthy gate. Response-parse only (request bytes unchanged). Tests: TestOpenAIReasoningDetailsEarlyArrival, TestOpenAIReasoningDetailsNonStringDataIgnored |
| `5b9b70d2` | 2026-06-22 | feat(coding-agent): add compaction reason and willRetry to extension compact events (#5962) | review | n/a | — | agent-session.ts + core/extensions/types.ts: `reason`/`willRetry` on SessionBeforeCompact/SessionCompact extension events — unported event lifecycle (compaction-trio rulings) |
| `b7908b49` | 2026-06-22 | docs(coding-agent): document slash command table | likely-n/a | n/a | — | README + docs/usage |
| `5641d6ba` | 2026-06-22 | fix: clear untriaged when no-action is added | likely-n/a | n/a | — | .github issue-triage workflow |
| `756a4e8f` | 2026-06-22 | fix(coding-agent): respect nested repo ignore boundaries in find | review | ported | `46302ad` | glob.go ignoreStack: new `boundaries` axis (respectNestedRepos) + crossesNestedBoundary/hasGitDir — outer repo-specific ignore sources stop at a nested `.git`; nested repo's own rules still apply; global excludesFile carries across (boundaryExempt); active only inside a repo. grep/rg unchanged (false). Pure-Go fd reimplementation, validated vs git oracle (#5960). Known minor under-reach: nested repo's own info/exclude not re-rooted (follow-up). Test: TestFindRespectsNestedRepoBoundaries |
| `5df5a1ce` | 2026-06-22 | docs(coding-agent): audit unreleased changelog | likely-n/a | n/a | — | changelog |
| `8e190066` | 2026-06-22 | Release v0.79.10 | review | ported | `c50acfc` | ai/models_catalog.json regenerated from npm 0.79.10 (endpoint-pinned both sides, integrity-verified). +1 (vercel-ai-gateway/sakana/fugu-ultra), −1 (openrouter/anthropic/claude-3.5-haiku), 17 openrouter cost/window churn. off:null tripwires intact. Dropped id was a resolve-fallback fixture → TestResolveModelProviderPrefixFallsBackToFullID updated to vercel-ai-gateway (sole remaining copy; logic unchanged). Independent parity review: faithful |
| `329dceb5` | 2026-06-22 | Add [Unreleased] section for next cycle | likely-n/a | n/a | — | changelog cycle header |
| `717a8f95` | 2026-06-22 | fix(ai): revert selective pi-ai base entrypoints | review | n/a | — | reverts the n/a `0d89a333` — test import paths + tsconfig/vitest/scripts (packaging only) |
| `4f71b2d3` | 2026-06-22 | fix(coding-agent): clarify ZAI Coding Plan label | review | n/a | — | provider-display-names.ts "ZAI" → "ZAI Coding Plan (Global)" (no Go equivalent — display/TUI) + cli/args help text; Go envkeys.go maps only provider→ENV_KEY (unchanged) |
| `71ca9b2b` | 2026-06-22 | fix(ai): expose OpenCode Go GLM-5.2 xhigh effort | review | n/a (data) | — | generate-models.ts + models.generated.ts (opencode-go/zai-org glm-5.2 thinkingLevelMap xhigh:max); lands **post-0.79.10** → deferred to the next catalog regen |
| `3b561346` | 2026-06-22 | fix(tui): bind ctrl+j as newline by default | likely-n/a | n/a | — | tui/keybindings.ts (TUI) |

## Ledger — 56b22768 → 2417adb4

| Upstream | Date | Subject | Hint | Status | Go commit | Notes |
|---|---|---|---|---|---|---|
| `783571a6` | 2026-06-19 | feat: track auto-closed issue triage | likely-n/a | n/a | — | .github issue-triage workflows |
| `47d1d90a` | 2026-06-19 | fix: close no-action issues as not planned | likely-n/a | n/a | — | .github issue-triage workflow |
| `373cd6ae` | 2026-06-19 | fix(coding-agent): prioritize provider matches in model selector | review | n/a | — | modes/interactive model-selector/model-search (TUI) — unported |
| `226a3168` | 2026-06-19 | fix: mark auto-closed issues not planned | likely-n/a | n/a | — | .github issue-gate workflow |
| `6e6ce70c` | 2026-06-19 | fix(ai): filter Copilot models by account availability | review | n/a | — | `ai/utils/oauth/github-copilot.ts` (OAuth, unported) + host model-registry; only model-registry *test* changed, not the registry |
| `1287b69f` | 2026-06-19 | fix(coding-agent): run legacy WSL bash commands via stdin | review | ported | `9f452a1` | tools.go getShellConfig: detect System32/Sysnative bash.exe (isLegacyWslBashPath) → `bash -s` + command on stdin; else `bash -c`. resolve-config-value half host-side (n/a). Test: TestLegacyWslBashDetection |
| `128330e3` | 2026-06-19 | fix(coding-agent): preserve untouched lines in fuzzy edit | review | ported | `18ef9eb` | editmatch.go: fuzzy edits overlay only touched line-blocks onto the LF-normalized original (splitLinesWithEndings/getLineSpans/getReplacementLineRange/applyReplacementsPreservingUnchangedLines). Golden: edit-tool output. Tests: single + multi preserve |
| `8b97e75c` | 2026-06-19 | feat(ai): add chat-template thinking compat | review | ported | `3c30dd2` | openai-completions `thinkingFormat:"chat-template"` → configurable `chat_template_kwargs` (openai_chat_template.go). Latent (no catalog model). Golden: request body. Host-side model-registry schema/mergeCompat unported. Cosmetic follow-up `56c73b7`. Tests: openai_chat_template_test.go |
| `3095977d` | 2026-06-20 | fix(tui): stabilize streaming code fence rendering (#5846) | likely-n/a | n/a | — | tui/markdown |
| `416c673d` | 2026-06-20 | fix: skip no-action for to-discuss issues | likely-n/a | n/a | — | .github issue-triage workflow |
| `8597ebaf` | 2026-06-20 | fix(ai): expose OpenRouter GLM-5.2 xhigh effort | review | n/a (data) | — | generate-models.ts + models.generated.ts; lands via 0.79.9 catalog regen (`5d8b72d`, openrouter/z-ai/glm-5.2 thinkingLevelMap xhigh:xhigh) |
| `a1da88ae` | 2026-06-20 | fix(coding-agent): make session path traversal linear | review | ported | `a88ef3b` | session_tree.go Branch: O(n²) prepend → append+reverse. Behavior-neutral (covered by session-tree parity tests) |
| `5505316e` | 2026-06-20 | fix(coding-agent): cache extension imports for session switches | review | n/a | — | core/extensions/loader.ts + resource-loader.ts — extensions runtime unported |
| `500b568b` | 2026-06-20 | fix(ai): use OpenAI endpoint for Fireworks GLM-5.2 | review | n/a (data) | — | generate-models.ts + models.generated.ts; lands via 0.79.9 regen (`5d8b72d`, fireworks glm-5p2 → api openai-completions, /inference/v1 baseUrl, compat, thinkingLevelMap) |
| `350ac3f3` | 2026-06-20 | fix: remove inprogress from auto-closed issues | likely-n/a | n/a | — | .github issue-triage workflow |
| `1aa79b9b` | 2026-06-20 | docs: update unreleased changelog audit | likely-n/a | n/a | — | changelog |
| `615bf2f8` | 2026-06-20 | Release v0.79.9 | review | ported | `5d8b72d` | ai/models_catalog.json regenerated from npm 0.79.9 (endpoint-pinned both sides, integrity-verified). 0 added, 2 removed (google gemma-4-E2B-it/E4B-it; no Go refs), 20 changed. Subsumes 8597ebaf/500b568b data + cost/metadata churn. off:null tripwires intact. Independent parity review: faithful |
| `b4f31408` | 2026-06-20 | Add [Unreleased] section for next cycle | likely-n/a | n/a | — | changelog cycle header |
| `d93b92ba` | 2026-06-20 | fix(coding-agent): show changelog URL in update notice | review | n/a | — | modes/interactive interactive-mode (TUI update notice) |
| `bc0db643` | 2026-06-21 | fix(coding-agent): install checked pi update version | review | n/a | — | config.ts (bin-dir) + package-manager-cli.ts — self-update/packaging unported |
| `542683b2` | 2026-06-21 | fix(coding-agent): fix plan-mode example | likely-n/a | n/a | — | examples/extensions/plan-mode |
| `2417adb4` | 2026-06-21 | fix(coding-agent): preserve startup extension UI | review | n/a | — | agent-session.ts `reload()` gains a beforeSessionStart hook (agent-session-runtime reload, unported) + interactive-mode (TUI) |

## Ledger — 29c1504c → 56b22768

| Upstream | Date | Subject | Hint | Status | Go commit | Notes |
|---|---|---|---|---|---|---|
| `068ab5d1` | 2026-06-17 | fix(coding-agent): horizontally pan tree selector | likely-n/a | n/a | — | TUI tree-selector + tui/index.ts |
| `ae89286d` | 2026-06-17 | docs: update changelogs for tree panning | likely-n/a | n/a | — | docs/changelog |
| `6d5ede31` | 2026-06-17 | fix(coding-agent): match provider-first model searches | review | n/a | — | modes/interactive model-selector/search (TUI) — unported |
| `58dd2f59` | 2026-06-18 | feat(ai): add GLM-5.2 to OpenCode Go model catalog | review | n/a (data) | — | models.generated data; lands via 0.79.8 catalog regen (`5164314`) |
| `008c76f9` | 2026-06-18 | feat(coding-agent): export project config dir name | review | n/a | — | `CONFIG_DIR_NAME` SDK constant export + trust-prompt/help-text string interpolation; no ported behavior (trust + interactive + SDK const only) |
| `51f75235` | 2026-06-18 | fix(coding-agent): include RPC request id on unknown commands | review | n/a | — | `modes/rpc` unported in Go (no rpc-mode) |
| `7a14325b` | 2026-06-18 | feat(tui): detect Warp terminal and enable Kitty image protocol (#5841) | likely-n/a | n/a | — | TUI terminal-image |
| `20da9bc1` | 2026-06-18 | fix attribution for 008c76f9 | likely-n/a | n/a | — | changelog attribution |
| `bc93655e` | 2026-06-18 | meta: Added report template | likely-n/a | n/a | — | .github issue template |
| `908be616` | 2026-06-18 | ref: Remove some options from package reporting | likely-n/a | n/a | — | .github issue template |
| `d0b46764` | 2026-06-18 | feat(coding-agent): add automatic theme mode (#5874) | review | n/a | — | TUI theme-controller + settings-manager (unported); theme is TUI |
| `2b46f388` | 2026-06-18 | feat(coding-agent): Expose edit-diff for extensions (#5756) | review | n/a | — | comment change + SDK export (`generateDiffString`/`generateUnifiedPatch`); no behavior change |
| `aae62dfa` | 2026-06-18 | feat(coding-agent): make bare update self-only | review | n/a | — | package-manager-cli/self-update + cli/args (unported packaging) |
| `71749422` | 2026-06-18 | docs: audit unreleased changelogs | likely-n/a | n/a | — | changelog |
| `c4ab61dc` | 2026-06-18 | Release v0.79.7 | review | ported (superseded) | `5164314` | catalog regen; superseded by 0.79.8 (no separate regen — final 0.79.8 build subsumes it) |
| `788a0444` | 2026-06-18 | Add [Unreleased] section for next cycle | likely-n/a | n/a | — | changelog cycle header |
| `6b9f3f49` | 2026-06-18 | fix(coding-agent): avoid retrying successful overflow compaction | review | n/a | — | agent-session-runtime overflow-recovery (`_runAutoCompaction` willRetry/stopReason gating) — unported; Go has no overflow-error-triggered compaction recovery |
| `7d08c81a` | 2026-06-18 | fix(coding-agent): avoid empty compaction summaries | review | n/a | — | `prepareCompaction` empty guard + `compaction_start/end` event reordering — both unported (Go has no prepareCompaction nor the event lifecycle; compacts inline via shouldCompact/findCutPoint) |
| `b09fbde0` | 2026-06-18 | feat(ai): add OpenRouter Fusion alias (#5866) | review | n/a (data) | — | generate-models.ts alias entry; lands via 0.79.8 catalog regen (`5164314`, id `openrouter/fusion`) |
| `c60f6a8a` | 2026-06-18 | feat(coding-agent): expose post-compaction token estimates | review | n/a | — | `estimatedTokensAfter` on `CompactionResult` SDK type + emitted in `compaction_end` — unported event lifecycle |
| `cab89d14` | 2026-06-18 | docs: audit unreleased changelogs | likely-n/a | n/a | — | changelog |
| `fd1ba2c7` | 2026-06-18 | test(coding-agent): seed auto-compaction queue fixture | likely-n/a | n/a | — | test-only; auto-compaction queue (unported orchestration) |
| `8025fdd0` | 2026-06-18 | meta: Update readmes slightly | likely-n/a | n/a | — | READMEs |
| `651d10d9` | 2026-06-18 | feat(ai): enable Mistral prompt caching | review | n/a | — | `ai/providers/mistral.ts` — Mistral provider unported; catalog cost-field data lands via 0.79.8 regen (44 changed mistral/* entries) |
| `9179734c` | 2026-06-18 | docs(coding-agent): audit unreleased changelog | likely-n/a | n/a | — | changelog |
| `1a418ad2` | 2026-06-19 | chore: remove inprogress label on close | likely-n/a | n/a | — | .github workflow |
| `0d89a333` | 2026-06-18 | feat(packages): Add selective pi-ai base entrypoints (#5348) | review | n/a | — | packaging/exports-map + test import paths + tsconfig/vitest/scripts; no behavior |
| `ea65a51a` | 2026-06-19 | fix: update vulnerable dependencies | likely-n/a | n/a | — | lockfiles/package.json (deps) |
| `a2f70e5f` | 2026-06-19 | fix(coding-agent): reset tool test mocks | likely-n/a | n/a | — | test-only |
| `74677bbf` | 2026-06-19 | docs: audit unreleased changelogs | likely-n/a | n/a | — | changelog |
| `8eb9704b` | 2026-06-19 | Release v0.79.8 | review | ported | `5164314` | ai/models_catalog.json regenerated from npm 0.79.8 (endpoint-pinned both sides, integrity-verified). +9/−3 ids (opencode-go glm-5.2, openrouter/fusion, fireworks glm-5p2, poolside/qwen/cohere/gemini-3-pro-image/liquid; −opencode-go glm-5, −raptor-mini, −xiaomi mimo). 44 changed = Mistral prompt-caching cost fields + fireworks/openrouter/vercel metadata. Subsumes v0.79.7 + 58dd2f59/b09fbde0/651d10d9 data. off:null tripwires intact (fable-5, kimi-k2.7-code). Independent parity review: faithful |
| `56b22768` | 2026-06-19 | Add [Unreleased] section for next cycle | likely-n/a | n/a | — | changelog cycle header |

## Ledger — f8a77f47 → 29c1504c

| Upstream | Date | Subject | Hint | Status | Go commit | Notes |
|---|---|---|---|---|---|---|
| `75b0d723` | 2026-06-16 | fix(ai): support Z.AI GLM-5.2 effort levels | review | ported | `788c832` | openai.go zai branch: when effort + `compat.supportsReasoningEffort`, emit `reasoning_effort` mapped via `thinkingLevelMap` alongside `thinking:{type}`. New `mappedEffortOrRaw` ports pi's undefined→raw / null→omit / string→mapped (distinct from `effortValue`, which returns raw on present-null). Catalog data via 0.79.6 regen. Golden: request body (zai). Tests: low/high/xhigh map, minimal:null omit, off, no-supportsReasoningEffort |
| `06d8c54d` | 2026-06-16 | fix(coding-agent): avoid Windows pi update exit assertion | review | n/a | — | main.ts self-update/CLI exit (unported) |
| `3039f3e1` | 2026-06-16 | fix(tui): restore cursorUp start-of-line jump (#5789) | likely-n/a | n/a | — | TUI editor |
| `7f29e7a3` | 2026-06-16 | feat: add provider-scoped environment overrides (#5807) | review | ported | `872c303` | `StreamOptions.Env` + `getProviderEnvValue` (scoped non-empty wins, else os.Getenv) threaded into `PI_CACHE_RETENTION` + Cloudflare base-URL across anthropic/openai-completions/openai-responses. Bun `/proc` fallback omitted (no Go analog). Host-side population unported (field latent). Owner ruling 2026-06-17. Golden: cache-retention/cloudflare request paths (byte-identical when Env unset). Tests: env precedence, cache-retention scoped env, cloudflare scoped override + empty fall-through |
| `8f0e9251` | 2026-06-16 | fix(coding-agent): do not open browser for device code login | likely-n/a | n/a | — | interactive login-dialog (TUI/oauth) |
| `0680726a` | 2026-06-16 | fix: upgrade marked to 18.0.5 | likely-n/a | n/a | — | export-html vendor min.js + tui dep |
| `91050859` | 2026-06-16 | feat(coding-agent): add settings http proxy | review | n/a | — | core/http-dispatcher: process.env HTTP(S)_PROXY + undici global-fetch host runtime config (unported; Go uses net/http) |
| `2d597f02` | 2026-06-16 | fix(ai): tolerate null Responses message content | review | ported | `e8f7511` | NO code change — Go ranges a nil slice (what `"content":null` unmarshals to) safely, rebuilding to "" exactly as pi's `?? ""`; the JS TypeError has no Go analog. Locked with `TestResponsesNullMessageContent` (mutation-verified non-vacuous) |
| `2431491c` | 2026-06-16 | fix(ai): avoid duplicate OpenCode DeepSeek reasoning controls | review | n/a | — | data-only generate-models.ts (deepseek-v4 compat); net no-op with `bd9f8773` (adds then reverts opencode-go); lands via 0.79.6 regen |
| `b6b5bed9` | 2026-06-16 | docs: update unreleased changelogs | likely-n/a | n/a | — | docs/changelog |
| `6561cb29` | 2026-06-16 | Release v0.79.5 | review | n/a | — | v0.79.5 catalog superseded by 0.79.6 (no separate regen) |
| `0b0b9eae` | 2026-06-16 | Add [Unreleased] section for next cycle | likely-n/a | n/a | — | changelog cycle header |
| `a78cd7cc` | 2026-06-16 | fix(coding-agent): stabilize self-update tests | likely-n/a | n/a | — | self-update test (unported) |
| `a93f0666` | 2026-06-16 | fix(coding-agent): preserve fetch overrides | review | n/a | — | core/http-dispatcher global-fetch/undici install guard (unported runtime config) |
| `bd9f8773` | 2026-06-16 | fix(ai): restore OpenCode Go DeepSeek thinking controls | review | n/a | — | data-only generate-models.ts (reverts the opencode-go arm of 2431491c); lands via 0.79.6 regen |
| `7da475db` | 2026-06-16 | fix(ai): regenerate model catalog | review | n/a | — | data-only catalog (cacheRead/maxTokens); lands via 0.79.6 regen |
| `34b6aea1` | 2026-06-16 | docs(coding-agent): add changelog entries for fetch override and DeepSeek V4 thinking-off | likely-n/a | n/a | — | docs/changelog |
| `31bfb2f1` | 2026-06-16 | Release v0.79.6 | review | ported | `c2221a7` | ai/models_catalog.json regenerated from npm 0.79.6 (endpoint-pinned both sides: old == 0.79.4 build, new == 0.79.6 build, integrity-verified). +11/−7 ids (GLM-5.2 across zai/openrouter/vercel/cf-workers-ai; legacy gemini-1.5/2.0 vertex pruned). Subsumes 2431491c/bd9f8773/7da475db + v0.79.5. Kimi K2.7 Code `off:null` landed → tripwire converted to `TestDeepseekDisabledThinkingGateLive`. Fable-5 off:null intact |
| `12bb8dd2` | 2026-06-16 | Add [Unreleased] section for next cycle | likely-n/a | n/a | — | changelog cycle header |
| `29c1504c` | 2026-06-17 | chore: approve contributor dodiego | n/a | n/a | — | contributor meta |

## Ledger — 93b3b7c1 → f8a77f47

| Upstream | Date | Subject | Hint | Status | Go commit | Notes |
|---|---|---|---|---|---|---|
| `b5e13bcd` | 2026-06-15 | docs(coding-agent): clarify active tools docs | likely-n/a | n/a | — | docs only |
| `ba0ec615` | 2026-06-15 | fix(coding-agent): restore terminal on SIGTERM | review | n/a | — | TUI/terminal SIGTERM handling (unported) |
| `5b6058c3` | 2026-06-15 | fix(tui): align overlays over CJK wide cells | likely-n/a | n/a | — | TUI overlay rendering |
| `24053eab` | 2026-06-15 | fix(tui): update tab overlay boundary expectation | likely-n/a | n/a | — | TUI test-only |
| `bb959aae` | 2026-06-15 | fix(coding-agent): wrap tree help on narrow terminals | likely-n/a | n/a | — | TUI tree-help rendering |
| `a8519681` | 2026-06-15 | docs(coding-agent): reorder containerization patterns | likely-n/a | n/a | — | docs only |
| `0be5bb6c` | 2026-06-15 | fix(ai): price anthropic 1h cache writes at 2x input (#5738) | review | ported | `eadac1a` | added `Usage.CacheWrite1h` (`json:"cacheWrite1h,omitempty"`); anthropic parses `cache_creation.ephemeral_1h_input_tokens` at message_start only (mirrors upstream); `CalculateCost` prices the 1h slice at 2×input and the 5m slice at `cacheWrite`, both /1e6; TestAnthropic1hCacheWriteCost (catalog claude-opus-4-8: 7.75 split / 6.25 fallback) |
| `28b3af5d` | 2026-06-15 | chore: approve contributor Mearman | n/a | n/a | — | contributor meta |
| `408ac103` | 2026-06-15 | fix(ai): update Copilot Claude thinking metadata | review | n/a | — | captured by 0.79.4 regen (github-copilot opus-4.7/4.8 +minimal:low, sonnet-4.6 +minimal:low/xhigh:max) |
| `3fa40956` | 2026-06-15 | fix: drain stdout before resolving when a child holds the pipe past exit (#5753) | review | ported | `e56f1f9` | replaced fixed `cmd.WaitDelay=100ms` drain with manual `runBashCommand`: merged stdout+stderr on one `os.Pipe`, reader goroutine feeds the updater, post-exit idle grace re-armed per chunk (100ms), releases on idle OR pipe EOF; `WaitDelay=1s` backstops cancel/kill. Tests: TestBashCapturesOutputPastExit (late TICK6 captured), TestBashReleasesPromptlyOnQuietHeldPipe (quiet sleeper releases <2s). Race-clean |
| `8a7ad60f` | 2026-06-15 | feat(coding-agent): add binary release checksums | n/a | n/a | — | CI/release |
| `b1ad469b` | 2026-06-15 | docs: audit changelog entries | likely-n/a | n/a | — | changelog only |
| `bba6af2c` | 2026-06-15 | Release v0.79.4 | review | ported | `ded439c` | catalog regenerated from npm 0.79.4 (Go catalog == npm 0.79.4). Diff 0.79.3→0.79.4: +5 (gemma-4-E2B-it, gemma-4-E4B-it, together Kimi-K2.7-Code, zai/zai-coding-cn glm-5.2), 0 removed, 11 changed (copilot thinking overrides 408ac103; opencode/* compat +supportsLongCacheRetention:false; openrouter deepseek-v4-flash & kimi-k2.7-code cost+maxTokens). claude-fable-5 thinkingLevelMap unchanged (`off:null` intact) → TestFable5DisabledThinkingGateLive safe |
| `1aa3c02d` | 2026-06-15 | Add [Unreleased] section for next cycle | likely-n/a | n/a | — | changelog cycle header |
| `0369bdb8` | 2026-06-15 | fix(ai): add Moonshot CN Kimi K2.7 metadata | review | ported (logic; data deferred) | `62fa1e3` | ported the openai-completions deepseek gate: no effort → send `thinking:{type:disabled}` only when `thinkingLevelMap.off !== null` (reuses `offEffortOrDefault` send flag); off:null omits the key. Catalog data (kimi-k2.7-code off:null) is post-0.79.4, deferred. Tests: TestDiffDeepseekThinkingOffGate (on/absent/null/string), TestDeepseekCatalogNoOffNull tripwire |
| `431d88f4` | 2026-06-15 | meta: Link to rfcs | n/a | n/a | — | repo meta |
| `bee8e9c8` | 2026-06-15 | feat(coding-agent): mark experimental sessions in footer | likely-n/a | n/a | — | TUI footer |
| `7cfd1af3` | 2026-06-16 | fix(coding-agent): keep empty session selector open | likely-n/a | n/a | — | TUI session selector |
| `b0c8f65f` | 2026-06-16 | fix(ai): update Google Vertex Gemini models | review | ported (logic; data deferred) | `62fa1e3` | ported the google.ts `isGemini3Flash` change only: also match `gemini-flash-latest` / `gemini-flash-lite-latest` (lowercased) → MINIMAL disabled-thinking config. google-vertex provider + catalog data deferred. Test: TestGoogleDisabledThinkingPerFamily +2 alias cases |
| `f8a77f47` | 2026-06-16 | feat(coding-agent): add Vercel AI Gateway attribution (#5798) | review | ported | `78f6687` | provider-attribution module ported faithfully per 2026-06-16 ruling; +vercel branch. New `ai/providers/attribution.go` (host/provider detection + per-provider header sets gated on install telemetry; PI_TELEMETRY env honored, default enabled per pi `getEnableInstallTelemetry() ?? true`). Wired into openai/openai_responses/anthropic/google at the BOTTOM of the header precedence (session-attribution then default-attribution emitted first, so model.Headers and opts.Headers both override them — matching pi's mergeProviderAttributionHeaders merge order; review caught and fixed an initial wrong-way precedence). Headers byte-exact: OpenRouter `HTTP-Referer:https://pi.dev`/`X-OpenRouter-Title:pi`/`X-OpenRouter-Categories:cli-agent`; NVIDIA `X-BILLING-INVOKE-ORIGIN:Pi`; Cloudflare `User-Agent:pi-coding-agent`; Vercel `http-referer:https://pi.dev`/`x-title:pi`; OpenCode session `x-opencode-session`/`x-opencode-client:pi`. Tests in `attribution_test.go`: all 4 APIs, vercel+openrouter+nvidia+cloudflare+opencode, telemetry gate, model.Headers+opts.Headers precedence, host detection |

## Ledger — 3f44d3e2 → 6f29450e

| Upstream | Date | Subject | Hint | Status | Go commit | Notes |
|---|---|---|---|---|---|---|
| `1c243365` | 2026-06-12 | fix(tui): keep WezTerm Kitty images visible | likely-n/a | n/a | — | TUI image rendering |
| `a455f62f` | 2026-06-12 | fix(ai): preserve Anthropic refusal details (#5666) | review | ported | `e0a362f` | parse `stop_details.explanation` in message_delta; refusal→errorMessage (or "The model refused to complete the request" fallback); throw path uses errorMessage; tests for both branches |
| `be7d5cf5` | 2026-06-12 | fix(ai): relax Codex SSE header timeout | likely-n/a | n/a | — | Codex provider (unported) |
| `1fc80f4f` | 2026-06-12 | fix(coding-agent): preserve custom fallback thinking | review | ported | `c82663e` | buildFallbackModel sets Reasoning:true when surfaced thinking level present and != "off"; fb is freshly cloned (no shared-catalog mutation); resolve_test :high(reasoning=true)/:off(stays false) on non-reasoning mistral template |
| `6102dd20` | 2026-06-12 | fix(coding-agent): handle missing export themes | likely-n/a | n/a | — | export-themes (settings) |
| `0caca6cf` | 2026-06-12 | fix(tui): support slash-separated fuzzy filter tokens | likely-n/a | n/a | — | TUI fuzzy filter |
| `1b2c32c6` | 2026-06-12 | fix(coding-agent): resolve authenticated slash model ids | review | n/a | — | no auth-aware resolution in Go |
| `adf567c1` | 2026-06-12 | fix(coding-agent): rechain fork paths without labels | review | n/a | — | fork/label runtime unported |
| `daab056a` | 2026-06-12 | fix(agent): ignore late tool progress updates | review | ported | `009dae7` | acceptingUpdates bool guarded by existing updateMu; flipped false right after Execute settles; onUpdate drops late calls under lock; ToolUpdateFunc doc updated; race-locked test |
| `17721d5e` | 2026-06-12 | fix(tui): preserve unordered user list markers (closes #5657) | likely-n/a | n/a | — | TUI markdown rendering |
| `a7cdc679` | 2026-06-12 | fix(ai): correct GPT-5 context window metadata | review | n/a | — | captured by 0.79.3 regen; nets to no change |
| `b4bff7f0` | 2026-06-12 | fix(coding-agent): avoid project trust prompt for update (#5674) | review | n/a | — | trust ruling (2026-06-12) |
| `7a3cb631` | 2026-06-13 | fix(ai): normalize generated model costs (#5634) | review | n/a | — | captured by 0.79.3 regen |
| `121f0edb` | 2026-06-13 | fix(ai): detect parenthesized context overflow errors | review | n/a | — | no overflow module in Go |
| `e320f096` | 2026-06-13 | docs: update unreleased changelogs | likely-n/a | n/a | — | docs only |
| `f21f3c4b` | 2026-06-13 | Release v0.79.2 | review | n/a | — | v0.79.2 superseded by 0.79.3 |
| `032c01c1` | 2026-06-13 | Add [Unreleased] section for next cycle | likely-n/a | n/a | — | changelog cycle header |
| `aa3a5233` | 2026-06-13 | fix(ai): restore Codex context limits | review | n/a | — | captured by 0.79.3 regen |
| `57b6bdce` | 2026-06-13 | docs(coding-agent): update Codex context limit changelog | likely-n/a | n/a | — | docs only |
| `f2585c4c` | 2026-06-13 | Release v0.79.3 | review | ported | `c12fa7d` | catalog regenerated from npm 0.79.3 (re-derived + endpoint-pinned, request diff 6/6). Adds `off:null` to claude-fable-5 thinkingLevelMap → the 9ccfcd7c disabled-thinking gate is now LIVE; tripwire converted to TestFable5DisabledThinkingGateLive (end-to-end via catalog model) |
| `b15148fe` | 2026-06-13 | Add [Unreleased] section for next cycle | likely-n/a | n/a | — | changelog cycle header |
| `6f29450e` | 2026-06-13 | fix(ai): update adaptive thinking model expectations | review | n/a | — | test-only, captured by regen |

## Ledger — 130ae577 → 3f44d3e2

Scope-hint is a mechanical pre-classification from touched paths
(`review` = touches packages/{ai,agent,coding-agent}/src outside unported
areas; `likely-n/a` = TUI/docs/unported only). The pipeline's SCOPE step is
the real decision.

| Upstream | Date | Subject | Hint | Status | Go commit | Notes |
|---|---|---|---|---|---|---|
| `38f18be4` | 2026-06-08 | fix(coding-agent): persist implicit project trust on reload | review | n/a | — | project-trust gating (non-port): trust-manager + main.ts wiring only |
| `f4f72d4e` | 2026-06-08 | docs(agent): add security advisory prompt | likely-n/a | n/a | — | upstream repo .pi/prompts only |
| `dce3e285` | 2026-06-08 | fix: show security advisories in prompt widget | likely-n/a | n/a | — | upstream repo .pi/extensions only |
| `718215bd` | 2026-06-08 | feat(coding-agent): add extension project trust decisions | review | n/a | — | trust excluded per 2026-06-12 ruling; ported-adjacent residue verified behavior-neutral (extension resource-loader refactor only) |
| `21917fed` | 2026-06-08 | Merge pull request #5499 from Roman-Galeev/fix/editor-cursor-move-refresh-autocomplete | likely-n/a | n/a | — | TUI editor autocomplete |
| `085a0858` | 2026-06-08 | fix(coding-agent): remove stale hooks export | likely-n/a | n/a | — | npm exports-map cleanup |
| `d8aef0fe` | 2026-06-08 | feat(coding-agent): allow project trust extensions to defer | review | n/a | — | rider on 718215bd — n/a under the 2026-06-12 trust ruling |
| `ce3a7244` | 2026-06-08 | docs(coding-agent): document security model | likely-n/a | n/a | — | docs only |
| `35120d7e` | 2026-06-08 | docs: audit unreleased changelogs | likely-n/a | n/a | — | changelogs only |
| `c10fb95f` | 2026-06-08 | Release v0.79.0 | review | ported | `d7c89c8` | catalog regenerated from npm 0.79.0 build (7 add/7 del/18 chg); go-review ship, parity faithful (endpoint-pinned); npm ref refreshed to 0.79.0 |
| `2edd6b43` | 2026-06-08 | Add [Unreleased] section for next cycle | likely-n/a | n/a | — | changelog cycle headers |
| `20b78eaf` | 2026-06-08 | fix(coding-agent): fix changelog links | review | n/a | — | changelog-link util consumed by TUI only + CI |
| `44e33798` | 2026-06-09 | Merge pull request #5527 from AJM10565/fix/bedrock-arn-region-parsing | likely-n/a | pending | — | |
| `4c486365` | 2026-06-09 | Merge pull request #5505 from awakenalive/patch-1 | likely-n/a | pending | — | |
| `c6bdfa19` | 2026-06-09 | chore: approve contributor davidlifschitz | likely-n/a | pending | — | |
| `2326d5cb` | 2026-06-09 | fix(ai): disable Moonshot thinking when requested | review | ported | `732cfa5` | data-only (moonshot thinkingFormat=deepseek); landed via the 0.79.1 catalog regen (`28df940f`) |
| `22e45492` | 2026-06-09 | Merge pull request #5283 from smoosex/main | likely-n/a | pending | — | |
| `def99d39` | 2026-06-09 | chore: approve contributor vdxz | likely-n/a | pending | — | |
| `8da077bc` | 2026-06-09 | fix(tui): wrap CJK text at grapheme boundaries | likely-n/a | pending | — | |
| `84cdd024` | 2026-06-09 | fix(ai): disable Azure OpenAI response storage | likely-n/a | pending | — | |
| `081a0a2b` | 2026-06-09 | chore: approve contributor dangooddd | likely-n/a | pending | — | |
| `db3f9953` | 2026-06-09 | feat(coding-agent): expose project trust to extensions | review | pending | — | |
| `e4907b3b` | 2026-06-09 | fix(tui): restore prompt draft after history browsing | likely-n/a | pending | — | |
| `19060743` | 2026-06-09 | fix(coding-agent): handle invalid models json during migration | review | pending | — | |
| `28c83e83` | 2026-06-09 | fix(coding-agent): sync queue modes on reload | review | pending | — | |
| `66335d3a` | 2026-06-09 | feat(coding-agent): add experimental feature guard (#5547) | review | ported | `16ed486` | coding/experimental.go: AreExperimentalFeaturesEnabled ⇔ PI_EXPERIMENTAL == "1" exactly |
| `64b51efb` | 2026-06-09 | fix(ai): use z.ai thinking payload | review | ported | `0b8a47c` | zai now sends thinking:{type:"enabled"\|"disabled"} instead of enable_thinking bool (openai.go applyReasoningFormat) |
| `9632bddd` | 2026-06-09 | fix(coding-agent): stabilize OAuth login prompt rows | likely-n/a | pending | — | |
| `3d02d1da` | 2026-06-09 | fix(ai): map OpenCode max tokens | review | ported | `732cfa5` | data-only (opencode/opencode-go maxTokensField=max_tokens); landed via the 0.79.1 catalog regen (`28df940f`) |
| `d041b5cc` | 2026-06-09 | Merge pull request #5549 from earendil-works/approval-settings | review | pending | — | |
| `69ea1a63` | 2026-06-09 | docs(coding-agent): clarify model name display docs | likely-n/a | pending | — | |
| `b7e721cb` | 2026-06-09 | feat(tui): support autocomplete trigger characters | likely-n/a | pending | — | |
| `ae7a885d` | 2026-06-09 | Closes #5045, /new should not persist if original session was ephemeral | review | pending | — | |
| `c5582102` | 2026-06-09 | Merge pull request #5553 from dannote/prompt-template-defaults | review | pending | — | |
| `a0c2465d` | 2026-06-09 | docs: audit unreleased changelogs | likely-n/a | pending | — | |
| `5a9d72ea` | 2026-06-09 | feat(ai): add Claude Fable 5 metadata | review | ported | `732cfa5` | data-only (claude-fable-5 entries, xhigh thinkingLevelMap); landed via the 0.79.1 catalog regen (`28df940f`) |
| `6b5923f1` | 2026-06-09 | fix(ai): correct Azure gpt-5.4/5.5 context window and gpt-5-pro maxTokens | likely-n/a | pending | — | |
| `66f432ca` | 2026-06-09 | fix(ai): regenerate models for Claude Fable 5 and Azure metadata overrides | review | ported | `732cfa5` | data-only (models.generated regen); landed via the 0.79.1 catalog regen (`28df940f`) |
| `4d9f9f45` | 2026-06-09 | fix(ai): regenerate image models for upstream Riverflow rename | review | pending | — | |
| `28df940f` | 2026-06-09 | Release v0.79.1 | likely-n/a | ported | `732cfa5` | ai/models_catalog.json regenerated from npm 0.79.1 build (11 add/0 del/51 chg; supersedes the 0.79.0 regen); captures `2326d5cb`/`3d02d1da`/`5a9d72ea`/`66f432ca` |
| `82f2b1e9` | 2026-06-09 | Add [Unreleased] section for next cycle | likely-n/a | pending | — | |
| `dacb367e` | 2026-06-09 | fix(ai): expect Claude Fable 5 in adaptive thinking model test | likely-n/a | pending | — | |
| `9ccfcd7c` | 2026-06-10 | fix(ai): omit disabled thinking for Claude Fable 5 | review | ported | `dbad9d5` | anthropic.go: skip thinking:{type:"disabled"} when thinkingLevelMap has off:null (present-nil); generate-models off:null lands with a future catalog regen |
| `a7f9fe68` | 2026-06-10 | fix: bump shell-quote to 1.8.4 in lockfile (GHSA-w7jw-789q-3m8p) | likely-n/a | pending | — | |
| `9fd75b8a` | 2026-06-10 | Merge pull request #5560 from haoqixu/fix-5552 | review | ported | `1c81b72` | coding/resolve.go: strip valid `:level` suffix before custom-id fallback, surface as ThinkingLevel, warning quotes stripped id |
| `e537dba3` | 2026-06-10 | Merge pull request #5561 from unexge/push-lpxyxwstnswr | likely-n/a | pending | — | |
| `2f5066d7` | 2026-06-10 | Merge pull request #5562 from Perlence/fix-tui-render-loose-lists | likely-n/a | pending | — | |
| `a3cd03e7` | 2026-06-10 | Merge pull request #5585 from haoqixu/fix-editor-cjk-wrap | likely-n/a | pending | — | |
| `0ab2aa86` | 2026-06-10 | feat(coding-agent): add experimental first-time setup flow (#5587) | review | pending | — | |
| `406a2214` | 2026-06-10 | fix(coding-agent): refine setup copy | likely-n/a | pending | — | |
| `1da90398` | 2026-06-11 | fix(coding-agent): skip first-time setup for forks (#5627) | review | pending | — | |
| `3f44d3e2` | 2026-06-12 | fix(ai): remove stale OpenRouter Kimi free model assertion (#5650) | likely-n/a | pending | — | |

## Ledger — 6f29450e → 93b3b7c1 (no-op cycle)

| Upstream | Date | Subject | Status | Notes |
|---|---|---|---|---|
| `f315d814` | 2026-06-13 | meta: update weekend policy in contributing | n/a | meta/docs |
| `9e9fc794` | 2026-06-13 | fix(coding-agent): treat uppercase config values as literals | n/a | config-migration / settings-manager (non-ported) |
| `21a904f4` | 2026-06-13 | fix(ai): disable OpenCode long cache retention for rejecting routes | n/a | data-only catalog flag; behavior already ported (openai_compat/openai.go); no release in window → next release regen absorbs it |
| `5be8c31f` | 2026-06-14 | meta: add extension disclaimer to bug reporting | n/a | meta |
| `2fbdff9d` | 2026-06-14 | fix(coding-agent): fix pnpm self-update bin-dir | n/a | self-update/packaging (non-ported) |
| `c48f656f` | 2026-06-14 | fix(coding-agent): handle npm package semver ranges | n/a | package-manager (non-ported) |
| `3fcfb7ab` | 2026-06-14 | docs(coding-agent): document extension resource lifecycle | n/a | docs |
| `f0989800` | 2026-06-14 | feat: detect first-run terminal theme (#5385) | n/a | TUI + interactive theme detection (non-ported) |
| `11b5403f` | 2026-06-14 | fix(coding-agent): exit after package commands | n/a | bun/CLI + package-manager (non-ported) |
| `6b40c99a` | 2026-06-14 | feat(examples): wrap question extension text instead of truncating (#5708) | n/a | examples |
| `d683a581` | 2026-06-14 | meta: update CONTRIBUTING.md for clearer language | n/a | meta/docs |
| `93b3b7c1` | 2026-06-14 | fix(tui): preserve WezTerm Kitty images on full redraw | n/a | TUI image rendering |
