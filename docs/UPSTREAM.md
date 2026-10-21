# Upstream provenance & sync ledger

Tracks exactly which upstream pi the Go port corresponds to, and the
commit-by-commit sync pipeline that keeps it current.

- **Upstream**: https://github.com/earendil-works/pi (TypeScript, by Mario Zechner)
- **This port started**: 2026-06-08 (cloned upstream `main` HEAD of the day)

## Current pin

| What | Value |
|---|---|
| TS source fully reviewed/ported | **`82432219a`** — "docs: note generated model data is gitignored in AGENTS.md" (advanced 2026-10-21 from `fdce48eb4`). **12 changes in `fdce48eb4..82432219a`, union = first-parent** (no merges). **No release crossed** (`git tag --contains fdce48eb4` empty; pi-ai still 1.3.0), so no regen and no tag. Upstream `ci.yml` (workflow-scoped): `cedefc532` and HEAD green; `e5fe9777b`'s run was cancelled, HEAD contains it. **Verdicts: 4 port / 8 n/a / 0 decide.** **Ported (2):** `cedefc532` (`23a0338`), `e5fe9777b`'s resolver hunk (`42f037f`). **Queued (2):** row 21 +1 (`bdd9f0399`), row 22 +1 (`885a564b3`). **CATALOG-ONLY (1):** `e5fe9777b`'s generator hunk. **n/a (8):** 6 E1 host (`396ecd7b8`, `2ed3118b0`, `9df819de3` tui; `38847563f` interactive-mode; `f8ab362cc` `utils/clipboard-command.ts`, whose only consumers are host; `22dc6c26f` `core/extensions/**`), 1 no meaning in Go (`16f5f0af4`: durable's browser-only `storage/indexeddb.ts`, dropped like the `*/cloudflare` adapters), 1 noise (`82432219a` docs). |
| npm build the byte-goldens were captured from | `@earendil-works/pi-ai` **1.3.0** (and `pi-coding-agent`, `pi-env`, `pi-durable` 1.3.0). `ai/models_catalog.json` regenerated once from it (`797d14f`): 867,163 → 939,772 B, 1,529 → **1,614 models** (+140/−55, 44 re-keyed, 366 changed) across an unchanged 42 providers, `azure-openai-responses` renamed `azure`. `@anthropic-ai/sdk` 0.124.0 → **0.129.0** (`b271b0a52`; diff audited, federation re-captured over a 0.124.0 control). **New captures** (1.3.0 build unless noted): `ai/testdata/{envkeys,setup-error-timing,key-order,validation-errors,builtin-auth}` and `telemetry` (src at `d2e7d8b7c`: requests, and compat's routing); 17 under `ai/providers/testdata` (`blank-tool-output` src at `4dd2b149d`; `cloudflare-auth`, 1,106 rows incl. 3 oauth), `ai/providers/testdata/unported-apis`, `coding/testdata/runtimekey` (a real pi `AgentSession` over `ModelRuntime.create`, 5 scenarios, 21 requests); `coding/testdata/{toolselection,modelresolver,readimagenote}`; sessparity `22_compaction_retained_tail`; in `env/`, `daemon/testdata/oracle/{darwin,linux}.json` (npm 1.3.0's Rust daemons), `testdata/{differential,wire,connection,ssh}` and `internal/nodepath/testdata/node.json`. **Re-captured**, each with a control at the sha's parent: retry-classify, response-model, streaming-json, google-thinking, openai-stream, anthropic-sse, httperror (+ byte rows), stream-abort, abort-before-response, jstrim, transcript anthropic-native, systemprompt and toolschemas (now from the build), defaultmodels (src `d2e7d8b7c`), chord/delta's `upstream_delta.json` (src `d2e7d8b7c`; every earlier record byte-identical). **Src captures to redo from the first release that ships them:** compaction `requests`/`resume` (`6fcb48670`), both telemetry files (`2ec55d808`). **2026-10-20 src captures (re-verify against the first build that ships them):** `ai/providers/testdata/transcript/responses-midconvo-530015d93.json` (13 → 26 cases) and `completions-native-caa6967b6.json` (+6 Kimi cases), each with a control at the parent reproducing the old file; `coding/testdata/defaultmodels` at `fdce48eb4` (table unchanged, superseded below); `internal/nodepath/testdata/node.json` (+186 `toNamespacedPath` rows, Node v26.4.0, control reproduced). The telemetry captures were re-run at `d9025a5f5` and are unchanged (they reduce carriers to span ids). **2026-10-21 src captures (re-verify against the first build that ships them):** `ai/testdata/retry-classify/classify-cedefc532.json` (207 → 210 rows; pi-telemetry shimmed to its src at the sha), its old `capture.mts` at the parent `e5fe9777b` reproducing `classify-d2e7d8b7c.json`; `coding/testdata/defaultmodels` at `82432219a` (`github-copilot` → `gpt-6.1-sol`, the only change), its control at `fdce48eb4` reproducing the old file. |
| Parity proofs at the pin | **2026-10-21.** Both ports observed RED for a behavioural reason first (in the commit bodies): `IsRetryableAssistantError` true where pi says false, the workspace hint missing on both the rejected-request and `response.failed` paths, the Copilot default `gpt-5.4` against pi's `gpt-6.1-sol`. The parity reviewer re-ran upstream `retry.ts` at `cedefc532` under node independently of the capture (0 of 210 rows differ), ran four extra inputs through pi src and Go (incl. U+017F) with identical answers, `cmp`-matched the hint bytes, and diffed all 41 `defaultModelPerProvider` entries. **difftest** re-pinned to src `82432219a` / npm 1.3.0: 66 PASS / 0 KNOWN / 0 FAIL. **Gate on the integrated tree, green:** `gofmt -l` clean, build + vet, `go test -race ./... -count=1`, windows build + vet, linux/386 build; env through a workspace over root + env: build, vet, `go test -race ./...`, windows and linux/386 build + vet; env with `GOWORK=off`: build + vet. |
| Reviewed via | 2026-10-21 — triage of all 12 changes from the real diffs (hunks read for every ai, coding-agent core, extensions and durable change; the five tui/modes ones from the diffstat). One judgement round (pi-go-review + pi-parity-review) over both ports: **0 filed**, so no refuters ran. |

## The scope boundary (rewritten 2026-08-27 — read this before triaging anything)

**The port is pi's SDK half.** Scope is decided by two **exclusion tests** —
E1 and E3. A hunk is `n/a` if and only if one of them fires; **otherwise it is
IN scope**. (E2 was an exclusion test until 2026-08-27; the owner replaced it
with a consult — see below.)

*The tests are the authority.* Every path list — in this file, in the
`pi-triage` skill, anywhere — is a **derived convenience**. When a list and a
test disagree, the test wins and the list is fixed in the same commit. This
replaces the old "Deliberately not ported" enumeration, which was a path list
pretending to be a boundary: 12 of 16 scope rulings in this ledger resolved a
path INTO scope, every reversal ran OUT→IN, and twice the cause was upstream
simply moving a file across the line.

**Reachability is evidence, not a test.** "Published, independently reachable
surface" (`src/index.ts`, the `exports` map) is what tells you something is SDK
surface *at all* — it is the deciding fact in 2026-08-07, 2026-08-11 and
2026-08-18, and it is why the default is IN. But it argues in **one direction
only**: it can never override E1–E3. pi exports its entire application from
`packages/coding-agent/src/index.ts`, `InteractiveMode` and the TUI components
included; being root-exported does not make something SDK surface.

### E1 — host surface

Fires when the file belongs to **the application pi ships around the SDK**
rather than to the SDK itself: its entry points and its modes of operation
(interactive, rpc, print — *headless or not*; `modes/rpc` is a JSON-stdio
embedding surface and is still host), its terminal UI, its operator
configuration, its packaging and installer — **or when its only consumers are
host surface.**

That last clause is load-bearing and is what the older rulings were reaching for
without naming: it covers host machinery that *populates* an already-ported SDK
seam (the seam is ported and latent; the populator is not — 2026-06-17) and
constructs that exist to serve the TUI (2026-07-30). **It fires only when EVERY
consumer is host surface. A mixed consumer set does NOT fire it — read the
hunk.** That restriction is the whole safety margin: without it the clause
would swallow files the port has already partly ported.

Derived list (today): `packages/tui`, `packages/evals`, `packages/ai/src/cli.ts`,
and under `packages/coding-agent/src` — `modes/**`, `cli/**`, `main.ts`,
`core/extensions/**`, `core/export-html/**`, `core/settings-manager.ts`,
`core/prompt-templates.ts`, `core/agent-session-runtime.ts` and the session
reload / `/new` lifecycle, `core/model-registry.ts`,
`core/resolve-config-value.ts`, `core/radius.ts`, `core/resource-loader.ts`
source-info accessors, `core/provider-composer.ts`, `core/model-config.ts`,
`migrations.ts`, `bun/**`, `package-manager-cli.ts`, `core/keybindings.ts`,
`core/tools/renderers/**`, `extensions/llama/**`, the `builtin:<name>`
extension resolution (`4259686d9`), and the host files of the built-in
extensions ruled IN on 2026-09-29 — `extensions/mcp/ui.ts`,
`extensions/mcp/cli.ts` + `cli.lazy.ts`, `extensions/codemode/renderer.ts`.
**`extensions/{mcp,codemode,tool-search}/**` otherwise are IN** (2026-09-29
ruling), and `extensions/mcp/index.ts` splits by HUNK.

**A commit whose excluded content is entirely on that list is triaged on its
non-host hunks alone, and never escalates on account of the host half.** No
hunk-reading is owed; the diffstat is enough. This is the single highest-value
rule here — roughly half of all mixed commits per 90 days are mixed *only*
because of host content, and ~65% of mixed commits end in `n/a` anyway.

#### Files that must be split by HUNK — never diffstat-dispatched

These are **partly ported**. The diffstat shortcut above does not apply to them,
and treating them as host surface would manufacture exactly the miss this
ledger has already recorded twice:

| file | ported half | host half |
|---|---|---|
| `core/model-resolver.ts` | `defaultModelPerProvider`, the fallback-model construction and (since `9a6d5bd`, `d7f7e0dfe`) the reference-resolution policy → `coding/resolve.go` (`defaultModelPerProvider`, `buildFallbackModel`, `resolveModelReference`, `resolveCliModel`) | the host resolution flow: `resolveModelScope*`, `findInitialModel`, `restoreModelFromSession`. **The table has been the source of a MISS twice** (2026-07-26 `24e5cc04`, 2026-08-04 `c1019d920`); the standing rule to diff `defaultModelPerProvider` on every touch is still in force. At every re-pin, re-capture `coding/testdata/defaultmodels` — from the npm build when the pin is a release, from a src extraction at the pin otherwise (`capture.mjs --src`, file named by the sha) — and point the test at it: `TestDefaultModelPerProviderMatchesPi` requires the whole table to equal it |
| `core/package-manager.ts` | project/user skill discovery → `coding/resources.go` (it is the `if (projectTrusted)` site the 2026-08-27 trust ruling cites) | npm install / self-update, no Go surface |
| `core/trust-manager.ts` | the trust decision and gate (2026-08-27) | the prompt, selector and persistent store |
| `packages/telemetry/src/index.ts` | the runtime contract (2026-08-06) with its propagation, links, metrics and capabilities, and the schema DATA (`telemetry/schema.go`, `NewTypedMetrics`; 2026-10-19) | the schema's type inference — E3 |
| `core/model-runtime.ts` (2026-09-24) | the `Models` contract it forwards — the typed accessors and the chat guard on `stream`/`streamSimple`/`streamDeferred`/`cancelDeferred` → `ai.Models` (`ai/models_runtime.go`); image/classifier operations → Scope queue row 3 | auth.json credential store, models.json + extension composition (`provider-composer.ts`), radius wiring; the remote-catalog wrapping is row 19 |

Three riders:
- **E1 applies to a HUNK, not only a file.** A hunk whose added lines serve only
  host surface is host, even inside a file that is not. The converse also holds
  and is the 2026-07-21 lesson: a hunk inside a host file is `port` when its Go
  home is a shared or generic function.
- The port deliberately has **no mode layer**. `cmd/pi` is a hand-rolled SDK
  CLI, not a port of pi's modes; never map a mode change onto it.
- **A gate is in scope exactly when the port has the thing it gates**
  (2026-08-27 project-trust ruling). Host code that only *asks* the user stays
  out; the decision and the gate come in the moment the port grows the consumer.

### E2 — a third-party dependency is a CONSULT, not an exclusion

**Owner ruling, 2026-08-27 (evening): if it is part of the pi SDK, the port
supports it. Full stop. If porting it would need an outside Go dependency, that
is a question to bring to the owner — not a reason to decide `n/a` on your
own.**

This replaces the earlier form of E2, which made "would this add a `require`
line?" an automatic exclusion and ruled bedrock and codex permanently out. That
was the port drawing its own boundary rather than following pi's, which is the
thing this whole rewrite exists to stop. **E2 is therefore no longer an
exclusion test** — the boundary has two: E1 and E3.

The standing preference still holds and is worth stating: the port's runtime
module today depends only on the Go standard library and `golang.org/x/*`, and
staying there is a real asset (trivial cross-compilation, no cgo, a tiny supply
chain). **As of 2026-08-31 this is enforced mechanically, not by this
paragraph** — `internal/policy/deps_test.go` fails when the build reaches a
module outside its allowlist, or when any non-stdlib package carries cgo files.
It runs under the base gate's existing `go test ./...`, which is the only thing
this repo runs every cycle (there is no CI). Adding a module means editing that
allowlist, which is the consult, made unskippable. So the rule is:

> A `packages/ai/src` adapter is **IN scope**, like everything else the SDK
> publishes. If a faithful Go port of it would require a third-party module for
> the transport, the wire encoding, or the credential chain, **resolve it with
> the standing formula** (see the 2026-08-31 governing ruling) and record the
> answer as an ordinary triage decision. Open a Scope queue row flagged
> `CONSULT` **only** when the formula's answer is one of the three escalating
> kinds: it requires cgo, it costs supported build targets, or it is large
> enough to change the port's shape. Do not port it silently, and do not
> `n/a` it. **Three of the first three E2 consults rested on a premise that was
> already false at the pin** — check what upstream does *today* before writing
> the question, and most of the time there is no question.

Riders that survive unchanged: a hunk inside such an adapter is still `port`
when its Go home is a shared or generic function (the 2026-07-21 lesson); and a
transparent JSON/SSE-over-HTTPS vendor wrapper like `@google/genai` raises no
dependency question at all, since the port already hand-rolls what the `openai`
and `@anthropic-ai/sdk` packages do.

**Consequence: `amazon-bedrock` and `openai-codex` are back IN scope**, as queue
entries with open consults. The catalog's 125 bedrock + codex models stop being
"accepted debt that fails at stream time" and become work with a question in
front of it.

#### The two consults, stated so they can be answered

- **Bedrock** — **ANSWERED 2026-08-31: TAKE `aws-sdk-go-v2` (with `config`),
  confined to a `github.com/sky-valley/pi/providers/bedrock` submodule** with its
  own tag series, so the root module's graph stays at its two `golang.org/x/*`
  requires. See the 2026-08-31 ruling and Scope queue entry 9. The question as
  originally posed — "needs AWS SigV4 signing, the `vnd.amazon.eventstream` binary
  frame format and the AWS credential chain; SigV4 is ~300 hand-rollable Go lines
  and eventstream ~200, so hand-roll or take the SDK?" — **and its conclusion that
  parity 'argues for hand-rolling' are WITHDRAWN**: that reasoning priced only the
  signer. Signing is roughly a third of the work and the credential chain is the
  rest, and pi carries no credential-resolution code at all, so `AWS_PROFILE`,
  ECS task roles and IRSA (all advertised in `packages/coding-agent/docs/providers.md`)
  work only because the SDK's default chain is linked in. Hand-rolling would ship
  a documented-feature regression.
- **Codex** — ANSWERED 2026-08-31, and the question as written was wrong on its
  facts. There is **no zstd decompression**: no `Accept-Encoding` is ever set and
  no response-side decode exists anywhere in `packages/ai/src`. It is request-body
  **compression** only, on the SSE path only, and the WebSocket transport sends
  uncompressed JSON. It is also severable by construction —
  `compressRequestBodyZstd` returns `null` when zstd is unavailable and the caller
  sends plain JSON with no header. Upstream takes **no** third-party package for
  either capability: zstd is `process.getBuiltinModule("node:zlib")` and WebSocket
  is `globalThis.WebSocket`. So: **take nothing for zstd**, and defer the
  WebSocket module to the WS slice. See the 2026-08-31 ruling.

### E3 — no Go representation

Fires when the construct is a **TypeScript type-level artifact with no runtime
behavior**, so there is nothing to port. Today this is exactly one thing: the
type inference of `packages/telemetry`'s schema (`defineTelemetrySchema`'s
typing, `Infer*`, `SchemaTelemetrySpan`, `createTypedSpanStarter`'s typing,
`TypedTelemetryMetrics` — 2026-08-06, narrowed 2026-10-19). It is
root-exported, which is why it needs E3 rather than reachability: exported
*types* are not runtime surface. **The schema's DATA is not E3** and is ported
(`telemetry/schema.go`): since `2ec55d808` pi-ai reads `AI_TELEMETRY_SCHEMA` at
runtime, through `createTypedMetrics`, to hand adapters each metric's
description, unit and boundaries. Inference and data share `src/index.ts` with
the runtime contract, so **this one must be split by HUNK** — and it is the
standing example of an exclusion that is not expressible as a path.

Go generics do not change the answer: the inference exists to make
`Infer<typeof schema>` produce a compile-time span type, which is a TypeScript
type-system feature with no runtime residue at all.

### What this leaves genuinely out

| out | test | note |
|---|---|---|
| `packages/tui` + `coding-agent/src/modes/**` | E1 | 37,257 LOC of implementation — **69% of the 53,706 LOC this table excludes**. TUI alone is 16,966 LOC (34,144 with upstream's own test suite) across 101 commits in 90 days, yet the sole cause of only **4** mixed commits, one triage lesson and zero defects: the cheapest exclusion the port has. `modes/**` is the other 20,291 LOC and 56 sole-cause mixed commits. The pair is ~95 port-cycles, and there is no oracle — a request body is a value you can diff, an escape-byte stream interpreted by a terminal is not. |
| extensions runtime (`core/extensions/**`) | E1 | 4,121 LOC. Runs operator-authored JS in the host process. |
| `cli/**`, `main.ts`, `bun/**`, `package-manager-cli.ts`, `migrations.ts`, `packages/ai/src/cli.ts` | E1 | 4,303 LOC plus `ai/src/cli.ts`. Entry points, installer/self-update, config migrations. `ai/src/cli.ts` was carved out by the old skill table ("except … cli") and keeps its exclusion here on E1 rather than on a list entry. |
| `core/settings-manager.ts`, `core/prompt-templates.ts` | E1 | 1,645 LOC. Operator configuration. |
| `core/agent-session-runtime.ts` + the session reload / `/new` lifecycle | E1 | Drives the interactive and print session lifecycle; its consumers are modes. |
| `core/model-registry.ts`, `core/resolve-config-value.ts`, `core/radius.ts` | E1 (only-consumers clause) | 445 LOC. The host-side machinery that POPULATES ported SDK seams — `StreamOptions.Env` (2026-06-17) and the Radius host wiring (2026-07-14). The seams stay ported and latent. **`core/model-resolver.ts` (782 LOC) is deliberately NOT here** — it is partly ported and is in the split-by-HUNK table above. |
| `core/provider-composer.ts`, `core/model-config.ts` | E1 (operator configuration) | 2026-09-24. They layer the operator's models.json and extension provider registrations over a built-in `Provider`; `ModelRuntime` bypasses the composer when neither layer exists (`model-runtime.ts:276-280` at `a328aa89a`). Previously out only by the 2026-07-17 note's only-consumers premise, which no longer holds for the cluster as a whole. |
| `core/export-html/**` | E1 | 746 LOC of TypeScript plus 4,275 lines of bundled template/asset files. Renders a session to an HTML file for a human. |
| `core/resource-loader.ts` source-info accessors | E1 (only-consumers clause) | 2026-07-30. Sole upstream consumer is `modes/interactive`. |
| `core/keybindings.ts`, `core/tools/renderers/**` | E1 | Recorded 2026-09-29 so the next touch does not default IN. Keybindings' importers at `4259686d9` are `cli/**`, `core/extensions/**`, the experimental TUIs, the extensions' `ui.ts` and `migrations.ts`; the renderers draw tool calls in the TUI. |
| `extensions/llama/**` | E1 (only-consumers clause) | 2026-09-21 (`1e39862f6`), re-verified 2026-09-29 at `4259686d9`: its only consumer is `extensions/index.ts`'s `builtInExtensions`, imported only by `main.ts`, and it is not root-exported. The built-ins that ARE root-exported and wired into `createAgentSession` — mcp, codemode, tool-search — are IN (2026-09-29 ruling). |
| the built-in extensions' host files — `extensions/mcp/ui.ts`, `extensions/mcp/cli.ts` + `cli.lazy.ts`, `extensions/codemode/renderer.ts` | E1 | 2026-09-29. The `/mcp` screen, `pi mcp list\|login\|logout`, and codemode's TUI rendering. `extensions/mcp/index.ts` is not here: it mixes registration with ~20 lines of `/mcp` command and `ctx.ui` code, so it splits by hunk. |
| `builtin:<name>` extension resolution — `package-manager.ts`'s built-in extension half, `resource-loader.ts`'s built-in loading, `source-info.ts` `BUILTIN_PATH_PREFIX`/`isSyntheticPath` | E1 (operator configuration) | 2026-09-29 (`4259686d9`). It enables and disables built-ins through the `extensions` setting (`-builtin:mcp`) and `-e builtin:<name>`; the Go features are switched on by session options, so it has no Go analog. `package-manager.ts`'s skill-discovery half is unaffected and stays split by hunk. |
| `packages/evals` | E1 | 1,277 LOC. Upstream's own eval harness. |
| — | — | **No longer out:** `amazon-bedrock` and `openai-codex` are IN scope as queue entries 9 and 10, not exclusions. Their dependency consults were both ANSWERED 2026-08-31 (bedrock: `aws-sdk-go-v2` in a submodule; codex: no dependency needed to ship) — see E2. |
| the telemetry schema's type inference | E3 | Split by HUNK — shares `src/index.ts` with the ported runtime contract and the ported schema data. |
| the trust prompt, selector and store — `cli/project-trust.ts`, `modes/interactive/components/trust-selector.ts`, and `core/trust-manager.ts`'s persistence | E1 | Asking the user is host surface. The trust *decision and gate* are ported (2026-08-27), so **`core/trust-manager.ts` splits by HUNK** — see the table above. |
| docs, CHANGELOG, CI, `.github/`, `.pi/`, examples, per-package `package.json` version bumps, repo-root `scripts/` | — | Always noise; not a scope question. |

**That table is exhaustive.** If a hunk is not covered by a row above and no
test fires on it, it is **in scope** — that is the default, and it is the
direction this boundary has always moved. If you believe something belongs out
and no test reaches it, that is a `decide`: it means a test needs changing.

### Rulings (answers to `decide` escalations — triage must not re-ask)

- **2026-10-19 — GOVERNING: pi-env is ported in full, this cycle, daemon
  included. The daemon is rewritten in Go, its binaries are embedded with
  `go:embed` in the `env` submodule, and `github.com/fsnotify/fsnotify` is
  allowed as a dependency of that submodule.** Owner ruling, answering the
  CONSULT every triage range that met `packages/env` raised (`ba03e03f2` added
  `@earendil-works/pi-env`, first published in 1.0.4: a Durable `ExecutionEnv`
  over SSH, with a Rust daemon shipped as prebuilt binaries in the npm package
  and deployed by SHA-256). Triage split — one range recommended a Go daemon,
  another deploying upstream's Rust binaries as a byte-golden peer — and the
  owner chose the Go daemon: the mirror, with no foreign binaries in the
  module.

  What it settles, and what triage may therefore never re-ask:
  1. `packages/env` is IN and has a Go base: the client (`src/**`) is package
     `github.com/sky-valley/pi/env`, the daemon (`daemon/src/**`) is
     `env/daemon` (mirrored module by module) with its binary `env/cmd/pi-env`.
     **A Rust-only upstream change to `packages/env/daemon` is an ordinary
     `port`** to `env/daemon`, never "the peer changed, re-check goldens". The
     oracle is the npm build's Rust daemon: `TestOracle` compares replies byte
     for byte, and upstream's `packages/env/test` runs against both daemons.
  2. The embedded daemons (`env/bin/pi-env-<platform>-<arch>.gz` +
     `manifest.json`, eight targets) are generated: `go generate ./...` in `env`
     rebuilds them reproducibly (pinned go environment, `-trimpath`, recorded
     toolchain). Any change to `env/daemon`, `env/internal/frame` or
     `env/cmd/pi-env` regenerates and commits `env/bin` in the same change;
     `TestEmbeddedDaemonsAreCurrent` fails until it does.
  3. The submodule's dependencies are `golang.org/x/sys` and `fsnotify` (Linux
     and Windows watching; macOS uses the daemon's own kqueue watcher), plus the
     root module's `x/image` and `x/text` reached through `coding`.
     `env/internal/policy` holds the daemon's graph to {x/sys, fsnotify} and
     forbids cgo. The root module's graph is unchanged.
  4. `env` is the first real submodule — see "Submodule shape" for its tag
     series and release steps. The daemon's `Version` tracks pi-env's npm
     version (`1.3.0`), since `hello.version` reports it.
  5. The Durable types the client needs (`FileError`, `ExecutionError`,
     `BinaryReader`, `WatchTarget`, …) live in package `env` until Scope queue
     row 8 lands the durable `ExecutionEnv`; then they move there.

- **2026-10-19 — skip the intermediate releases: one regen, from the last
  release crossed, and one port release.** Owner, on this cycle's drift (eight
  releases, v1.0.0 to v1.3.0). The catalog is regenerated once, from npm 1.3.0,
  and goldens are captured from that build; the triage worklists' per-release
  regens (1.0.2, 1.0.3/1.0.4, 1.1.0, 1.2.0) are drained by it, not run; and one
  tag is cut, versioned by the last release (`v1.3.27`). The intermediate
  releases get no regen, no tag and no `RELEASES.md` row, and the catalog-only
  queue drains by ancestry of the LAST release (`git merge-base
  --is-ancestor`). This is how the earlier two-release cycles ran too (v0.86.22,
  v0.85.21), and it matches pi-sync's one-tag-per-cycle rule.

