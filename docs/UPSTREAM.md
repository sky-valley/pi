# Upstream provenance & sync ledger

Tracks exactly which upstream pi the Go port corresponds to, and the
commit-by-commit sync pipeline that keeps it current.

- **Upstream**: https://github.com/earendil-works/pi (TypeScript, by Mario Zechner)
- **This port started**: 2026-06-08 (cloned upstream `main` HEAD of the day)

## Current pin

| What | Value |
|---|---|
| TS source fully reviewed/ported | `93b3b7c1` — "fix(tui): preserve WezTerm Kitty images on full redraw" (2026-06-14); previous pins `6f29450e` (06-13), `3f44d3e2` (06-12) |
| npm build the byte-goldens were captured from | `@earendil-works/pi-ai` **0.79.3** (request goldens re-verified 6/6 + 2 zai scenarios against the 0.79.3 build); `pi-coding-agent` 0.78.1 (session/image goldens — unaffected by 0.79.x) |
| Parity proofs at the pin | requests 6/6 · session tree 8/8 · image decisions 8/8 byte/decision-identical |
| Reviewed via | initial port + parity sweep 1 + parity sweep 2 (`3be3911`), registration fix (`b09cb46`) |

Deliberately not ported (out of scope for the ledger unless a commit changes
that decision): TUI, extensions runtime, OAuth token acquisition, project-trust
gating, Bedrock/Vertex/Mistral/Azure/Codex providers, image generation, bun/CLI
packaging, prompt-templates, settings-manager, config migrations,
agent-session-runtime (session reload + /new flow).

### Rulings (answers to `decide` escalations — triage must not re-ask)

- **2026-06-12 — project trust stays excluded** (re: `718215bd`, `d8aef0fe`,
  and the wider upstream trust push). Criteria set by the owner: not an SDK
  use case (host apps control what loads), postponable (purely additive
  subsystem), and verified not to change behavior of ported surface (the only
  ported-adjacent diff was a behavior-neutral refactor inside the unported
  extension resource-loader; `skills.ts` untouched). Future trust commits are
  `n/a` under this ruling UNLESS they change behavior of surface we ported —
  that re-escalates.

## Drift at last sync check (2026-06-14)

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