- **2026-09-29 — GOVERNING: the built-in extensions pi's SDK exports are Go SDK
  features. MCP, codemode and tool_search are IN; the port mirrors their file
  layout; `extensions/llama` stays out.** (re: `8562bcf66` "feat(coding-agent):
  codemode and MCP", riders `1d74741e1` and `4259686d9`.) Asked whether the Go
  port should offer them as SDK features or port only the underlying libraries,
  the owner chose SDK features ("make it so", on the option recommended as A)
  after noting what makes it cheap to sustain: the extensions live in the same
  upstream repo, so their changes arrive in the same drift range.

  What upstream did: codemode (`packages/codemode`, `@earendil-works/pi-codemode`),
  MCP (`packages/mcp`, `@earendil-works/pi-mcp`) and tool_search ship as
  built-in extensions — plug-ins on the extension runtime, which is E1 — but
  `createCodemodeExtension`, `createMcpExtension` and `createToolSearchExtension`
  are root-exported from `packages/coding-agent/src/index.ts`, and
  `examples/sdk/14-codemode-mcp.ts` wires them into `createAgentSession`. The
  core gets only general mechanisms: tool exposure, `prepareLoadout`,
  `ctx.executeTool` nested calls, `structuredContent`/`outputSchema`, and
  `isError` results.

  What it settles, and what triage may therefore never re-ask:
  1. `packages/mcp` and `packages/codemode` are IN — published SDK packages,
     no exclusion fires. `packages/coding-agent/src/extensions/{mcp,codemode,tool-search}/**`
     are IN as SDK features, not E1: the port has no extension runtime, so in
     Go they become opt-in session features over the same general mechanisms.
     Their host files stay E1 — `extensions/mcp/ui.ts` (the `/mcp` screen),
     `extensions/mcp/cli.ts` + `cli.lazy.ts` (`pi mcp list|login|logout`) and
     `extensions/codemode/renderer.ts` (TUI rendering). `extensions/mcp/index.ts`
     mixes both (about 20 of its 985 lines are `/mcp` command and `ctx.ui`
     code), so it splits by HUNK and is never diffstat-dispatched.
  2. **The layout mirrors upstream** (2026-09-03 mirror ruling): Go packages
     mirroring `packages/mcp` and `packages/codemode`, and the built-ins'
     logic mirrored file for file — MCP's config, oauth, runtime, tools and
     resources; codemode's tool and execute; tool-search's tool — with the
     session-option wiring a thin layer on top. Following their upstream
     changes must stay a file-to-file mapping: a reshaped Go structure turns
     every later delta into a hand translation, which is where this ledger's
     recorded misses came from.
  3. `extensions/llama/**` stays E1. Its only consumer is `extensions/index.ts`'s
     `builtInExtensions`, imported only by `main.ts`, and it is not
     root-exported (re-verified at `4259686d9`). The line the owner drew is
     root-export plus SDK wiring, not "built-in".
  4. `builtin:<name>` resolution (`4259686d9`) is operator configuration of
     the extension runtime — E1. The Go features are switched on by session
     options, so it has no Go analog.
  5. codemode's dependency is answered by the 2026-08-31 formula, not a
     CONSULT. Upstream takes `quickjs-wasi` inside its own package and loads it
     lazily; the Go equivalent is `github.com/tetratelabs/wazero` (pure Go, no
     cgo, every target) running the same QuickJS wasm, contained in a
     `codemode` submodule with its own go.mod — the settled bedrock shape — so
     the root module stays stdlib + `golang.org/x/*`. Pre-decided, as the
     Codex WebSocket module was: if the slice meets cgo, a lost target or a
     shape change, that is when it becomes a CONSULT. `packages/mcp`'s one
     dependency, `cross-spawn`, is Node's Windows spawn shim; `os/exec` covers
     it, so take nothing.
  6. Sequencing, Scope queue rows 20-24: 20 (the tool orchestration core)
     first, because MCP, codemode and tool_search all stand on it; then 21
     (MCP), 23 (tool_search), 22 (codemode). 24 (virtual models) depends on
     none of them.

- **2026-09-03 — GOVERNING: the port is a MIRROR of upstream. Harness shape (b)
  is OVERRULED in favour of (a). Breaking the port's public Go API is not a
  cost that needs weighing.** Owner, verbatim: *"our repo should be a mirror
  repo, but in go. we can in our monorepo bundle a new equivalent of chord. then
  integrate that into wherever it is used and is relevant. then cleanup all the
  things that were removed. no backwards compatibility when the upstream didn't
  maintain it themselves. mirror image, maximum parity, with go affordances per
  usual. breaking is not an issue. backwards compat is not an issue."*

  Made in response to the 2026-09-03 drift, where upstream fast-forwarded `main`
  onto its long-lived `dev` branch: 338 first-parent changes, +90,866/−24,426,
  a new published package (`@earendil-works/chord`), `protocol`/`client`/`server`
  rebuilt on service-addressed routing, and the deletion of eleven files whose Go
  counterparts this port had shipped under tags through `v0.84.20`.

  What it settles, and what triage may therefore never re-ask:
  1. **Layout mirrors upstream's packages.** The recorded shape (b) — keep
     `coding/` as the port's harness and port only the delta — is dead. It bought
     cheapness at the price of a layout that drifts from upstream's, and the
     owner has priced that trade the other way. `chord/` becomes a Go package in
     this monorepo, and the durable runtime grows beside `coding/` (upstream's
     `agent/src/harness` until `7fd478a2e` deleted it; `packages/durable` now).
     Note what mirroring actually requires here: upstream runs the durable and
     coding-agent forks side by side with zero cross-imports, so the mirror
     keeps **both** — see "Durable shape" below before assuming a merge.
  2. **Upstream deletes, we delete.** No shims, no aliases, no deprecation
     window, no keeping a symbol because a tag published it. The 2026-08-19
     revert ruling's "unless a cut tag already published it" carve-out is
     **withdrawn** — it was the last thing standing between the port and this
     ruling, and it no longer holds.
  3. **An API break is not an escalation.** Do not open a `decide` to ask
     whether breaking is acceptable, and do not propose staging one.
  4. **"Mirror" is architectural, not lexical.** The standing formula still
     governs the how: Go's idioms and upsides, not transliterated TypeScript.
     Where upstream's shape exists only to satisfy TypeScript or the Node
     runtime — Promise chains, `AbortSignal` option bags, structural types,
     `undefined`-vs-absent, module-graph tricks — the mirror does the equivalent
     thing idiomatically. A Go file that reads like translated TS is a failed
     mirror, not a faithful one.
  5. **Irrelevance in Go is not a scope question.** Owner, verbatim: *"on the
     bundling, if its irrelevant in go, don't include it. simple as."* Chord's
     `bundler.ts` and `node/bundle*.ts` bundle JavaScript facet plugins with
     esbuild; this port has no JS extension runtime, so they have no subject and
     are simply not mirrored. This is a **triage call, not a CONSULT** — where a
     construct's whole purpose is absent in Go, drop it and say so in the cycle
     entry. Reserve E2 consults for things the port genuinely needs and cannot
     reach with stdlib + `golang.org/x/*`.

- **2026-09-02 — the boy-scout rule.** Do not leave an old bug to rot because it
  predated the change you are porting: a pre-existing defect surfaced while
  porting is fixed in the same cycle, not filed as debt and deferred. **The
  boundary drawn the same day:** the rule yields where the repair means changing
  a subsystem's shape and carries its own parity surface — the parallel
  `tool_execution_end` ordering divergence (**D2**) was recorded and NOT fixed on
  exactly that ground. Fix in place when the fix is local and provable; record it
  with its cost stated when it is not. **This voids every "not folded into this
  cycle per the standing practice" deferral written before it.**

- **2026-09-02 (`23842b1e6`) — `core/http-dispatcher.ts` is host surface;
  `n/a`.** Ruled on substance, and this **replaces** the mechanism-only grounding
  of the four earlier `n/a`s on this file (`91050859`, `a93f0666`, `2117b61c`,
  `14551e769`), which rest on "Go uses `net/http`" — a statement about mechanism
  that does not reach the substance of any change. Do not inherit them.
  Structural ground: every non-test importer is host surface (`cli.ts`,
  `main.ts`, `rpc-entry.ts`, `core/settings-manager.ts`, two `modes/interactive`
  files) and the module is re-exported from neither package index; it is also
  process-global undici configuration, i.e. packaging. **Re-verify the importer
  set before reusing this — the only-consumers clause fires only while every
  consumer stays host.** Add to E1's derived list.

- **`core/auth-storage.ts` is E1 host surface, no Go analog** (`f8bec25f`, then
  `d2be68dbe`, 2026-08-02). The port takes an injected `ai.CredentialStore`
  (`ai/auth_types.go`) and deliberately does not port the host-side on-disk
  credential store, its process-shared read cache, or its coalesced in-flight
  reload. Add to E1's derived list — it is absent from the exclusion table, which
  this document declares exhaustive, so the next touch would otherwise default IN
  or open a `decide`.

- **2026-09-01 (`62835ea81`) — the ctx-passthrough seam is settled; resolve with
  the formula, do not re-escalate.** "Use `ctx.cwd` for cwd-sensitive tools" is
  `port` with an EMPTY Go delta, not `n/a`. Do not re-cite E1's only-consumers
  clause here: `wrapToolDefinition` takes its factory from host code, but its
  *caller* is `core/agent-session.ts`, which wraps both `customTools` and the
  built-in definitions — ported SDK surface — so the consumer set is mixed and
  the clause cannot fire. The verdict rests on the change being a provable no-op.
  **Two generalizations:** follow a supplier chain to its CALLER, never to the
  first factory site — the question is who decides a context is installed, not
  who installs it; and prefer a no-op grounding to a category grounding, because
  a no-op survives a boundary rewrite.

- **Two verified-and-rejected parity findings; do not re-raise.** (1) The 15s
  OAuth refresh bound belongs to `resolveStoredOAuth` only
  (`ai/auth_resolve.go:203`); the second refresh site
  (`ai/models_runtime.go:962`) is unbounded upstream too, so bounding it would
  invent a limit pi lacks. (2) `publishProviderModels` returning an *error* on
  cancel is correct: upstream returns `raceWithAbortSignal(queued, signal)`, so
  the promise the provider awaits rejects on abort and only the inner `queued`
  resolves false. Both look like bugs to a fresh reviewer and have each been
  filed once.

- **`defaultTools` tripwire.** pi's `defaultTools` setting gives an initial active
  *built-in* tool selection decoupled from the allowlist — a capability Go's
  `SessionOptions` cannot express. It is reachable only through the settings
  file, so `4d9aa837c` / `541045ae0` were `n/a`. **Tripwire: the first upstream
  commit that surfaces `defaultTools` as a `createAgentSessionOptions` field
  (published SDK surface) is `port`**, with a new `SessionOptions` field seeding
  `resolveTools`' initial built-in set without populating the allowlist.

- **2026-08-31 — GOVERNING: the owner's standing formula decides dependency
  questions; E2 escalates only for cgo, a target loss, or a shape change.** Owner,
  verbatim: *"follow upstream in terms of impl, engineering and ergonomics by
  understanding the motivations. find the go equivalent. implement."* That is a
  decision procedure, not a preference, and it supersedes the reflex to escalate.
  As a procedure:

  1. **Establish what upstream actually does, at the pin.** Not from this ledger,
     not from the consult text — from `git show <pin>:<path>`. Three of the first
     three E2 consults were written against premises that were false by the time
     they were asked: session-backends was said to need `better-sqlite3` (upstream
     had moved to builtin `node:sqlite` and declares **zero** sqlite deps), Bedrock
     was said to favour hand-rolling (true of the signer, which is a third of the
     work, and false of the credential chain, which pi does not implement at all),
     and Codex was said to need "zstd decompression" (there is none — no
     `Accept-Encoding`, no response decode; it is severable request-body
     compression). A stale premise is the normal case, not the exception.
  2. **If upstream took no dependency, take none.** The Go question is then
     "what is the Go equivalent of their builtin or their fallback", and the
     answer is usually the stdlib or simply doing without, exactly as upstream
     does on every non-Node runtime.
  3. **If upstream took one, take the Go equivalent — and reproduce their
     containment, not just their dependency.** Upstream's containment is
     expressed in whatever mechanism their ecosystem gives them (a lazy
     variable-specifier import so bundlers cannot follow; a package split so the
     core does not pull native code). Go has no bundler, so the equivalent
     mechanism is the module boundary. Contained upstream → submodule here.
     Uncontained upstream → root is fine.
  4. **Escalate only when the Go equivalent is materially worse than upstream's
     position**, which means exactly three things: it requires **cgo**, it
     **costs supported build targets**, or it is large enough to **change the
     port's shape**. Binary size alone is not an escalation — record the measured
     number and decide. Measure marginal cost against the **real `cmd/pi`**, never
     against hello-world, which links neither `net/http` nor `crypto/tls`.

  Consequence for triage: a third-party dependency question is an ordinary
  decision with a recorded ruling, like every other parity gap resolved by the
  standing formula. It is not a `decide`, and it does not wait on the owner.

- **2026-08-31 — the Codex consult is answered NO DEPENDENCY REQUIRED TO SHIP;
  zstd takes nothing, and the WebSocket module is deferred to the WS slice and
  pre-decided as `github.com/coder/websocket` in the ROOT module.**

  **The premise was wrong.** The consult said Codex "needs a WebSocket transport
  and zstd **decompression**". There is no decompression: `git grep -i
  'accept-encoding\|decompress'` over `packages/ai/src` at the pin returns
  nothing, and the response is read as `text/event-stream`. It is request-body
  **compression**, on the SSE path only; the WebSocket transport sends
  uncompressed JSON. The distinction decides the entry — a decoder is a
  correctness requirement, an encoder is an optimization.

  **zstd — take nothing.** Upstream takes no package for it: zstd is
  `process.getBuiltinModule("node:zlib")` → `zstdCompressSync`, and it is
  severable by construction — `compressRequestBodyZstd` returns `null` when
  unavailable, the caller does `sseBody = compressedBody ?? bodyJson`, and the
  `content-encoding` header is set only on success. Every browser/Vite build
  already takes that path. Upstream pays zero for zstd; Go would pay 264 KiB and
  a supply-chain entry for a bandwidth optimization on the *fallback* transport.
  Parity risk is near zero and was checked rather than assumed: `difftest/`
  captures via `onPayload`, which upstream fires on the body object **before**
  `JSON.stringify` and long before compression, so zstd is structurally invisible
  to the port's fidelity harness.

  **WebSocket — deferred, not declined.** Ship SSE first. An SSE-only adapter is
  an upstream-defined runtime state rather than a parity gap: `transport: "sse"`
  has its own `prompt_cache_key` affinity path, upstream ships a live e2e test
  for it, and an explicit `transport: "websocket"` falls back to SSE when the
  runtime lacks one — emitting `provider_transport_failure` with
  `fallbackTransport: "sse"` and the literal message "WebSocket transport is not
  available in this runtime". The Go port emits the same diagnostic and is
  faithful.

  **When the WS slice lands, the module is `github.com/coder/websocket`, in the
  root.** `golang.org/x/net/websocket` is **disqualified on capability, not
  policy** — it is already inside the allowlist, so policy was never the binding
  constraint. Its entire message API (`Codec.Receive`) has no continuation-frame
  accumulation. Measured by execution against an independent server: one
  4,000-byte message sent in 4 fragments arrived as **5 separate `Message.Receive`
  results**, and `JSON.Receive` failed with `unexpected end of JSON input`;
  `Conn.Read` does span fragments but reports no message boundary, so a JSON
  event stream cannot be delimited from it. Codex's WS transport is exactly a
  stream of discrete JSON events, and the failure is server-controlled and
  intermittent — it works until the backend fragments. `coder/websocket`
  reassembled the same message correctly, dials through the caller's
  `*http.Client` (so it inherits `ai/providers/retry.go`'s
  `Proxy: http.ProxyFromEnvironment` for free — proxy support is a documented pi
  feature, and hand-rolling would regress it), has **zero transitive requires**,
  is cgo-free, and builds on all 12 supported targets. Measured on the real
  `cmd/pi` (linux/amd64, `-s -w`): **+92 KiB, 1.009×**; zstd would have been
  +264 KiB, 1.025×.

  **Why this departs from the Bedrock submodule shape**, as that ruling requires
  a departure to be stated. Magnitude: 92 KiB against the 3.12 MiB already
  accepted for Bedrock, while the submodule's price — a two-step tag ritual every
  release plus silent rot with no CI — is fixed and recurring. Structure: Bedrock
  severs cleanly because AWS owns *both* halves, wire and credentials; Codex's
  credential half is **pi's own OAuth**, which is entry 1, root-module and
  stdlib-only. A `providers/codex` submodule would split one adapter across a
  module boundary with the root owning half of it.

  **Sequencing:** entry 10 is **blocked on entry 1**. Codex is OAuth-only —
  `options.apiKey` *is* the OAuth access token, there is no API-key path — and
  the port's `OAuthAuth`/`LazyOAuth` seams have zero implementers. The
  dependency question must not hold the entry's ~1,000 lines of business logic
  hostage; it no longer does.

  **Tells that flip the zstd half to "take klauspost" as a correctness
  requirement**, worth one grep per cycle: upstream adds an `Accept-Encoding`
  header, `compressRequestBodyZstd` loses its `return null` branch, or any
  response-side decode appears.

- **2026-08-31 — the Bedrock consult is answered TAKE `aws-sdk-go-v2`, CONFINED
  TO A SUBMODULE: `github.com/sky-valley/pi/providers/bedrock`, own tag series,
  root module untouched.** This is the first ruling to admit a non-`golang.org/x`
  module into the port at all, and it is admitted *outside* the root module.

  **Why not hand-roll.** The 2026-08-27 framing ("~500 Go lines, stdlib only")
  priced the signer and nothing else. A trimmed SigV4 plus an eventstream decoder
  is plausibly 600–900 Go lines — the SDK's own are `signer/v4` 1,197 +
  `signer/internal/v4` 528 + `eventstream` 1,353 = 3,078 — and that is the cheap
  third of the job. The expensive part is credentials, which pi does not
  implement: it contains **no credential-resolution code**, delegating entirely
  to the SDK default chain. `packages/coding-agent/docs/providers.md` advertises
  `AWS_PROFILE`, ECS task roles and IRSA as supported Bedrock credential sources,
  and `AWS_ENDPOINT_URL_BEDROCK_RUNTIME` likewise appears in zero lines of pi
  source. All of it is free only because the chain is linked in. The Go
  equivalent of that surface is `config` 7,297 + `credentials` 2,970 + `imds`
  1,752 = **12,019 LOC**, and the sharpest edge is not EKS but **SSO** — the
  `~/.aws/sso/cache` token store and OIDC refresh, i.e. the enterprise laptop
  path. Lambda alone would survive a hand-roll, because it injects static env
  keys. So hand-rolling does not buy independence; it buys a documented-feature
  regression.

  **Why the root module still must not carry it.** This follows upstream's own
  engineering rather than departing from it. Upstream *took* the AWS SDK
  (`@aws-sdk/client-bedrock-runtime` + `@smithy/node-http-handler`, declared in
  `packages/ai`), then deliberately quarantined it: `bedrock-converse-stream.lazy.ts`
  loads the implementation through a **variable specifier** so bundlers cannot
  follow the import into the Node-only SDK, with a `setBedrockProviderModule`
  override for the Bun binary build. Take the dependency; do not let it reach
  everyone. Go has no bundler, so the module boundary is the mechanism that
  expresses the same intent.

  **Verified in a three-module prototype, not argued:** root `go list -m all`
  contains **0** `github.com/aws/` modules and `go.sum` stays at **4 lines**; the
  submodule builds and carries 6 aws modules; `internal/policy/deps_test.go`
  stays **green** on the root, so this shape needs no exception to the gate
  landed the same day. Marginal binary cost, measured on the real `cmd/pi` at
  linux/amd64 `-s -w`: **10,674,338 → 13,951,138 bytes, +3.12 MiB (1.31×)** with
  the full credential chain — and only for consumers who opt in. The earlier
  "5.5× blowup" figure was measured against hello-world, which links neither
  `net/http` nor `crypto/tls`; pi already pays for both.

  **Scope note:** roughly **1,021 of the ~1,459 upstream lines is business logic**
  — message conversion, thinking payloads, GovCloud display omission, the
  byte-stable error prefixes `agent-session` string-matches, diagnostics, stop
  reasons. That work is identical under every option, so the dependency question
  governs about a third of the entry and must not be allowed to hold the other
  two thirds hostage.

  **Known risk — the submodule rots silently, because there is no CI.** Only the
  root is built each cycle, so a change to `ai`'s `ApiProvider`, `StreamOptions`
  or the assistant event-stream contract leaves root green while nothing compiles
  `providers/bedrock`. Tells: `go build ./...` inside the submodule fails at a
  pin advance, `go list -m -u github.com/sky-valley/pi` there reports the root
  requirement behind the newest root tag, or a cycle tags `v0.8x.NN` with no
  matching `providers/bedrock/v0.8x.NN`. Mitigation follows the enforcement
  correction already on record — not a `CGO_ENABLED=0` build, which is
  structurally useless, but a per-module build plus a dependency-set assertion
  (zero `^github.com/aws/` in root, non-zero only in the submodule), extending
  `internal/policy/deps_test.go` when the submodule actually exists. Absent CI it
  is a release-checklist step run at each pin advance.

- **2026-08-31 — SQLite: when the port needs it, the driver is
  `modernc.org/sqlite`, in a `github.com/sky-valley/pi/backends/sqlite`
  submodule with its own tag series, never in the root module.** Asked first
  for `packages/session-backends`, which `7fd478a2e` deleted (Scope queue row 7
  closed); it now governs the SQLite storage on row 8 — durable's
  `storage/sqlite/**`, whose drivers `c268154ca` moved into chord's `./sqlite`,
  `./sqlite/node` and `./sqlite/cloudflare` exports. Upstream still takes no
  sqlite package: the node driver is Node's builtin `node:sqlite`, and the
  cloudflare one adapts a Durable Object's `ctx.storage.sql`, which has no
  meaning in Go (dropped when row 8 lands).

  **The merits, established by EXECUTING a probe against both drivers:**
  `modernc.org/sqlite` v1.57.0 (SQLite 3.53.3) passes every exotic construct
  the upstream schema used — FTS5 external-content with `content_rowid`, the
  `trigram remove_diacritics 1` tokenizer, the `ai`/`ad`/`au` sync triggers,
  `VALUES('rebuild')`, `bm25()` + `MATCH`, conditional `UPSERT … RETURNING`,
  nested `SAVEPOINT`/`ROLLBACK TO`, `WITHOUT ROWID`, WAL and `BEGIN IMMEDIATE` —
  **with no build tags**. `mattn/go-sqlite3` needs cgo *and* a non-default
  `-tags sqlite_fts5`, failing at runtime with `no such module: fts5` without
  it. Measured cost of putting the driver in the root: non-stdlib modules
  **2 → 11**, binary **13.9 → ~21.9 MB**, and the loss of the dragonfly,
  solaris, illumos and aix targets that build clean today. Re-run the probe
  against durable's schema at the sha when row 8's storage slice opens. Land
  the in-memory backend in the same slice, so the storage seam never ships
  with zero implementers.

  **Correction to the enforcement story, worth more than the ruling itself.**
  The obvious no-cgo check does not work: adding `mattn/go-sqlite3` and
  building with `CGO_ENABLED=0` **succeeds**, because mattn ships a
  build-tagged stub that fails only at runtime ("requires cgo to work. This is
  a stub"). And "assert no package has `CgoFiles`" is blind for the same
  reason when it runs with cgo off (`mattn/go-sqlite3 v1.14.50`: `cgofiles=0`
  under `CGO_ENABLED=0`, `10` under `CGO_ENABLED=1`). The gate that works
  asserts the dependency set, and any cgo check forces `CGO_ENABLED=1`.
  **Implemented 2026-08-31** as `internal/policy/deps_test.go` — two
  independent tests (allowlist, cgo), both proven to fail against a real
  violation before landing, the cgo one against third-party *and* first-party
  `import "C"`. `env/internal/policy` follows the same shape for the env
  submodule.

- **2026-08-27 — scope is decided by EXCLUSION TESTS, not by a path list; the three
  tests are in "The scope boundary" above and they are authoritative over
  every list in this repo.** This is the structural answer to a failure the
  ledger had recorded six separate times without naming: the boundary was drawn
  on paths, and upstream refactors move files across paths. Evidence, all
  already in this file — host→SDK relocations (2026-06-23, 06-25, 07-17), a
  type's home moving into a brand-new package (08-06), an exclusion becoming a
  published npm artifact (08-07), a carve-out outliving the directory it named
  (08-25), and the 2026-08-22 sweep rewrite independently concluding that "some
  real exclusions are unexpressible as paths at all." **Twelve of sixteen scope
  rulings resolved a path INTO scope**. The other four (2026-06-12 trust,
  2026-06-24 null headers, 2026-07-30 resource-loader, 2026-08-05 harness) all
  DECLINED to bring something in; **not one ruling has ever moved surface that
  was already in scope back OUT**, and two of those four were themselves
  reversed IN later — null headers after 41 days, the harness after two. So a
  list of exclusions was a lagging indicator being maintained as though it were
  a contract. Consequence for triage: when a list and a predicate
  disagree, the predicate wins and the list is fixed in the same commit — and a
  hunk no predicate excludes is IN scope by default. This ruling does not move
  any single boundary by itself; it changes what a boundary IS.

- **2026-08-27 — the host seam replaces the TUI / modes / extensions / cli /
  settings / packaging entries as a single structural exclusion, and a commit
  spanning it never escalates on account of its host half.** These were seven
  separate list entries describing one thing: pi's host application. Stated as a
  seam it is self-maintaining — a new host file is host surface on the day it
  appears, with nothing to add to a list. **This is a restatement, not a
  boundary change**: nothing moves in or out, and the TUI's exclusion is
  reaffirmed on its merits (16,966 LOC of implementation, 34,144 with
  upstream's own tests; 101 commits touching it in 90 days yet
  the sole cause of only 4 mixed commits; zero defects; and no oracle — a
  request body is a value you can diff, an escape-byte stream interpreted by a
  terminal is not). `modes/**` is a further 20,291 LOC and 56 sole-cause mixed
  commits; the pair costs ~95 port-cycles to take on. The cost it removes is
  per-commit hunk-reading that ended in `n/a` ~65% of the time.

- **2026-08-27 — `go.mod` decides adapter scope. Azure, Mistral, Vertex, Radius
  (+ `radius-config`) and the whole OAuth token-acquisition tree are IN scope
  and queued. Bedrock and Codex are OUT for as long as the go.mod policy holds.** This **supersedes the 2026-07-14 ruling** insofar as that ruling
  put the Radius provider and its host wiring out: `providers/radius-config.ts`
  is reachable through pi-ai's `"./providers/*"` exports wildcard, i.e.
  published SDK surface, which is the exact fact that ruled the Cloudflare
  binding IN on 2026-08-11 — and the 2026-08-22 tripwire had already recorded
  that "daylight in the ruling text," noting it was spared a `decide` only
  because `packages/ai` had no delta that range. It is closed now rather than
  when it fires. The OAuth tree is the highest-value item and is not really "an
  excluded provider" at all: `OAuthAuth.Refresh`/`ToAuth` and `LazyOAuth` are
  **already-ported seams with zero implementers**, so Anthropic Pro/Max,
  Copilot, OpenRouter, Kimi and xAI subscriptions are unreachable from Go
  today — and closing that needs no dependency the port does not already have.
  The host-side *env-override population* machinery (2026-06-17) and the Radius
  HOST wiring (`core/model-registry.ts`, `core/resolve-config-value.ts`,
  `core/radius.ts`) stay OUT under **E1's only-consumers clause**, not under the
  reachability default, and are named in E1's derived list and in the out-table.
  `core/model-resolver.ts` is the exception and must NOT be dispatched from a
  diffstat: it is partly ported (`defaultModelPerProvider` → `coding/resolve.go`)
  and sits in the split-by-HUNK table instead. The queue entry covers the
  `packages/ai` half only.

- **2026-08-27 — image generation is IN scope and queued.** It was a
  founding-day list entry with **no recorded reasoning**, which is precisely why
  it kept regenerating the question: a triager had nothing to apply.
  `packages/ai/src/index.ts` carries `export * from "./images-models.ts"`, so
  `ImagesModels` / `createImagesModels` / `createImagesProvider` sit on the
  **root** export of `@earendil-works/pi-ai` — strictly stronger reachability
  than the `./api/*` wildcard that decided 2026-08-11. Under predicate 2 it was
  never out. `image-models.generated.ts` joins the catalog-regen surface;
  `scripts/generate-image-models.ts` becomes `port-but-CATALOG-ONLY` like its
  sibling.

- **2026-08-27 — project trust: a gate is in scope exactly when the port has the
  thing it gates, and any new project-local read must ship WITH its gate.**
  This **supersedes the 2026-06-12 ruling in part**. That ruling rested on three
  criteria, the third being "verified not to change behavior of ported surface";
  that criterion stopped holding — not through an upstream commit but because
  **the port grew the consumer**. `coding/session.go` called `LoadSkills(cwd)`
  unconditionally, and `coding/resources.go` scanned `<cwd>/.pi/skills`, a
  directory upstream reaches only inside `if (projectTrusted)`
  (`package-manager.ts:2417`) and answers *untrusted* for a UI-less host
  (`project-trust.ts`). The port answered *trusted*: a parity inversion on a
  security default, in public MIT code, letting a hostile repository author part
  of the system prompt. Fixed in `873e35a` with the gate defaulting untrusted
  and `SessionOptions.TrustProject` as the opt-in.
  **The load-bearing half of this ruling is the corollary**, and it is a rule
  that would have caught the regression on the day it shipped: *whenever the
  port adds any new project-local read — anything under `<cwd>/.pi/**` or an
  ancestor `.agents/**` — that same change must carry the gate. A new reader
  shipping ungated is a defect, not a scope question.* The trust *prompt*,
  selector and store stay out under E1 (asking the user is host surface); the decision and the gate are in. Ancestor `<project>/.agents/skills`
  discovery remains unimplemented (2026-08-18) and `TrustProject` does not
  enable it — when it is implemented, it ships gated.

- **2026-08-27 (evening) — if it is part of the pi SDK, the port supports it; a
  third-party dependency is a CONSULT, not a reason to say no.** Owner ruling,
  overturning the `go.mod`-decides form of E2 written earlier the same day.
  That rule made "would this add a `require` line?" an automatic exclusion and
  put `amazon-bedrock` and `openai-codex` permanently out — which was **the port
  drawing its own boundary instead of following pi's**, the exact failure this
  rewrite exists to stop. A dependency question is a real question, but it is
  the owner's to answer per case, not a standing no.
  **Effects:** the boundary now has TWO exclusion tests (E1, E3), not three.
  Bedrock and codex return to scope as queue entries with open consults, each
  stated concretely enough to answer — bedrock may need no dependency at all
  (SigV4 and `vnd.amazon.eventstream` are both hand-rollable against stdlib),
  codex realistically needs `klauspost/compress` for zstd. The session-backends
  sqlite question (entry 7) becomes the same kind of consult rather than a
  precondition for the entry surviving. The stdlib-plus-`golang.org/x/*`
  preference stays as a stated preference and a thing to weigh, not a gate.

- **2026-08-27 (evening) — the durable runtime is FUNDED (ruled as "harness is
  in").** Scope queue entry 8 is an active drain, not a parked item, and triage
  carries its backlog as ordinary queued deltas; its shape is the 2026-09-03
  mirror. **The subject moved twice, the funding did not:** the Pico kernel
  left `packages/agent/src` for the new `packages/durable` package
  (`080160162`, 2026-09-18), and `7fd478a2e` (2026-10-01) deleted
  `packages/agent/src/harness/**`, `search/**`, the harness subpath exports and
  `packages/session-backends`, leaving pi-agent-core as `agent.ts`,
  `agent-loop.ts`, `proxy.ts`, `stream-fn.ts` and `types.ts`. **Applied
  2026-10-19:** the Go mirror of the deleted trees is gone (`0e4f9e1`: the
  harness-only compaction `retainedTail`), and `coding/execenv.go` is NOT
  deleted, because its upstream source moved rather than died — it is
  `packages/durable/src/env/{index,node}.ts` (published `./env`, `./env/node`),
  re-pointed in `8adf788`. Entry 8 is now `packages/durable`.

- **[AMENDED 2026-08-27] 2026-08-25 — the `pi-triage` skill's `port` definition is the single list of
  in-scope trees, and this ledger is authoritative over it.** Closing an item
  carried OPEN since 2026-08-22. The skill's verdict-assigning text named three
  trees while these Rulings had put five more in scope (protocol, client, server,
  telemetry, session-backends), and a second, correct map sat further down the
  same file — so the skill held both the wrong answer and the right one. It never
  bit only because those trees happened not to change. **Not a boundary change:**
  no tree was added or removed, the definition was made to match rulings already
  given. The duplicate map is gone; there is now ONE table, in the verdict rules.
  If the table and this file ever disagree again, this file wins and the table
  gets fixed.


  > **AMENDED 2026-08-27.** The mechanism this ruling installed — one
  > authoritative LIST — is superseded by the three exclusion tests in "The
  > scope boundary". Its *substance* stands: this ledger is authoritative over
  > the skill, and there must never be two copies of anything. Read "single
  > list" as "single source of truth", which is now the tests.
- **2026-08-19 — an upstream REVERT is ported like any other change, even when
  it removes exported Go API the port shipped days earlier.** (re: `3a0b9a3ee`
  reverting `2509b5c03`, which the port had landed the previous day as
  `0f0b461` + `b548506`.) Triage must not treat "we just built this" as a
  reason to keep it, and must not escalate the API removal as a `decide`.
  **The deciding fact is that the port's public surface is downstream of pi's,
  not a commitment of its own.** `BuildProviderContext`,
  `Agent.BuildProviderContext` and `ContextPipeline` existed for exactly one
  reason — to mirror `2509b5c03` — so once upstream withdrew it, keeping them
  would have *created* a divergence rather than avoided one. The pi-sync hard
  rule "anything that would change the public Go API → escalate" aims at the
  port diverging from pi under its own steam; following pi back is the rule's
  purpose, not its exception.
  **Bounded by the release line.** This was safe to do silently because the API
  was never released: it landed after `v0.84.18` and this cycle crossed no
  release tag, so no consumer could have depended on it. **If a future revert
  targets surface that a cut tag already published, that IS a `decide`** — the
  question stops being "what does pi do" and becomes "what did we promise" —
  and the escalation should offer keeping the Go symbol as a deprecated
  no-op-forwarding shim until the next minor.
  **Verify the un-port is exactly the revert, no more.** The 2026-08-19 cycle
  proved it by byte-comparing `agent/loop.go` and `agent/agent.go` against
  `b79c9e6` (the pre-port parent) — identical — and by diffing `go doc -all`
  across the change to confirm the exported delta was exactly the three
  identifiers upstream removed, with no remaining callers anywhere including
  `examples/` and `cmd/`. Do that comparison every time; a revert is the one
  shape of port where "restore the old file" is checkable exactly.
  **Test coverage does not revert with the code.** The un-port deleted a test
  that pinned four properties; the replacement initially covered two, and the
  gap was invisible to the suite. When a revert removes a test, re-establish
  what it pinned through whatever path survives before shipping.

- **[AMENDED 2026-08-27] 2026-08-18 — `.agents/skills` discovery: the USER directory is IN scope
  (port it, as backlog); the project-ancestor directories stay `n/a` under the
  trust ruling.** (re: `5e11f6586` "load nested markdown skills", which only
  widens `collectSkillEntries` for mode `"agents"`.) Deliberately NOT escalated
  as a `decide`: the owner's **standing formula** (2026-08-11) already answers
  it, and re-asking would be re-litigating. Full pi SDK functionality as
  represented in Go; deciding fact = **published, independently reachable
  surface**.
  **The user half is in scope.** `collectAutoSkillEntries(join(homedir(),
  ".agents","skills"), "agents")` is unconditional — no gate, no runtime
  dependency, plain filesystem enumeration — and it feeds the same `loadSkills`
  whose Go counterpart (`coding/resources.go` `loadSkillsFromDir`) is already
  ported and already feeds the ported system prompt. That makes this a **parity
  gap inside a ported function**, not a boundary question.
  **The project half is not.** `collectAncestorAgentsSkillDirs(cwd)` is gated on
  `settingsManager.isProjectTrusted()`, and project trust is on the non-port
  list (2026-06-12) — the same fact that decided the resource-loader accessors
  (2026-07-30).
  **Recorded as a pre-existing gap, not this cycle's work.** Agents-mode
  discovery predates the port: it landed upstream in `39cbf47e4` (2026-02-20),
  before the port started (2026-06-08). So `5e11f6586` itself is `n/a` — a no-op
  for the `"pi"` mode the port implements — and the user-dir port is backlog.
  When it lands, implement HEAD semantics directly: in mode `"agents"` a NESTED
  `.md` counts and a ROOT-level `.md` does not (the mirror image of `"pi"` mode),
  `SKILL.md` still stops recursion, and the agents directory carries its own
  baseDir (`~/.agents`) for relative-path resolution.
  **Landed the same day in `d7f4daf`**, implementing HEAD semantics directly:
  `~/.agents/skills` is scanned last (upstream's own position among its four
  discovery calls), in agents mode, and since names dedup first-wins it can only
  ADD names no pi directory already resolved. Three mutations verified red.
  Triage from here: `package-manager.ts` skill-discovery commits are judged
  against BOTH modes now; the ancestor `<project>/.agents/skills` dirs stay `n/a`
  while project trust is unported.


  > **AMENDED 2026-08-27 — the trust legs below are DEAD; do not cite them.**
  > Every sentence in this ruling that grounds the project-ancestor `n/a` on
  > "project trust is on the non-port list" or "while project trust is
  > unported" is false since `873e35a`: the gate IS ported
  > (`SessionOptions.TrustProject`, `LoadSkillsWithTrust`).
  > **Corrected disposition:** ancestor `<project>/.agents/skills` discovery is
  > IN scope and has no Go home, which under the rewritten boundary is a
  > **Scope queue** matter, not an `n/a` — it is folded into queue entry 3's
  > neighbourhood as a one-file item and ships GATED when built, per the
  > 2026-08-27 project-trust corollary. Marking it `n/a` would re-create exactly
  > the "in scope, no home, called n/a" pattern the queue exists to end.
- **2026-08-11 — a transport is not out of scope for being runtime-specific.**
  (re: `230029078`, the Cloudflare AI-binding gateway transport, #7901.) The
  surviving rule: **triage must not re-escalate a transport merely for depending
  on a runtime Go does not target — that axis is settled.** A genuinely new
  question would be a transport that changes the `ai.HTTPDoer` seam itself. The
  deciding fact was published, independently reachable surface (pi-ai's
  `exports` map carries a `"./api/*"` wildcard), and latency-pending-a-runtime
  was judged not to matter.

  **Its subject was replaced, and the port followed on 2026-10-19 (`602450c`).**
  Upstream deleted `packages/ai/src/api/cloudflare-gateway-binding.ts`
  (`55adba4f2`) for `api/cloudflare-ai-binding.ts`, a plain binding fetch
  (`AiBinding`, `createAiBindingFetch`, the auth sentinel
  `"cloudflare-gateway-binding"`). The translating shim
  `ai/providers/cloudflare_gateway_binding.go` is deleted and the binding fetch
  is ported as `ai/providers/cloudflare_ai_binding.go` (`AIBinding`,
  `CreateAIBindingFetch`, `CloudflareGatewayBindingAuthSentinel`) — latent, as
  pi's is outside a Worker.

- **2026-08-07 — a package published to npm is SDK surface, whatever its
  maturity.** (re: `6fb2d766a`, the Harness factory.) The deciding fact was the
  npm registry, not the source tree: `@earendil-works/pi-session-backend-sqlite-node`
  0.84.0 and 0.84.1 were the first releases to ship a harness-v2 component as an
  independently installable artifact, and `pi-agent-core@0.84.1` exported the
  harness API, so both trees were reopened IN. Both are deleted upstream since
  `7fd478a2e`; what survives is the rule, and its corollary: port against the
  published npm surface, which carries the stability claim, rather than
  head-of-churn.

- **2026-08-06 — the telemetry package is IN scope** (re: `04d6447f7` +
  `6b461b75b`, `@earendil-works/pi-telemetry`). Owner call (noam): **port the
  telemetry as well.** Scope as ported, widened 2026-10-19 by `48909d987` and
  `2ec55d808` (`061629c`, `7fedbfa`, `32eca5d`): the top-level Go `telemetry`
  package — `Context`/`Span` (callback-scoped `StartSpan`;
  `AddEvent`/`SetAttributes`/`SetStatus`/`AddLink`), `SpanOptions` with links,
  the `Metrics` and `Propagation` capabilities with their no-op forms,
  `NoopContext` (a span, as pi's `NOOP_TELEMETRY_CONTEXT` is), the in-memory
  recorder, the schema DATA (`telemetry/schema.go`, `NewTypedMetrics`), and
  pi's adapter conformance suite (`src/testing/**` → `telemetry/telemetrytest`,
  `348aa86`); chord's context member (`chord.Telemetry`, `WithTelemetry`, `TraceIn`); and
  `ai.ProviderRequestOptions.Telemetry telemetry.Span` (renamed from
  `TelemetryContext`), under which `Models` emits pi's `pi.ai.request` /
  `pi.ai.poll` spans, events and metrics (`ai.TelemetrySchema`). **Excluded:**
  only the schema's TypeScript type inference (E3). The 2026-08-06 text
  excluded the whole schema half as "compile-time machinery whose only consumer
  is the unported agent harness"; that stopped holding when pi-ai began reading
  `AI_TELEMETRY_SCHEMA` at runtime. Going forward: commits to the runtime
  contract or the schema data are `port`; commits to the inference typing are
  `n/a` (E3); durable's span catalog (`DURABLE_TELEMETRY_SCHEMA`) is row 8.
  One Go-only contract rides it: every `Context`, `Span`, `Metrics`,
  instrument and `Propagation` must be safe for concurrent use (documented on
  each; `telemetrytest`'s `concurrency` group checks it under `-race`).

- **2026-08-04 — null-`ProviderHeaders` suppression is now IN scope; port it**
  (re: `a24fb9e96` "preserve auth header deletion markers", #7539). Owner call
  (noam): **port it.** This closes the 2026-06-24 deferral on its own terms —
  that ruling declined `Record<string,string|null>` null-suppression explicitly
  *"Revisit only if a consumer needs to suppress a default header"*, and this
  commit is that consumer arriving: upstream's host stops stripping nulls and
  passes `ProviderHeaders` through intact, with a cloudflare-compat test. The
  hunk itself is in `coding-agent/src/core/model-registry.ts` (host), so it is
  not `port` by path — it is `port` because it fires the named trigger. The
  resulting public Go API change (`StreamOptions.Headers` / `Model.Headers` can
  no longer be a plain `map[string]string`) is sanctioned under the 2026-07-17
  clause that upstream-driven public Go API breaks are allowed. **Supersedes
  item 1 of the 2026-06-24 divergence list** — Go's conditional-skip workaround
  for cloudflare-ai-gateway's `Authorization` suppression should collapse into
  the real null-marker mechanism rather than sitting beside it. Future commits
  to null-header plumbing in `packages/ai/src` are `port`.

- **2026-08-04 — upstream `DRAFT:` commits are ported like any other change
  (INTERIM)** (re: `382aa641c` "DRAFT: add openai background mode responses",
  the deferred-response capability class: `DeferredHandle`, new `StopReason`
  value `"deferred"`, `fetchDeferred`/`cancelDeferred` on
  `Provider`/`Models`/`ProviderStreams`, `SimpleStreamOptions.deferred`,
  `lazyApi` capability flags, `providers/faux.ts`). Owner call (noam): **port
  it for now**, pending an answer to the open question below. Rationale: on
  *scope* the 2026-08-01 ruling already answers this — it is `packages/ai/src`
  SDK surface, i.e. exactly the "full functionality of the pi SDK as
  represented in Go" class. The only thing the DRAFT label raised was
  *stability*, which is a different axis and is not on its own a reason to
  hold. Triage must not re-escalate a change **solely** because its subject
  says `DRAFT:` — judge it on scope like anything else.
  **OPEN QUESTION (unresolved — when answered, replace this ruling): when is a
  draft no longer a draft?** Upstream's `DRAFT:` is a free-text subject prefix
  with no lifecycle behind it: the commit is already on `main` (not a branch or
  a PR left open), carries no tag, and nothing marks the transition when it
  stabilizes — a later commit may simply edit the surface without ever removing
  the word. So there is no upstream signal to wait *for*, which is part of why
  holding was the weaker option. Candidate signals if we want a rule later:
  first release tag that ships the surface, or the surface surviving N sync
  cycles unmoved.
  **Standing caution while this is interim**: the new `StopReason` value
  `"deferred"` enters the **session/message format** (a byte-golden surface).
  Port it, but treat any golden movement it causes as requiring an explicit
  parity note rather than a silent regen, and expect churn — this surface is
  not frozen upstream.

- **[PARTLY SUPERSEDED 2026-08-27] 2026-07-17 — the model-runtime facade is ported SDK-scoped** (re:
  `ff28097a` "merge model runtime facade" + rider `bd9e09db` "expose dynamic
  provider refresh"). Owner call (noam): maximum fidelity to source via
  maximum Go idioms, and the port supports **only the SDK and everything it
  depends on**. Judgement: the facade's `packages/ai/src` deltas are **port**
  — Provider `refreshModels(context)`/`filterModels`, `ModelsStore` (+
  `ModelsStoreEntry{models, checkedAt}`), `Models`
  `refresh(options)→{aborted,errors}` / `checkAuth` / `getAvailable` /
  `login`/`logout` / overloaded `getAuth(+overrides)` / stream transforms,
  the auth-type overhaul (`AuthInteraction` rename, provider-scoped
  `ApiKeyAuth.resolve`, optional `check`, `CredentialStore.list`,
  `Credential.availableModelIds` typed), github-copilot `filterModels`,
  `radius`→`RADIUS_API_KEY`. Upstream-driven public Go API breaks are
  sanctioned under this ruling (pre-1.0, upstream broke the same APIs).
  **Not ported**: the OAuth reorg (`utils/oauth`→`auth/oauth` — acquisition),
  `cli.ts`, extension-oauth compat types, the host `coding-agent/src/core`
  restructuring (`model-runtime`/`model-config`/`provider-composer`/
  `models-store`/`remote-catalog-provider` + the
  `model-registry` collapse and `ModelRegistry`→`ModelRuntime` renames), the
  radius provider (2026-07-14 ruling stands), and `bun-oauth.ts`. G3 stands
  (`ai.Stream`'s doc: a setup failure is an error in the stream, where pi
  throws); the compat-routing and inline-Cloudflare divergences this ruling
  also kept were ported away 2026-10-19 (see 2026-06-24). Future commits to the facade surface in
  `packages/ai/src` are `port`; commits only to the host runtime files above
  are `n/a`.


  > **PARTLY SUPERSEDED 2026-08-27.** Its "Not ported" list has moved: the
  > **OAuth reorg / acquisition tree is now IN scope and queued** (entry 1),
  > and `radius`/`radius-config` with it (E2). `cli.ts` and the host
  > `coding-agent/src/core` restructuring (`model-runtime`, `model-config`,
  > `provider-composer`, `models-store`,
  > `remote-catalog-provider`) stay out — `cli.ts` under E1 directly, the `core`
  > cluster under E1's only-consumers clause where every consumer is host.
  >
  > **RE-JUDGED 2026-09-24 (standing formula — the test wins and the list follows).**
  > At `a328aa89a` the only-consumers premise is false for two of those files.
  > `core/model-runtime.ts` is imported by `core/sdk.ts` (`createAgentSession`
  > defaults `ModelRuntime.create` and streams through `modelRuntime.streamSimple`),
  > `core/agent-session.ts`, `core/cache-warmer.ts` (entry 17) and
  > `experimental/**` (entry 11), so it is split by HUNK: its `Models`-contract
  > half is `ai.Models` (the chat guard shipped 2026-09-24), its composition half
  > — auth.json, the extension registry, radius wiring — stays E1.
  > `core/remote-catalog-provider.ts` inherits the mixed consumer set and is now
  > Scope queue row 19. `core/provider-composer.ts` and `core/model-config.ts`
  > stay OUT on E1's operator-configuration ground, not the only-consumers one:
  > they layer models.json and extension registrations, and `ModelRuntime`
  > bypasses the composer entirely when neither layer exists
  > (`model-runtime.ts:276-280`). `core/runtime-credentials.ts` was re-examined
  > 2026-10-20: PORTED as `coding.RuntimeCredentials` (`059d9c6`). It is pi's
  > only way to hand an SDK session a key (`ModelRuntime.setRuntimeApiKey`;
  > `createAgentSession` takes none), so it is SDK surface whatever its one
  > importer is; it is exported because its owner, `ModelRuntime`, is not ported
  > (D109). `models-store.ts` was not re-examined.
- **[PARTLY SUPERSEDED 2026-08-27] 2026-07-14 — port the `pi-messages` provider API; leave Radius OAuth + host
  wiring out** (re: `961fa6c1` "feat(ai): add Radius gateway support"). The
  commit adds a new first-class provider API `pi-messages` — a generic
  POST-`{model,context,options}` + SSE wire protocol — plus a `radius` provider
  that authenticates via OAuth with a dynamic (non-catalog) model list. Owner
  call (noam): stay loyal to canon + lean into Go idioms, and we currently ship
  **only the SDK**. Judgement: **port the `pi-messages` API** (`ai/providers/pi_messages.go`,
  new `Api` const `APIPiMessages`, `radius`→`PI_GATEWAY_API_KEY` env-key —
  renamed upstream to `RADIUS_API_KEY` in `ff28097a`, ported in the 07-17
  cycle — `RegisterPiMessages`) — it lives in `packages/ai/src/api/` (the ported layer,
  not an excluded provider), is a **generic protocol usable by any custom
  provider** via `"api":"pi-messages"`, and authenticates by plain API key
  (`PI_GATEWAY_API_KEY` through the already-ported env-key path) with **no OAuth
  dependency in the streaming path**. **Not ported** (stay on the existing
  boundary): `packages/ai/src/utils/oauth/radius.ts` (OAuth token acquisition),
  `coding-agent/src/core/radius.ts`, the `model-registry.ts` `oauth:"radius"`
  wiring, `model-resolver.ts` (`radius:"auto"`), `provider-display-names.ts` —
  all host-side / OAuth-acquisition. `radius` is a dynamic OAuth-only provider
  with **no static catalog entry**, so no catalog model is added; the built-in
  `radius` provider stays latent until a host wires OAuth. Future commits to
  `packages/ai/src/api/pi-messages.ts` are `port`; commits only to the Radius
  OAuth/host machinery are `n/a` under this ruling.


  > **PARTLY SUPERSEDED 2026-08-27.** The Radius *provider* + `radius-config`
  > are IN scope and queued (E2). The *host wiring* stays out under E1's
  > only-consumers clause. See the 2026-08-27 go.mod ruling.
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
  unported — see **K15**. **[CORRECTED 2026-09-03: the reason originally given
  here, "upstream keeps it in `agent-session.ts`", is false. Upstream keeps it in
  `packages/ai/src/utils/overflow.ts` and re-exports it from
  `packages/ai/src/index.ts:43` — published SDK surface. The correct ground is a
  recorded absence pending a consumer, not a scope exclusion.]**
  Future commits to `packages/ai/src/utils/retry.ts` (new patterns, etc.) are
  `port` under this ruling. This makes `371adcf3` a `port`, no longer a `decide`.

- **2026-06-24 — models-runtime migration completed under the "globals stay as
  compat" divergence** (re: the `129eb460` "complete models runtime migration"
  consolidation). **[The three pieces it left untransliterated are all ported
  now — (1) 2026-08-04, (2) and (3) 2026-10-19; only its premise stands.]** A
  coding session streams through `SessionOptions.Models` — pi's `modelRuntime` —
  when the host gives it one, and through the **compat globals**
  (`ai.StreamSimple` → `withEnvAPIKey` → the built-in provider) when it does not
  (D109; `4a4de98`). No session carries an API key of its own; `cmd/pi`'s
  session has no `Models` and `requireProviderAuth` only gates, so compat
  resolves auth per request. The three pieces:
  1. **`ProviderHeaders` null-suppression** (`Record<string,string|null>`) —
     **[SUPERSEDED 2026-08-04: this is PORTED. See the 2026-08-04 ruling above;
     the public-API-break objection below is additionally void under the
     2026-09-03 mirror ruling. Retained only because D15 cites the original
     reasoning by analogy.]** NOT
     ported. `Headers` stays `map[string]string`. Porting it would change the
     public Go API (`StreamOptions.Headers`/`Model.Headers`) for a **latent**
     capability: zero 0.80.2 catalog models set a null header, and pi's only
     real null use (cloudflare-ai-gateway suppressing `Authorization`) is
     already handled in Go by a conditional skip, not a null marker. Revisit
     only if a consumer needs to suppress a default header.
  2. **Cloudflare base-URL placeholders + gateway auth** — PORTED 2026-10-19
     (`828dd7a`, `ccefe2b`; upstream `129eb460c`, `49fbe6834`, `bdd5c53bc`,
     `9993c9690`, `fed6009cc`, `a7d17e39a`) where pi has them:
     `ai/cloudflare_auth.go` (`cloudflareWorkersAIAuth`/`cloudflareAIGatewayAuth`:
     key, account id and, for the gateway, gateway id all required, each read
     from the credential and then the env; the ids handed on as resolution env;
     the gateway key as header-owned auth with `Authorization`/`x-api-key`
     markers; pi's login prompts) and `ai/cloudflare_stream.go`
     (`cloudflareStreams`: placeholders filled from the request env alone, never
     failing). The adapters carry no Cloudflare code (`ai/providers/cloudflare.go`
     deleted). The inline version this item kept was not byte-identical, as it
     claimed: with an explicit key or a `cf-aig-authorization` header, 65 compat
     rows of `ai/providers/testdata/cloudflare-auth` differed (pi sends the key
     as `Authorization`/`x-api-key` to a URL whose unfilled placeholders stay
     literal, `%7BCLOUDFLARE_ACCOUNT_ID%7D`; the port filled them from the process
     env and sent `cf-aig-authorization`).
  3. **compat `shouldUseBuiltinModels` routing** — PORTED 2026-10-19 (`6e11500`,
     `c5dbe91`). compat `Stream`/`StreamSimple` send a built-in model through its
     built-in provider (`builtinProviderFor` = `getBuiltinProviderForModel`: the
     registry must still hold the registration `RegisterBuiltInApiProviders`
     recorded for the api) with `withEnvAPIKey`, and a Cloudflare model without
     a key or `cf-aig-authorization` of its own through `compatModels`
     (`BuiltinModels`), where it is observed under `opts.Telemetry` and must be
     a chat model, as in pi.
  In scope and ported that cycle (all faithful): `ef231c49` (request-scoped auth
  overrides — the named `auth/resolve.ts` boundary edge), `49fbe683`
  (`api-key`→`api_key`, credential `metadata`→`env`), `cd95c274` (OpenAI
  Responses terminal-event requirement + compaction zero-usage guard),
  `6184307c` (anthropic compat now from catalog — byte-safe; the 0.80.2 catalog
  carries the fields the removed auto-detect synthesized), `129eb460`'s
  `getClientApiKey` "unused" sentinel + vercel routing ungate (byte-safe for the
  catalog). The catalog-data reorg (per-provider `*.models.ts` + huggingface
  registration provider) landed via the 0.80.2 regen. Future commits to the
  null-`ProviderHeaders` plumbing, compat routing or Cloudflare auth in
  `packages/ai/src` are `port`.

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

- **[RE-DERIVED 2026-08-27] 2026-06-17 — provider-scoped env overrides ported faithfully** (re:
  `7f29e7a3`). Owner call: maximum parity. `StreamOptions.Env`
  (`map[string]string`) is consulted ahead of `os.Getenv` (helper
  `getProviderEnvValue`: non-empty scoped value wins, empty falls through —
  pi's `||`) for the consumer pi touches: `PI_CACHE_RETENTION`, across
  anthropic/openai-completions/openai-responses. (Cloudflare's base-URL
  placeholders were the second until `9993c9690`; pi, and since 2026-10-19 the
  port, fills them in `cloudflareStreams` from the request env alone — no
  process env, and `??`, so an empty value fills.) pi's `getBunSandboxEnvValue` `/proc/self/environ` fallback
  is DELIBERATELY OMITTED — it works around oven-sh/bun#27802 (Bun compiled
  binaries expose an empty `process.env` in Linux sandboxes), a runtime defect
  Go does not have. The host-side population machinery stays unported (field
  latent, matching pi SDK consumers that don't populate it). Future commits to
  the env-override *plumbing in ported providers* are `port`; commits only to
  the unported host-side population are `n/a`.


  > **RE-DERIVED 2026-08-27.** Verdict unchanged; the host-side population
  > machinery stays out under E1's only-consumers clause, and those files are
  > now named explicitly in E1's derived list rather than left to prose.
- **2026-06-16 — provider-attribution ported faithfully** (port-it ruling); SDK
  sends pi's default attribution headers (http-referer/x-title/...) on the
  providers pi does.

- **[PARTLY SUPERSEDED 2026-08-27] 2026-06-12 — project trust stays excluded** (re: `718215bd`, `d8aef0fe`,
  and the wider upstream trust push). Criteria set by the owner: not an SDK
  use case (host apps control what loads), postponable (purely additive
  subsystem), and verified not to change behavior of ported surface (the only
  ported-adjacent diff was a behavior-neutral refactor inside the unported
  extension resource-loader; `skills.ts` untouched). Future trust commits are
  `n/a` under this ruling UNLESS they change behavior of surface we ported —
  that re-escalates.


  > **PARTLY SUPERSEDED 2026-08-27.** The trust DECISION and GATE are ported
  > (`873e35a`); only the prompt/selector/store stay out under E1. This
  > ruling's third criterion — "verified not to change behavior of ported
  > surface" — stopped holding when the port grew the consumer.
- **2026-07-30 — catalog data lives only in the npm build, never in upstream
  git.** The per-provider model data (`dist/providers/data/*.json`) is generated
  at publish time by `scripts/generate-models.ts`; it is **not committed**
  upstream. A `git diff <pin>..origin/main` sweep therefore can NEVER detect a
  catalog change, and `packages/ai/src/models.generated.ts` is a small
  aggregator whose own bytes rarely move. This cycle's triage initially and
  wrongly concluded "no regen needed" from exactly that git sweep, and a
  file-level `cmp` of the two builds' `dist/models.generated.js` **confirmed the
  error** — that file is byte-identical (4,373 B) across 0.82.0 and 0.83.0 while
  the real data differs by 20,653 B. **Rule: whenever a release tag is crossed,
  decide the regen by executing `JSON.stringify(MODELS)` against both npm
  builds and comparing — never by reading git, and never by `cmp`-ing
  `models.generated.js`.** A generator-only (`scripts/generate-models.ts`) hunk
  in the delta is a positive signal that the next release WILL move the catalog.

- **[RE-DERIVED 2026-08-27] 2026-07-30 — `core/resource-loader.ts` source-info additions stay `n/a`**
  (re: `bff5ab71`, extending the `66eead65` precedent). The port has no
  `ResourceLoader` object, no `SYSTEM.md`/`APPEND_SYSTEM.md` discovery, and no
  path-or-inline prompt source — `BuildSystemPromptOptions.AppendSystemPrompt`
  is a plain literal string. The new `getSystemPromptSource()` /
  `getAppendSystemPromptSources()` accessors have exactly one upstream consumer
  (`modes/interactive/interactive-mode.ts`, unported TUI), and their backing
  discovery is gated on `isProjectTrusted()`, which is itself on the non-port
  list (2026-06-12). Porting them would mean inventing an entire exported
  subsystem to serve no ported consumer. Future `resource-loader.ts` commits are
  `n/a` UNLESS the port grows a resource-loader analog.


  > **RE-DERIVED 2026-08-27.** Verdict unchanged, ground restated: it is E1's
  > only-consumers clause (sole upstream consumer is `modes/interactive`). The
  > second leg below — "gated on `isProjectTrusted()`, itself on the non-port
  > list" — is DEAD: the trust gate is ported as of `873e35a`. Do not cite it.
## Scope queue (IN scope, not yet built — opened 2026-08-27)

The 2026-08-27 rewrite moved surface into scope faster than it can be ported.
Those items live here, and this queue is why that is **not** the
"IN-but-deferred" trap the harness fell into: each entry has an order, a size
measured at `ccfe79ed2`, and a place to accumulate deltas, and the queue is
reported in every cycle's ledger entry like the catalog-only queue is.

**How triage handles a queued tree.** An upstream change touching one is
`port-but-QUEUED`: verdict `port`, no Go commit this cycle, delta appended to
the entry below. That is the `port-but-CATALOG-ONLY` pattern generalized — these
trees have **no Go base yet**, so a hunk-sized port of a file that does not
exist is not a thing that can land. Once an entry's base port ships, its queued
deltas land with it, the entry closes, and that tree triages as ordinary `port`.

**How to OPEN an entry — this is the part the harness never had.** If a change
is IN scope (no exclusion test fires) and the port has no Go home for it, do
**not** escalate and do **not** mark it `n/a`. Open a new row here: name the
tree, size it at the pin, and put it at the bottom. Opening a row is a normal
triage action requiring no owner decision, because the scope question was
already answered by the tests — only the scheduling is open. Say so in the
cycle's ledger entry so the row is visible the day it appears.

Drain order is value-first, not size-first. Sizes are upstream source lines at
`ccfe79ed2`, implementation only.

| # | entry | size | est. | why here | queued deltas |
|---|---|---|---|---|---|
| 1 | **OAuth token acquisition** (`auth/oauth/**`, `oauth.ts`) | 2,983 LOC / 12 files — **corrected 2026-08-31**: the old parenthetical read "2,439 excluding `openai-codex.ts`, whose adapter is OUT under E2", which has been stale since the 2026-08-27 rewrite (the Codex adapter is entry 10, back in scope) and contradicted it. `openai-codex.ts` is the 544-line **OAuth** file and belongs to this entry; note it is also inside entry 10's 2,228, so the two entries **double-count** those 544 lines — 2,983 − 2,439 = 544. Do not sum the queue's sizes | ~3 | Highest value in the queue. `OAuthAuth.Refresh`/`ToAuth` and `LazyOAuth` are **ported seams with zero implementers** — Anthropic Pro/Max, Copilot, OpenRouter, Kimi and xAI subscription auth are simply unreachable from Go. Needs nothing beyond `crypto/rand`, `crypto/sha256`, `net/http`, `os/exec`. Carries `auth/oauth/radius.ts` (403), so land it with entry 2. | **+1 (2026-09-20)** — `b73412a37`: `metaOAuth` (`auth/oauth/meta.ts`, 208 LOC), Meta's Muse SUBSCRIPTION flow — `isSubscription: true` with a `Sign in with Meta` login label, the first entry-1 row that is a subscription rather than a key exchange. The provider's models, base URL and env-key auth already arrived with the 0.86.1 regen and `7cb43ec`; only the acquisition is missing, so a Meta user with `META_API_KEY` set is served today and one relying on the subscription is not. **+2 (2026-09-30)** — `4df157433`: one loopback callback server and sign-in page for every flow that takes a browser redirect (`auth/oauth/callback-server.ts`, new; `utils/oauth-page.ts`, moved from `auth/oauth/`); the anthropic, openai-codex, openrouter and radius flows use it, and so does MCP's (row 21). Oracle: `oauth-callback-server.test.ts`. `02eed88fd`: Sign in with ChatGPT — `auth/oauth/openai-chatgpt.ts` (the flow; it sends `LoginOptions.getDeviceId()` as the agent host ID), and the `openai` provider gains `oauth: lazyOAuth({name: "OpenAI (ChatGPT subscription)", isSubscription: true, loginLabel: "Sign in with ChatGPT"})` beside its API key. Its request side (fields a token omits, the usage hint, retry patterns, `LoginOptions`) shipped (`d8532cc`). Oracles: `openai-chatgpt-oauth.test.ts`, `oauth-auth.test.ts` › 'OpenAI exposes ChatGPT OAuth alongside API-key auth'. **+1 (2026-10-01)** — `7a11fe1c7`: `anthropicOAuth.login` prompts a `select` between "browser" (the existing callback-server flow) and "copy_code" (headless): PKCE with `redirect_uri` `https://platform.claude.com/oauth/code/callback` and `code=true`, an `auth_url` notify, a `manual_code` prompt parsed as `code#state` (state must equal the verifier), then the same code exchange. Unknown method ids throw `Unknown Anthropic login method: <id>`. Oracle: `anthropic-oauth.test.ts`. **+4 (2026-10-19)** — `233f17440`: the sign-in page's `LOGO_SVG` is coloured (`utils/oauth-page.ts`). `eeac84ca9`: `openai-chatgpt.ts` throws "Port … is in use …" on `EADDRINUSE` instead of falling back to a manual paste. `8d8ae2fc2`: Anthropic's callback server falls back to a free port when its own is taken (`auth/oauth/anthropic.ts`). `9ad083102`'s flow half: the ChatGPT and Codex flows name the app with `LoginOptions.agentName` (the field shipped, `47bdcc8`); its two tests are the oracle. |
| 2 | **Radius provider** (`providers/radius.ts`, `providers/radius-config.ts`) | **178 LOC** | ~0.5 | Smallest entry by an order of magnitude, and it closes the **2026-08-22 tripwire**, which is loaded and will fire on the next behavioral commit: `radius-config.ts` is reachable through pi-ai's `"./providers/*"` exports wildcard, i.e. published SDK surface the 2026-07-14 ruling did not name. Only ~8 first-parent commits in 90 days touch anything Radius-named, so this is bought for the tripwire, not for churn relief. | **+1 (2026-09-20)** — `4d38031fb`: `radiusProvider` stops serving only the dynamically fetched list. It now merges a STATIC baseline (`RADIUS_MODELS`, 27 models, shipped in the 0.86.1 catalog and present in the port's embedded copy since this cycle's regen) under the fetched ones, by id, and only when the gateway is the default. So an offline or unreachable gateway still lists models. The port carries the catalog rows already; what is missing is the merge and the fetch. **Also carried, found by the 0.86.1 parity review:** the 27 Radius catalog models carry three keys no other provider has — `lab`, `enabled` and `providers` — and `ai.Model` has no field for any of them, so the port decodes those rows and silently drops the keys. Harmless while nothing reads them; the base port has to decide what they mean before it can serve Radius's routing faithfully. **+1 (2026-10-19)** — `43d376399`: once the gateway's catalog is known (fetched, or restored from the store) it REPLACES the static baseline instead of merging over it — `getModels` is `dynamicModels ?? baselineModels` — so a model an organization owner disabled stops being listed; the baseline covers only the time before any catalog exists. Supersedes the 2026-09-20 merge above. Oracle: `radius-provider.test.ts`. |
| 3 | **Non-chat model operations — image generation + classification** (widened 2026-09-24) | at `a328aa89a`: ~670 LOC of image/classifier files in `packages/ai/src` (`api/openrouter-images.ts`, `api/typesafe-system-one.ts` 187, `providers/typesafe.ts`, `image-models.ts`, `images.ts`, `images-api-registry.ts`, `providers/images/register-builtins.ts`) + ~90 in `models.ts` + ~120 of `types.ts`. The old "684 is `image-models.generated.ts`" figure is dead: `a328aa89a` deleted that file | ~2.5 | Root-export surface (2026-08-27 ruling). **Widened 2026-09-24:** `a328aa89a` unified image generation and a new `classifier` model type (TypeSafe System One, provider `typesafe`, model `typesafe/jev-latest`) on the ONE `Models`/`Provider`/`createProvider` infrastructure, and both catalogs now come out of `models.generated.ts` (`IMAGE_MODELS`, `CLASSIFIER_MODELS`), so they queue together. **The base port must build on `ai.Models`/`ai.Provider`, never an `ImagesModels` collection** — upstream deleted `images-models.ts` and the separate `openrouter-images` provider. The chat-side half of the same commit SHIPPED this cycle (`Model.Type`, `GetModelType`/`IsModelType`, `AssertChatModel`, the optional `AllModelsLister`/`AllModelsFilterer`, the typed `Models` accessors, the unknown-type store filter, `TYPESAFE_API_KEY`). Contents still owed: `Model.Output` for image models; `Models.GenerateImages`/`Classify` (never reject — `imageErrorResult`/`classifierErrorResult`: stopReason `aborted` when the signal is aborted else `error`, errorMessage = message); `Provider` optional generate/classify; `CreateProviderOptions` `Images`/`Classifiers` maps and their arms of the `Provider <id>: at least one of "api", "images", or "classifiers" is required.` validation; `assertImageModel`/`assertClassifierModel` (`Model p/id is not an image model` / `… a classifier model`); dispatch errors `Provider X does not support image generation` / `… classification` and `Provider X has no image generation implementation for "<api>"` / `… no classifier implementation for …`; the openrouter-images api + the openrouter provider's `images` wiring (OAuth via row 1); the typesafe-system-one api (POST `systemone` under the base URL, bool questions sent as `type:"noul"`, headers `providerHeadersToRecord({authorization, content-type}, model.headers, options.headers)` — so a null marker can delete the adapter's own authorization — retry maxRetries 2, `Request timed out after ${ms}ms`) + `typesafeProvider()` in `builtinProviders()`; the compat `image-models.ts`/`images.ts`/`images-api-registry.ts`/`register-builtins.ts`; embedding the `IMAGE_MODELS`/`CLASSIFIER_MODELS` dumps from the npm build. **Known gap this row closes:** pi's `builtinModels()` registers `typesafe` (classifier-only) so `Models.getProviders()`, `checkAuth` and `getAllModels("typesafe")` see it; the port's catalog-derived `BuiltinModels` cannot until the classifier base lands. `scripts/generate-image-models.ts` no longer exists (the image catalog is `generate-models.ts` + `scripts/openrouter-catalog.ts`). Oracles: `typesafe-system-one.test.ts`, `classifier-models.test.ts` (classify half), `images-models.test.ts` (generateImages cases), `openrouter-images.test.ts`, `images.test.ts`, coding-agent `model-runtime-images.test.ts` (a),(d),(e) and case (g)'s built-in half, `model-runtime-classifiers.test.ts`. | **SUBSUMED 2026-09-24:** the four image-catalog deltas this cell carried (`5ce4afbd9`, `fbc87d25a`, `bdee230f1`, `b7f4b05c7`) targeted `image-models.generated.ts`, which `a328aa89a` deleted — they are retired, not replayed; the image catalog now lands whole as the `IMAGE_MODELS` dump of whichever npm build the base port pins. **+1 (2026-09-24)** — `a328aa89a`: the unification above, including the new `typesafe` classifier provider. **+1 (2026-09-25)** — `a7d17e39a`: the classifier operation reaches two more providers. OpenRouter serves TypeSafe's System One protocol at `/api/v1/systemone`, so `providers/openrouter.ts` lists its decision models and registers `classifiers: {"typesafe-system-one": typesafeSystemOneApi()}`; Cloudflare Workers AI gets a `cloudflare-workers-ai-system-one` classifier API (`api/cloudflare-workers-ai-system-one.ts` + `.lazy.ts`: `POST <base>/run` with `{model, input}`, answers read out of Cloudflare's `{success, result: {state: "Completed", result: {answers}}}` envelope, errors joined from `errors[].message` as "Cloudflare Workers AI error: …"), wrapped in `cloudflareClassifier` (`providers/cloudflare-stream.ts`, the `{CLOUDFLARE_ACCOUNT_ID}` base-URL substitution for classifiers, with `resolveCloudflareModel` generalized to any model with a `baseUrl`). Request building and answer parsing move from `typesafe-system-one.ts` into a shared `api/system-one-shared.ts` (`classifySystemOne(transport, …)`), which the base port should mirror rather than duplicate; `KnownClassifierApi` gains `"cloudflare-workers-ai-system-one"`, and `api/cloudflare.ts` exports `CLOUDFLARE_WORKERS_AI_REST_BASE_URL`. Its generator hunks are catalog-only. Upstream CI failed on it until `567469096` fixed two model-count tests. **+1 (2026-09-29)** — `04efdfc38`: a llama.cpp classifier API, `api/llama-cpp-classify.ts` (458 lines) + its `.lazy.ts` loader — `KnownClassifierApi` gains `"llama-cpp-classify"` — and `ClassifierOptions.temperature` (answer logits divided by it before normalization; APIs that cannot apply it ignore it). Oracle: `llama-cpp-classify.test.ts`. Its `extensions/llama/provider.ts` half is E1. **+2 (2026-09-30)** — `33e203354`: OpenCode Zen and Vercel AI Gateway serve System One (`classifiers: {"typesafe-system-one": typesafeSystemOneApi()}` and their `*_CLASSIFIER_MODELS`). `89a5c7bda`: `ClassifierResult.usage` from System One's `{input_tokens, output_tokens}`, priced at the catalog rate and set before the answers parse (a malformed answer was still billed); the transports return the whole output (`output`, was `answers`). Oracles: `classifier-models.test.ts`, `typesafe-system-one.test.ts`. **Known gap at 1.3.0 (`797d14f`):** the embedded catalog is chat-only, so every non-chat model is missing from the Go `Models` accessors — the 60 image and 40 classifier models in this row's 2026-10-19 delta — and `BuiltinModels` passes over `typesafe`, a provider with no chat models, which pi registers through `createProvider`'s `classifiers` — so `BuiltinModels().GetProvider("typesafe")` is nil and `CheckAuth` reports nothing for it where pi reports `TYPESAFE_API_KEY`. `TestCatalogProvidersWithoutChatModels` fails if that set grows. Landing this row embeds `IMAGE_MODELS`/`CLASSIFIER_MODELS` and removes the skip. **+3 (2026-10-19)** — `4812cb268`: `api/cloudflare-workers-ai-system-one.ts`'s `output()` accepts Cloudflare's direct `{model, answers, usage}` result as well as the run record (the Clef classifiers). `ce8972a0e`: the `openai-decisions` classifier API (OpenAI Decisions, `gpt-6-luna`), `classifier-shared.ts`, `ClassifierContext.images` with `assertClassifierInputSupported` (in `models.ts` and `model-runtime.ts`'s classifier half), the openai provider hiding classifiers under OAuth (`filterAllModels`), and `provider-retry.ts`'s `noRetryStatuses`, whose Go home is `ai/providers/retry.go` but whose only consumer is openai-decisions. `6b5854454`: empty OpenAI classifier catalogs are typed. Rider: `195120a91`'s opencode Jev URLs (`/inference/v1/systemone`, `classifier-models.test.ts`). **The 1.3.0 dumps (not embedded):** `IMAGE_MODELS` 60, all openrouter on `openrouter-images` (57 at 0.99.2); `CLASSIFIER_MODELS` 40 across six providers — cloudflare-workers-ai 3, openai 1, opencode 2, openrouter 23, typesafe 1, vercel-ai-gateway 10 (15 at 0.99.2). `TestCatalogProvidersWithoutChatModels` still reads `[typesafe]`. |
| 4 | **Azure OpenAI Responses** — provider `azure` (renamed from `azure-openai-responses` by `a37306d43`; the api id is unchanged): `api/azure-openai-responses.ts` and the Responses half of `providers/azure.ts` | 249 + 55 LOC at `d2e7d8b7c` | ~1 | JSON over HTTPS, header auth. **The Chat Completions half SHIPPED** (`48bf4b6`): `api/azure-openai-config.ts`'s base-URL and deployment-name resolution (env only) and `providers/azure.ts`'s completions wrapper, keyed on provider `azure` inside `StreamOpenAICompletions` because Go dispatches by api. **Still owed:** the options layer (`azureBaseUrl`, `azureResourceName`, `azureDeploymentName`, `azureApiVersion`, which precede the env in pi's resolvers), `resolveAzureConfig`'s api version (`AZURE_OPENAI_API_VERSION`, default `v1`), the AzureOpenAI client's semantics (the `api-version` query, its auth header), `formatAzureOpenAIError` ("Azure OpenAI API error"), `AZURE_TOOL_CALL_PROVIDERS`, and its own buildParams and stream over `responsesInput` and the Responses event processing — factor that out of `StreamOpenAIResponses`' goroutine. Until then an azure Responses model fails "Provider azure has no API implementation for "azure-openai-responses"" (D107). | **+1 (2026-09-24)** — `002fc8385`: `stream()` forwards `options.onProviderStreamEvent` into `processResponsesStream`; landing on the Go responses loop inherits `openai_responses.go`'s callback site (shipped this cycle). Oracle: `azure-openai-base-url.test.ts` › 'forwards parsed events in order before normalizing the response' (response.created + response.completed, identity, responseId `resp_azure`). **+1 (2026-09-27)** — `c01f687e5`: `buildParams` ends `Object.assign(params, model.samplingParams, options?.samplingParams)`: the model's defaults, then the request's per key, after the named request fields. Land it as `openai_responses.go`'s `buildResponsesParams` does since `433fcdd`. Oracle: `sampling-options.test.ts` › the azure-openai-responses row of 'applies model-level sampling params with request keys taking precedence for %s'. **+2 (2026-10-19)** — `a37306d43`: the provider rename and `providers/azure.ts`'s api table (`azure-openai-responses` → `azureOpenAIResponsesApi()`); its completions half shipped. `76dfb88f6`: `azure-openai-responses.ts` resolves sampling params per thinking level (`resolveSamplingParams` at `reasoningEffort ?? (reasoningSummary ? "medium" : undefined) ?? "off"`), as `openai_responses.go` does since `f6a41b0`. **+1 (2026-10-20)** — `caa6967b6`'s azure hunk: `buildParams` resolves transcript tools through `getResponsesToolSupport({supportsMidConvoSystemMessages, supportsAdditionalTools, supportsToolSearch})` (each defaulting false) — inherited for free if the azure Responses half reuses `openai_responses.go`'s `responsesInput` and `getResponsesToolSupport`, shipped this cycle. `530015d93` rides along through the shared conversion. |
| 5 | **Google Vertex** | 710 LOC | ~2 | IN under E2's transparent-wrapper rider (`@google/genai`). ADC is the risk — scope it to the credential paths Go reaches with stdlib. | **+1 (2026-09-24)** — `002fc8385`: `onProviderStreamEvent` fires first in the googleStream loop with the `generateContentResponseFromVertex` shape (+ `sdkHttpResponse`); mirror `google.go`'s shipped callback (the Generative-AI half shipped this cycle). Oracle: the Vertex case of `google-raw-stop-reason.test.ts` › 'forwards each SDK chunk in order before normalizing it'. **Rider (`a328aa89a`):** route `options.headers`/`model.headers` through `headerObject.applyAsRecord(h, literals...)` (the case-folding `providerHeadersToRecord` shipped this cycle), passing the SDK's own default headers as literals in their exact spelling, never a bespoke merge. **+1 (2026-10-19)** — `1692bba63`: Gemini 4 and later send `thinkingLevel` (`usesGoogleThinkingLevel`, shared with `google.go`, shipped `2c687bb`). The motivating model is catalog `google-vertex/gemini-4-argon` (#10890: Vertex rejects `thinkingBudget`), which fails "Provider google-vertex has no API implementation for "google-vertex"" until this row lands (D107). Measured against pi 1.3.0: with an api key it posts to `https://aiplatform.googleapis.com/v1/publishers/google/models/gemini-4-argon:streamGenerateContent?alt=sse` with google's `thinkingConfig`. |
| 6 | **Mistral conversations** | 963 LOC | ~1.5 | JSON over HTTPS, header auth. | **3** — `6c87d9a02` (2026-08-29: merge indexed Mistral tool-call chunks — `consumeChatStream`'s `toolBlocksByKey` is rekeyed from the composite `` `${callId}:${index}` `` string to `toolCall.index ?? callId`, so chunks of one call that arrive with differing ids still merge. No Go base: `ai/providers/` has no mistral adapter, only registry-level references in `ai/envkeys.go`, `ai/types.go` and `coding/resolve.go`), `96617628e` (2026-09-08: `usesReasoningEffort` widens from the exact ids `mistral-medium-3.5` to the prefix `mistral-medium-`, so every reasoning-capable `mistral-medium-*` model sends `reasoning_effort` instead of prompt-mode reasoning. No Go base: `ai/providers/` still has no mistral adapter, only registry-level references in `ai/envkeys.go`, `ai/types.go` and `coding/resolve.go`), `4bd3f48df` (2026-09-10: `usesReasoningEffort` gains the exact id `zai-glm-5-2`, so Mistral-hosted GLM-5.2 sends `reasoning_effort` when thinking is on instead of `prompt_mode`, which Mistral ignores for that model (#9375); thinking off still sends neither. `mistral/zai-glm-5-2` is already in the shipped catalog as a `mistral-conversations` model, so the adapter must carry the id when it lands, and pi's two new `mistral-reasoning-mode.test.ts` cases are its oracle. No Go base: `ai/providers/` still has no mistral adapter) **+1 (2026-09-24)** — `002fc8385`: `consumeChatStream` takes `onProviderStreamEvent` and awaits it on `event.data` before `output.responseId \|\|= chunk.id`. Oracle: `mistral-http-transport.test.ts` › 'forwards each parsed SSE payload before normalizing it' (unknown `provider_metadata` round-trips). **+1 (2026-09-27)** — `8930b9ec0`: `consumeChatStream` skips an empty text delta (a string item, or a `{type: "text"}` item whose text is empty after `sanitizeSurrogates`) instead of opening a text block: GLM models on Mistral send empty deltas around thinking and tool calls, and a block opened for one splits the thinking into several blocks, which Mistral rejects on replay. Oracle: `mistral-http-transport.test.ts` › 'ignores empty content deltas'. **+1 (2026-09-29)** — `dc84c1ac0`: `streamSimple` sends the requested thinking level to Mistral reasoning models. A reasoning model with a `thinkingLevelMap` sends `reasoning_effort` = the map's value for the clamped level (`?? "high"`), or the map's `off` value when reasoning is off; one without a map (Magistral) sends `prompt_mode: "reasoning"`. `usesReasoningEffort`/`usesPromptModeReasoning`/`mapReasoningEffort` and their model-id lists are deleted, and `MistralReasoningEffort` widens to `none\|low\|medium\|high\|max`. Oracle: `mistral-reasoning-mode.test.ts`. Its generator half is catalog-only. **+4 (2026-10-19)** — `7fb59f995`: a `finish_reason: "error"` becomes `Provider stopped with: error (server error)`, which the retry patterns accept. `6b07b4e57`: the request timeout covers only the wait for response headers ("Mistral response headers timed out after Nms"); body reads abort only on the caller's signal. `87f9a3641`'s mistral hunk: finished tool-call arguments go through `parseToolCallArguments` (`Object.assign(block, …)`; Go's `ToolCall.ArgumentsError` is ready). `911309429`: another model's thinking is dropped, through the shared `transformMessages` (`ai/providers/transform.go`), automatic once the adapter uses it. |
| 7 | ~~**session-backends**~~ (`packages/session-backends/**`) | — | — | **CLOSED 2026-10-19** — `7fd478a2e` deleted `packages/session-backends` (sqlite-node was its only backend); the port had no code for it, and the four sqlite deltas it carried died with it. Its driver answer now governs row 8's storage (Rulings, 2026-08-31 SQLite). | — |
| 9 | **Bedrock adapter** | 1,459 LOC | ~3 + consult | **CONSULT ANSWERED (2026-08-31) — TAKE `aws-sdk-go-v2` (with `config`), CONFINED TO A `github.com/sky-valley/pi/providers/bedrock` SUBMODULE** with its own tag series; the root module's graph stays at its two `golang.org/x/*` requires. Back in scope 2026-08-27. The 2026-08-27 note that parity "favours hand-rolling" is **withdrawn**: it is true that there is no pi-authored byte sequence to match, but it priced only the signer. Signing is roughly a third of the work and the credential chain is the rest — and pi carries **no credential-resolution code at all**, so `AWS_PROFILE`, ECS task roles and IRSA (all advertised in `packages/coding-agent/docs/providers.md`) work only because the SDK's default chain is linked in. Hand-rolling ships a documented-feature regression. See the 2026-08-31 ruling. | **2** — `a63fb12c1` (2026-09-01: `utils/node-http-proxy.ts` rewrites `NO_PROXY` matching — entries are normalized by stripping a leading `*.`/`.`/`*` to a bare domain, then matched by exact host OR dot-suffix, with `stripBrackets` applied to BOTH the entry and the target host, and port-scoped entries parsed for bare and bracketed IPv6. `bedrock-converse-stream.ts:207` is one of the file's only two importers, so this lands with the adapter. **Do NOT assume `http.ProxyFromEnvironment` covers it** — measured non-equivalence, see the 2026-09-01 cycle note), `8a7b0c03d` (2026-09-15: `bedrock-converse-stream.ts` `handleMetadata` sets `usage.cacheWrite1h` to the sum of `event.usage.cacheDetails[].inputTokens` over entries whose `ttl === CacheTTL.ONE_HOUR`, so the shared cost path bills Bedrock's 1h cache writes at 2× input (#9457). Checked against the 2026-07-21 generic-collapse lesson and it does NOT collapse: the generic half — `Usage.CacheWrite1h` (`ai/types.go:446`) and the `rateInput*2*longWrite` term in `ai/models.go` `CalculateCost` — shipped with the Anthropic port, and upstream's `models.ts` is untouched in range; the delta is adapter-local extraction with no Go home until the adapter lands. Oracle: `packages/ai/test/bedrock-cache-write-1h-cost.test.ts` (new, 78 lines) as amended by `4c2d91339`, which derives the expected `cost.cacheWrite` from `model.cost` instead of the literal 7.75. **Rider:** the comment at `ai/types.go:444–445` says only Anthropic reports this split — true of the port today and false the day this lands; fix it in the same commit) **+1 (2026-09-24)** — `002fc8385`: `onProviderStreamEvent` fires first for every SDK union item, exception items included, BEFORE the adapter throws on them. Oracle: `bedrock-raw-stop-reason.test.ts` (errorMessage `bedrock stream failed`). **Rider (`a328aa89a`):** custom headers via `headerObject.applyAsRecord(h, literals...)`, the SDK's defaults passed as literals. **+6 (2026-10-19)** — `69f0be6f0`: Claude replays send `thinking.block_binding {prefix_mismatch_behavior: "drop_block"}` and `anthropic_beta: ["thinking-binding-controls-2026-08-01"]` for Opus 4.7/4.8/5, Sonnet 5 and Fable 5, never on GovCloud. `2989eb581`: reasoning effort for OpenAI models on Bedrock. `f76c1db66`'s bedrock half: Haiku 5.5 joins the adaptive-thinking, xhigh, block-binding and prompt-caching predicates. `87f9a3641`'s bedrock hunk: finished-call arguments through `parseToolCallArguments`. `5ab7fe74c`'s bedrock hunk (Claude only): an error tool result's images go after the tool results, through `splitErrorToolResultImages` (`ai/providers/transform.go`) as `convertAnthropicMessages` does. `911309429`, through the shared `transformMessages`. |
| 10 | **Codex adapter** | 2,228 LOC | ~3 + consult | **CONSULT ANSWERED (2026-08-31) — NO DEPENDENCY REQUIRED TO SHIP.** Upstream takes no third-party package here (`node:zlib` builtin, `globalThis.WebSocket`), and the consult's premise — "zstd decompression" — does not exist: it is request-body compression only, SSE-path only, with a `return null` fallback to plain JSON. **zstd: take nothing**, mirroring upstream's own null-compression branch. **WebSocket: deferred to the WS slice**, pre-decided as `github.com/coder/websocket` in the **root** module (not a submodule — see the ruling). Ship SSE first; an SSE-only adapter is an upstream-defined runtime state, not a parity gap. **Blocked on entry 1** — Codex is OAuth-only and the port's OAuth seams have zero implementers. Back in scope 2026-08-27. | **2** — `a63fb12c1` (2026-09-01: the same `utils/node-http-proxy.ts` `NO_PROXY` rewrite; `openai-codex-responses.ts:972` is the file's other importer. Shares the delta with entry 9 — **the two entries do not sum**, it is one upstream change landing in whichever adapter ships first), `e86102f18` (2026-09-10: `buildRequestBody` stops collapsing a `null` `thinkingLevelMap.off` into `"none"` — `reasoningEffort: "none"` now sends the mapped value only when the key is DEFINED, so a null mapping suppresses the `reasoning` block instead of forcing `effort: "none"` — and a request that passes NO `reasoningEffort` at all now sends `reasoning: {effort: thinkingLevelMap.off ?? "none"}` whenever the model reasons and `off` is not null. No Go base: `grep -rin codex --include='*.go'` finds only the `APIOpenAICodexResponses` api id, a tool-call-id provider-set entry and a default-model row — `ai/types.go:583` states the adapter is deliberately not ported. Checked against the 2026-07-21 generic-collapse lesson and it does NOT collapse: `openai_responses.go` is a different adapter with its own effort mapping) **+1 (2026-09-24)** — `002fc8385`: `mapCodexEvents` fires the callback for every parsed SSE and WebSocket event before the type check; a throwing callback is wrapped as `ProviderStreamEventCallbackError` (message `formatThrownValue(cause)`), which `isCodexNonTransportError` accepts, so it skips the WebSocket retry and the SSE fallback. Oracle: `openai-codex-stream.test.ts` (5 events, `response.done` in the WS fixture). **+1 (2026-09-30)** — `02eed88fd`: the provider's display name becomes "OpenAI Codex (legacy)" now that the `openai` provider signs in with ChatGPT. The Go provider is named by its id until this row lands. **+1 (2026-10-19)** — `0cf65d2bf`: `buildBaseCodexHeaders` sets its `originator`/User-Agent defaults first, so a caller's headers override them. **+1 (2026-10-20)** — `caa6967b6`'s codex hunk: `buildRequestBody` resolves its transcript tools through `getResponsesToolSupport({supportsMidConvoSystemMessages, supportsAdditionalTools, supportsToolSearch})` (`"changes"` / `"additions"` / `"none"`), the shared Responses conversion shipped this cycle in `ai/providers/openai_responses.go`; the adapter must call it the same way. Codex passes `includeSystemPrompt: false`, which Go's shared conversion does not model (no other caller passes it): carry pi's `if (!isLeadingSystemMessage || includeInitialSystemMessage)` guard around the removal notice and its unavailable-tool bookkeeping when this lands. |
| 8 | **Durable** — `packages/durable` (`@earendil-works/pi-durable`: the durable harness and task runtime, storage backends, the `env/` ExecutionEnv, its tools and code mode, the `./testing` suites), plus the chord exports whose consumer is durable (`./output`, `./sqlite`, `./sqlite/node`) | 26,012 LOC / 84 files of `src` at `d2e7d8b7c` | not yet sized | **FUNDED 2026-08-27**; shape: the 2026-09-03 mirror (see "Durable shape"). **Retitled 2026-10-19:** `7fd478a2e` deleted `packages/agent/src/harness/**` and `search/**`, so this row is `packages/durable` alone (the Pico kernel moved there on 2026-09-18, `080160162`), and the harness, `pico3/**` and agent-Pico deltas it carried are pruned. **Shipped ahead of the base:** slice 8b-i (`b677517`, `7c3ea77`) — the `ExecutionEnv`/`FileSystem`/`Shell` contract as `coding/execenv.go`, at the harness shape of `ccfe79ed2` + `3e4bc2680`, its provenance re-pointed to `packages/durable/src/env/{index,node}.ts` (`8adf788`); a hunk on a method `LocalEnv` has is an ordinary `port`. **Durable `ExecutionEnv` members with no Go interface yet** (measured at `d2e7d8b7c`): `FileSystem.id` (`"node:local"`); `openBinaryReader` → `BinaryReader{info, read, scanLines → LineScan, close}`; `truncateFile`; `flushFile`; optional `listFiles` (`ListFilesOptions`, `FileListing`); `openDirReader` → `DirReader`; `watch` (`WatchTarget`, `WatchChange`, `FileWatcher{mode, close}`); `createDir`; `remove`; `createTempDir`; `createTempFile`; `cleanup` on FileSystem (Go has `Shell.Cleanup` only); `writeFile`/`appendFile` of `string \| Uint8Array`; and Durable's `exec` — `string \| argv` (argv runs without a shell), output only through `onOutput(text, ctx, {stream, skipped?})`, `spill` and `window` options, `{exitCode, spillPath?}` — where `coding.Shell.Exec` is the older collect-stdout/stderr shape; typed `ExecutionError{code, spillPath?}` and `FileError{code, path?}`. Semantics that already differ: `readTextLines` with `maxLines <= 0` returns `[]` (Go: no limit); a non-file/dir/symlink is `FileError("invalid")` (Go: `FileKindOther`); exec `timeout <= 0` is `ExecutionError("timeout")` (Go: no timeout). `NodeExecutionEnv` takes a `shellPath` option and its no-bash error reads "3. Configure an explicit shellPath" (`LocalEnv` has no option and returns coding's "3. Set shellPath in settings.json"); `EnvBashOperations` truncates a fractional timeout to whole seconds for `ShellExecOptions.TimeoutSeconds`, so 0.5 means no timeout (2026-10-20). **pi-env carries these types today** in package `env` (`FileError`, `ExecutionError`, `BinaryReader`, `DirReader`, `LineScan`, `ListFilesOptions`, `FileListing`, `WatchTarget`, `WatchChange`, the exec option types; `RemoteExecutionEnv.RunShell`/`RunProgram` for Durable's exec). When this row lands they move to the Go durable package, `RemoteExecutionEnv` implements its `ExecutionEnv`, and pi-env's differential "read and bash tool results" case lands with Durable's tools. Re-align `TestTextLineReaderMatchesPiContract` (transliterated from the deleted harness test at `3e4bc2680`) with `packages/durable/test/env-node.test.ts` then. SQLite: Rulings, 2026-08-31 SQLite. Durable Object adapters (`da866ada1`'s `./storage/sqlite/cloudflare`, now chord's `./sqlite/cloudflare`) have no meaning in Go and are dropped. | **Carried: 32** (durable only). **+4 (2026-09-23)** — `packages/durable` (`@earendil-works/pi-durable`): `8158b0321` versioned document storage (memory storage + types); `5901c9b9e` a SQLite storage backend on Node's builtin `node:sqlite` (`storage/sqlite/**`, new `./storage/sqlite` and `./storage/sqlite/node` exports) — the driver question is already answered by the 2026-08-31 ruling: `modernc.org/sqlite` in a `backends/sqlite` submodule, never the root module; `898ab8040` a JSONL storage backend plus `env/**` (node environment, output capture, truncation, adaptive publisher); `b313731b8` JSONL sidecar reclamation. **+1 (2026-09-24)** — `b45597504`: `packages/durable`'s storage conformance suite moves from `test/` into a published `./testing` export (`createStorageConformance`, `registerStorageConformance(runner, name, withStorage)`, `createExpectAssertions`, the `StorageConformance*` types) and the memory/JSONL/SQLite backend tests are rewired onto it. Land it with the durable base port as a Go testing helper package. **+5 (2026-09-25)** — `packages/durable`: `601437d5a` `env/utils/output-capture.ts` reports `truncatedBy` from the retained window's own decision and a trailing newline's `lastLineBytes`, keeps a four-byte allowance past twice the byte limit, and fixes `truncateHead`/`truncateTail`'s `truncatedBy` (only omitted lines prove the line limit) — durable's fork only: `coding-agent/src/core/tools/truncate.ts`, which `coding/truncate.go` mirrors, keeps the old rule at `49681e1b7`, so the port must not carry the fix across; `3f0573877` exports the storage benchmark workloads from `./testing`; `19a0361be`'s durable half, transactional sessions and documents (`session/{session,transaction,publications}.ts`, `documents.ts`, the storage backends and `types.ts`), which copies with chord's `copyJson` — build it on `chord.CopyJSON`, shipped this cycle; `5d4de953c` checkpoints and document migrations; `5fd446ca1` document replay (conformance cases added). **+2 (2026-09-27)** — `packages/durable`: `507d7649e` conversation document forks (`session/forks.ts`: when a fork is created, the backend copies the conversation's documents by policy; `Tx.createConversation()` is split from `Tx.forkConversation()` and raw conversation-record input goes; `ConversationQuery` on Storage and transaction conversation scans; `errors.ts` holds `ReadAfterWrite` and the new `StorageRejected`, for a batch storage rejected before any durable effect; conformance cases added); `04b5bad66` typed IDs and explicit ownership (branded numeric ID types in `ids.ts` replace untyped record ids and the `TaskRef` wrapper, with a result-typed `TaskId<R>`, branded commit sequences and a generic `Storage.mintId()`; a conversation is created explicitly ownerless or task-owned; a task's conversation membership is immutable after creation). `d6af72e18` rewrites only `test/scratch.ts`'s usage examples (n/a). **+4 (2026-09-29)** — `packages/durable`: `35180b9df`'s durable half, document states and buffered watches (`session/observation.ts`, new, 289 lines; `session/session.ts`, `session/transaction.ts`, `types.ts`; `session/publications.ts` shrinks as delivery moves into observation); `11894012d`, deltas since base exposed to checkpoint predicates (`documents.ts`, `session/{session,transaction}.ts`, `storage/memory.ts`, `storage/sqlite/storage.ts`, `types.ts`); `3883fcb1f`, the openable Harness with its registry and conversation config (`harness/{config,context,harness,registry,types}.ts`, new, ~1,035 lines; `entries.ts`, `index.ts`); `cb7969d21`, the durable task runtime (`harness/scheduler.ts`, new, 703 lines; `tasks.ts`, `types.ts`). No Go base: `grep -rn --include='*.go' -i -e pi-durable -e 'package durable'` is empty. The milestone docs commits `cd5ff1125` and `034be02d2` are n/a. **+5 (2026-09-30)** — `837d8a16b` (Package 15: the first runnable chat turn — `harness/{generation,scheduler,submissions,prompt,live}.ts`, `session/transaction.ts`), `445770e03` (Package 16: the first coding-agent tool turn — `harness/{tool,output,post-tools}.ts`, `env/{node,utils/output-capture,utils/adaptive-publisher}.ts`, `tools/{bash,edit,edit-diff,read,write,image}.ts`), `47f65f1a3` (Package 17: inbox, reset, usage, conversation view and experimental events — `harness/{inbox,usage,view,events}.ts`, `session/observation.ts`), `2532a0bef` (Package 18: conversation abort, ownership cascades, subagent handles), and `ba7d5fbed`'s durable half (TypeBox dropped from `harness/generation.ts`; its `pi-ai/models` package entry is packaging, E1). `1b347794e` adds only race tests for `2532a0bef` — the oracle to start from. The spec and README commits are n/a. **+11 (2026-10-01)** — the async SQLite storage PR (`f3e68e8ea` async storage, `fd3af5ee2` transaction handle to callbacks, `cd0ba2fab` SQL-text queries with a per-connection statement cache, `77ac62f22`, `a0feabda7`, `ed391c4f0` queue and close hardening, carried by merge `8f89293a2`; `storage/sqlite/**`), `03180653c` (Package 19: structured concurrency — task ownership, waiting, completing holds, bottom-up abort; `harness/scheduler.ts`, `post-tools.ts` deleted), `ed0d6b91b` (Package 20: compaction and overflow — new `harness/compaction.ts`), `b56702ad3` (Package 21: extensions and per-conversation agents — new `harness/{agent,define}.ts`, `registry.ts` −334, `config.ts` emptied), `49683a364` (Packages 22–23: lifecycle conformance and the task-graph view, new `harness/task-graph.ts`). The spec and README commits are n/a; `5609b0d6c`'s experimental TUI agent on pi-durable is E1 (unexported, `dist/experimental` is not published). **+57 (2026-10-19)**, plus riders. *Env:* `1543dd8f6` (`flushFile` on a directory is `is_directory`), `4748c627a` (bounded binary and directory readers, argv exec with stream info, the published `./testing` env conformance suite), `acfc60198` and `be882f3fa` (conformance timing), `a19c09d9b` (a bounded read tool over `BinaryReader.scanLines`; `env/{decode,line-scan}.ts`), `a84510819` (`FileSystem.watch`, `env/node-watch.ts`), `864777ba6` (polling watches on Windows), `1965a8069` (the FSEvents 500 ms settle rescan), `cd60a5b99` (watcher coverage, growing reads, output BOM), `abfe62928` (optional `listFiles` with conformance). *Tools and output:* `cdf79797b` (windowed shell output with counted skips, `ShellOutputSkip`), `68c22123b` (powershell over argv exec), `42a3497d0` (read returns images; Photon's Go equivalent is `coding/imageresize.go`, no consult), `cdaf6c227` (image preparation off-thread: a goroutine), `b04299717` (base64 with `btoa`), `4a5d37291` (`OutputBuffer` and `Progress` move to chord's `./output`), `f75b3240a` and `ec1be23eb` (`Progress` takes an injectable `ProgressClock`; its timer re-checks the clock). *Harness:* `70eceaade` (a persisted per-conversation provider session id, UUIDv7), `674d64f09` (configurable progress commit intervals), `68ccef176` (a scanned context range reused), `b0114ef5f` (models on `ToolExecutionApi`/`HookApi`), `76f6c06da` and `636703a0a` (context as of an earlier entry, as an options object), `92216fa15` (the initial system message leads context), `ae92585d3` (settings failures reported), `eba849739` (nested tool calls, structured output, callers; storage errors end the Session), `cb4375493`, `4ac0bd8c7` (one document per nested call, `ownedTasks()`), `e37cdc420`, `ea448f454` and `eb2fce43d` (scheduler indexes, bounded ownership, wait-cycle rejection), `f44707e7f` (`documentRetentionMs`), `e6b9a6af5` (inputs as application entries), `89e5c2c07` (a failed blocking compaction fails the run), `d77d2752e` (a schema tool's error result gives programs its output), `5713c9507` (the system baseline after a compaction that keeps assistant messages), `77332f7a3` (deferred tools and `tool_search`), `6085986e4` with `c4cf7957b`/`790271cd0` netting to its tree (per-model `compaction.modelOverrides`; `ToolControl.terminate` a boolean), `7ec1f648e` (`models`/`settings` as per-use getters), `ea7c64843` and `00bf7193d` (Durable Object hosting timers), `a30c2a81d`, `440a5fcdb`, `5ba1cbaf3` and `91b33cab3` (crash-loop guard, report sources, a failed-run inbox, blocked tasks, typed `ReportInfo` reasons). *Code mode:* `7b597d148`, `127b77121`, `04566631c`, `8bb180bec`, `c661bc574` (`pi-durable/codemode`). *Storage:* `4dd2af42c` (scans in either order), `da866ada1` (in-memory context; its Durable Object SQLite adapter is dropped), `f7e3cb543` (namespaced SQLite, #10937), `c268154ca` (the SQLite drivers move to chord's `./sqlite{,/node,/cloudflare}`). *Telemetry:* `48909d987`'s `HarnessOptions.telemetry`; `2ec55d808`'s `DURABLE_TELEMETRY_SCHEMA`, the `pi.durable.submit`/`pi.durable.phase` spans, stored trace carriers on submissions and tasks, `TaskOptions.trace`/links, the phase/active-task/report metrics and `harness/telemetry.ts` — the Go pieces it needs (propagation, links, `NewTypedMetrics`, `SchemaDefinition`) shipped in `061629c`. Riders: `15e7bdc14` (no thinking in the compaction summary), `f11e50f72` (`output.ts`'s linear join), `36a686ee8` (durable task times), `ed94330a2` (the output decoder is `TextDecoder(ignoreBOM)`). **+3 (2026-10-20)** — `5c1736f82`: `checkpointWhen` becomes REQUIRED on `defineDoc`/`defineDocFamily` (`documents.ts`, `types.ts`; no default), so `Transaction` calls it unconditionally; the built-in documents set it — `CodemodeRunDoc`, `CodemodeJournalDoc` and `NestedCallDoc` `() => false` (they retire with their task), `CodemodeStoreDoc` `deltasSinceBase >= 64`. Oracles: `session-definitions.test.ts`, `codemode-checkpoints.test.ts`. `48c8f3228`: chord's `./sqlite/node` gains `NodeSqliteDatabase.exclusive()` (an operation alone on the connection, outside a transaction, rolling back a transaction it left open) and an UNEXPORTED replicated-connection wrapper (`sqlite/replication/node.ts`: automatic checkpoints off, every operation reported, a hook before checkpoint + WAL delete on close). Oracles: `test/sqlite/{node,replication-node}.test.ts`. `d9025a5f5`'s chord half: the new `@earendil-works/chord/telemetry-sqlite` export (`createSqliteTelemetry` over chord's `./sqlite` — settled spans with inline events and links, metrics aggregated per series per 10 s, a 1 s batched flush with retention, a bounded queue that drops and counts — implementing the `TelemetryStore` whose contract and in-memory implementation shipped this cycle in `telemetry/`). It lands with this row's SQLite drivers. Oracles: `test/telemetry-sqlite{,-store}.test.ts` and pi-telemetry's `test/store-conformance.ts`. |
| 11 | ~~**The 2026-09-03 architecture migration**~~ | — | — | **CLOSED 2026-10-19 — every subject it carried was deleted upstream.** `7472cda0d` deleted `packages/{protocol,client,server}` and `coding-agent/src/{client,experimental,cli/experimental}/**` (Go: `83a344c`, ten of the eleven deletions this row listed); `7fd478a2e` deleted the harness (row 8 is durable now); `0104fef1e` deleted chord's service runtime (`d202916`, with `internal/jsonstrict`). The eleventh deletion was a re-port: `cloudflare-ai-binding.ts` (`602450c`, Rulings 2026-08-11). The chord2 host runtime that replaced the services is row **27**; the `services/**` and `experimental/**` deltas recorded here died with their sources. | — |
| 12 | **`SessionManager` / `AgentSession` base port** — `core/session-manager.ts` (1,746 LOC), `core/agent-session.ts` (3,524 LOC) | 5,270 LOC src | ~6 as hunk-ports, ~16 as base ports | **OPENED 2026-09-03.** The port has no `SessionManager` class: `SessionRecorder` is an append-only writer keyed to a file, `SessionTree` a read-only parser; there are **no label entries** (`grep -c label coding/session.go coding/session_store.go coding/session_tree.go` = 0; the only `coding/session*` hits are a fixture and comments in `session_tree_test.go`), no `createBranchedSession` fork writer, no `inMemory` constructor. Read-side version migration is NOT a gap: `coding/session_store.go` `readSessionEntries` → `migrateSessionEntries` ports `migrateToCurrentVersion` V1→V2→V3 on both read paths. `coding/session.go` has `WaitForIdle` but no `IsIdle` and no compaction/branch-summary abort controllers. **`core/session-manager.ts` is PARTLY ported — split it by HUNK (2026-09-16):** its static discovery surface is `coding/session_store.go` — `getDefaultSessionDir` → `DefaultSessionDir`, `list` → `ListSessions`, `LatestSession`, `findById` → `FindSessionByID` (`57a3304`), `readSessionHeader`/`readSessionHeaderForDiscovery`, the `list`/`findById` cwd-filter prologue (`sessionListingDir`, `sessionCwdMatches`), plus `generateId`, the safe-path encoding and the migrations — so a hunk on that half is an ordinary `port` (the row-8 lesson: this cycle's draft parked `findById` here first, and the adversarial pass caught it); only hunks on the class itself (labels, fork, `inMemory`, tree navigation, compaction/abort state, `SessionInfo`'s full shape) queue here. Known remainder inside the ported half, carried by this row: `SessionInfo` is a five-field shape, and `ListSessions` orders by header timestamp where pi's `list()` orders by `modified` (last message activity → header → mtime). | **8** — `2631b25c3` (preserve compaction boundary when forking: `firstKeptEntryId` re-chained through removed labels), `2b768ba42` (`inMemory(cwd, options, entries)` + `_loadEntries`), `bea67d90d` (abort cancels compaction + branch summary; `isIdle` includes compacting), `e266507b6` (drop duplicate `auto_retry_end` event type), `e44d75c20` (2026-09-04: branch-summary output cap `maxTokens: 2048` → `Math.min(4096, model.maxTokens > 0 ? model.maxTokens : Infinity)`, plus the `reserveTokens` doc correction — it reserves for *history selection*, not for prompt+response. No Go base: `coding/compaction.go` ports the COMPACTION summarizer (`summaryMaxTokens(frac, reserveTokens)`), a different function; `generateBranchSummary` itself is unported and its only `src` consumer is `core/agent-session.ts`. The port renders branch summaries (`coding/messages.go`, `coding/session_tree.go`) but never generates one), `e687434a6` (2026-09-07: `navigateTree` gains an `isCompacting` guard beside its existing `isStreaming` one, throwing "Wait for the current compaction or tree navigation to finish before navigating the session tree." No Go base: the port has no tree-navigation API at all — no `Navigate`/`SetLeaf`/`ResetLeaf`/`Branch` surface and no compaction-state flag in `coding/`). `c37b0e03b` (2026-09-09: `_scheduleAutoRetry`'s delay becomes `retryDelayMs(settings, this._retryAttempt)` instead of the inline `settings.baseDelayMs * 2 ** (this._retryAttempt - 1)`, so the AgentSession auto-retry inherits the 60s cap. No Go base: `grep -rni --include='*.go' -e auto_retry -e retryPolicy -e retryCallbacks coding/ agent/` returns nothing — the port has no AgentSession auto-retry loop. The shared helper it now calls DID ship this cycle as `ai.RetryDelayMs`, so this delta is the call site only), `46bde88a1` (2026-09-07: the four `settingsManager.getCompactionSettings()` call sites become `getCompactionSettings(model)`, so compaction budgets resolve against the CURRENT model at each call rather than once. The resolution itself is host surface — `core/settings-manager.ts`'s new `compaction.modelOverrides` table, E1, and the port has no settings reader (`grep -rn SettingsManager --include='*.go'` finds three comments and no type) — but the CALL SITES are `core/agent-session.ts`, this entry's tree, and the portable semantic is that pi RE-RESOLVES per call where `coding/compaction.go` freezes `CompactionSettings` at `EnableCompaction` while reading `s.Model.ContextWindow` live. Filed here rather than `n/a` on this file's own rule that a queued tree is never `n/a`; that is not in tension with 2026-09-09's `faa9863cb` ruling, which turned on a provable no-op — the whole path there was gated on an extension runtime the port lacks, and nothing gates this one). **Counter corrected 2026-09-08: this cell read `**4**` while enumerating FIVE deltas** — `e44d75c20` was appended by `cb254e1` without bumping the number — so appending `e687434a6` took it to 6, not 5; the 2026-09-10 cycle's two took it to 8. **+2 (2026-09-20), taking it to 10.** `de2de549b`: compaction cancellation races. `abort()` now sets `_agentRunAbortRequested` while a run is active, and every continuation point in `_runAgentPrompt`/`_handlePostAgentRun` consults it, so an abort during a retry or an auto-compaction stops the loop instead of being overtaken by the next continue; `_willRetryAfterAgentEnd` returns false outright; the summarization auth call takes the compaction `AbortSignal`; and `aborted` is decided by `signal.aborted \|\| cancelledByExtension` rather than by STRING-MATCHING the error message, which is the part worth carrying — the port would inherit the same fragility if it copied the old shape. `3c75b2747`: bug reporting adds `AgentSession.summarizeForBugReport` (asks the current model to describe what went wrong, for a user who declines to share the transcript) and splits `serializeSessionBranch` out of `exportSessionToJsonl`. The rest of that commit is E1 — `modes/interactive/bug-report.ts`, the `/bug` slash command, `utils/zip.ts`, `core/crash-log.ts` and the upload — and the port has no session export at all. **+1 (2026-09-22)** — `466db0fec` "canonical session context boundaries", the SessionManager/AgentSession half. **Shipped this cycle on the static half:** `context_edit` entries, retain-none compaction and the newest-compaction-only rule in `SessionTree.BuildContext`/`BuildProjection` (`fb1f9df`), and `estimateTokens`' system case. **Queued here:** `SessionManager.appendContextEdit` (validation: target on the active branch, editable role, string→text-block normalization for assistant/toolResult), `appendCompaction(summary, null, …)` storing its own id, the fork-path re-chaining of a self-referencing `firstKeptEntryId`, `ReadonlySessionManager.buildSessionProjection`; `compaction.ts`'s projection-based `prepareCompaction` (`findProjectedCutPoint` with the recovery-omission suffix rule, `estimateProjectedContextTokens` invalidating usage after a later `context_edit`/compaction); and `agent-session.ts` making the SessionManager projection canonical for every provider request via `agent.prepareRequest` (assigning `agent.state.messages` no longer changes history), turn-end boundary drafts committed through `agent.finishTurn`, and post-run recovery omitting the failed attempt with a persisted `context_edit` instead of slicing agent state. The agent hooks it builds on (`finishTurn`, `prepareRequest`, `peekQueuedMessages`) SHIPPED (`5c6af5a`). The extension boundary events (`turn_end` result, `agent_before_settle`) are E1. **Two estimate gaps this entry must close when the cut-point rework lands** (measured 2026-09-22 against pi 0.87.0): (a) `SessionTree` converts `bashExecution`, `branchSummary` and `compactionSummary` to user messages at load, so `EstimateMessageTokens` weighs the wrapped text where pi's `estimateTokens` weighs the raw fields (6/30/32 vs 3/5/5 tokens on the reviewer's fixtures) — `BuildProjection`'s per-entry messages carry the converted form, and `findProjectedCutPoint` will consume them, so carry the raw role's weight through the projection first; (b) without trusted usage the port counts every system patch, where pi's `estimateProjectedContextTokens` counts only `getCurrentSystemMessage(projection.messages)` once. **+ (2026-09-23)** — **Shipped ahead of the base port, from this cycle's reviews:** a compaction aborted mid-summary is no longer checkpointed (pi's `throwIfAborted()` before `appendCompaction`; `224fd46`); resuming a session file resumes its compaction (`Session.LoadBranch`; `edc0727`, `11ebece`); and the invalidation half of `estimateProjectedContextTokens` — usage recorded before the latest compaction or `context_edit` is not trusted, and without trusted usage the current system message counts once (`9ad9374`), which closes estimate gap (b) on that path; gap (a) stays open. **Queued here — compaction trigger timing** (measured 2026-09-23 against 0.87.1 by the round-4 review): pi checks at three sites — before the prompt lands, on the last assistant's usage alone; at `agent_end`, on the final assistant's usage (`_checkCompaction`); and between turns, in `_compactBeforeNextAssistantResponse` via `prepareNextTurn` — while the port checks only in the per-request transform, after the new prompt and its system declaration are in the transcript. Measured effects: a large prompt that tips the estimate compacts one turn early and moves the cut; a threshold crossed at the end of a run compacts at the next request with the new prompt already inside, so the cut can land on it; a switch to a model with a larger window right after the threshold skips a compaction pi performs (32 messages sent where pi sends 10); a thinking-level change after the threshold makes the summarization requests carry the new level. With small prompts the bytes are identical. Fix shape: mirror the three sites and keep the transform only to re-apply the checkpoint, locked by a live pi-vs-Go capture whose prompts are big enough to move the cut; lock two estimate mutants with it (the current system message dropped from the estimate, and usage distrusted exactly at the boundary). Drains with the `agent.prepareRequest` item above. **+1 (2026-09-24)** — measured at `a328aa89a` `agent-session.ts:1677-1690`: `AgentSession.prompt()` throws before any streaming when neither `hasConfiguredAuth(provider)` nor `checkAuth(provider)` passes — `Authentication failed for "<p>". Credentials may have expired or network is unavailable. Run '/login <p>' to re-authenticate.` for OAuth, `formatNoApiKeyFoundMessage(provider)` otherwise. `coding/` holds neither text. So pi reaches `a328aa89a`'s chat assertion from `prompt()` only once auth is configured, while the port's session chat guard (`6e78ccc`, `sessionStreamFn`) runs for any session. **+2 (2026-09-27)** — `e473b5cd8`: `prompt()` reports how an accepted prompt was dispatched through `preflightResult(disposition)` — `"handled"` (an extension command, or input an extension handler consumed), `"queued"` (steered or followed up while streaming) or `"started"` — and no longer calls it on a rejection, which still throws; `steer()` and `followUp()` return a `QueuedInputDisposition` (`"handled" \| "queued"`). `coding.Session`'s `Steer`/`FollowUp` pass messages to the agent's queues and are not this text-input API, so nothing in `coding/` changes; the RPC consumers are host (E1). `ff72faba2`'s `createBranchedSession` half: a fork writes its file at once iff the branched path already holds a user or assistant message (`_hasConversation`, the rule `_persist` uses, which `SessionRecorder` ships since `c0687e6`), otherwise the first such message creates it. Oracles: `tree-traversal.test.ts` › 'does not duplicate entries when forking from before the first user message' and 'writes file immediately when forking at a user message'. **+5 (2026-10-19)** — `27c7b6ff4`: the user `!` command strips an ANSI sequence split across output chunks (`core/bash-executor.ts`'s `executeBashWithOperations`, `utils/ansi.ts`'s `splitIncompleteAnsiSuffix`; Go has neither). `503c60552`: `agent_settled` gains `aborted: boolean` (Go has no `agent_settled`). `a447ba11f`: `_startRun`/`_prepareRun` give `sendCustomMessage({triggerTurn})` the same setup as `prompt()` — `before_agent_start` fires for custom-message runs, the session is busy from setup, held messages flush after preparation, and an abort during setup aborts at `agent_start`. `6fcb48670`'s branch-summary half: `generateBranchSummary` sets no `maxTokens` (the `e44d75c20` cap above is gone). `8e53e0e49`'s session half (pre-pin): `_runDefaultCompaction` passes `settingsManager.getRetrySettings()` (enabled by default; `maxRetries`, `baseDelayMs`, `maxAgentDelayMs` from `SETTINGS_DEFAULTS`) and `_summarizationRetryCallbacks` (`summarization_retry_scheduled`/`_finished` events). Its compact() half shipped (`8c84cb3`: `CompactionSettings.Retry`/`RetryCallbacks`, nil = no retry), and `DefaultCompactionSettings().Retry` is pi's `SETTINGS_DEFAULTS.retry` (`24adc3e`, `0eba718`); what stays here is a *configured* retry setting (`getRetrySettings` with user overrides) and the `summarization_retry_*` events. pi passes `sessionId: undefined` there; Go's `CompactionSettings.SessionID` stays compact()'s parameter. |
| 13 | **Edit-tool result `details`** — `core/tools/edit-diff.ts` (structured diff + unified patch + `firstChangedLine`) | 556 LOC | ~1 | **OPENED 2026-09-03.** pi's edit returns `details: {diff, patch, firstChangedLine}` (`core/tools/edit.ts:211`); the Go edit tool returns a bare `textResult("Successfully replaced %d block(s) in %s.")` with no details (`coding/tools.go`). Surfaced while auditing every `Details` site during the write-tool port. Not a one-liner: it lands a new structured payload on the session entry and the server bridge, so it carries its own goldens. Slice: port `edit-diff.ts` as `coding/editdiff.go` with parity tests against pi's diff/patch text, then wire it into the edit result. The ~500-line matching/apply engine is byte-identical between upstream's two forks, so it is shared (`internal/`), not duplicated. | — |
| 14 | ~~**`assistant-message-frame.ts`**~~ | — | — | **CLOSED 2026-10-19** — `b39979799` deleted `utils/assistant-message-frame.ts` and its root export. The port never had a frame encoder, so there was nothing to delete; its two deltas died with it. | — |
| 15 | **Mirror pi's attribution placement** — `core/provider-attribution.ts` is homed in `ai/providers/attribution.go`, and pi's new `providers/opencode-headers.ts` has no Go counterpart | — | ~1 | **OPENED 2026-09-09.** Deliberately a queue row and **not** a Divergences row: a recorded divergence makes every future finding against it refuted-by-default, and this is fixable work with a known shape. pi splits this surface in two, and `561a2e066` is what made the split load-bearing: the **ai** package injects `x-opencode-session` from `options.sessionId` for the opencode/opencode-go providers alone (`providers/opencode-headers.ts`), while `x-opencode-client`, the `opencode.ai` host gate and the whole default-attribution bundle stay in **coding-agent** and reach the request through `transformHeaders` (`core/sdk.ts:331` → `core/provider-attribution.ts`). The port collapses both halves into the ai layer. That is precisely why `561a2e066` needed no Go change — the port had already closed the gap upstream was closing — and it is why three cells still differ on the **bare-SDK** path, all in the same direction (the port sends MORE than pi's ai package): it also sends `x-opencode-client`; it also honours the `opencode.ai` host gate; and it puts the session header BELOW `model.Headers`, where post-commit pi's bare path puts it above. A fourth, narrower cell — a caller's case-variant deletion marker suppressed the header on the three `defaultHeaders` adapters but not on google — **CLOSED 2026-09-24** (`a328aa89a`'s case-folding `providerHeadersToRecord`, ported in `44bf5a7`; `TestOpenCodeSessionHeaderCallerDeletionMarker` now asserts google too). **None of the three is reachable from `coding.Session`**, which matches pi's coding-agent path exactly. With one Go function standing in for two pi paths that now disagree, the port matches whichever path its own consumers take — and that is the right one to match, which is why this is a ~1 and not urgent. The slice: move the attribution bundle into `coding/` behind the `TransformHeaders` seam the port already has (`ai/models_runtime.go`), leave the ai layer holding only the opencode session header keyed on provider identity, and keep `ai/providers/opencode_provider_headers_test.go` green throughout. **Landing it changes a recorded Go-home mapping** — the `pi-triage` skill's table sends `core/provider-attribution.ts` → `ai/providers/attribution.go` — so fix that table in the same commit, per this file's own rule that the test wins and the list follows. Deferred out of the 2026-09-09 cycle under the 2026-09-02 boy-scout **boundary**, not against the rule: the repair changes a subsystem's shape and carries every provider's header order as its parity surface. | — |
| 16 | **Context-overflow detection** — `packages/ai/src/utils/overflow.ts` | 180 LOC / 1 file (at `16292398a`) | ~1 | **OPENED 2026-09-18.** `isContextOverflow` and its `OVERFLOW_PATTERNS` / `NON_OVERFLOW_PATTERNS` decide whether a failed assistant turn was a context overflow — the signal that drives compaction rather than a retry. The port has **no Go base**: `grep -rn --include='*.go' -e context_length_exceeded -e 'too many tokens' -e 'token limit exceeded' ai coding agent` is empty, and `coding/compaction.go` triggers on its own estimate alone, never on a provider's refusal. So the port cannot react to a provider that says "too long" when the estimate said otherwise. Root-exported (`export * from "./utils/overflow.ts"`), no dependency question, and the patterns are the same JS-RegExp-under-RE2 translation `ai/retry_classify.go` already solves with `buildProviderErrorPattern` — so this is a small, well-shaped slice, not a research problem. Land it beside `retry_classify.go`, not inside `coding/`. | **1** — `661619e87` (2026-09-18: the Cerebras bodyless-400/413 pattern moves OUT of `OVERFLOW_PATTERNS` into its own `CEREBRAS_BODYLESS_OVERFLOW_PATTERN`, applied only when `message.provider === "cerebras"` (#9482). A bodyless 400/413 from any other provider stops counting as an overflow. Port it WITH the base, not after: shipping the base without it would build in the bug upstream just fixed.) **+1 (2026-09-20)** — `0e283203c`: `OVERFLOW_PATTERNS`' first entry widens from `/prompt is too long/i` to `/prompt (?:is )?too long/i`, so z.ai's `{"code":"1261","message":"Prompt too long"}` is recognised as an overflow instead of a hard failure. z.ai also overflows SILENTLY on other paths (`usage.input > contextWindow`), which the same file handles; the doc comment is updated to stop claiming z.ai never errors. Land the widened pattern with the base port — it is one character class, but it is the whole value of the row for z.ai users. **+1 (2026-09-22)** — `466db0fec`: `_checkCompaction` now treats an explicit `stopReason: "error"` overflow as recoverable only while the assistant is still projected (no later `context_edit` hid it), and the recovery omits the attempt with a persisted `context_edit` (entry 12) rather than dropping it from agent state. Land together with entry 12's recovery half. **+1 (2026-09-30)** — `3dd803d7e`: `OVERFLOW_PATTERNS` gains `/prompt exceeds max length/i`, Z.AI's CN endpoint's `{"code":"1261","message":"Prompt exceeds max length"}`. Oracle: `overflow.test.ts`. **+1 (2026-10-20)** — `73f8de80b`: `OVERFLOW_PATTERNS` gains `/^(?:[\w -]+ API error \(413\):\|413(?:\s\|:\|$))/i` — an HTTP 413 is a request-size overflow whatever the provider or body (GitHub Copilot answers an oversized request with 413 "failed to parse request"), matched on a raw SDK error's leading status or `formatProviderError`'s `<provider> API error (413):` prefix, never on a 413 mentioned in the body. Oracles: `overflow.test.ts`, `github-copilot-request-size-overflow.test.ts`; the coding-agent regression `copilot-request-size-overflow.test.ts` lands with entry 12's recovery half. |
| 17 | **Prompt cache warming** — `core/cache-warmer.ts` (437 LOC) plus its wiring: `core/cache-stats.ts`, `core/usage-totals.ts`, and the `createAgentSession` streamFn refactor in `core/sdk.ts` (`provider-composer.ts`'s `promptCache` merge was listed here until 2026-09-24; it is the models.json layer, E1 — see the out-table) | ~500 LOC src | ~1.5 | **OPENED 2026-09-20** (`c596d09d9`, rider `3390bd936`). pi issues SPECULATIVE provider requests to keep a prompt cache alive between turns, driven by the new `Model.promptCache` lifetimes. The data half SHIPPED this cycle (`120e5d9`: `ai.ModelPromptCache` + `Model.PromptCache`, and the 0.86.1 catalog carries it for 14 anthropic models), so the port reads the lifetimes and does nothing with them. The warmer itself has no Go base: `grep -rn 'CacheWarmer\|cache_warm' --include='*.go'` returns nothing, and its collaborators are absent too — no `UsageTotals`, no `CacheStats`. **Not E1**, and that is the call worth recording: its consumers are mixed (`core/sdk.ts` and `core/agent-session.ts` alongside `settings-manager`, `model-registry` and the interactive footer), so the only-consumers clause does not fire and the default is IN. It changes what requests reach the provider, which is SDK behaviour, not presentation. `3390bd936` is already a bug fix on it — a late refresh must be skipped once the context it was warming has moved on — so expect the upstream shape to keep moving; take it after entry 16, which is smaller and unblocks compaction. | **1** — `3390bd936` (skip late cache-warming refreshes) **+1 (2026-09-30)** — `9a100c7cc`: `combineUsage` moves from `compaction/compaction.ts` to `usage-totals.ts`, exported (compaction imports it; behaviour unchanged), and becomes the adder for nested-call usage (row 20). |
| 18 | **A model on the tool execution context** — pi's `AgentTool.execute` receives a ctx carrying the request's `model`; the port's `agent.AgentTool.Execute(ctx, toolCallID, params, onUpdate)` does not | ~1 | ~1 | **OPENED 2026-09-20** (`f5c946480`). The read tool reads `ctx?.model?.inputLimits?.images?.resize` to pick an image resize profile, and pi's own `ReadToolOptions.resizeOptions` exists as the fallback for a context with no model. The port shipped that fallback this cycle (`c87df16`) as an `imageResizeFn` getter installed by `NewSession`, read per call so a mid-session `SetModel` still reaches the tool. **The second reader has arrived:** `getNonVisionImageNote` (`2f4f283cc`, pre-pin) also reads `ctx.model`, and was missing until `418b74e` generalized the getter to the request's model (`toolModelFn`, `Session.currentModel`). The read tool now reads two things off the model through a bespoke getter, which by this row's own measure is the point where widening `Execute` is the better trade. Cheap, and an API break the mirror ruling already permits. | — |
| 19 | **pi.dev remote model-catalog overlay** — `core/remote-catalog-provider.ts` (`withRemoteCatalog`, the stored-overlay restore, the ETag/Last-Modified refresh protocol, `?types=` negotiation, `minimumPiVersion`) | ~250 LOC src at `a328aa89a` | ~1.5 | **OPENED 2026-09-24.** Filed `n/a` since 2026-07-17 under E1's only-consumers clause, whose premise is false at `a328aa89a`: its sole importer is `core/model-runtime.ts`, which `createAgentSession` (`core/sdk.ts`, SDK) defaults via `ModelRuntime.create`, and `ModelRuntime.create` wraps every built-in except radius and runs `refresh({allowNetwork:false})`, which publishes the restored stored overlay; the networked path is `ModelRuntime.create`'s `refreshFromNetwork` (its earlier `experimental/services/models-provider.ts` caller died with `7472cda0d`). Mixed consumers, so the clause does not fire and no other E1 category fits a catalog fetch. No Go base (`grep -rn --include='*.go' -e 'api/models/providers' -e withRemoteCatalog -e parseCatalog ai coding agent cmd internal` is empty apart from the attribution HTTP-Referer); the `ai.ModelsStoreEntry.{Etag,LastModified,CheckedAt}` seams are already ported and latent. Wire note: `url.searchParams.set("types", "chat,image,classifier")` percent-encodes the commas (`types=chat%2Cimage%2Cclassifier`), which `url.Values.Encode` reproduces and a hand-built query does not. Oracles: `remote-catalog-provider.test.ts`, `model-catalog-protocol.test.ts` (`7fd564cbb`: the `?types=` request, the 307 to `&pi-version=<VERSION>`, per-`minimumPiVersion` selection). | **1** — `a328aa89a`: type-aware overlay — `REMOTE_CATALOG_MODEL_TYPES = ["chat","image","classifier"]`, `parseCatalog` keeps entries whose `type` is undefined or a listed string (drops `"video"`; drops `type: null`, unlike pi-ai's `getModelType`), `mergeModels` replaces by `(getModelType, id)`. **+1 (2026-10-01)** — `c34f2d6ad`: `mergeModels` builds a `Map` keyed `` `${getModelType}\0${id}` `` over `[...baseline, ...dynamic]` — positions match the old replace-in-place, but duplicate keys inside `baseline` now collapse to one entry (the last wins, at the first's position). **+2 (2026-10-19)** — `ee409a85f`'s session half: `withPersonalizedCatalog` (`remote-catalog-provider.ts`) and `model-runtime.ts`'s OpenRouter branch; the ai half shipped (`7f6069b`: `providers.PersonalizeOpenRouterCatalog`, `RefreshModelsContext.BaseURL`). `a3f265b1c`'s `scripts/publish-model-catalog.mjs` raises `MINIMUM_PI_VERSION` to 1.2.1: the overlay serves the kimi-coding Chat Completions catalog only to clients declaring at least 1.2.1, a bar the port's `pi-version` must clear when this lands. |
| 20 | **Tool orchestration core** — the general mechanisms `8562bcf66` adds for tools that call tools: `ToolExposure` (`direct`, `model-only`, `codemode`, `deferred`, `hidden`) and `defaultActive`; `prepareLoadout` and the hidden-declarations projection over the transcript's system messages; `ctx.executeTool()` through `core/nested-tool-calls.ts`'s `NestedToolCallRunner` (bounded `nestedCalls` recorded onto the calling tool's result message at `message_start`; `parentToolCallId` on nested `tool_execution_*` events and `tool_call`/`tool_result` hooks); `getCallableToolNames`; `setActiveToolsByName`'s dedup and hidden filtering; loadout restore from the transcript through `setActiveToolsByName` | ~550 LOC src at `4259686d9` (`nested-tool-calls.ts` 238 + the `agent-session.ts` hunks + the `ExecuteToolOptions`/`ToolLoadout`/`ExtensionToolContext` types) | ~2 | **OPENED 2026-09-29** under the ruling of that date. Rows 21-23 stand on it. The agent-core half shipped this cycle (`7559a04`: `RunToolCall`, `AgentToolCallOutcome`, structured content, `IsError` results, `OutputSchema`), as did the `nestedCalls` record type and its compaction reader (`58ad297`), so this row is the session half only. No Go base: `grep -rn --include='*.go' -e Exposure -e prepareLoadout -e NestedToolCallRunner -e parentToolCallId coding agent` is empty. **Carries a Go-only requirement from the review of `7559a04`:** `agent.RunToolCall` runs its hooks on the caller's goroutine, outside the parallel executor's `serialMu`, so nested hook bodies must be serialized with the loop's own (pi gets this free from its single thread). A lock private to the runner is not enough — `serialMu` is a local of `executeToolCallsParallel` — so either the session installs mutex-wrapped hooks on the Agent and hands the same wrappers to `RunToolCall`, or agent core exposes its lock. Nested `tool_execution_*` emits take the same lock (the loop serializes its listeners through `safeEmit`). Hold it around hook bodies and emits only, never across a nested tool's `Execute`, which can itself call tools and would deadlock a non-reentrant mutex. Serializing whole hook bodies is stricter than pi, which interleaves at awaits (a pending permission prompt would stall sibling tools), and matches what the loop already does for `AfterToolCall`. The refuter's `-race` repro — two parallel tools calling `RunToolCall` with the agent's hooks — is the test to start from. | **+1 (2026-09-30)** — `9a100c7cc`: `NestedCallRecorder.addUsage` sums the `usage` of every nested result, calls dropped from the record included; `takeRecord` returns `{calls, usage}`; `AgentSession` adds that usage onto the calling tool's result message at `message_start` (`combineUsage`), so the session's cost counts codemode's nested calls. Oracles: `nested-tool-calls.test.ts`, `suite/agent-session-codemode.test.ts`. **+1 (2026-10-01)** — `028c0ec56`: tools in `_hiddenDeclarations` are dropped from the system prompt's tool snippets, both in `_rebuildSystemPrompt` and in the per-request section diff, so the `<tools>` list matches the declarations the request carries (codemode `only`). Oracle: the commit's `agent-session` codemode test. **+6 (2026-10-19)** — `c662ec7e3`: restored or reloaded loadout tools that are not registered yet wait in `_pendingToolNames` and activate when registered (dropped when `setActiveToolsByName` deactivates them or the next run starts); `_restoreToolsFromTranscript` goes through `_setActiveTools`. `6f2f164ab`: resume restores the transcript's loadout when `usesDefaultTools`, applying `defaultToolModifiers` (#10887). `04b97ef00`'s `_isActivatable` (MCP tools stay activatable under an allowlist; `toolNameMatcher` shipped, `602d46e`). `c30840c2e`'s session wiring: `_hiddenDeclarations` feed `hiddenTools` into `_rebuildSystemPrompt`, and `ToolLoadout.getPromptGuidelines` (`BuildSystemPromptOptions.HiddenTools` shipped, `51cba15`; `NewSession` sets none). `36a686ee8`'s `nested-tool-calls.ts` rider (nested results carry `durationMs`). `24382bedd`: `reachesTools(input)` passed through on tool info. |
| 21 | **MCP** — `packages/mcp` (`@earendil-works/pi-mcp`: JSON-RPC client, stdio and streamable-HTTP transports, OAuth discovery/flow/provider, protocol content types, in-memory transport and `./testing`) and `extensions/mcp/**` (config from global and trusted-project `mcp.json` plus `pi.registerMcpServer`, lazily loaded runtimes, tool exposure per server, resources, OAuth token storage, log), `core/mcp-servers.ts` (registry and config validation), `core/tools/truncate.ts`'s `truncateMiddle` (its only consumer is `extensions/mcp/tools.ts`), and `mcp.json`'s project-trust gate (`trust-manager.ts` lists it as trust-requiring) | 3,020 LOC / 18 files (`packages/mcp/src`) + 2,742 of `extensions/mcp`'s 3,586 (`ui.ts`, `cli.ts`, `cli.lazy.ts` are E1) + 237 + 38, at `4259686d9` | ~6 | **OPENED 2026-09-29** under the ruling of that date. Shape: a Go package mirroring `packages/mcp`, the extension's logic mirrored file for file, a thin session option on top. `cross-spawn` → `os/exec`, no dependency. `extensions/mcp/index.ts` splits by hunk. After row 20. | **1** — `1d74741e1`: MCP OAuth refreshes are serialized across processes, because rotating refresh tokens (Cloudflare) were lost when two pi processes refreshed at once. A refresh holds a per-server lock file (`proper-lockfile` on `mcp-auth-refresh-<sha256(key)[:16]>` in the agent dir; stale after 20s, waits up to 25s at 100ms retries) from reading the tokens to saving new ones, reuses tokens another process already refreshed, bounds each refresh request to 15s, and closing a connection waits for a running refresh (`McpAuthProvider.settled()`; `extensions/mcp/{oauth,runtime}.ts`). Oracle: `mcp-oauth-refresh.test.ts`. **+1 (2026-09-30)** — `4df157433`: MCP's OAuth callback (`packages/mcp/src/oauth/callback.ts`, `extensions/mcp/oauth.ts`) uses the shared callback server and sign-in page (row 1). Oracle: `packages/mcp/test/oauth.test.ts`. `5257d0d5f`'s `renderCall`/`renderResult` on MCP tools are host rendering (E1). **+10 (2026-10-01)** — `7ef68d4f2` (Streamable HTTP calls `fetch` without a receiver), `7c9fe6645` (custom OAuth `clientName` per server), `b29db895c` (MCP tool names aligned with codemode identifiers; its `core/extensions/loader.ts` hunk is E1), `91f9f3b5d` (a server can authenticate with a provider login: `auth: {provider}` in config, `runtime.ts` resolves the provider's credential), `e029c3ed0` (the first prompt stops waiting for MCP servers to connect; `extensions/mcp/index.ts` +215, with codemode and tool_search adjusting to late tools), `1c7e7df76` (codemode lists MCP servers rather than their tools; `core/mcp-servers.ts`, `extensions/mcp/{index,tools,config}.ts`), `d850edee9` (`oauth.authServerMetadataUrl` and the RFC 9207 `iss` check — `packages/mcp/src/oauth/{discovery,errors,flow,types}.ts`), `8ce69e9d2` (empty and null optional OAuth fields read as absent), `e529a82c9` (step-up authorization keeps granted scopes), `5806068c2` (OAuth credentials stored per server name AND URL). `a4715ec9b` adds upstream's run of the official MCP client conformance suite in CI — the oracle to start from. `0582d9c11`'s result previews and `41169ba2a`'s clickable login link are E1. **+10 (2026-10-19)** — `1499466d8` (opt-in OAuth CIMD: `oauth.clientRegistration: "dcr"\|"cimd"`, exact redirect-URI acceptance), `1387af7b4` (a trusted project `mcp.json` overrides a global server's `enabled` and exposure; keep the trust gate), `147b50281` (`application_type` in OAuth DCR), `8c911797c` (shutdown cancels connecting servers and their retry sleeps), `2db5e359b` (connection attempts, `getClient` resolving the current connection, serialized reconnects), `f10993bc7` (cancellable OAuth sign-ins; no refresh after close), `c5f5b3282` (**security**, the RFC 9207 mix-up: reject `iss` when no AS metadata was discovered — land it with the OAuth flow), `f4cca2dc2` (no resource listing on connect), `67996ab75` (`structuredContent` is any JSON — Go's `StructuredContent` must admit arrays and null), `da0eadf04` (a per-request signal on `McpTransport.send`; no retry of a cancelled request after an auth refresh). Riders: `6f1072cc0` (`renderServersSection`'s intro per reach, `serversSectionIntro`), `d677d0ee7` (`saveToTempFile` through `writeOutputFile`; `createOutputFile` shipped, `1a5f453`), `04b97ef00` (`isMcpToolName`, `_allowlistFiltersMcp`: an allowlist with no `mcp__` entry keeps MCP tools registered for codemode and tool_search, so `isAllowed` gains pi's third arm), `24382bedd` (activation driven by `reachesTools`, namespace aliases, a self-describing result schema). **+1 (2026-10-21)** — `bdd9f0399`: `extensions/mcp/config.ts` strips a UTF-8 BOM (`stripBom` from `utils/text.ts`, already ported as `coding/text.go` `stripBOM`) before `JSON.parse` on both reads: `readConfigFile` and `editMcpServers` (#11191). Oracle: `mcp-extension.test.ts` › the BOM case. |
| 22 | **codemode** — `packages/codemode` (`@earendil-works/pi-codemode`: a QuickJS wasm VM in a worker, the host/worker protocol, tool declarations rendered for scripts, source handling) and `extensions/codemode/**` (the Codex-compatible `codemode` tool, `on`/`only` modes, per-session store, catalog and classifier access from scripts, large results to a JSON temp file) | 1,579 LOC / 10 files + 933 of `extensions/codemode`'s 1,047 (`renderer.ts` is E1), at `4259686d9` | ~4 + submodule | **OPENED 2026-09-29** under the ruling of that date. Dependency pre-decided by the formula (ruling point 5): `github.com/tetratelabs/wazero` running the same `quickjs-wasi` wasm, in a `codemode` submodule; a CONSULT only if the slice meets cgo, a lost target or a shape change. Needs row 20's nested calls. | **+1 (2026-09-30)** — `9a100c7cc`: codemode reports its classifier usage and its nested tools' usage in the session cost (`extensions/codemode/{execute,tool}.ts`; `renderer.ts` is E1). `1ff5b6fdd`'s 1 MiB structured bash output, which codemode scripts receive, shipped (`9c75fa1`). **+2 (2026-10-01)** — `d2931ad3d`: the prelude's `image()` validates base64 and detects the image type (`runtime/prelude-source.ts`); `1c7e7df76`/`e029c3ed0`'s codemode halves (`extensions/codemode/{execute,tool}.ts`: MCP servers listed instead of their tools, late-connecting servers). `4a42f8faf`'s `workerUrl: string \| URL` exists for Bun's compiled binary and has no meaning under wazero (dropped; its `config.ts` half is packaging, E1). **+14 (2026-10-19)** — `aab34df65` (`models.generateImages()` for scripts; needs row 3), `319fecb89` (a cap on script output), `b223082bb` (scripts that patch built-ins), `269121616` (lookup helpers described as async), `eb326d265` (separate output items), `748b351fe` (the execution/inline/vm/remote host split and `/portable`; `cloudflare.ts`, a Worker Durable Object module, has no Go subject and is dropped), `0fb1aee74` (deterministic runs: a virtual clock, seeded randomness, `sandbox.start`), `8a9c051f8` (schema constraints as `@` tags in declarations), `427052247` (description: schema tools resolve with a value on failure), `24382bedd` (MCP decoupled from codemode: `MCP_TYPESCRIPT_PREAMBLE` and `mcpStructuredContentSchema` leave pi-codemode), `f11e50f72` (linear output joining), `014873d7d` (an initial empty `details` update before the first nested call), `b6d505cd1` (a `// @options:` line that is not the first throws `CodemodeSourceError`), `3daa20280` (the cancellation prompt kept and bounded). Riders: `6f1072cc0` (`renderToolOutputType`, `prelude-source.ts`, the shrunk prompt), `d677d0ee7` (`image()` output saved to private temp files, labelled `[Image saved to …]`), `c30840c2e` (guidelines in declarations), `ce8972a0e` (classifier images in `execute.ts`). **+1 (2026-10-21)** — `885a564b3`: worker admission in `extensions/codemode/execute.ts` queues instead of failing at the limit: `reserveCodemodeWorker` waits in arrival order for a free slot, settling `reserved`/`aborted` (signal)/`timeout` (`CODEMODE_WORKER_WAIT_MS` 60,000, for a script waiting on another script, e.g. an in-process subagent), `admitCodemodeWorkers` grants freed slots, and the limit is `PI_CODEMODE_MAX_WORKERS` when a positive integer (read per call), else `DEFAULT_MAX_CODEMODE_WORKERS` 2. Oracle: `codemode-worker-admission.test.ts`. |
| 23 | **tool_search** — `extensions/tool-search/**`: a tool that lets the model look up `deferred` tools instead of seeing every declaration up front | 287 LOC at `4259686d9` | ~0.5 | **OPENED 2026-09-29** under the ruling of that date. Needs row 20's `deferred` exposure and loadout. | **+1 (2026-10-19)** — `24382bedd`'s tool-search hunk: activation through `reachesTools`. |
| 24 | **Virtual models** — `core/virtual-models.ts` (`VirtualModelDefinition`, `ModelRoute`, `getBranchSelection`, `VIRTUAL_MODEL_STATE_ENTRY` router state on the session branch), `ModelRuntime.registerVirtualModel`/`unregisterVirtualModel`/`resolveModel` and the routing of direct `streamSimple` calls (credentials withheld from another provider's model, output budget capped to the routed model), `AgentSession`'s per-request routing and `model_change`-based selection, and `sdk.ts` restoring the session model through `getBranchSelection` | ~650 LOC src at `540e174c7` (`virtual-models.ts` 227 + the `model-runtime.ts`, `agent-session.ts` and `sdk.ts` hunks) | ~2 | **OPENED 2026-09-29** (`540e174c7`). IN on reachability: `ModelRuntime` is root-exported and `createAgentSession` takes one, so a virtual model can be registered without the extension API (`pi.registerVirtualModel`, E1). The agent-loop half shipped this cycle (`86fd2a4`: responses record the requested `thinkingLevel`). The `sdk.ts` restore differs from `buildSessionContext().model` only for a virtual selection, so it lands here, not before. Off the spine; independent of rows 20-23. | **+1 (2026-10-01)** — `a0660b174`: `getBranchSelection` scans the branch backwards and returns at the first `model_change` or physical assistant response, looking up at most one model (the last `model_change` before that response) to decide whether a virtual selection holds. Equivalent to the forward scan except that it no longer calls `getModel` per entry. |
| 26 | **agent `streamProxy`** — `packages/agent/src/proxy.ts` (`streamProxy`, `ProxyStreamOptions`, `ProxyAssistantMessageEvent`), root-exported by pi-agent-core (`export * from "./proxy.ts"`) | 393 LOC at `d2e7d8b7c` | ~1 | **OPENED 2026-10-19** (triage of `f284a2460`). A stream function for apps that route model calls through their own server: the server holds the credentials and sends events with `partial` stripped, and `streamProxy` rebuilds the message from them. IN on reachability, no exclusion fires, and no Go home (`grep -rn streamProxy --include='*.go'` is empty); earlier cycles filed its hunks "subsumed, no Go analog" without opening a row. Independent of every other row. (Row 25 was never opened: triage's proposed pi-env row shipped in the same cycle.) | **1** — `f284a2460`: `streamProxy` returns a real `AssistantMessageEventStream` (`createAssistantMessageEventStream()`), so proxied responses get `durationMs` (shipped for the port's own streams, `a1fec17`). |
| 27 | **chord2 host runtime** — `packages/chord/src` beyond the ported value layer: `contract`, `declare`, `cell`, `cluster`, `connection`, `host`, `link`, `loader`, `manager`, `object`, `publish`, `registry`, `reload`, `remote`, `schema`, `state` (replicated state; the renamed `services/state.ts`), `types`, `websocket`, `wire` (frames), `errors` (`ChordError`) and `./node` (host manager, child control, process locks) | 9,189 LOC / 22 files at `d2e7d8b7c` | not yet sized | **OPENED 2026-10-19**, from row 11's chord half — a row of its own because row 11's other subjects are all deleted and this one is new code with a new size, so the title should say what is owed. `0104fef1e` deleted the facet and service runtime the port mirrored (`d202916`) and put this runtime in its place. Go `chord` keeps `context.go` (with its telemetry member, `7fedbfa`), `json.go` and `delta/`. The WebSocket link takes `github.com/coder/websocket`, pre-decided (2026-08-31 Codex ruling), so no consult. `loader.ts`'s `nodeLoader` (`node:module` hooks and dynamic `import()`) has no meaning in Go and is not mirrored (`7d75a0e3e`). chord's `./output` and `./sqlite*` exports are durable's code and ride row 8. Carries the runtime interfaces the old value/wire mirror (D52, pruned) left to the runtime slices: `ReplicatedState`/`MutableReplicatedState`, `ReplicatedStateDelivery`, and the source-backed `replicatedState(source, options)` family (`ReplicatedStateSource`, `…Attachment`, `…Frame`, `…Options`, `AttachedReplicatedState`). **Slice `state.ts` first:** durable (row 8) imports `replicatedState` and `AttachedReplicatedState` from chord's root. | **15** — `0104fef1e` (the runtime itself), `7f022043f` (never keep a connection whose link was already closed), `cd6b2a555` (the process-lock socket path fits `sun_path` on macOS), `90f17f59a` (wire frames declared once, as the schema that validates them; `CHORD_ERROR_CODES`; its `delta/index.ts` widening needs no Go change), `018c87ddc` (large frame payloads deflated on links), `a4d040d30` (a forked child host does not inherit `CHORD2_LOCK`), `db6497c3d` (end-to-end flow control: `ack` frames, `hello.acks`, a call limit per link), `2688aba7d` (concepts renamed — `record.ts` → `object.ts` — object variants, a link protocol version, object lifetimes), `d20c6e966` (declarations by name, void and no-arg methods, per-cell state versions), `00d8d0495` (reload and retirement across hosts), `39294a5e9` (a failed IPC write ends the link), `301617df7` (lifetime `"host"` checked by the type system), `aa9cf15be` (init for state, typed principals, contract placement: protocol 3), `b45e0ebcc` (references hydrate their state before use: protocol 4), `9a3ca68bd` (remote references refuse a foreign ctx). |

Entries 1–6 total **~10 port-cycles**. Entries 7, 11 and 14 are CLOSED
(2026-10-19: their sources were deleted upstream); their rows stay one line
long so older references resolve. Entry 8 (durable) is the spine now that 11
is gone. It needs one slice of entry 27 (chord2) first: replicated state
(`state.ts`'s `replicatedState`, `AttachedReplicatedState`), which durable
imports from chord's root beside `Context`, `JsonValue` and `copyJson`; the rest
of 27 is independent of 8. Entries 16, 17, 18 and 26 are off that spine and depend on
nothing in it — take them in any cycle with room, smallest first: 18 is an
afternoon, 16 and 26 one slice each, 17 about one and a half. Entries 20-24 were
opened by the 2026-09-29 ruling: 20, then 21, 23 and 22 in that order; 24 is
independent.

### Catalog regen checklist

The `port-but-CATALOG-ONLY` queue: `packages/ai/scripts` deltas that land only
when a release tag is crossed and `ai/models_catalog.json` is regenerated from
the matching npm build. **Currently 3 (`d2e7d8b7c`; `fdce48eb4`, 2026-10-20: `openai-codex/gpt-5.5` and `openai-codex/gpt-5.3-codex-spark` leave the Codex list — the backend rejects them for ChatGPT accounts — and `CODEX_SPARK_CONTEXT` goes; `e5fe9777b`, 2026-10-21: models.dev retired models — `TOGETHER_REASONING_ONLY_MODELS` and `TOGETHER_FIXED_REASONING_LEVEL_MAP` go, so Together's compat and thinking map fall through to the toggle profile and `detectOpenAICompletionsCompat` drops `isTogetherReasoningOnly`; Copilot loses `claude-opus-4.6`/`4.7`, `gpt-5.4`/`5.5` from the extended-context set and `claude-opus-4.7`'s thinking override). Drained 20 on 2026-10-19
by the 1.3.0 regen (`797d14f`):** `28eaccb8e`, `c10bfb0d7`, `4665fafb4`,
`c71a496ff` (a no-op, as predicted), `4812cb268`, `a37306d43`'s generator hunk,
`f76c1db66`, `943a10e74`, `a2eef9eb6`, `ce8972a0e`'s generator hunk,
`ce950d78f`, the generator halves of `ee409a85f`, `195120a91`, `60ac3bcc3` and
`dc939913f`, `6374c2baf`, `5712b134e`, and `b06c1f2cf`/`6a1f799b6`/`a3f265b1c`
(a switch, its revert and its reapply). Each was checked as an ancestor of the
v1.3.0 release commit (`git merge-base --is-ancestor <delta> 71023aec1`, per row,
never by log order), the generator log `5806068c2..71023aec1 --
packages/ai/scripts` is exactly those twenty, and each landed as its row
predicted. The measurements are in `797d14f`'s message.

**How the regen is decided, and it is not by reading git.** Catalog data is
generated at publish time and never committed upstream, so a `git diff` over
the delta can never see it (2026-07-30). Execute `JSON.stringify(MODELS)`
against `dist/models.generated.js` in both npm builds and compare. **Verify the
method before trusting the output:** dumping the OLD version must reproduce the
committed catalog byte for byte. On 2026-10-19 that check passed at 867,163
bytes (0.99.2), and a second derivation of the new file, through the build's
`compat.getProviders()`/`getModels()`, matched the 1.3.0 dump byte for byte
(939,772 B). Also re-capture `ai/testdata/envkeys` at every regen
(`capture.mjs`): `TestAPIKeyEnvVarsMatchPi` fails when a catalog provider is
missing from the capture. Re-capture `ai/testdata/builtin-auth` too:
`TestBuiltinAPIKeyAuthNamesMatchPi` walks the capture, so a provider added
since is not checked until it is in it.

**A queued row can be killed by a later upstream change, and this queue is
where that gets noticed.** `d92eb8d4b` queued `supportsToolReferences: true`
onto the Fireworks compat; `9e05370b2` then deleted that key from
`AnthropicMessagesCompat` outright, along with the row's own oracle. Replaying
it would have emitted a key the type no longer has. It was RETIRED, not
drained. **So: verify every queued row against the generator at HEAD before
replaying it** (`git -C <clone> grep -n "<the key>" origin/main -- packages/ai`)
— a row is a description of code that may no longer be there.

| delta | what regenerates |
|---|---|
| `d2e7d8b7c` | `generate-models.ts`: MiniMax-M3.1-Flash added; the MiniMax denylist narrowed to the old models only; MiniMax's adaptive thinking with effort levels (`thinkingLevelMap`, request-body-affecting). Landed after v1.3.0, so the next release's regen drains it. |

**Regen tripwire — BILLING.** `allowedFallbackModels` entries must carry
`provider`. A regen that drops it decodes fine, but `anthropicFallbackModelCost`
(`ai/providers/anthropic.go`) gates on `f.Provider == provider`, so no entry
matches and every fallback-served response is billed at the requested model's
rates. `TestAnthropicCatalogFallbacksAreLive` does NOT catch this — the wire
projection strips `provider`. Check the generator shape by eye after any regen
touching `applyAnthropicAllowedFallbackModelMetadata`. Checked and clean at
1.3.0 (claude-fable-5, 2/2 entries carry it).

**Regen tripwire — thinkingLevelMap is request-body-affecting.** Since
`265a33393` most of the tree derives it from models.dev rather than a
hand-maintained map, so diff it PER MODEL after a regen, not just the model
list. 0.86.1 moved **57** of them, concentrated in fireworks (`6b94ae2ec`) and
google/vertex (`aa50fe778`), and the direction matched each row's prediction. At
0.99.1 it moved 13: seven mistral maps (`dc84c1ac0`), the openrouter aliases
`~anthropic/claude-sonnet-latest` and `~openai/gpt-sol-latest` (off `none` →
null, their targets moved), three radius models (Radius's own catalog) and
cloudflare-workers-ai `gpt-oss-20b` (models.dev reasoning options). At 1.3.0 it
moved 24, among them xiaomi's on/off maps (`6374c2baf`) and claude-opus-5 and
claude-haiku-5-5 losing `off: null` (`dc939913f`), which made `off` selectable
and needed that commit's runtime half (`24b908d`) to send disabled thinking.

**Regen tripwire — a hard-coded generator entry can be displaced by models.dev.**
The generator hard-codes a model "until models.dev includes it"; once models.dev
lists it, the models.dev entry wins and the hard-coded overrides no longer
apply, so upstream's CI (which regenerates from live models.dev) and the
published build can disagree. **The build wins**, and a Go test that mirrors
upstream's expectation fails by design until it follows the build. Recorded
case: `github-copilot/claude-opus-5.5` (`27c072e98` hard-coded it, models.dev
listed it by `b313731b8`, `fde38ed7c` extended the effort-map override to it);
the 0.99.1 build kept `[low medium high xhigh max]` and `TestCatalogClaudeOpus55`
held.

**Regen tripwire — model types (`a328aa89a`, embedded since 0.99.1).** (i) Every
`MODELS` entry carries `"type":"chat"` (the last key, the first for
openrouter's), decoded into `ai.Model.Type`; a test that deep-compares a catalog
model against a literal needs `Type: "chat"`. (ii) A provider key with no chat
models is still a built-in provider (pi's `getBuiltinProviders` is
`Object.keys(MODELS)`): the loader records every key
(`TestGetProvidersListsEveryCatalogKey`), and `BuiltinModels` passes over such a
provider until Scope queue row 3 brings the classifier and image models pi
registers it with. `TestCatalogProvidersWithoutChatModels` fails when that set
changes from `[typesafe]`; record the new provider on row 3 before moving it.
(iii) Per-provider key order can churn under `localeCompare`: compare dumps
parsed, after reproducing the outgoing catalog byte for byte. (iv) Dump
`IMAGE_MODELS` and `CLASSIFIER_MODELS` at every regen for row 3's record; do
not embed them until it lands (1.3.0: 60 image, 40 classifier).

**Regen tripwire — a copilot model moving onto Responses changes its code
path.** `ai/providers/openai_responses.go` carries `github-copilot`-specific
branches, so a copilot model whose `api` flips from `openai-completions` starts
executing code it never reached. `7d8ab31a4` did exactly that to
`github-copilot/gpt-6-astra` at the 0.86.1 regen. Its request body was read by
hand and is correct. **The hole this exposed is now closed:**
`TestCopilotGPTModelsUseResponsesApi` derives every `github-copilot` `gpt-*`
model from the catalog and asserts the Responses api, with a precondition check
so the loop cannot go vacuous — before it, nothing pinned any copilot model's
api and the flip would have happened in silence. The same holds for any api
flip: at 1.3.0 four kimi-coding models moved from `anthropic-messages` to
`openai-completions` at `https://api.kimi.com/coding/v1` (`a3f265b1c`), and
opencode-go's qwen3.7-plus and qwen3.8-max the other way (models.dev data); each
flipped model's payload was swept against the 1.3.0 build.

**Regen tripwire — model ids are load-bearing in Go tests.** A regen that
removes a model can turn a green test red or, worse, leave it green while it
silently stops testing anything. **Prefer fixtures that DERIVE the expectation
from the catalog and assert the precondition that makes the behaviour
deterministic.** 0.86.1 removed 56 models, including `deepseek/deepseek-v4-flash`
(`12f59336a`), which two tests in `ai/providers/openai_compat_test.go` named
outright. Both now iterate `ai.GetModels("deepseek")` and fail loudly on an
empty result, so the next rename can leave them neither red nor vacuous.

**Regen tripwire — the difftest harness can be wrong about which side is
ahead.** `difftest/pi/capture.mjs` used to early-return the raw context for
`dist` scenarios because "the published build predates `9e05370b2`". 0.86.1
does not, and that one stale assumption turned **46 of 58** scenarios red at
once, each reading as a port bug — pi apparently missing the system message and
the tools. It now PROBES the backend for `utils/transcript.js` instead. The
general rule: when a bulk of scenarios fails the same way right after an npm
bump, suspect the harness's model of the build before suspecting the port.
A bump also re-checks every `catalogModel` scenario, which `pi/capture.mjs`
refuses until its copy is refreshed from the new build's entry.


### Submodule shape for heavy optional dependencies (settled 2026-08-31)

The Bedrock ruling establishes a reusable layout, and future consults of the
same kind should follow it or say why they depart from it.

```
github.com/sky-valley/pi                     root — stdlib + golang.org/x/* only
github.com/sky-valley/pi/env                 SHIPPED 2026-10-19 — pi-env; x/sys + fsnotify; tags env/vX.Y.Z
github.com/sky-valley/pi/providers/bedrock   planned (row 9) — own go.mod, own tag series, requires root
```

**`env` is the first real submodule** (2026-10-19 pi-env ruling). Its go.mod
requires the published root (`github.com/sky-valley/pi v0.99.26` when it
landed; bumped at every release), `golang.org/x/sys` and
`github.com/fsnotify/fsnotify`, with the root's `x/image` and `x/text` as
indirect requires reached through `coding`. What it adds to the rules below:
- **Development runs in an uncommitted workspace** (`go.work` listing the root
  and `env/`; never commit one), because the client imports root packages that
  change in the same cycle. `GOWORK=off` builds against the required root tag,
  so between releases it proves only what that tag already had; both are gated
  (pi-sync §4).
- **Two-step release, in this order:** tag root `vX.Y.Z`, push it; in `env/`,
  bump the root require to `vX.Y.Z` and run `GOWORK=off go mod tidy`; commit;
  tag that commit `env/vX.Y.Z`; push. The env tag points one commit past the
  root tag (pi-sync §5).
- **Generated artifacts:** `env/bin` (the eight gzipped daemons and
  `manifest.json`) is rebuilt by `go generate ./...` in `env` whenever
  `env/daemon`, `env/internal/frame` or `env/cmd/pi-env` change, and committed
  with that change; the staleness tests fail until it is.
- **Its own dependency gate:** `env/internal/policy` holds the daemon's graph
  (`./cmd/pi-env`) to {x/sys, fsnotify}, the module to those plus the root's
  allowlist, and forbids cgo on every target. The root's
  `internal/policy/deps_test.go` never sees `env/`, by the rule below.
- Root `internal/` packages are importable from `env` (same module path
  prefix); the daemon avoids `internal/jstext` because it drags `x/text` into
  the daemon graph.
- **Owed at the next env require bump:** root `internal/nodepath` (2026-10-20,
  for `coding`'s Windows shell resolution) is a superset copy of
  `env/internal/nodepath` (adds `ToNamespacedPath` and its corpus rows). It
  could not be imported yet because `GOWORK=off` builds env against root
  v1.3.27, which lacks it. When the release step bumps env's root require past
  the release carrying it, delete `env/internal/nodepath` (sources, tests,
  testdata) and import `github.com/sky-valley/pi/internal/nodepath`.

Rules that make it work, all verified in prototype rather than assumed:

- The submodule **requires a tagged root**, never a `replace` directive —
  `replace` is ignored for downstream consumers, so a `replace`-based submodule
  is unimportable and proves nothing. This is what makes `difftest/` (module
  `pidiff`, `replace … => ..`) precedent for confining something nobody imports,
  and *not* precedent for this.
- It gets its **own tag series** (`providers/bedrock/v0.8x.NN`), which means a
  two-step release: tag root, then tag the submodule against it.
- The seam it plugs into already exists and is public: `ai/registry.go` exposes
  `RegisterApiProvider` over an open map, so a provider registers at runtime and
  no root code changes to admit one.
- Root stays clean by construction, and `internal/policy/deps_test.go` proves it
  each cycle — the submodule's dependencies never enter the root graph because
  `go list ./...` in the root does not descend into a nested module.

The cost is the risk recorded in the Bedrock ruling: with no CI, a submodule
the cycle gate does not build can rot silently against root API changes. That
cost is accepted deliberately — it is the price of keeping the root module's
dependency budget, the port's most-stated invariant — and it is why the cycle
gate builds every submodule that exists (`env` since 2026-10-19).

### Durable shape — SETTLED 2026-09-03: mirror upstream

Closed by the 2026-09-03 governing ruling. **`coding/` STAYS, and a Go
`durable` package mirroring `packages/durable` grows beside it. They are not
merged** — upstream keeps `packages/coding-agent/src/core` and
`packages/durable/src` side by side with zero cross-imports (as it kept the
`agent/src/harness` fork until `7fd478a2e` deleted it), and the forks differ on
purpose. Merging `coding/` into a Go durable package would be a divergence FROM
upstream, not convergence on it.

Three riders that are not optional:
- **`coding/execenv.go` is a relocation, not a fork.** Upstream has exactly one
  `ExecutionEnv`, in `packages/durable/src/env` (it was
  `agent/src/harness/types.ts` until `7fd478a2e`; provenance re-pointed in
  `8adf788`). Its exported symbols leave package `coding` for the Go durable
  package when row 8's env slice lands, the seven `Env*Operations` bridges in
  `coding/tooloperations.go` absorb the break, and pi-env's Durable types move
  with them (2026-10-19 pi-env ruling).
- **Genuinely identical halves get shared, not duplicated.** `edit-diff.ts`
  diverges between the forks by 69 lines, *all* of them TUI-preview deletions —
  the ~500-line matching/apply engine is byte-identical. Those move to
  `internal/`. Fork only where upstream forked, and never reuse one fork's
  constant for the other: the same name can hold a different value in each.
- **Mirror at file level inside a package, not at directory level.** Upstream's
  TS graph has real cycles Go forbids (`core/extensions/loader.ts` →
  `../../index.ts`). Fold such directories into one Go package.

**Slice 8b-ii's remainder — coding-agent code, shipped 2026-08-28 with a
remainder, and not entry 8's tree.** The seven per-tool `*Operations` seams
(`coding/tooloperations.go`, `ce76e94`, `cd0e3b9`, `1f49233`; structs of funcs,
since pi's members are individually overridable): (a) three members are
declared, defaulted, bridged and **never consulted** — `EditOperations.Access`,
`LsOperations.Exists`, `FindOperations.Glob` — while pi consults all three
(`edit.ts:349`, `ls.ts:133`, `find.ts:169-178`); `Glob` is the substantive one,
since upstream a supplied glob replaces the matcher wholesale. (b) **No host can
reach any of this** — every `*ToolOps` constructor is unexported and
`CreateTool`/`CreateCodingTools`/`CreateAllTools` take only `cwd`. (c)
`EnvBashOperations` drops `BashExecOptions.Env` (the `PI_*` session metadata
never reaches the child) and never yields `ErrShellAborted`/`*ShellTimeoutError`.
(d) find and grep still enumerate the local disk directly, faithful to pi's
fd/ripgrep architecture but meaning an injected remote filesystem enumerates
nothing.

**Queue discipline.** An entry untouched for more than ~10 cycles is evidence
the ruling that created it was wrong; re-open the ruling rather than letting the
row rot. Entry 8 taught it for 16 cycles.

## Divergences

Places the Go port knowingly behaves differently from pi and intends to keep
doing so. **Each is decided: a review finding against one of these is refuted,
not fixed.** Where a code comment carries the mechanism, this list carries only
the decision. Roughly two dozen further divergences live only at their code
site — the source is the port's real divergence register, and this list is not a
shadow copy of it.

**D1 — proxy handling diverges from pi, measured.** `http.ProxyFromEnvironment`
is NOT a drop-in for pi's `resolveHttpProxyUrlForTarget`. Six measured divergent
rows: `.example.com`/`*.example.com` vs `example.com`, `*example.com` vs
`api.example.com`, portless bracketed IPv6, `[::1]:9001` vs port-mismatched
target, empty `NO_PROXY` vs loopback, and CIDR entries (which pi has no concept
of). Go's `httpproxy` only suffix-matches any entry starting with `.` and never
exact-matches the root, short-circuits loopback ahead of `NO_PROXY`, and
supports CIDR. **Consequences:** the Bedrock and Codex adapters need their own
resolver — the stdlib will not do; pi reads `no_proxy` from the provider env
overlay first, which the stdlib cannot, so honouring `StreamOptions.Env` proxy
settings is part of that work. Live and narrow: upstream pins CONNECT
tunnelling for plain-HTTP origins (`23842b1e6`), while Go forwards an `http://`
target by absolute URI.

**D2 — parallel batches do not reproduce pi's `tool_execution_end` ordering.**
pi invokes deferred entries inside one synchronous `finalizedCalls.map(...)`, so
end events are strictly slot-ordered. The port spawns a goroutine per slot and
gates emits behind `serialMu`, so ordering follows the Go scheduler. Measured
over 300 iterations of a plain 3-call batch: four orderings (229 `c,a,b`, 64
`c,b,a`, 4 `a,b,c`, 3 `a,c,b`) where pi always emits `a,b,c`. The all-aborted
batch IS deterministic, and tool *result messages* are slot-ordered on both
sides. **The comment at `agent/loop.go:476-483` claiming `serialMu` "keeps the
emit order and tool-result ordering identical to pi" overstates this and should
be narrowed when the area is next touched.** Second residual: the single
`batchAborted` snapshot matches pi for an external abort but not when a tool
aborts synchronously from inside its own `Execute`. Invisible to difftest, which
compares request bodies only.

**D3 — compat overrides: the port skips a type-mismatched key and applies the
default.** Go cannot type-erase, so `applyCompat` leaves the default standing
where pi passes the mistyped value through to its use site. Two live gaps: a
**falsy** mistyped value on a default-TRUE flag (`{"supportsMaxOutputTokens":""}`
— pi omits `max_output_tokens`, the port emits it); a **truthy** mistyped value
on openai-completions' `supportsStrictMode`, default-FALSE since `890f92088`
and tested `!== false` at the tool site (`{"supportsStrictMode":"yes"}` — pi
0.87.0 sends `strict:false`, the port omits the key; unreachable through
`models.json`, whose schema types it boolean); and nested mistyping inside
an object-valued key, in both `allowedFallbackModels` and
`vercelGatewayRouting`.

**D4 — steering typed during compaction lands one turn later.** pi compacts
inside `prepareNextTurn` and re-polls steering after compaction, sending one
request carrying both. The port compacts in a per-request `TransformContext`
invoked after `runLoop`'s pending-injection loop, so a message typed while
compaction runs is picked up by the end-of-turn poll and lands one turn later —
one extra provider round-trip. Final transcript content converges; the turn
boundary and request count do not. **Tripwire:** this becomes a defect the
moment the port gains a `PrepareNextTurn`-driven or persisted compaction path.

**D5 — `reasoning_details` merge: a saturated `index` renders differently.**
`{"index": 1e400}` followed by an adjacent same-type delta yields no `index` key
in the port and `"index":null` in pi. Unmerged, the two agree. Mechanism stated
in full on `mergeOpenAIReasoningDetail` (`ai/providers/openai_reasoning_details.go:241-257`);
this entry exists so that comment's "see docs/UPSTREAM.md" resolves.

**D6 — `PackageDir`: two divergences from pi's `getPackageDir`.** (1) Above
cosmetic: `PI_PACKAGE_DIR` is returned raw (`coding/resources.go:37-38`) where
pi returns `normalizePath(envDir)`, so a quoted/systemd/Docker
`PI_PACKAGE_DIR="~/pkg"` leaves a literal `~` in the system prompt's doc paths.
(2) LOW, effectively unreachable: Go's `fileExists` rejects directories where
Node's `existsSync` accepts them. **Do not close (1) by reusing `coding/tools.go`
`normalizePath` — see K13.**

**D8 — Google honors retry headers and retries transport failures; pi's Google
does not.** pi routes Google through `retryProviderRequest` with `@google/genai`'s
`ApiError`, which carries `status` but no `headers`, so pi's Google adapter alone
ignores `retry-after`/`x-should-retry`. That is SDK information loss, not policy
— upstream's own docstring states the intent is the shared honor-retry-after
policy. Go speaks raw `net/http`, has the headers, and keeps using them. **A
parity sweep must not re-flag this, and must not reproduce pi's absence as an
invented mechanism** (the `headerlessErrors` proposal was rejected once already).

**D9 — every request body HTML-escapes on the wire.** Go's `encoding/json`
marshals with `escapeHTML=true`, so every request body escapes `<`, `>`, `&`,
U+2028 and U+2029 where `JSON.stringify` emits them literally. `SetEscapeHTML`
appears nowhere in `ai/`; the six body-marshal sites are in `anthropic.go`,
`google.go`, `openai.go`, `openai_responses.go` (two) and `pi_messages.go`. Value-preserving after parse, which is why nothing catches
it. **INVISIBLE TO DIFFTEST** — its Go side captures bodies through its own
`marshalWire` with `SetEscapeHTML(false)`, not through the real client. Fix is
one shared encoder helper across all six; needs its own golden pass.

**D10 — Google: a record entry spelled differently from one of genai's own
defaults arrives comma-joined with it; the port sends the entry alone.** Since
`a328aa89a` pi's `providerHeadersToRecord` folds names case-insensitively (last
slot wins, last spelling kept), so pi's own sources no longer collide. What is
left is `@google/genai` 2.21.0 (the lockfile version at `a328aa89a`): it seeds
`{"User-Agent": "google-genai-sdk/2.21.0 gl-node/<v>", "x-goog-api-client":
<same>, "Content-Type": "application/json"}`, spreads the record over them by
exact key, and fills the request with `Headers.append`, which comma-joins case
variants. Measured: record `{user-agent: X}` → `google-genai-sdk/2.21.0
gl-node/v26.4.0, X` (port: `X`); `{X-Goog-Api-Client: m}` → the genai value
`, m`. `{content-type: text/plain}` → `application/json, text/plain`, which the
port DOES reproduce: Content-Type is passed to `applyAsRecord` as an adapter
literal (`f9c0b54`); `x-goog-api-key` in any spelling is replaced by the record
value, since genai appends the key only when absent (port matches). The gap is
therefore only genai's User-Agent and x-goog-api-client defaults, which the port
never sends (D12). Pinned by `TestGoogleCaseCollidingModelUserAgentWins`.

**D11 — an empty or whitespace-only User-Agent vanishes in Go; pi sends it
present-and-empty.** pi sends `User-Agent: ` for `""`, `" "`, `"\t"` on all five
ported adapters (measured at `8676a0dcd` with @anthropic-ai/sdk 0.124.0, openai
6.40.0, genai 2.21.0; both openai loops re-measured at `2b0a123de` under openai
7.19.0, unchanged). Since `8c4f08f` every header value is trimmed as fetch's
`Headers` trims it, so the whitespace-only values become `""` too, and both
net/http's HTTP/1 writer and its HTTP/2 encoder drop an empty User-Agent. NOT
fixable through `http.Header`, and RFC 9113 forbids a raw `" "` over HTTP/2
anyway. Pinned by `TestEmptyUserAgentIsDroppedEntirely` (its two `""` rows; the
whitespace widening is documented in its comment, not pinned).

**D12 — base-UA substitution class, and no `x-goog-api-client` at all.** Once a
deletion marker removes pi's user agent, the runtimes substitute different
transport defaults (`Go-http-client/1.1` vs undici's `node` vs
`google-genai-sdk/2.21.0 gl-node/<v>`). Separately, the port sends no
`x-goog-api-client` header at all on google requests. Since `a328aa89a` this row
also owns google's case-variant marker: opts `{user-agent: null}` folds pi's
record to `undefined`, so pi's wire carries genai's own agent and the port's
`Go-http-client/1.1` (`TestUserAgentMarkerBeatsPiDefault`, weak assertion). The
request headers undici's fetch adds by default are the sibling class, K22. **This is why the marker tests assert only
that pi's own agent did not survive rather than asserting a literal — that weak
assertion is deliberate and must not be "tightened."**

**D13 — `StreamOptions.HTTPClient` does not carry `TimeoutMs`.** An injected
client bypasses `sharedClient`, so `TimeoutMs` does not apply. pi keeps its
timeout under an injected fetch because its SDKs apply it outside fetch, whereas
Go expresses it as `ResponseHeaderTimeout` on the transport being replaced. On
the port's own client the three providers that honour `TimeoutMs` through
`sendWithRetry` (anthropic, openai-completions, openai-responses) wait for
headers until the first of `TimeoutMs` and undici's headersTimeout
(`undiciHeadersTimeoutMs`, 300s), as pi's fetch does (`49bf984`, measured at
`2b0a123de`). When undici's fires first, both openai loops fail with openai
7.19.0's `Request timed out. Node.js fetch timed out waiting for response
headers; configure a matching undici fetch and fetchOptions.dispatcher with an
Agent whose headersTimeout is at least the SDK timeout.` and anthropic with
`Request timed out.`. A tie is the SDK's own timer, which starts first
(`Request timed out.`); undici's timers are coarse (about 500ms), so in pi a
`timeoutMs` within that of 300s can go either way. A dial or TLS handshake that
times out is undici's connect timeout, not its headers one. google also routes
through `sendWithRetry` but, like pi's, ignores `TimeoutMs` and waits on
undici's 300s alone (`bb33378`), and so does pi-messages, which calls fetch
itself. pi's CLI installs its `httpIdleTimeoutMs` (default 300s) as undici's
headersTimeout and as the default timeoutMs; `undiciHeadersTimeoutMs` is
undici's own default, fixed. `ProviderRequestOptions.HTTPClient`'s doc points
here.

**D14 — bash child `PATH` lacks pi's bin-dir prepend.** pi's `getShellEnv()`
prepends pi's own bin directory to `PATH`; `coding/tools.go:795` passes
`os.Environ()` verbatim. Pre-existing, never ported.

**D16 — retired 2026-09-17 (`16dda13`).** pi trims with
`String.prototype.trim` (strips U+FEFF, keeps U+0085), `strings.TrimSpace` the
reverse. Every site whose pi counterpart is a trim now uses
`internal/jstext.Trim`/`TrimStart`/`TrimEnd` (node's whitespace set, pinned by
`testdata/whitespace-node.json`); `strings.TrimSpace` remains only where pi's
counterpart is not a trim (the site table is in `16dda13`'s message). Kept as a
row because code comments cite D16 for that rule.

**D17 — session repair is narrower than pi's.** pi repairs an unterminated tail
inside its one shared loader, so read-only loads repair too and merely listing
sessions rewrites files. The port repairs only in `ResumeSession`, so reads never
mutate. Nothing model-visible differs. Upstream's ordering constraint is
preserved exactly — the header validates first.

**D17 addendum — session-version migration keeps the read/write boundary.**
pi's `_loadEntries` migrates a v1/v2 file to v3 in memory and rewrites the file
whenever anything changed, on every open. The port carries both migrations
(`migrateSessionV1ToV2`/`migrateSessionV2ToV3`, `coding/session_store.go`)
behind pi's `if (header)` gate, but `LoadSessionTree`/`LoadSessionMessages`
migrate in memory only and never write; `ResumeSession` migrates and rewrites
before appending. Two unobservable deltas in the rewritten bytes: sorted key
order rather than pi's insertion order (the recorder already writes sorted), and
the v1 id walk registers each generated id in its collision set where pi's
`migrateV1ToV2` never adds to `ids`.

**D19 — auth substrate ships as files in package `ai`, not a subpackage.
DECIDED 2026-09-03: the carve-out stays.** pi keeps auth resolution in
`packages/ai/src/auth/`; the port ships `auth_context.go` / `auth_helpers.go` /
`auth_resolve.go` / `auth_types.go` inside package `ai`, because a subpackage
creates an import cycle with `ai` itself. This is a **named exception** to the
2026-09-03 mirror ruling's "layout mirrors upstream's packages", granted on the
ruling's own clause 4: working around a language constraint is what "Go
affordances" means, and upstream's layout is not expressible here without
restructuring `ai` itself. A mirror-driven refactor of `ai/` will walk into this
— it is an exception, not an oversight.

**D21 — empty-string `namespace` is dropped.** `ai.ToolCall.Namespace` is a
plain `string` with `omitempty`, so an explicitly empty namespace does not
replay on the Responses wire; pi guards on `namespace !== undefined` and emits
`"namespace": ""`. Same class as the `ThoughtSignature`/`ErrorMessage`
precedents. Pinned by `TestResponsesEmptyNamespaceDroppedDivergence`; do not
"fix" without reopening the decision.

**D22 — the skills formatter's fall-through arm is inverted, deliberately.**
pi's `formatSkillsForPrompt` picks its load sentence with
`fileReadTool === "read" ? readLine : bashLine` (`skills.ts:355-368` @
`64eeb82a4`), so anything that is not exactly `"read"` gets the BASH sentence.
`FormatSkillsForPromptWithTool` (`coding/resources.go`) tests
`== SkillFileReadToolBash` instead, so anything that is not exactly `"bash"`
gets the READ sentence. **On both members of pi's union the two are
byte-identical** — no reachable path differs; both arms and the one-argument
default are pinned by `TestFormatSkillsForPromptWithToolBothArms`. They part
only OUTSIDE the union, unreachable for typed callers. **Kept:** Go has no
default argument, so the fall-through arm is also what the zero value lands on,
and pi's documented default is `"read"`. Matching pi's ternary literally would
make an unset field silently mean bash — a worse footgun than an out-of-union
disagreement no in-union caller can reach.

**D23 — the WebP RIFF walk stops on a non-EXIF chunk whose size has the sign
bit set.** pi's `findWebpTiffOffset` assembles the chunk size with JS bitwise
operators, so a declared size ≥ 2^31 is NEGATIVE. On the EXIF chunk itself the
port mirrors that exactly (`int32` size, negative passes the EOF check, payload
read as a bare TIFF block — pinned by `TestWebpOrientation` "chunk size with
the sign bit set", pi = 6 on identical bytes). On a chunk the walk must move
PAST, pi adds the negative size to the offset and walks backwards; with
`0xFFFFFFF8` it lands on the same chunk and spins forever (observed: the
verbatim transcription hangs until SIGALRM). The port returns -1 there —
orientation 1 — because there is no non-hanging value to match. Deliberate.

**D25 — `AssistantMessageEvent.partial` is an event-time snapshot, not pi's
shared live object.** pi's `partial` (types.ts docblock, rewritten by
`5c6655e76`) is "the shared live response-so-far helper": every event points at
the same mutable `AssistantMessage`, so a consumer that buffers events and reads
`partial` afterwards sees the final message. Port emitters pass
`AssistantMessage.Clone()` at push time (`ai/util.go`), so a deferred read sees
the state at that event. Live-consuming code observes identical values; only a
deferred read differs. Deliberate: events cross goroutines here, and a shared
mutable message would make every consumer a data race. Recorded in the
`AssistantMessageEvent` doc comment (`ai/events.go`).

**D61 — the anthropic adapter accepts one credential pi's adapter does not:
`ANTHROPIC_AUTH_TOKEN`.** `8b5899dce` re-attached pi's `assertRequestAuth` to
`streamSimple`, and the port's gate was widened to match its acceptance set —
an api key, or a non-empty `authorization` / `x-api-key` /
`cf-aig-authorization` request header (`hasAnthropicAuthHeader`,
`ai/providers/anthropic.go`). The port keeps a third arm pi's *adapter* has no
counterpart for: it reads `ANTHROPIC_AUTH_TOKEN` from the env inside the
adapter (upstream `24e5cc04`). pi resolves that env var one layer **up**, in the
provider resolver, into exactly the `Authorization: Bearer` header the first arm
already accepts (`8b5899dce:packages/ai/src/providers/anthropic.ts:24-31`), so
**end to end the two accept the same set** — the port's compat path
(`ai.StreamSimple` → `withEnvAPIKeySimple`, which deliberately leaves `APIKey`
empty for this var) simply reaches it later. The sets differ only for a caller
who invokes `StreamSimpleAnthropic`/`StreamAnthropic` directly with the env var
set and no header, which pi's adapter would reject and this one serves. Pinned
by `TestAnthropicStreamSimpleAuthTokenStillAuthenticates`. Do NOT "fix" this by
deleting the arm — that would break the port's own env-key path, which has no
resolver layer to fall back on. **Federation (upstream `a9424cd43`) follows the
same arm:** with `ANTHROPIC_AUTH_TOKEN` and the federation variables both set,
the port's compat path sends the auth token and exchanges nothing — pi's
resolver order end to end (the auth token resolves before federation is
consulted), though pi's own compat `stream()`, which never reads the auth
token, would federate. Pinned by `TestAnthropicFederationAuthTokenWins`.
**Behind the gate, the SDKs' own check (`fc3b432`):** pi builds the request
through the vendor SDK, whose `validateHeaders` runs after pi's gate. A request
whose only credential is a `cf-aig-authorization` header (no key, no
`Authorization`/`x-api-key` deletion marker) passes `assertRequestAuth` and then
fails in `@anthropic-ai/sdk` 0.129.0 with "Could not resolve authentication
method. …", as does an `X-Api-Key` or `Authorization` the caller replaced with
`""` (openai 7.19.0 likewise for an empty `Authorization`). The port runs both
SDKs' checks and fails those requests as pi does (`TestAdapterAuthHeadersMatchPi`,
21 rows). The Models path is unaffected: `cloudflareAIGatewayAuth` hands the
adapter its markers, which satisfy the SDK.

**D63 — a terminal event releases the consumers it did not reach; pi leaves them
pending.** pi's `push` hands the terminal event to one waiter and leaves every
other registered waiter parked until `end()` resolves them with `{done: true}`
(event-stream.ts `push`/`end` @ `96617628e`). `ai/eventstream.go` closes the
remaining waiters as soon as a terminal event sets `done`, so their `Next`
returns `ok=false` immediately. Observationally identical whenever `end()`
follows — which pi's own producers always do — and it diverges only when a
producer terminates via a `done`/`error` event and never calls `End`, where pi
would park those consumers forever. A pending JS promise costs nothing; a parked
goroutine is a leak. This preserves the behaviour the port has had since the
`sync.Cond` implementation, where `Broadcast` woke every consumer and the
`done` check returned them. Deliberate.

### chord (ported 2026-09-03, upstream `packages/chord` at `64eeb82a4`; service runtime deleted 2026-10-19)

Recorded by the porters and re-verified by independent reviewers, several by
running pi under node against the Go function on identical inputs. Go `chord`
mirrors chord's value layer — `context/index.ts`, `json.ts` and `delta/` — and
imports only `chord/delta`, `chord/internal/jsonvalue` (the strict-JSON rules
both share) and `telemetry` (since `7fedbfa`; upstream's boundary test allows
pi-telemetry since `1402973a9`). The facet and service runtime it also mirrored
was deleted upstream by `0104fef1e` and here by `d202916`; the chord2 runtime
that replaced it is Scope queue row 27.

**D26 — chord core.** Context: chord defines NO Context type, no BACKGROUND_CONTEXT/TODO_CONTEXT, no withAbortSignal/withCancel/withoutAbortSignal/awaitWithContext. Upstream's src/context/index.ts is a TypeScript reimplementation of Go's context (README: "a Go-like context system"); the mirror is context.Context + typed keys. Key[T] is a pointer identity (upstream Symbol), Key.Value returns (T, bool) (upstream T | undefined), WithValue is a generic free function over context.WithValue so T binds at both ends. The upstream toString assertions ("[Context BACKGROUND_CONTEXT].WithValue(first)...") are context-implementation internals and are not mirrored; Key.String() returns its name so Go's own ctx.String() reads sensibly. **Telemetry member (`48909d987`, `2ec55d808`; `7fedbfa`):** upstream's `Context.telemetry` (always a `TelemetrySpan`, `NOOP_TELEMETRY_CONTEXT` by default) is a value under an unexported `Key[telemetry.Span]`, read through `chord.Telemetry(ctx)`, which defaults to `telemetry.NoopContext`; `WithTelemetry(ctx, span)` and `TraceIn(ctx, options, fn)` mirror `withTelemetry` and `traceIn`.

**D30 — chord core.** IsValue and CopyJSON (`chord/internal/jsonvalue`, which draft placements share) are one Go spelling of strict JSON. Every Go numeric kind is a number, and so is a json.Number (JS has one number type); only finite ones pass; []byte is rejected (upstream rejects Uint8Array; encoding/json would base64 it); structs are rejected as class instances; pointers/funcs/chans/complex have no JSON form; any string-kind map key is an object key, and a typed slice, array or map copies to the plain one. No depth limit since `19a0361be`: a cycle is refused by an ancestor set keyed by a map's header, or a slice's backing array plus its length — a descendant equal to an ancestor in both is the same array, so the key is sound (the claim this row used to make, that no visited set could be, was wrong). Go has no undefined, so upstream's `{omitted: undefined}` case is pinned as an object holding a non-JSON member (a func), and copyJson's `omitUndefinedProperties` has no Go form. A `*delta.Draft` is not a value to CopyJSON, where pi's copyJson reads a proxy through its traps; placed in another draft, a draft is copied as it holds its content now.

**D31 — chord core.** Value is `type Value = any` (alias), so a decoded tree already is one; IsValue is the contract check. Upstream JsonRepresentation<T> is a type-level mapping with no Go form and is not mirrored.

**D35 — chord/delta.** `chord/delta` — a path segment that is neither a string nor an integral number (null, bool, object, 1.5) is refused by ParseOp/ParseWireOp/Path.UnmarshalJSON as an ErrInvalidOp-wrapped shape error; upstream's assertSafePath throws UnsafePathError(seg) for it. Both refuse before any walk; only the error class differs, because such a value is not a Seg in Go (Seg is sealed to Key and Index). A negative or unsafely large integer IS an Index, decodes, and fails Validate as *UnsafePathError exactly as upstream.

**D36 — chord/delta.** `chord/delta` — an integral number of any magnitude parses (Number.isInteger(1e300) is true): a "t" count and "p" index/remove saturate to math.MaxInt, and apply clamps them against the value's length, so the outcome equals pi's slice(1e300) / splice(0, 1e300) / splice(1e300, 0, x) (truncate-all, remove-all, append). Two residuals: (1) an Index beyond 2^53 is refused by Path.Validate as *UnsafePathError — pi's own class when the parent is an array (assertIndexInRange), but where the parent is an OBJECT pi writes the property spelled by String(n) ("1e+300") and Go refuses instead, since no double spells that address exactly and an int cannot spell "1e+300"; (2) a "#" id beyond 2^53 is refused (ErrInvalidOp) where pi's decoder would define it — pi's encoder counts ids up from 0 and can never emit one. A saturated count re-marshals as 9223372036854775807, not 1e300; no pi producer emits either.

**D37 — chord/delta.** `chord/delta` — Path.String/PathError text escape U+2028/U+2029 as `\u2028`/`\u2029` (Go's encoder) where JSON.stringify writes the raw characters. The codec dictionary key stays injective and the wire bytes decode identically; only error text differs.

**D39 — chord/delta.** `chord/delta` — Apply[[]any](nil, []Op{Splice{...}}) splices an empty array and returns the items where pi's apply(undefined, [p]) throws PathError []: a nil Go slice is a legitimate empty array (and a Replace could carry one). Apply[any](nil, …) and a nil map root DO fail as pi does. A stream missing its base batch is therefore caught by an any- or map-typed replica but absorbed by a []any-typed one.

**D40 — chord/delta.** `chord/delta` — (1) upstream splits inserts into 10,000-element splice(...items) calls because a JS spread is bounded by the engine's argument-count limit; slices.Insert takes any count (pinned by the 300,000-item case). (2) upstream writes with Object.defineProperty and reads with Object.hasOwn so an inherited accessor never runs; a Go map has no prototype, so m[k] = v is the whole story; ReservedSegments are still refused because a Go producer's ops reach TypeScript replicas. (3) a "t" that splits a surrogate pair leaves U+FFFD where pi holds a lone low surrogate no Go string can carry; U+FFFD is what encoding/json makes of that value crossing the wire, so later counts agree (one unit either way).

**D41 — chord/delta.** `chord/delta` — `Overlap(a, b, scan)` is exported as upstream's `overlap` is; its optional `probe` and `maxCandidates`, which no caller in pi passes (diff.ts and delta.test.ts give three arguments), are fixed at their defaults, 64 and 8. It counts scan, probe and result in UTF-16 code units, pi's unit and the unit a "t" count carries; the search runs on bytes with the head/tail boundaries translated, and the pair-splitting cuts pi's units produce (a head ending in a lone high surrogate, a tail opening with a lone low one) are reproduced by a same-high-surrogate check on the following rune and by starting the tail after the split pair. 37 inputs pinned against pi under node (TestOverlapMatchesUpstream, re-checked at `9a139c62b`), including those cuts. The two sides can only differ on strings one of them cannot hold: a Go string with invalid UTF-8 (each bad byte counts one unit) or a pi string with a lone surrogate — neither is a JSON value.

**D42 — chord/core.** IsValue rejects a fixed-size [N]byte as well as []byte. Upstream's only oracle is the Uint8Array case, which maps to []byte; a Go [N]byte has no upstream counterpart (encoding/json would write it as a number array, not base64). The port treats every byte container as a binary blob rather than an array; unreachable from the wire, no test pins it either way, so the reviewer's narrowing mutation survives by design rather than by gap.

### chord/delta immutable tracking (ported 2026-09-23 at `9a139c62b`; the overlay tracker 2026-09-25 at `9e70c3d50`)

Upstream replaced the diff-based tracker with an overlay (`d5cba1d97`): `track(initial)` takes ownership of the root in O(1), `beginChange()` opens an overlay whose nodes record writes, deletions and structural edits, `prepare()` emits the ops those records mean — without diffing — and materializes the revision, `adopt()` swaps the root and stales every competing change. `diffRevisions` stays, unused by the tracker. The oracle is `chord/delta/testdata/upstream_delta.json`, captured by `testdata/capture.mts` from pi's own `packages/chord/src/delta` at `d2e7d8b7c` (recaptured with `091bcc099`'s append path, `4b8f194`; every record of the `9e70c3d50` capture byte-identical; no npm build carries either): 284 diffRevisions pairs (upstream's state-diff suite, 120 random identity-sharing arrays, the op cap, the anchor and semantic-table budgets, and the snapshot threshold and each cost term at pi's flip point), 119 tracker/draft scenarios, 17 generated cases (dense regions, the op cap, a 20,000-op queue, fragmentation, 100,000-item inserts, an append to a 10,000-item array, a whole-root replacement), state-fuzz's 100 seeds × 100 steps, and 20 random scripts over every draft operation. Every batch is compared byte for byte: a tracker's in pi's own order, a diff's modulo D69.

**D69 — chord/delta.** Object member order, in the diff. JavaScript enumerates an object's keys integer-like first (ascending), then in insertion order; a Go map keeps none, so `DiffRevisions` visits a revision's members — and `Draft.Keys` lists them — integer-like first, then by UTF-16 code unit: the order of an object whose keys were inserted sorted (`Draft.Keys` lists the string keys a change adds after them, in the order it added them, as pi does). Where pi's insertion order differs, a diff lists the same member ops in a different order (sets, then deletes, on both sides; nothing else moves) — upstream's own state-diff case "emits sets and deletes" is one (`change` was inserted before `add`). A tracker's batches are not affected since `d5cba1d97`: the overlay emits in the order the change made its edits, which the Go overlay keeps (`chord/delta/ordered.go`), so they match pi's op for op, in pi's order. encoding/json marshals a payload's members in a third order, byte order (`{"10","9","a","｡","😀"}` in that order, where `Draft.Keys` gives `9 10 a 😀 ｡`), so a pi replica decoding a Go payload inserts them in byte order. The replica is the same value either way. Not fixable without an ordered object in the port's value model, which has none (`chord.Value` is `any`, D31) and adds none, since a replica is the same value either way; the golden records pi's own diff order as `raw` beside Go's.

**D70 — chord/delta.** The draft is methods, not a Proxy; doc.go maps each trap to its method. Consequences: a write through a settled draft returns `ErrDraftSettled` and a read panics with it (pi throws a TypeError from both); `Has` on an array sees its indices and `"length"`, not `Array.prototype`'s members (`"push" in draft` is true in pi), and `Get` of a named key on an array reads nothing where pi reads the prototype's; `Pop`/`Shift` return `(value, present, err)` where pi returns undefined; an array method on an object draft fails with upstream's "Array mutator called on incompatible receiver" (pi's text for a borrowed mutator), and `SetLen` on an object draft is refused (pi's `obj.length = n` writes a member, as `Set("length", n)` does here); `Splice` always takes a start and a count, so upstream's zero- and one-argument forms have no Go spelling; Go has no undefined, so `Set(k, nil)` writes null where pi's `draft.k = undefined` deletes (that is `Delete(k)` here); a numeric Go key is the key its integer spells, and an integral float past int64 saturates (`Set(1e21, v)` writes `"9223372036854775807"` where pi's `draft[1e21]` writes `"1e+21"`); `Change.State` still returns the draft after the change settles, where pi's getter throws, and fails on first use instead; the traps with no Go spelling (defineProperty, getPrototypeOf, setPrototypeOf, preventExtensions, symbol keys, `6966636db`'s `then` probe) have no port. `Get("length")` and `Set("length", v)` on an array are kept, since they are the property they spell; `Set` converts `v` as ArraySetLength does (ToNumber: null 0, booleans 0/1, strings by StringToNumber, arrays through their string form, an object with a "toString" member a TypeError), pinned by 64 assignments captured from pi.

**D71 — retired 2026-09-25.** Upstream stopped freezing revisions (`d5cba1d97`: immutability is an ownership contract, nothing is frozen or copied), so applying a tracker's batch with the mutable applier writes into its revisions in pi as in Go; doc.go (Streams) directs in-process consumers to `ApplyImmutable` on both sides' terms.

**D72 — chord/delta.** Container identity. `===` on containers is a map's header address, or a slice's backing array plus its length; an empty slice with no capacity — every `[]` encoding/json decodes, each its own array in pi's `JSON.parse` — has none, since Go gives every one the same zero-size address. The overlay therefore finds the node of a container read from the revision by its slot (member key or entry index), not by identity, so two such empty arrays stay two drafts (TestDraftEmptyArraysAreDistinct, red with identity keys); a container a change placed is its own copy, allocated with capacity at least one, and is found by identity. `DiffRevisions` treats an identity-less array as identical to nothing, so decoded revisions align exactly as pi's do (golden "parsed empty arrays …"). Two residuals of `DiffRevisions` over values a Go caller builds by hand: one empty `[]any{}` shared by both revisions is one array in pi but identical to nothing here (give it capacity, `make([]any, 0, 1)`, as the golden's `$ref` does — "a shared empty array …"), and two headers over one backing array with one length are one array here where pi would hold two. Either can change which elements anchor an alignment, never the replica. A value with no JSON form handed to `DiffRevisions` matches nothing.

**D73 — chord/delta.** 32-bit lengths. Growing an array's length past 2^31 - 1 fails on a 32-bit build with an error naming the limit, where pi's arrays reach 2^32 - 1; indices, keys and wire addresses past 2^31 otherwise mean the same as on a 64-bit build (`Index` and `PathID` are int64). Growth inserts nulls in both, materialized in both since `d5cba1d97`.

**D74 — chord/delta.** `Draft.Sort` sorts its entries with `slices.SortStableFunc`; V8 runs TimSort. Both are stable, so a consistent comparator yields the same order and the same ops, but the comparator is called on different pairs a different number of times — measured under node, 5 descending ranks take 8 calls in V8 and 10 in Go, 5 ascending 6 and 4, 2 elements one each. A comparator with side effects agrees only as far as its calls do: a write inside an element stays, a write over an element is put back when the sort ends, and a comparator that restructures the array leaves duplicated entries as copies — each pinned by a golden comparator that does the same thing on every call (write, unshiftOnce). A comparator that prepares or aborts its own change, or adopts a competitor, is out of contract upstream (README: unspecified; pi throws an internal TypeError on the first two); Go's Sort returns `ErrDraftSettled`. The nil comparator is JavaScript's default order, each element's `String()` in UTF-16 code units, taken once before sorting, which matches V8 exactly: with two or more elements every one is compared, so an element with no string form (an object with an own "toString" member, directly or inside an array) fails the sort with pi's TypeError whichever pair V8 would have compared first, and the array is left untouched either way.

**D75 — chord/delta.** Go values in, JSON values out. Track and PrepareReplace take the root as it is, in O(1), as pi does — a nil map or slice root is null, which a change cannot draft nor a replacement be (ErrScalarRevision, pi's refusal of a null root): it must already be strict JSON in the representation encoding/json decodes (float64 numbers, []any, map[string]any, no nil map or slice — encoding/json writes one as null, which a replica cannot hold members of — and no container reachable twice), and one that is not has unspecified behavior, as in pi. `chord.CopyJSON` makes exactly that representation from any Go value; `chord.IsValue` accepts more (aliases, other numeric kinds, typed and nil containers), so it is no check for a root. Every value a draft places is copyJson's copy (D30): any Go numeric kind or json.Number becomes a float64, a typed slice, array or string-keyed map the plain one, a typed nil container an empty one, and anything else is refused with pi's copyJson text before the draft changes. The Go errors also name the offending type, and a map is walked in key order, so that of several bad members the one reported is always the same.

**D76 — chord/delta.** Not ported: `delta/revision-validator.ts`'s `JsonRevisionValidator` (`d5cba1d97`). Its one caller, `services/state.ts`'s `ReplicatedStateReplica`, went with `0104fef1e`: at `d2e7d8b7c` the file survives with no caller in `packages/chord/src` (only `test/delta-traversal.bench.ts` imports it) and no export (`./delta`'s `index.ts` does not re-export it), and `src/state.ts`, the renamed replicated state, holds no replica. So there is nothing to mirror (C2). Port it with its first caller, which would land on Scope queue row 27. `value.ts` (the `JsonRevisionStore`) and `draft.ts`'s `produce` helpers are gone upstream, and so is their Go.

### coding shell exit status (ported 2026-09-18, upstream `a8b3dd199`)

**D68 — coding/execenv.go + coding/tooloperations.go.** A shell killed by a
signal **Node cannot name** is a FAILED command in the port (128 + the signal
number) and a SUCCESSFUL one in pi. pi reads Node's `close` event rather than
the raw wait status, and `??` falls through only on null/undefined: for a signal
outside Node's own 31-entry `os.constants.signals` table — **7 (SIGEMT) on
darwin, the realtime signals (34–64) on Linux** — Node reports
`code: 0, signal: null`, so `code ?? (signalCode ? 128 + … : 1)`
short-circuits to `0` and `bash.ts`'s `exitCode !== 0` test passes the
termination off as a clean exit with whatever partial output the command
produced. Measured under node v26.4.0 on darwin: `printf 'before\n'; kill -EMT $$`
→ `close(code=0, signal=null)` → pi exit code **0**, tool SUCCESS; the port
returns **135**, tool FAIL. Named signals agree on both sides (KILL 137,
TERM 143, ABRT 134).

**Kept deliberately, against pi's observable behaviour and with pi's stated
intent.** `a8b3dd199`'s own commit message is "Use the standard shell convention
so callers do not mistake the termination for a successful command" — which is
exactly what pi fails to do here, because Node's table, not pi's logic, decides.
Reproducing pi would mean importing an accident that reports a killed command as
successful, in the one commit whose purpose was to stop doing that.

**Worth stating plainly: `a8b3dd199` is what introduced this gap.** Before it
the port returned a nil exit code for every signal and the tool treated that as
success, so it agreed with pi on the unnamed case by accident too. The port
traded agreement on a rare case for agreement on the common one, and this row is
the record of that trade rather than a claim that the two now match. Pinned by
`TestLocalBashOperationsUnnamedSignalStillFails`. Revisit if upstream ever reads
the raw status or widens its table — the port would then simply match again.
Since `8562bcf66` a non-zero exit is an error RESULT rather than a thrown error
on both sides, and the divergence carries into its structured content: for the
unnamed signal pi reports `exit_code: 0` on a success, the port `exit_code: 135`
on an error result.

**D77 — `ai.Model.Type`: an explicit JSON `"type": ""` reads as chat.** Go's
zero value must mean chat (every hand-built `*Model` literal is a chat model),
and a string field cannot tell `""` from absent. pi's `getModelType` is
`model.type ?? "chat"`, so an explicit `""` stays `""`: `assertChatModel` throws,
`isModelType(chat)` is false, and `hasKnownModelType` drops the model from stored
and fetched catalogs. pi-generated data never holds it (its model-data
validation rejects any type outside chat/image/classifier); only a host
`ModelsStore` or hand-written JSON can. JSON `null` is safe — both read it as chat.

**D78 — `Models.GetAllAvailable`'s fallback for a handwritten provider.** pi keeps
every model of `getAllModels()` when the provider has no `filterModels`; the Go
`Provider` interface always has `FilterModels`, so the fallback always filters
chat ids through it. A handwritten provider without `AllModelsFilterer` whose
`GetAllModels` lists a chat model its `GetModels` does not: pi keeps it, Go
drops it. `CreateProvider` providers are unaffected (`FilterAllModels`
reproduces pi's branch). Documented on `ai.AllModelsFilterer`.

**D79 — `ModelsError` with a cause whose message is empty.** pi's
`withCauseDetail` uses `formatThrownValue(cause)`, which is `message || name`, so
an `Error("")` cause appends `: Error`. A Go error has no name; nothing is
appended. The rest of pi's message rule is exact since `06b134a` (no code
prefix, cause appended only when not already contained, `jstext.Trim`).

**D80 — response-header records on the wire: pi's CLI speaks HTTP/1.1 through
undici, the port negotiates HTTP/2 and reads through net/http.** pi's
coding-agent pins HTTP/1.1 for every provider (`http-dispatcher.ts`
`allowH2:false`); the port's `sharedClient` negotiates h2 with TLS hosts, so on
real traffic `onResponse`/`sdkHttpResponse.headers` records differ in hop-by-hop
headers (`transfer-encoding: chunked`, `connection`). Over HTTP/1.1, net/http
also consumes headers before any record sees them: a `Connection` header holding
`close` in any case becomes a lone `close`, `Transfer-Encoding` comes out
lowercase `chunked`, `Trailer` names come out sorted and canonical, two equal
`Content-Length` lines are merged (undici fails `fetch failed`), and transparent
gunzip deletes `Content-Length` (restored for google, which now undoes its own
codings). `responseHeadersRecord` (anthropic, pi-messages) and
`googleSDKResponseHeaders` restore what can be restored; the rest are tagged
divergence scenarios in `ai/providers/testdata/google-stream-events` guarded by
`TestGoogleDivergencesStillDiffer`. A joined request value with an empty
contribution differs in bytes only (`Bearer k, ` on pi's HTTP/1.1 wire, `Bearer
k,` over HTTP/2). Forcing HTTP/1.1 for every adapter is the fix if this ever
matters; it is a transport change for all providers, not taken this cycle.

**D81 — google offers only `gzip, deflate`.** Since `a5a1f96` google undoes its own
content codings, as undici does, and sends undici's cleartext value
`Accept-Encoding: gzip, deflate` unless the caller names one. Over https undici
offers `br, gzip, deflate, zstd`; Go's standard library has no brotli or zstd
decoder, so the port never offers them, and a caller who asks for one gets a
hinted error. Taking them would be an E2 consult; nothing needs it.

**D82 — JavaScript values no Go field can hold (representation class).** Where
pi stores a wire value as it came and the port has a typed field: google usage
counts sent as strings (pi `"2"+1` = `"21"`), fractional or infinite counts,
a numeric `responseId`, a non-string text delta, non-string tool-call
id/name/thoughtSignature and non-object args; openai-completions tool-call
ids/names that are not strings (`String()` is stored; `[]` and `[""]` are truthy
in pi and `""` here); `parseStreamingJSON` results that are not objects (pi keeps
`12`, `"x"`, `[1]`, `null` as the arguments; the port has `{}`), non-finite
numbers (held as nil; the persisted bytes match, since JSON.stringify writes
null) and half a surrogate pair (U+FFFD); a pi-messages block event whose
`contentIndex` names no array slot (`"01"`, `1.5`) carries `ContentIndex -1`, and
an array-inherited name (`"length"`, `"__proto__"`) is an ordinary key here.
anthropic text, thinking and signature that pi holds as a number (a numeric or
boolean seed plus non-string deltas, `f19e982`) are held as their String(); after
a block's stop, `undefined + <number>` makes pi's partialJson NaN, which only
non-object arguments show.
Each measured row is a tagged scenario in the capture oracles
(`ai/providers/testdata/{google-stream-events,openai-stream,streaming-json,pi-messages,anthropic-sse}`);
the observer (`OnProviderStreamEvent`) still sees pi's value. A pi-messages `toolcall_end` carrying a tool-call key outside pi's `ToolCall`
type (a gateway's extra) loses it here, where pi keeps it through
`Object.assign`; a null or non-string `argumentsError` persists as `""` or its
JS string where pi keeps the raw value (validation outcome and text identical;
`e12b8e6`).

**D83 — port limits pi does not have.** Each fails with a hinted error: an
openai or anthropic SSE line past 16 MiB (`maxOpenAISSELine`,
`maxAnthropicSSELine`), openai stream JSON nested past 10,000 levels
(encoding/json's limit; `jstext.JSONSyntaxError` recurses per level too), which
fails with the port's hinted error whether pi's JSON.parse would read it or fail
it with openai 7.19.0's `Error reading response: malformed server-sent event
JSON.`, a response body
that returns no data and no error 100 reads in a row, where pi's reader would
wait forever (`progressReader` in `ai/providers/body_progress.go`,
`maxEmptyBodyReads`, on the openai, anthropic, pi-messages and google SSE
readers alike: `the <provider> response body returned no data and no error 100
reads in a row; this is a port guard against a broken body reader, report it
with the provider: multiple Read calls return no data or error`, wrapping
`io.ErrNoProgress`), and a pi-messages message holding more than 1,024 unset
content slots at once (`piMessagesMaxContentHoles`; pi's sparse array holds any
number). And JSON repair (`87f9a3641`): should Go's decoder ever refuse a text
V8's `JSON.parse` accepts, the `argumentsError` is the decoder's error plus
"(JSON.parse accepts this text; report it as a port bug)" — not observed over
the 5,500-input capture.

**D84 — an openai-completions error whose `metadata.raw` cannot be stringified.**
pi's completions catch block throws inside itself (e.g. `{toString: 1}`) and its
stream never ends: for an SSE error chunk and, since the completions HTTP path
follows pi (`b9de81e`), for a non-2xx response too (measured at `2b0a123de`: no
end after 3s, an unhandled `TypeError: Cannot convert object to primitive
value`). A hang cannot be ported. The port ends the stream with the message and
does not append raw (`openai_stream.go` `metadataRaw`, `errors.go`
`formatCompletionsHTTPError`).

**D85 — a failure later in the chunk that ended a gzip member (google).** node's
zlib works one body chunk at a time and drops a whole chunk's output on a
failure anywhere in it; Go's decoders hand output over as they go, so a complete
member's events reach the callback before a corrupt next member in the same
chunk fails the read. Both end `terminated`. Tagged in the google capture.

**D86 — undici reads one HTTP chunk at a time; Go reads everything buffered.**
A bare-JSON google error sent in the same TCP segment as an event makes pi throw
the SDK's ApiError and the port fail `Incomplete JSON segment at the end`.
Tagged in the google capture.

**D87 — node v26's streaming TextDecoder can drop a U+FEFF after a character
held across reads at a stream's start; the port decodes as the Encoding
Standard does.** node's UTF-8 fast path decodes the rest of a read that
completes a character held from earlier reads with `ignoreBom || prefix`, a
Uint8Array where the binding keeps a mark only for `true`. So while no text has
been decoded yet, a U+FEFF leading that rest is dropped as if it began the
stream: `EF | BB BF EF BB BF 78` reads `x` where one read gives U+FEFF `x`, and
`E2 | 82 AC EF BB BF 78` reads `€x` where one read gives `€`, U+FEFF, `x`. It
touches every streaming decode in pi (anthropic, pi-messages, @google/genai,
and coding-agent's shell OutputAccumulator, whose display snapshot drops that
U+FEFF while its readFullOutput, one decode of the saved file, keeps it), is
node's bug rather than pi's code, and depends on the node version. The port
follows the Standard (`jstext.TextDecoder`, and anthropic's and pi-messages'
per-line decode); `internal/jstext/testdata/text-decoder` tags these rows
`nodeQuirk`.

**D88 — a custom body's openai read still pending at an abort gets 100ms.**
openai 7.19.0 races each body read against the request's signal, and
JavaScript's event order settles the race: a custom fetch's body keeps its own
error only when it errors its stream before the microtask openai's abort
listener queues — at once from an abort listener, or a microtask later from one
that fires ahead of openai's (on the caller's signal, or added to the fetch's
signal before openai began reading) — and only when that error is not an
AbortError. An error after that microtask (two microtasks on, setTimeout(0),
5ms, 50ms), or none, is dropped: openai ends its reading and cancels the body
at the abort, and pi fails the stream `Request was aborted` (completions, after
closing its open blocks) or, short of a terminal event, `OpenAI Responses
stream ended before a terminal response event`, within a few ms (measured at
`2b0a123de`, node v26.4.0). Go has no such order, as a body answers ctx on a
goroutine of its own, so `openaiAbortableBody` gives a read still pending when
ctx ends `openaiAbandonedReadGrace` (100ms) and takes what it returns by then.
A custom HTTPClient's body that fails within 100ms of the abort therefore
always fails the port's stream with its own error (`context.Canceled` aside),
where pi keeps that error only in the cases above; one that answers only to
Close, or not within 100ms, ends the stream as an abort 100ms after it rather
than at once. The stopReason is `aborted` on both sides. A body that answers
the abort with `context.Canceled` at once, as an `*http.Client`'s does, ends the
stream as an abort at once, and the port's own client's body is not raced
(`fetchBody` fails a read at once when ctx ends). The grace serves
stream-abort's rows `a custom fetch's body fails once the signal aborts` (its
body fails at once from a listener on the caller's signal; with the grace at
zero the row flakes) and `a custom fetch's body hangs, ignoring an abort while
a read is pending`.

**D89 — `TimeoutMs` zero is unset.** pi passes any defined `timeoutMs` to the
anthropic and openai SDKs (`options?.timeoutMs !== undefined`), and they treat
an explicit `0` as an immediate timeout (pi's own CLI sends int32 max to mean
"no timeout" for that reason, #5294); the port's `TimeoutMs` is a plain `int`
whose zero means the SDKs' default (`retryFromOptions` maps anything not
positive to 600s). An explicit zero cannot be expressed without making the field a
pointer, and it would only reproduce a configuration that fails every request.

**D90 — a shell's full output that cannot be saved fails the call; pi can
crash or hang instead.** pi's OutputAccumulator writes the full output to a
temp file through a WriteStream with no 'error' listener until `closeTempFile`
attaches one at finish. An error before then (the temp directory missing or
unwritable, a write failing while the command runs, ENOSPC or EFBIG) is an
unhandled 'error' event: the process exits (interactive pi prints "pi exiting
due to uncaughtException"), or, with the exception swallowed, the call never
settles. An error after it (a create that happens only at finish, a write queued
after the command exited) rejects `closeTempFile` and fails the call with the
stream's error. pi never returns a success with missing data. The port keeps
the first error, writes nothing after it, and always fails the call at finish,
before the abort, timeout and exit-code checks as pi's `finishOutput` orders
them, with the cause and how to fix it: "could not save the full command
output: <cause>; point TMPDIR (TMP on Windows) at a writable directory with
free space" (`coding/tools.go`, `da98715`). Measured against pi's own
accumulator and bash tool at `3dd803d7e`.

**D91 — telemetry's instrument cache holds one slot.** pi caches the four
request instruments per metrics capability in a WeakMap. Go has no ephemeron
map: any map holding the instruments would pin the adapter they record into,
and with it, for an `InMemoryContext`, every recorded span. So
`ai.instrumentsFor` keeps one slot, holding the last capability; a program that
alternates between adapters re-creates the instruments at each switch, which
only an adapter counting its factory calls can see. The no-op capability never
takes the slot, and a capability that is not comparable is not cached. With one
adapter per host, the normal shape, the result is pi's. The slot's two costs
are on its comment (`7d7826f`): it keeps the last adapter, and what that adapter
recorded, alive until another takes the slot; and it compares with `==`, so
equal values of a comparable value type share one entry where pi's WeakMap
keeps one per object.

**D92 — telemetry's shape in Go.** (1) pi's optional `metrics?`/`propagation?`
members are `Context.Metrics()`/`Propagation()` methods that return nil when the
capability is absent, so a tracing-only adapter adds two one-line methods. (2)
`RecordedSpan.ParentID` is 0 for pi's null, and `InMemoryContext.Measurements()`
is pi's `getMetrics()`, since `Metrics()` is the capability accessor. Since
`d9025a5f5` carrier identifiers are unchecked, and an extracted own-scope carrier
whose span is not a safe integer, or is `""`/`"0"`, also reads 0 — pi records
`Number(span)` there (0, a fraction, NaN, Infinity); that input is unspecified. (3)
`StartSpan` is callback-scoped, so a traced request runs the span's callback on
a goroutine that hands the span back and waits for the request to finish; a
stream that never ends keeps that goroutine, as pi keeps a pending promise. (4)
`errorMessage.slice(0, 500)` can end on a lone high surrogate no Go string
holds; `ai.truncateUTF16Units` writes U+FFFD there — one code unit, pi's length,
and what encoding/json makes of pi's value (capture row `error-bounded`). (5)
Whether the request span or the caller's span settles first is scheduling on
both sides (microtasks in pi, goroutines here); upstream's own tests wait
rather than assert it, and the capture does not compare `endSequence`. Go does
not wait for the adapter to settle the request span before delivering, which
would hang a request whose adapter never returns from `StartSpan`. (6) pi's
"unreadable" Proxy payloads (getters that throw) have no Go form. (7) Order
(`156067c`): the observer records each event before the consumer sees it, and
the result before the terminal event and `End`, as pi's lazyStream does, so no
Go consumer sees a result whose telemetry is unrecorded. pi's own lazyStream
test registers a `result()` reaction inside `onEvent` and pins
`["event","resolved","result"]`; Go cannot order a `Result` waiter apart from an
iterating consumer, so both come after `onResult`: `["event","result","resolved"]`
(a pi `await complete()` can see an unrecorded result). (8) An adapter whose
`StartSpan` returns without running its callback (`4cebca3`) fails the request
as pi does — the first event delivered, then an error event and an error
result; a stream without events ends with its own result — with a hinted Go
message where pi's is the TypeError reading `addEvent` of undefined. On a setup
failure pi then never ends the stream, and its `onResult` throw is an unhandled
rejection; Go ends the stream with the setup error and records nothing. (9)
`telemetrytest` (`348aa86`) is pi's `./testing` (`Cases` =
`createTelemetryAdapterConformance`, `Run` = pi README's registration loop), case
for case with upstream since `d9025a5f5` dropped the malformed-input cases; a fixture with nil `Spans` is a non-recording
adapter, checked for behaviour only (how `NoopContext` runs it; pi requires
`getSpans`); recorded values compare as JSON (pi `deepStrictEqual`), so an
adapter may normalize `[]string` to `[]any` or ints to floats; and a Go-only
`concurrency` group drives one adapter from many goroutines (run it with
`-race`). (10) `render.Trace` (pi's `./render` `renderTrace`, `d9025a5f5`) lists
span, event and link attributes as array-index keys ascending, then the rest by
UTF-16 code unit — D69's rule — where pi uses insertion order, which a Go
`SpanAttributes` map does not keep; upstream's first render case is ported with
its expected line reordered. Fixable only with an ordered `SpanAttributes`. (11)
`telemetry.Store` is pi's `TelemetryStore`: each method takes a `context.Context`
and returns an error; `GetTrace` returns nil for undefined; `TraceQuery`'s
`StartMin`/`StartMax` are `*float64`, an empty `Name`/`Cursor` means absent (pi's
`name: ""` filter has no Go form), and `Limit` 0 is the default 20 where pi throws
on `limit: 0` once a trace matches. A negative `Limit` is refused with a hinted
error: pi throws a TypeError when no more than `|limit|` traces match and
otherwise returns all but the last `|limit|` with a cursor. A cursor
`FindTraces` did not write is refused with a hinted error where pi's
`JSON.parse` throws or compares garbage. pi's non-string-carrier half of the
remote-trace case has no Go form (`Carrier` is `map[string]string`). Settlement
also covers `runtime.Goexit`, the Go analog of pi's new `finally`. pi's store
conformance (`test/store-conformance.ts`, unpublished) is package-internal in
`telemetry/store_test.go`; the chord `telemetry-sqlite` port (Scope queue row 8)
moves it to a root `internal/` package with a clock hook, since chord's test
reaches it by relative path.

**D93 — a stream that ends without a result: pi waits forever, the port ends.**
pi's `forwardStream` (the `Models` lazy stream) awaits `source.result()`, which
never resolves when a provider ends with neither a result nor a terminal event;
the port ends the outer stream with no result and reports `onResult(nil)` to
telemetry. Coding's `completeSummarization` likewise fails such an attempt like
a dropped stream (retryable under its policy) where pi would wait. A hang cannot
be ported (D63's class), and no well-formed provider reaches either.

**D94 — OpenRouter: a present-but-null `min_prompt_tokens`.** pi's
`openRouterCost` keeps it as `inputTokensAbove: null`; Go's
`ModelCostTier.InputTokensAbove` is an int, so 0. Pricing is identical
(`input > null` coerces null to 0); only the persisted JSON differs, and the
shape is unrealistic, so the capture has no row for it. Its kin, commented in
`ai/providers/openrouter.go` and not pinned: a price object with an own
`toString` key, a non-array `overrides` and a null override throw in pi and fall
back here.

**D95 — tool selection around the port's extra tool.** `web_fetch` (Go-only) is
admitted by its exact name only — a plain allowlist entry or `+web_fetch` — and
a `*` pattern never opts it in, so `ToolNames: ["*"]` selects exactly pi's eight
built-ins (`TestSessionWebFetchOptInByNameOnly`). The `Tools` override, Go's
analog of `baseToolsOverride`, supplies the default selection that
`+name`/`-name` modifiers edit, as it already supplied the default active set.

**D96 — `localeCompare` in the model resolver is a root-locale collator.**
`bestFuzzyMatch`'s `b.id.localeCompare(a.id)` and the ambiguity list's sort use
a root-locale `collate` collator, as the ls tool does. Byte order would differ
for `_`, case and some symbols; pi's ICU tailoring (Danish "aa", for one)
depends on the machine's locale.

**D107 — compat's error for a built-in model whose api the port has not
ported.** With `ai/providers` imported, a built-in model on one of the five
unported apis (`bedrock-converse-stream`, `mistral-conversations`,
`google-vertex`, `openai-codex-responses`, `azure-openai-responses`) fails,
through compat's `Stream`/`StreamSimple`, `Provider <provider> has no API
implementation for "<api>"` — pi's `createProvider` dispatch error — where
before `c5dbe91` it failed `No API provider registered for api: <api>`; a host
that does not import `ai/providers` gets the same for every built-in api. This
is pi's compat algorithm on the port's state: such an api has no registry entry
and no recorded built-in, which compare equal (`getBuiltinProviderForModel`:
`undefined === undefined`), so the request goes to the built-in provider, which
has no implementation of the api. Unreachable in pi, whose `BUILTIN_APIS`
implement all ten apis and register them at load. An api registered and later
unregistered (a recorded built-in, no entry) fails `No API provider registered
for api: <api>`, as pi's does. Through `Models` an unconfigured provider fails
`Provider is not configured: <provider>` before dispatch, as pi's does. Pinned
by `TestUnportedApisFailAsPisDispatch` (`testdata/unported-apis`, npm 1.3.0:
both texts for one catalog model per built-in api).

**D108 — "Invalid tools option" carries a hint.** `coding.NewSession` fails an
invalid `ToolNames` list with pi's "Invalid tools option: <problem>", followed
by how to fix it: " (pass either tool names and * patterns, or only +name/-name
entries naming exact tools, e.g. ["+grep", "-write"])" — errors carry
resolution hints. `TestSessionToolSelectionMatchesPi` compares pi's text as a
prefix followed by " (".

**D109 — a coding session without `Models` streams through compat, and the
runtime-key overlay is a public type.** pi's `createAgentSession` always has a
model runtime: `options.modelRuntime ?? ModelRuntime.create()`, which composes
`auth.json`, `models.json`, the remote catalog and extension providers (E1). The
port has no `ModelRuntime`. A session given `SessionOptions.Models` streams
turns, compaction and branch summaries through `Models.StreamSimple`, as pi's
streams through `modelRuntime.streamSimple`; a session without one streams
through compat's `ai.StreamSimple`, which resolves each request's key from the
environment (env keys, ambient auth, an unkeyed Cloudflare model through the
built-in Models). For auth that comes from the environment the requests are
pi's (`TestSessionRequestsResolveAuthAsPi`, scenario `env/compat`); they differ
only where pi's default runtime would read `auth.json`, `models.json` or an
extension provider. `coding.RuntimeCredentials` is exported although pi's is
internal to `ModelRuntime`, because it is the only place a Go host can set pi's
runtime key: `NewRuntimeCredentials(store)` with
`ai.BuiltinModels(&ai.CreateModelsOptions{Credentials: credentials})` as
`SessionOptions.Models`. A nil store is an empty in-memory one (pi's default is
`auth.json`), and setting a key does not refresh the provider's models (pi's
`setRuntimeApiKey` also runs `synchronizeCredentialState`, a `ModelRuntime`
composition step). A summary lays the session's `Headers` (a Go-only option)
over the auth headers it resolved; pi has no session headers.

### pi-env (ported 2026-10-19, upstream `packages/env` at `d2e7d8b7c`, npm 1.3.0)

The daemon is measured against npm 1.3.0's Rust daemons (`TestOracle`; the
`PI_ENV_RUST_DAEMON` differentials) and upstream's own suite, the client against
upstream's TS client (wire, connection and ssh oracles) and `NodeExecutionEnv`
(the differential). **Run only on darwin-arm64 and linux-arm64 (a container).**
The Windows code, a line-by-line port of `sys/windows.rs`, and the Android
behaviours are cross-built and vetted, never run; Android's pidfd case was shown
under a seccomp filter, not on a device.

**D97 — macOS watching is the daemon's own kqueue watcher, not FSEvents.**
FSEvents needs a C callback (cgo). `watch_darwin.go` (x/sys only) lists a
directory again on NOTE_WRITE and opens regular files with O_SYMLINK for their
writes, never following links; Linux and Windows watch through fsnotify
(inotify on Linux, as the notify crate does). Cost on a 710-file, 90-directory
worktree: 857 descriptors and 442 ms to `ready`, against Rust's 7 and 26 ms,
because every ancestor's entries are opened too. A tree beyond the descriptor
limit falls back to polling with `overflow`, the path Rust takes when it runs
out of watches. The FSEvents 500 ms settle rescan is kept for its timing.

**D98 — android/amd64 is a tagged linux/amd64 build.** Go links android/amd64
only with cgo, so that daemon is `GOOS=linux GOARCH=amd64 -tags pienv_android`,
which flips the three Android behaviours (`hello.os` "android", `$PREFIX/tmp`,
bionic's default PATH; `env/daemon/onandroid*.go`). Commands start through
`syscall.ForkExec` and `wait4`, never `os.StartProcess`, so Go's pidfd probe,
which Android < 12's seccomp kills with SIGSYS, never runs (`970d4ed`). Other
GOOS=android-only runtime behaviour is absent; none is known to matter.
android/arm64 is a real GOOS=android build.

**D99 — signals the daemon inherited ignored.** Children always get SIG_DFL and
an empty mask, as in Rust (`c4db8fa`, `a941ed1`). The daemon itself keeps
ignoring SIGHUP, SIGINT, SIGTSTP, SIGTTIN and SIGTTOU when started with them
ignored, but the Go runtime installs its handler over any other inherited
SIG_IGN before `main` and keeps no record of it: a daemon started with SIGTERM
ignored dies on SIGTERM, where Rust's keeps ignoring it.

**D100 — the daemon answers what Rust's cannot, and takes its whole descriptor
budget.** An exec `timeoutMs` that is negative or beyond a Duration panics
Rust's request thread (`Duration::from_secs_f64`) and the request never
settles; Go replies `EINVAL invalid field timeoutMs` (the client validates
timeouts first, so it never sends one). At start Go raises the soft
RLIMIT_NOFILE to the hard limit (children get the original back), so under a
low `ulimit -n` it reaches its own 4,096-handle EMFILE where Rust hits the
system's limit first.

**D101 — the C library's behaviour is emulated, not linked.** Rust's daemons
link musl (Linux), Apple's libc and bionic (Android). `execvp` is foreseen
rather than called: each candidate's `execve` outcome is predicted from
stat/access (and on macOS from `#!`/Mach-O magic, since a failed pty child
revokes the terminal there), and only the predicted-runnable one starts; a
failure metadata cannot show (ETXTBSY, a foreign-arch ELF) ends the search where
the library might move on, and macOS execvP's "path too long" stderr line is not
reproduced. `realpath` is ported line for line, musl's on Linux and Apple's on
macOS (getattrlist case, mount points), and pinned by `TestOracle/musl` and an
8,000-path differential. Bionic's `execvpe` (`.` for an empty entry,
`/system/bin/sh` for ENOEXEC) and realpath (the glibc walk, taken to agree) come
from source and were never run. On Windows, `remove_dir_all` is `os.RemoveAll`,
which continues past errors, and environment values that are not valid UTF-16
decode lossily where Rust's `env::var` treats them as unset.

**D102 — the embedded daemons' size.** Stripped Go daemons are 3.1–3.55 MB per
target (Rust: 1.9–2.6 MB), 26.5 MB raw for the eight; embedded gzipped, they add
**11.1 MB** to any program that imports package `env`. That is the price of
`go:embed` under the owner's ruling.

**D103 — client contexts.** `Connection.Request` waits for the daemon to start
without its ctx, as upstream does (the cancel follows the request). Where
upstream checks for an abort right after that wait — file operations,
`RenameFile`, `Watch`, `Exec`/`RunShell`/`RunProgram`, `ListFiles` — a Go
operation stops waiting when ctx ends: the same `aborted`, earlier.
`AbsolutePath`, `JoinPath`, `CreateTempDir` and every post-request path lookup
wait as upstream does. `Connection.Info(ctx)` stops waiting for a start when ctx
ends (a started daemon's info is returned regardless), and
`ScanHostKey`/`DetectPlatform`/`DeployDaemon`/`ConnectSSH` kill ssh when ctx
ends, where upstream has only its timeouts.

**D104 — client values: zero means unset, and numbers are integers.**
`SSHTarget.User`/`IdentityFile`/`ConfigFile` `""` and `Port` 0 are not passed
(upstream refuses `""` and 0, and passes `-F ""`); `RunOptions.Timeout` 0 is no
timeout (upstream: "Invalid timeout"); `CreateTempDir(ctx, "")` uses `tmp-`
(upstream keeps `""`); zero `RemoteWatchOptions` fields are not sent. Offsets,
lengths, bounds and `timeoutMs` are integers, so upstream's `1.5` conformance
assertions are not ported. `ReadTextLines(ctx, path, maxLines)` follows
`coding.FileSystem` — `maxLines <= 0` reads every line — where Durable returns
`[]` without opening the file (row 8 re-aligns it).

**D105 — client delivery and hangs.** A `watch` or `pty` request that ends
successfully before `ready`/`started` leaves upstream's promise pending
forever; Go returns an error. A panicking `onChange` is recovered, as upstream
catches a throwing one. `onChange` runs on the watcher's own goroutine, one call
at a time: `Close` from another goroutine while a call runs returns without
waiting for it (no call starts after it returns), and a reopened watcher's
`Overflow` is queued when the new watch is ready, so it always precedes that
watch's changes (upstream can deliver a change from the same chunk first).
`OnLog` runs on the connection's own goroutine, in order, possibly after the
output that caused it. Queues are unbounded, like Node's buffering. Upstream has
one thread, so none of this arises there.

**D106 — client packaging and representation.** `PackagedDaemon` returns the
embedded bytes (upstream: a path into the npm package); `LocalDaemon` writes
them to `os.UserCacheDir()/pi-env/pi-env-<sha256[:32]>[.exe]` (0700, temp and
rename), and a Connection whose command is such a path re-verifies and rewrites
it at each start; the SHA-256 that names the remote file is over the
uncompressed bytes. Durable's `FileError`/`ExecutionError` live in package `env`
until row 8; `FileError.Error()` is the message alone, and both also answer
`errors.Is(fs.ErrNotExist|fs.ErrPermission)`, `errors.Is(coding.ErrShellAborted)`
and `errors.As(*coding.ShellTimeoutError)`; spawn failures copy Node's
`spawn <program> ENOENT|EACCES`, others carry Go's text. A request's `env` object
encodes sorted where JS keeps insertion order; neither daemon sees the order
(serde_json's BTreeMap, a Go map), and every other request field is
byte-identical (the wire oracle). `readTextFile` decodes the whole file once
(WHATWG, BOM kept) where Node streams through StringDecoder at 512 KiB reads —
equivalent, since StringDecoder splits only where WHATWG ends a sequence (the
differential's 600 KB "é" file crosses those boundaries). `LocalExecutionEnv`
embeds `coding.LocalEnv`, not Durable's `NodeExecutionEnv` (row 8), so only its
`ListFiles` is pi-env's.

**Upstream-shared, not a port defect:** `ssh -o SendEnv=-*` does not stop a
`SendEnv` in an ssh config file (OpenSSH 10.3). Upstream passes the same option,
and the port keeps it.

## Open re-judgements

Two entries carry a re-judge instruction rather than a settled decision. They are
here rather than under Divergences so a skim does not read them as decided.

**D15 — pi's tool options: bash and PowerShell ported, the rest owed.** Ported
2026-10-20 (`dd78ecd`, `2755190`): `BashToolOptions` (operations,
commandPrefix, shellPath, exposeSessionEnvironment, spawnHook), `CreateBashTool`,
`PowerShellToolOptions`/`CreatePowerShellTool`,
`LocalBashOperations(LocalBashOptions{ShellPath})`, `LocalPowerShellOperations`,
with pi's timeout validation inside the local operations. **Owed:** pi's
`ToolsOptions` and the options-taking `createTool`/`createCodingTools`/
`createReadOnlyTools`/`createAllTools`; the other tools' `*ToolOptions` (read,
edit, write, grep, find, ls); `createBashToolDefinition`/
`createPowerShellToolDefinition` (the port has no ToolDefinition→AgentTool
split); `BashSpawnContext.cwd` from `ctx.cwd` (no ctx in Execute; row 18). The
session's `shellPath`/`shellCommandPrefix` come from pi's settings manager (E1),
so SDK callers pass them through `SessionOptions.Tools` + `CreateBashTool`.
pi's `windowsHide` on the `where` and shell spawns has no Go counterpart.

**K16 — session-entry `usage` fields.** pi records `usage` on branch-summary,
compaction and tool-result session entries; `coding/session_tree.go` and
`coding/session_store.go` carry zero occurrences. Triaged `n/a` 2026-07-21 on
the grounds that every consumer is unported so the fields would be dead code.
**That justification is exactly what the mirror ruling voids** — a mirror carries
upstream's fields whether or not the port consumes them. Not covered by Scope
queue entry 8 (which is for trees with no Go base; this one has a Go base that
omits fields), so nothing else will resurface it.

## Known parity debt

Places the port is KNOWN WRONG and pi is right, with no fix scheduled. Distinct
from Divergences: these are bugs, not decisions. **Not
`difftest/known-divergences.json`** — that file excuses one scenario at one
request-body JSON path, and only D3/D9-class items can ever be entries in it.

**K31 — JSON key order that one layout cannot reproduce.** The port writes
each message type with one key layout — pi's, since `1849a00`, measured by
`ai/testdata/key-order/key-order-1.3.0.json` (five adapters × success/400 ×
stream/agentLoop) — but pi's order depends on which adapter assembled the
object and when (`Object.assign`, late writes): (1) anthropic's strict-fallback
`diagnostics` sit right after `timestamp`/`providerThinkingLevel`; (2)
anthropic's `usage.reasoning` comes last, after `cacheWrite1h`; (3)
pi-messages' done and error events order their keys their own way; (4) a
Responses tool call whose `namespace` first appears at `output_item.done` gets
`argumentsError` before `namespace` (the port always writes `argumentsError`
last); (5) a message decoded from a pi session and re-encoded takes the
encoder's layout (canonical; Responses' `providerThinkingLevel` first by api;
the error literal only for messages built with `ai.NewErrorMessage`), so a
pi-written error literal re-encodes with `errorMessage` after `timestamp`; (6)
`endTurn` (codex only, unported) sits after `rawStopReason` with no oracle; (7)
`ai.Model` and `ModelCostTier` serialize in Go's struct order rather than the
catalog's (`type` first, a tier's `inputTokensAbove` last), which only
models-store persistence shows. Values are equal after parsing and no model or
provider sees the order — K12/K24/K28's class. Fix shape: per-adapter layouts
captured from pi, as `key-order` does for the common case.

**K32 — `Models` streams take no API-native options.** pi's
`ModelsApiStreamOptions` can carry an api's native fields (bedrock's and
pi-messages' `reasoning: ThinkingLevel`, among others); Go's
`ModelsStreamOptions` is `StreamOptions` plus transforms, so a Go caller cannot
pass native options through `Models` at all. Seen through telemetry's
`reasoning` start attribute, which pi reads from `options.reasoning` when it is
a string: no Go input reaches that branch, so the capture's `stream` row passes
none. Predates `2ec55d808`. Fix shape: a native-options field on
`ModelsStreamOptions`, forwarded to the adapter, with telemetry reading it as pi
does.

**K33 — pi-env's `nodepath.FileURLToPath` parses no IPv6 or IDNA host.**
`file://[::1]/x` and punycode or non-ASCII hosts are kept as typed (an ordinary
relative path), where Node v26's `url.fileURLToPath` returns a UNC path on a
Windows remote: 3 cases of the 13,260-answer node corpus, logged by
`TestAgreesWithNode` and not asserted. POSIX remotes are unaffected (Node
refuses any host there too).

**K34 — four built-in providers' api-key auth still differs from pi's.**
`TestBuiltinAPIKeyAuthNamesMatchPi` (`ai/testdata/builtin-auth`, npm 1.3.0)
compares every built-in provider's api-key auth name and login prompts, and its
`builtinAuthDebt` pins these as still differing: `amazon-bedrock` and
`google-vertex` (pi's api-key logins first ask for an auth method — "Select
Amazon Bedrock authentication method:", "Select Google Vertex AI authentication
method:" — and the port's ambient resolvers have no `Login`; the names match),
`openai-codex` (pi has OAuth auth only; the port keeps an ambient api-key
resolver because its OAuth login is unported and a `ProviderAuth` needs a
method), and `typesafe` (classifier-only; `BuiltinModels` passes over it).
Login acquisition is Scope queue row 1, typesafe row 3; each closes there.

**K35 — the session's auth gates.** pi's `AgentSession.setModel` throws
``No API key for ${provider}/${id}`` when `modelRuntime.checkAuth` finds
nothing, and `prompt` refuses before sending with
`formatNoApiKeyFoundMessage(provider)` when the runtime has no auth for the
model. The port's `Session.SetModel(model)` switches regardless and returns
nothing, and `Run` sends the request, which fails in the stream (`Provider is
not configured: <provider>` through `Models`, the adapter's own error through
compat). `cmd/pi` gates both with `requireProviderAuth`. Fixing it is `SetModel`
returning an error after `Models.CheckAuth` (compat:
`ai.BuiltinModels(nil).CheckAuth`, as `requireProviderAuth`), and the prompt
pre-check with pi's message; an API break the mirror ruling permits.

**K30 — a Retry-After that only V8's fallback date parser reads backs off in
Go; pi retries at once.** pi reads a `Retry-After` that `parseFloat` rejects
with `Date.parse`; the port uses `net/http`'s `ParseTime` (IMF-fixdate,
RFC 850, ANSI C). Only a value that does not start with a number gets this far
(an ISO date's year is read as seconds first), and there V8's fallback parser
accepts far more: measured in node v26.4.0, the isomorphic-decoded bytes
`c2 a0 35` (`"\u00c2 5"`) parse as 2001-05-01 local time, so pi computes a
finite, negative delay and retries immediately, where Go now falls back to the
backoff (since `2bbfcca43` an unreadable header dictates nothing). ANSI C dates
are local time in V8 and UTC in `ParseTime`. Neither reaches a real server's
`Retry-After` (seconds or IMF-fixdate), which is why this is debt rather than
a port of V8's `DateParser`. Pinned by `TestJSTrimRetryAfterHeaderBytes`,
which fails the day the `c2 a0 35` row starts matching.

**K20 — a narrowed `maxBytes` resizes to different dimensions than pi.** The
shrink loop tries PNG and then each JPEG quality at every size, and takes the
first candidate under the byte budget — so where it stops depends on how big
each ENCODER's output is. pi encodes with Photon (Rust); the port with Go's
standard library, which produces different byte counts for the same pixels. The
header of `coding/imageresize_parity_test.go` already says the pixels are not
byte-identical, but no row said the DIMENSIONS can diverge. Measured by this
cycle's parity review against pi 0.86.1's own `resizeImageInProcess`, on a
1024x768 noise image: `maxBytes: 200000` gives **576x432 in pi and 768x576 in
Go**, and `maxBytes: 20000` gives 324x243 against 243x182 — so the dimension
note the model reads says "Multiply coordinates by 1.78" in pi and "1.33" here.
12 of 14 profile runs agreed, including the defaults and every width- or
height-only profile. **Not live at 0.86.1:** no catalog model carries
`inputLimits`, and `f5c946480`'s generator stamps the default 4.5 MiB budget,
where real images clear on the first candidate. It becomes live the day a
provider narrows `maxBytes`, which is when to measure it again. Fixing it means
matching Photon's encoder output size, which the port cannot do without
Photon — so it is debt, and pinning the port's own dimensions in a green test
would only lock in the wrong answer.

**K19 — `ai/transcript.go` `GetCurrentSystemMessage` latches the FIRST system
message's timestamp; pi's `??=` keeps looking.** pi does
`timestamp ??= message.timestamp`, so a system message carrying no `timestamp`
key leaves the latch unset and a later message's timestamp wins, and
`getCurrentSystemMessage` returns `undefined` outright when no timestamp was
ever seen and no tools were declared. Go decodes an absent `timestamp` to `0`
and latches unconditionally. Measured: `[{role:system,content:"a"},
{role:system,content:"b",timestamp:1000}, user]` yields a replayed timestamp of
**1000 in pi and 0 in Go**, and `[{role:system,content:"base"}, user]` yields
**`undefined` in pi and `{…,"timestamp":0}` in Go** — which the forced-prompt
projection then reads for its head. **Debt, not a divergence row, because it is
unreachable from pi:** `SystemMessage.timestamp` is REQUIRED in pi's `types.ts`
at `16292398a`, so no pi producer emits a system message without it; it takes a
hand-edited or foreign session file, the same class as D21/K4. Fixable without
guessing — `SystemMessage` already records its own key order, so the replay
could latch on key presence rather than on the first message — and that is the
shape to use if a reachable case ever appears.

**K1 — openai max-token handling: truthiness gate and out-of-union field.**
`ai/providers/openai.go:928` gates on `*opts.MaxTokens > 0`; pi gates on JS
truthiness, where a negative value is truthy. And `:929` writes
`params[compat.MaxTokensField]` under whatever string the compat carries, where
pi's ternary funnels anything outside its union onto `max_completion_tokens`.
Both flip the thinking budget, because the clamp reads the max-tokens field back
out of `params`.

**K2 — `resolveThinkingBudgets` null-vs-absent (LATENT).** A nil `*int` means
"not overridden", but pi's spread copies an explicit `null` over the default and
`Math.min(null, room)` coerces to 0, which pi drops as non-positive. Latent —
Go exposes `*ai.ThinkingBudgets` only as an embedder-set struct field. **Fix
this when a thinking-budget settings decode lands.**

**K3 — `Schema.UnmarshalJSON` decode-boundary flips in strict tool conversion.**
The lenient-decode convention means that for tool schemas decoded from JSON, the
strict probe resolves `strict=true` where pi errors or falls back. Unreachable
through the built-in tools; reachable by a library caller. Fixing it means
strictifying `UnmarshalJSON` — a decision about the decode convention.

**K4 — anthropic native `ToolChoice` guard is `!= nil` where pi is truthy.**
`AnthropicOptions{ToolChoice: ""}` emits `tool_choice:{"type":""}` where pi omits
the key. Unreachable from the ported simple path; reachable through
`AnthropicOptions` directly.

**K6 — no thinking-level clamp on model switch; the anthropic leg turns thinking
ON.** `/think high` then `/model <non-reasoning>` leaves `ThinkingLevel ==
"high"` where pi lands on `"off"`. Mostly display-level because most adapters
re-clamp at request time — **but NOT anthropic: `ai/providers/anthropic.go:303`
gates only on `reasoning == ""` and never consults `model.reasoning`.** The
helper already exists: `ai.ClampThinkingLevel`; `SetModel`/`SetThinkingLevel`
simply do not call it.

**K11 — find-tool nested-repo ignore under-reach.** Only the outer `repoRoot`'s
`.git/info/exclude` is ever read (`coding/glob.go:236`); a nested repo's own
`info/exclude` is never re-rooted. grep path unaffected.

**K12 — `Usage.reasoning` `omitempty`; the code comment's rationale is
circular.** Real pi emits `reasoning:0` unconditionally for openai-completions,
openai-responses and google; the port drops the key. The note at
`ai/types.go:451-458` justifies it "to keep session goldens byte-identical" —
**the goldens are ours, so that is circular.** The fix requires regenerating the
sessparity goldens from the reference build: its own port+parity cycle.

**K13 — `coding/tools.go` `normalizePath` is not a drop-in for pi's.** It
applies unicode-space folding and `@`-stripping unconditionally where pi makes
both opt-in, and omits `normalizeWindowsShellPath` entirely, which pi applies
unconditionally on win32. The Windows gap affects every ported tool call site
that resolves a path. **Do not "fix" D6 by reusing this helper — that introduces
a divergence rather than closing one.**

**K14 — the system-prompt goldens do not cover PackageDir-derived bytes.**
`coding/systemprompt_golden_test.go:22-30` passes explicit paths, and the second
golden omits the docs block — so `PackageDir()` is never on that path, though a
live session takes the fallback at `coding/systemprompt.go:138-149`. **A change
to `PackageDir()` moves shipped prompt bytes with the golden suite fully green.**

**K15 — `packages/ai/src/utils/overflow.ts` has no Go home.**
`isContextOverflow`, `isRecoverableLength` and `getOverflowPatterns` are
unported, because their consumer is the unported agent-session runtime. **This
corrects the 2026-06-25 ruling's stated reason** — upstream keeps them in
`packages/ai/src/utils/` and re-exports from `packages/ai/src/index.ts:43`, i.e.
published SDK surface. A commit touching `overflow.ts` is a recorded absence,
not a port obligation, until an auto-retry/compaction loop lands in Go.

**K17 — no `SessionManager`: no label entries, no fork writer, no in-memory
session.** The port's session layer is `SessionRecorder` (append-only writer
keyed to one file) plus `SessionTree` (read-only parser of one file). No `label`
entry type, no `labelsById`/`appendLabelChange`/`getLabel`, no
`createBranchedSession` (writes a NEW file with labels stripped and the retained
path re-chained under `header.parentSession`), no constructor from in-memory
entries. Upstream `2631b25c3` and `2b768ba42` therefore have no Go home and are
carried by **Scope queue entry 12**. Two SDK-shape items ride with it: the
exported variadic-as-optional parameters (`StartSession(cwd, model,
thinkingLevel ...string)`, `Branch(fromID ...string)`, `BuildContext(leafID
...string)`) become an options struct + named variants in one change; and pi's
`uuidv7` is public SDK API (`packages/ai/src/index.ts:47`) where the port keeps
it unexported in `coding/session_store.go` — it moves to `ai.UUIDv7`/`ai.UUIDv7At`
with entry 12.

**K18 — google HTTP-error messages do not match pi's; measured.**
`ai/providers/testdata/httperror/pi-ai-1.3.0.json` (the published build: 52
rows, plus 5 `byteRows` with base64 bodies carrying invalid UTF-8, every
adapter's column recorded) holds all four adapters' `errorMessage` and a
`failFast` section, the retry fail-fast quote, which the port matches
(`TestHTTPErrorFailFastMatchesPi`). For `{"error":{"message":"blocked"}}` at 403
pi's google adapter surfaces the body verbatim with no status — the
`@google/genai` error's message already carries it, so `formatProviderError`
returns it unchanged — where the port says `Google API error 403: blocked`,
capped at 4,000; google hands the retry quote no SDK message, and no google
counterpart of `anthropicSDKErrorMessage` exists. **The anthropic half is
FIXED** (`9c569f5`): a terminal non-2xx is the SDK's message
(`${status} ${JSON.stringify(body)}`, uncapped, over `jstext.DecodeUTF8` text),
the retry quote is uncapped too, and stream-abort's "the connection drops while
an error body is read" now runs for anthropic. Both openai loops follow pi
(Responses `b32a55b`, completions `b9de81e`). **One transport gap remains on the
openai paths:** pi's fetch decodes an error body with the WHATWG UTF-8 decoder
(U+FFFD per maximal subpart) before any client parses it, while the error-body
path in `errors.go` passes the raw bytes; the byte rows' openai columns show
pi's U+FFFD. Fix shape: google's formatter at its call site against the
`google-generative-ai` column, and `jstext.DecodeUTF8` in `errors.go`'s shared
body path; rewrite the tests that pin the Go shape.

**K22 — request headers undici's fetch adds by default.** Measured after
`c231659`: pi's wire carries `accept: */*` when no Accept is present (always on
google, and on the SDK adapters after an accept deletion marker), plus
`accept-language: *`, `sec-fetch-mode: cors` and an `accept-encoding`. The port
sends none of them (google now sends `Accept-Encoding`, D81). Sibling of D12;
fixable by adding them where pi's fetch would.

**K23 — `strings.ToLower` where pi uses `toLowerCase`.** The anthropic tool-name
mapping (`ccToolLookup` and the Claude Code name table) and google's model-id
lowering still use `strings.ToLower`; header-name folds and the two auth gates
moved to `jstext.ToLower` (`b2e6612`). Differs only on the few code points where
Go's simple case mapping and JS's full mapping disagree.

**K24 — `Usage.CacheWrite1h` is `omitempty`.** pi writes `"cacheWrite1h": 0` on
every anthropic message; the port drops the key, and
`anthropic_response_model_test.go` normalizes it away
(`withoutZeroCacheWrite1h`). Same class as K12.

**K25 — anthropic and pi-messages parse failures use encoding/json's wording.**
pi's cause is V8's `JSON.parse` SyntaxError text. `jstext.JSONSyntaxError`
reproduces it exactly (2,244 node rows) and the google loop surfaces it; wire it
into `iterateAnthropicSSE`'s parse failure and pi-messages' unparseable frame,
whose capture tests currently check pi's text only around the cause. The openai
loops no longer surface V8's text: under openai 7.19.0 a data line JSON.parse
rejects fails with the SDK's own `Error reading response: malformed server-sent
event JSON.` (`1660cbe`), and `jstext.JSONSyntaxError` only decides which lines
JSON.parse rejects (`openaiStreamJSON`).

**K26 — request URLs are parsed with net/url; pi's fetch uses the WHATWG URL
parser.** The SDK adapters join the base URL and endpoint path as their SDKs'
`buildURL`/`constructUrl` do, dropping one slash, so a base ending in two sends
an empty path segment (`sdkJoinURL`); pi-messages trims every trailing slash.
Each adapter sends what `requestURL` (`ai/providers/fetch_url.go`) makes of that
string, where pi builds it (after onPayload for the SDK adapters, before it for
pi-messages). `requestURL` applies the WHATWG parser's input handling before
net/url parses: leading and trailing C0 controls and spaces are stripped, every
tab, LF and CR is removed, and other C0 controls and DEL are percent-encoded;
for a special scheme, a run of slashes and backslashes after `scheme:` reads as
`//` and a backslash before the query or fragment as `/`. A special URL is then
held as that parser holds it: ASCII host lowercased, default port and leading
zeros dropped, an empty path as `/`, an empty userinfo dropped. It refuses with
pi's `Invalid URL` a URL net/url cannot parse, one with no scheme, a special one
with no host, or a port past 65535. A URL with credentials is refused at the
send, as undici's fetch refuses it, for the port's own client only: google and
pi-messages fail with `Request cannot be constructed from a URL that includes
credentials: <href>`, anthropic and both openai loops with their SDKs'
`Connection error.`, or `Request timed out.` when the URL passes the SDKs'
`/timed? ?out/i` test of the refusal's text (`sdkFetchError`); a custom client
is handed the URL. Measured in `ai/providers/testdata/base-url` (65 rows, 39
hrefs, 4 custom-fetch rows, 230 requests). Where the parsers still differ, rows
are tagged and guarded by `TestInvalidBaseURLDivergencesStillDiffer`,
`TestRequestURLDivergencesStillDiffer` and
`TestBaseURLRequestDivergencesStillDiffer`. Only net/url refuses a malformed
percent escape (`%zz`, a lone `%`) or a space in the userinfo; pi sends those.
Only the WHATWG parser refuses an IPv4 part past 255 (`256.0.0.1`). The WHATWG
parser also resolves dot segments (`/a/../v1`, `%2e%2e`), reads `127.1` as
`127.0.0.1`, leaves `|` in a path unescaped (the port sends `%7C`), and
percent-decodes a host (`ex%61mple.com`, which net/url refuses). The SDKs
re-serialize a query in the base URL (pi-messages does not), and @google/genai
puts `alt=sse` before a fragment in the base URL, where the port's lands inside
it. Not in the capture, measured ad hoc: pi runs a non-ASCII host through UTS
46, while the port leaves it to net/http, which punycodes the Host header
without that mapping. So `ëxample.com` goes out as pi sends it
(`xn--xample-ova.com`), but `BÜCHER.example` does not (pi
`xn--bcher-kva.example`, the port `xn--bcher-2pa.example`). Path to close: a
WHATWG host parser (percent-decoding, UTS 46 via golang.org/x/net/idna, and IPv4
number parsing) and path serializer (dot segments, the path percent-encode set).
x/net is a new root requirement, so `internal/policy/deps_test.go` must list it
with its reason.

**K27 — an empty anthropic thinking signature is omitted.** pi's anthropic
thinking block always carries `thinkingSignature` (`?? ""`), so pi writes
`"thinkingSignature":""`; the port's field is `omitempty`. Same class as K12
and K24.

**K28 — session files are written with `encoding/json`.** `SessionRecorder`'s
`writeLine` marshals each entry map with `json.Marshal`, so an entry's own keys
come out sorted — `{"cwd","id","timestamp","type","version"}` for the header,
`{"id","message","parentId","timestamp","type"}` for a message — where pi's
`JSON.stringify` keeps insertion order (`{"type","version","id","timestamp",
"cwd"}`, `{"type","id","parentId","timestamp","message"}`), and every string
escapes `<`, `>`, `&`, U+2028 and U+2029 (D9's class), which pi writes
literally; a message's own keys keep pi's order (measured 2026-09-27), though
not the keys of objects nested in it that the port holds as maps — a tool
result's `details` (the shell and grep tools' come out sorted where pi's keep
insertion order; the review of `58ad297` measured it and ruled it this
entry's class, since no model or provider observes it). Nested calls'
`arguments` are raw JSON since the same review, so they keep pi's order.
Values are equal after parsing, and both loaders read either file. Fix shape: write
each entry in pi's key order with JavaScript's escaping
(`internal/jstext.Stringify` is the JSON.stringify port), with a golden from
pi's `SessionManager`.

**K29 — undici's 300s bodyTimeout is not modelled.** pi's fetch fails a response
body that sends nothing for undici's bodyTimeout (300s by default, and pi's CLI
sets it from `httpIdleTimeoutMs`) with `terminated`: measured at `2b0a123de`
with the timeout shortened to 1s, all three SDK adapters fail `terminated` about
1.5s into a stalled stream. `sharedClient` deliberately leaves body reads
uncapped, so the port waits until ctx ends. Fix shape: an idle timer beside
`fetchBody` on the port's own client (reset by every read that returns data)
that cancels the request after `undiciBodyTimeoutMs` of silence, the read then
failing `terminated` since the caller's ctx is live; google and pi-messages read
through fetch too, so they need it as well.

## Conventions

Standing procedure with no other home. Rules whose authority is a skill file
live there and are not repeated here.

**C1 — the base gate includes a cross-target Windows pass.** Run `GOOS=windows
go build ./...` and `GOOS=windows go vet ./...` as part of **every** base gate,
not only when a `//go:build windows` file changes. The two constrained files
(`coding/proc_windows.go`, `ai/providers/pi_user_agent_windows.go`) are called
from *unconstrained* code, so a rename in ordinary `coding/` code breaks
`GOOS=windows` while touching no constrained file. The darwin host gate never
sees either file.

**C2 — the port's exported Go surface follows pi's package index, not the TS
`export` keyword.** Upstream marks module-internal helpers `export` because its
test files are separate modules; a Go test in the same package reaches them for
free, so mirroring that `export` widens the public API for a reason that does not
exist in Go. The mirror ruling sharpens this rather than voiding it: mirror is
architectural, not lexical.

**C3 — keep upstream's stale names and its inline copies on shared machinery.**
When upstream generalizes a mechanism but keeps its original narrow name, the
port keeps that name too — renaming increases the distance to the stem and makes
every future diff harder to map. Recorded case (`80e62761f`): a finding proposed
renaming the bash-prefixed shared shell machinery once it served powershell;
refuted, because upstream did the same in that exact commit. Same class: do not
extract a repeated string literal into a Go constant where pi holds the same
inline copies at the same structural sites (the three `"Operation aborted"`
literals at `agent/loop.go:572,670,687`). This is about distance to the stem, not
style — it does not license transliterated TypeScript.

**C4 — a "pure rename" can still move the wire.** When a rename touches a
package that serializes errors or protocol payloads, grep the renamed identifier
inside **string literals**, not just as an identifier, and check whether any hit
crosses the protocol boundary. A diffstat that reads mechanical is not evidence:
an unasserted wire string is green both before and after. (`bb6a1cddc`: a renamed
literal was built into a `ProtocolError` with code `invalid_request` and passed
verbatim to the peer.)

**C6 — the port has no compat allowlist.** `ai.Model.Compat` is a bare
`json.RawMessage`. Upstream hunks that only register a compat key in a TypeBox
schema have no Go home and produce no delta. **The trap is the DEFAULT:** pi
writes `model.compat?.<key> ?? <default>` and several keys default to TRUE, where
Go's zero value is false — seed those in the reader's defaults literal rather
than leaving them implicit.

**C7 — a golden whose upstream source is gone is frozen, and its test cannot
pass empty.** When upstream deletes the source of a golden the port still uses,
the golden keeps its section, frozen at the last sha that had the source (so it
can never be regenerated by mistake), its generator says so, and the test
`t.Fatal`s on an empty section. A table-driven test whose table can become
empty is not a test: `TestAssertCompleteFrameMatchesUpstream` passed for a cycle
iterating nothing after the `assertCases` section was dropped (the test went
with `protocol/` in `83a344c`). A golden whose user the port deletes goes with
it; freezing is for goldens the port still uses.
