---
name: pi-triage
description: Decide whether an upstream pi change needs porting to the Go port. Use when assessing upstream commits/PRs ("should we port X?"), or as the triage stage of /pi-sync. Outputs a WHY/WHAT/SCOPE verdict per change.
---

# pi-triage — port-or-not verdict for upstream changes

Input: one or more upstream main-line shas (or a range `A..B`). Each unit is a
first-parent change: a merged PR is ONE unit, analyzed via `git diff <sha>^1..<sha>`.

## Setup
- Upstream clone: `$PI_UPSTREAM_DIR` if set, else `~/.cache/pi-upstream`. If
  missing: `git clone https://github.com/earendil-works/pi "$dir"`. Always
  `git fetch origin main` first, then `git -C "$dir" checkout -B main
  origin/main` to keep the checkout from drifting (it is a read-only mirror;
  the harness extracts by `git archive <sha>`, so this is safe).
- **Never judge from the working tree — name a sha.** `git show <sha>:<path>`,
  `git diff <sha>^1..<sha>`, `git grep <pattern> <sha> -- <path>`. A stale
  checkout can show the exact opposite of what the pin holds (2026-08-11: the
  tree was ~2 months behind and read as though a ported change had been
  reverted), and a fresh one is at `origin/main`, which is ahead of the pin.
- **The authoritative scope boundary is `docs/UPSTREAM.md` -> "The scope
  boundary" — the exclusion tests E1 and E3, rewritten 2026-08-27. Read it before judging
  anything.** It replaced an exclusion path-list; the tests beat every list,
  including the tables in this file. Also read "Scope queue" (what is IN scope
  but has no Go base yet) and the "Current pin" table.

## Per change, produce:
1. **WHY** — intent, from the commit/PR message (`git log -1 --format=%B`) and
   any linked issue number in it.
2. **WHAT** — read the actual diff (`git diff <sha>^1..<sha> --stat` then the
   hunks for non-trivial files). Note behavior, constants, model-visible
   strings, new/changed tests.
3. **SCOPE** — verdict, one of:
   - `port` — touches behavior we ported.

     **Scope is decided by the EXCLUSION TESTS (E1 and E3) in `docs/UPSTREAM.md` ->
     "The scope boundary" (rewritten 2026-08-27). Read them; they are
     authoritative over this table.** A hunk is `n/a` iff one fires; otherwise
     it is IN. The table below is a *derived convenience* — if it and a test
     disagree, the test wins, you port accordingly, and you fix the table in the
     same commit.

     The tests, in one line each (E2 is NOT one of them):
     - **E1 host surface** — its job ends at presenting to / prompting /
       configuring a human at a terminal, or packaging the Node artifact, **or
       its only consumers are host surface**. Applies to a HUNK, not just a file.
     - **E2 is NOT an exclusion** (owner ruling 2026-08-27 evening). If it is
       part of the pi SDK, the port supports it. If a faithful port would need
       an outside Go dependency, open a Scope queue row flagged `CONSULT` and
       put the specific question to the owner — never `n/a` it yourself.
     - **E3 no Go representation** — a TS type-level construct with no runtime
       behavior (today: the telemetry schema's type inference only; its schema
       DATA is IN).

     **Reachability is evidence, not a test.** Root-exported means "probably SDK
     surface" and is why the default is IN — but it argues one direction only
     and NEVER overrides E1-E3. pi root-exports its whole application, TUI
     components included.

     | upstream tree | Go home | notes |
     |---|---|---|
     | `packages/ai/src` | `ai/`, `ai/providers/` | **OUT:** only `cli.ts` (E1). `amazon-bedrock` and `openai-codex` are IN scope with open dependency consults (queue entries 9 and 10) as of 2026-08-27 — do NOT mark them `n/a`. Azure, Mistral, Vertex, Radius, images and `auth/oauth/**` are IN as of 2026-08-27 — most are QUEUED, see below |
     | `packages/ai/scripts` | `ai/models_catalog.json` **at the next release regen** | generator-only; verdict is `port-but-CATALOG-ONLY`. Includes `openrouter-catalog.ts` and `model-data.ts` (`generate-image-models.ts` was deleted by `a328aa89a`; images and classifiers now come out of `generate-models.ts`) |
     | `packages/agent/src` | `agent/` | Since `7fd478a2e` (2026-10-01) pi-agent-core is only `agent.ts`, `agent-loop.ts`, `proxy.ts`, `stream-fn.ts`, `types.ts`; the harness and `search/**` are deleted. `proxy.ts` (`streamProxy`) has no Go home yet: Scope queue 26 |
     | `packages/coding-agent/src/extensions` | Scope queue 21-23 (no Go base yet) | **2026-09-29 ruling:** `mcp/**`, `codemode/**`, `tool-search/**` are IN as Go SDK features — root-exported and wired into `createAgentSession` — except their host files `mcp/ui.ts`, `mcp/cli.ts` + `cli.lazy.ts`, `codemode/renderer.ts` (E1). `mcp/index.ts` splits by HUNK. `llama/**` is E1 (only `main.ts` loads it; not root-exported). A NEW built-in: check the root export before judging it |
     | `packages/mcp`, `packages/codemode` | Scope queue 21, 22 | 2026-09-29, IN — published packages. codemode's `quickjs-wasi` → wazero in a submodule (pre-decided) |
     | `packages/durable` | Scope queue 8 | IN, FUNDED, QUEUED — never `n/a`, never escalate. **Split by hunk before you queue it:** `src/env/{index,node}.ts`'s `ExecutionEnv`/`FileSystem`/`Shell`/`NodeExecutionEnv` are SHIPPED as `coding/execenv.go` (slice 8b-i, at the old harness shape), so a hunk on a method `LocalEnv` has is an ordinary `port`; the members it lacks are listed on row 8. Durable Object adapters (`*/cloudflare`) have no meaning in Go — drop them |
     | `packages/chord/src` | `chord/` (`context/index.ts` → `context.go`, `json.ts` → `json.go` + `internal/jsonvalue`), `delta/**` → `chord/delta/` | The chord2 host runtime (everything else, and `./node`) is Scope queue 27. `./output` and `./sqlite*` are durable's code: Scope queue 8. `loader.ts`'s `nodeLoader` has no meaning in Go (`7d75a0e3e`) |
     | `packages/env` | `env/` — its own module, `github.com/sky-valley/pi/env` (2026-10-19 ruling) | Client `src/**` → package `env` (+ `env/internal/nodepath`); daemon `daemon/src/**` (Rust) → `env/daemon`, ported Go, so **a Rust-only change to the daemon is an ordinary `port`**, judged against npm's Rust daemon. A daemon port regenerates `env/bin` (`go generate ./...` in `env`) |
     | `packages/telemetry` | `telemetry/`; `src/testing/**` → `telemetry/telemetrytest` | 2026-08-06, widened 2026-10-19: the runtime contract (propagation, links, metrics, capabilities), the schema DATA (`telemetry/schema.go`) and the adapter conformance suite are IN; only the schema's type inference (`defineTelemetrySchema`'s typing, `Infer*`, `SchemaTelemetrySpan`, `TypedTelemetryMetrics`) is E3. The contract, the data and the inference share `src/index.ts`, so split it by HUNK |
     | `packages/coding-agent/src/{core,utils}` | `coding/` | `src/client/**` and `src/experimental/**` are deleted (`7472cda0d`) but for `experimental/source-resolver.ts`, which has no Go subject. **OUT** (E1, and this list must match the ledger's out-table exactly): `core/extensions/**`, `core/export-html/**`, `core/settings-manager.ts`, `core/prompt-templates.ts`, `core/agent-session-runtime.ts` + the session-reload / `/new` lifecycle, `core/model-registry.ts`, `core/resolve-config-value.ts`, `core/radius.ts`, `core/resource-loader.ts` source-info accessors, `core/provider-composer.ts`, `core/model-config.ts`, `migrations.ts`, `core/keybindings.ts`, `core/tools/renderers/**`, and the `builtin:<name>` extension resolution (`source-info.ts`'s `BUILTIN_PATH_PREFIX`, `package-manager.ts`'s and `resource-loader.ts`'s built-in extension halves). **Partly ported, split by HUNK, never diffstat-dispatched:** `core/model-resolver.ts`, `core/package-manager.ts`, `core/trust-manager.ts`, `core/model-runtime.ts` (its `Models` contract is `ai.Models`, 2026-09-24). `core/remote-catalog-provider.ts` is Scope queue row 19. The trust *decision and gate* are IN (2026-08-27); the trust prompt, selector and store are host. **Also split by HUNK, inside the IN-scope queued tree:** `core/session-manager.ts` — its static discovery surface (`DefaultSessionDir`, `ListSessions`, `LatestSession`, `FindSessionByID`, the header reader, id generation, migrations) is `coding/session_store.go`, the class itself is Scope queue entry 12; a hunk on the shipped half is an ordinary `port`, never an entry-12 append (2026-09-16: the draft parked `findById` and the adversarial pass caught it). `core/client-identity.ts` -> `coding/client_identity.go` (pi's identity for its own requests: detailed User-Agent, attribution while install telemetry is on); the identity machinery itself is pi-ai's since `adc5fa9f8` (`packages/ai/src/utils/identity.ts` -> `ai/identity.go`, `providers/opencode-headers.ts` -> `ai/opencode_headers.go`, the adapters' use of it -> `ai/providers/identity_headers.go`); `core/provider-attribution.ts` and pi-ai's `utils/pi-user-agent.ts` are deleted upstream, and coding's `utils/pi-user-agent.ts` is a host-only helper (its only consumer is `tools-manager.ts`, E1) |

     `utils/` is judged **by its consumer, not its path** — in scope only when a
     ported core file consumes it.

     **Project-local reads carry their gate.** Any change that adds a read of
     `<cwd>/.pi/**` or an ancestor `.agents/**` must ship with its project-trust
     gate (`SessionOptions.TrustProject` / `LoadSkillsWithTrust`). An ungated
     new reader is a **defect**, not a scope question (2026-08-27).

   - `port-but-QUEUED` — the tree is IN scope but has **no Go base yet**, so a
     hunk-sized port of a file that does not exist cannot land. Verdict is
     `port`; no Go commit this cycle; append the delta to its entry in
     `docs/UPSTREAM.md` → "Scope queue" instead. This is the
     `port-but-CATALOG-ONLY` pattern generalized. The current entries are the
     rows in `docs/UPSTREAM.md` -> "Scope queue" —
     read them there rather than trusting any list here, and **open a new row
     yourself** when an in-scope change has no Go home. Once an entry's base
     port ships, the entry closes and that tree triages as ordinary `port`.

   - `port-but-CATALOG-ONLY` — a `packages/ai/scripts` generator delta. It is a
     `port`, but nothing lands until a release tag is crossed and the catalog is
     regenerated from the npm build. Add it to the queue in `docs/UPSTREAM.md`
     rather than porting it now.
   - `n/a` — excluded by a test. Give the specific reason **naming which test
     fired (E1/E2/E3)**, and prefer a **structural** check over precedent alone (grep
     the port for the identifiers the change touches; a recorded absence is
     worth more than a category).

     **Host surface (E1) — dispatch from the diffstat, no hunk-reading owed.**
     `packages/tui`, `packages/evals`, `packages/ai/src/cli.ts`, and under
     `packages/coding-agent/src`: `modes/**` (including `rpc/` — headless is
     still host), `cli/**`, `main.ts`, `core/extensions/**`,
     `core/export-html/**`, `core/settings-manager.ts`,
     `core/prompt-templates.ts`, `core/agent-session-runtime.ts` + the
     session-reload / `/new` lifecycle, `core/model-registry.ts`,
     `core/resolve-config-value.ts`, `core/radius.ts`, `core/resource-loader.ts`
     source-info accessors, `core/provider-composer.ts`, `core/model-config.ts`,
     `migrations.ts`, `bun/**`, `package-manager-cli.ts`, `core/keybindings.ts`,
     `core/tools/renderers/**`, `extensions/llama/**`, the `builtin:<name>`
     extension resolution, and the built-ins' host files `extensions/mcp/ui.ts`,
     `extensions/mcp/cli.ts` + `cli.lazy.ts`, `extensions/codemode/renderer.ts`.

     `core/model-registry.ts`, `core/resolve-config-value.ts`, `core/radius.ts`
     and the `core/resource-loader.ts` accessors are E1's **only-consumers**
     clause — host machinery populating a ported-and-latent SDK seam. That
     clause fires **only when EVERY consumer is host**; a mixed consumer set
     does not fire it, so read the hunk.

     **NEVER diffstat-dispatch these five — they are partly ported and the
     shortcut would manufacture a miss** (see the split-by-HUNK table in
     `docs/UPSTREAM.md`): `core/model-resolver.ts` (its
     `defaultModelPerProvider` is `coding/resolve.go`, and that table has caused
     a miss TWICE — diff it on every touch), `core/package-manager.ts` (skill
     discovery is `coding/resources.go`), `core/trust-manager.ts` (decision and
     gate are ported), `core/model-runtime.ts` (its `Models` contract is
     `ai.Models`, 2026-09-24), `packages/telemetry/src/index.ts` (schema data
     IN, type inference E3). **A commit spanning the seam is triaged on its
     non-host hunks alone and NEVER escalates on account of the host half.**
     This is the rule that removes most of the per-commit cost — roughly half of
     all mixed commits are mixed only because of host content.

     Also `n/a`: the telemetry schema's type inference (E3 — split by HUNK, it shares `src/index.ts` with the ported contract and schema data); the trust prompt,
     selector and store; and the always-noise set — docs, CHANGELOG, CI,
     `.github/`, `.pi/`, examples, per-package `package.json` version bumps,
     repo-root `scripts/`.

     **No longer `n/a` as of 2026-08-27** — do not carry these forward from
     memory or from an older cycle section: OAuth acquisition, Radius +
     `radius-config`, image generation, Azure, Mistral, Vertex, and the
     project-trust decision/gate. They are `port` (mostly `port-but-QUEUED`).
   - `decide` — changes the *boundary* itself. Escalate to the user instead of
     deciding silently.

     **But do not reach for `decide` reflexively.** The exclusion tests plus
     the owner's **standing formula** — full pi SDK functionality as represented
     in Go, close faith to the source architecture, leaning into Go's idioms —
     already answer nearly everything that looks new. Re-asking a settled
     question is itself a failure mode. These axes are SETTLED and must not be
     re-escalated:
     - a transport or feature depending on a runtime Go does not target
       (2026-08-11);
     - a `DRAFT:` subject prefix (2026-08-04);
     - an upstream REVERT, even one removing Go API the port shipped days
       earlier (2026-08-19; the "unless a cut tag already published it"
       carve-out was WITHDRAWN 2026-09-03 — following upstream in deleting
       published Go API is never an escalation);
     - a pre-existing parity gap inside an already-ported function (2026-08-18);
     - **anything E1 covers** — never escalate on a host hunk (2026-08-27);
     - **whether an SDK adapter is in scope** — it is. A dependency question is
       a `CONSULT` queue row, not a `decide` and never an `n/a` (2026-08-27);
     - **the durable runtime** (the agent harness until `7fd478a2e` deleted it)
       — FUNDED 2026-08-27, and its SHAPE was SETTLED 2026-09-03 as a mirror of
       upstream's layout (a Go `durable` package grows beside `coding/`; they
       are not merged, because upstream keeps both forks with zero
       cross-imports). Not a triage question any more. Carry its deltas as
       `port-but-QUEUED` against Scope queue entry 8;
     - **pi-env and its daemon** — ported whole, daemon in Go (2026-10-19). A
       Rust-only daemon change is an ordinary `port`;
     - **whether following upstream into an API break is allowed** — it is
       (2026-09-03 mirror ruling). Upstream deletes, we delete;
     - **whether a construct with no meaning in Go is in scope** — if its whole
       purpose is absent here (chord's esbuild JS-plugin bundler, e.g.), drop it
       and say so in the cycle entry. Triage call, not a CONSULT (2026-09-03).

     **IN scope but no Go home is NOT a `decide`.** Open a Scope queue row
     instead — the tests already answered scope; only scheduling is open, and
     opening a row needs no owner decision. See "How to OPEN an entry" in
     `docs/UPSTREAM.md`. Escalating here is exactly how the harness got stuck.

     A genuinely new question changes a **test**, not a path. The two that
     exist: *"should the port take a third-party dependency?"* (answered for
     bedrock, codex, SQLite, codemode and pi-env) and a transport that changes the
     `ai.HTTPDoer` seam itself. Both are owner policy questions with durable
     answers.
4. For `port` verdicts: map upstream files → our Go files (e.g.
   `core/tools/grep.ts` → `coding/tools.go` + `coding/glob.go`), and flag
   whether any **byte-golden surface** is touched (system prompt, tool output
   strings, request bodies, session format, image decisions) — the parity
   reviewer needs to know.

## Output
A table: `| sha | date | subject | verdict | reason | upstream files → Go files | golden surface? |`
plus one line per `decide` item explaining the boundary question.

Rules: judge from the DIFF, not the subject line (subjects lie; refactors
hide behavior). A change that is 90% TUI but moves one constant in
`coding-agent/src/core` is `port` (for that constant).

Specific rulings (from pilot runs — keep appending):
- **A built-in extension is IN when pi root-exports it for SDK use**
  (2026-09-29: mcp, codemode, tool-search), and E1 when only `main.ts` loads
  it (llama). Its logic is mirrored file for file under the Scope queue row, so
  a hunk there is `port` (or `port-but-QUEUED` until the row's base lands),
  its UI/CLI files are E1, and `extensions/mcp/index.ts` is read by hunk.
- **Release commits are `port`**: they regenerate `packages/ai/src/models.generated.ts`,
  which IS ported surface (→ `ai/models_catalog.json`, regenerated from the
  matching npm build). The version bump/changelog parts are noise; ALSO note
  the release tag so /pi-sync refreshes the npm reference build.
  Since `a328aa89a` the image and classifier catalogs are `IMAGE_MODELS` / `CLASSIFIER_MODELS` inside `models.generated.ts` (`image-models.generated.ts` is deleted): dump them alongside `MODELS` at every regen for Scope queue row 3's record, and embed them only once row 3 lands.
  A release tag ALSO drains the `port-but-CATALOG-ONLY` queue — but only of the
  deltas that are **ancestors of the release sha**. Check that with
  `git merge-base --is-ancestor <delta> <release>`, never by log order: generator
  deltas landing after the release commit belong to the NEXT regen. And decide
  the regen itself by EXECUTING `JSON.stringify(MODELS)` against both npm builds
  (2026-07-30 ruling) — a git sweep can never see catalog data, since it is
  generated at publish time and not committed upstream.
- `packages/coding-agent/src/utils/` is in scope only if a ported core file
  consumes it — judge by the consumer, not the path. Note this rule is
  UNEXECUTABLE if the reconciliation sweep never printed the path, which is why
  the sweep must never carve out a sub-path (see below).
- `.pi/` files (upstream repo's own agent config/extensions) are always `n/a`.
- Generated/data files count as ported surface when we embed their derivative
  (currently: models.generated.ts → ai/models_catalog.json — the only one).
- A follow-on commit to a pending `decide` inherits that escalation: mark it
  `decide (rider on <sha>)` and batch into ONE question to the user.
- Once the user rules on a `decide`, record the ruling in docs/UPSTREAM.md
  ("### Rulings", and update "The scope boundary" if a test moved) so
  future triage doesn't re-ask.
- Cost control: changes whose `--stat` touches only docs/CHANGELOG/.github/
  scripts/.pi/packages/tui may be dispatched from the diffstat alone; read
  hunks only when any path is in or near ported surface.
- **Merges can smuggle in-scope hunks** (2026-07-17 lesson: `5220aba6`'s xai
  responses hunk + `97f9978f`'s force flag + the v0.80.8 release all rode in
  via merges and were missed by per-first-parent diffstats — caught only by
  the adversarial parity review). After per-commit triage, ALWAYS reconcile
  with **both** passes. They catch different things; neither replaces the other:
  1. **Detector — never skip.** `git diff <pin>..origin/main --name-status
     --no-renames` with NO pathspec, and classify every path it prints (with
     rename detection on, a moved file prints only its destination and the
     deletion of its source is invisible — `b45597504`, 2026-09-24). This is the pass that
     surfaces a brand-new top-level package, a file moved out of `packages/`,
     or a new repo-root directory — things no pathspec written today can
     anticipate. It is what caught the 2026-08-22 blind spot.
  2. **Accounting.** `git diff <pin>..origin/main --stat -- packages
     ':(exclude)packages/tui' ':(exclude)packages/evals'
     ':(exclude)packages/coding-agent/docs'
     ':(exclude)packages/coding-agent/examples'` — account for every file's
     delta against the verdicts.

  Merge-commit diffstats must be read in full, not truncated.
- **Sweep whole packages; never carve out a sub-path.** That exclusion list is
  the ONLY place a path may be pre-judged, and each entry earns its place
  structurally — leaf UI library, upstream's own eval harness, prose, samples —
  never by citing a current ruling. Sub-path carve-outs fail three ways, all
  observed here:
  - **They hide live code.** `coding-agent/src` holds ported code well outside
    `core/`:
    `src/utils/{text,mime,paths,frontmatter,image-resize-core,image-process,tool-result-images}.ts`
    → `coding/{text,tools,resources,imageresize}.go`. A guard scoped to
    `src/core` prints none of them. Note this also makes the "judge `utils/` by
    the consumer, not the path" rule above *unexecutable*: you cannot judge a
    path the sweep never printed.
  - **They outlive the tree they describe.** The 2026-08-01 ruling's "minus
    `server/src/legacy/`" was dead text for weeks before upstream deleted the
    rest of `packages/server` (`05bf9df65`, then `7472cda0d`). A carve-out
    inherited from a ruling reads as coverage while giving none.
  - **Some real exclusions are unexpressible as paths.** The telemetry ruling
    excludes the schema's type inference — but `defineTelemetrySchema`, the
    ported schema data and the ported runtime `Span` contract share
    `src/index.ts`. Only a human reading the hunk can separate them.

  Exclusions belong in the CLASSIFICATION step, executed on a hunk, never in
  the pathspec. The pathspec's job is to put paths on your screen.
- **A ported path does not imply a same-named Go package.** Classify what the
  sweep prints against the EXCLUSION TESTS in `docs/UPSTREAM.md` -> "The scope
  boundary", using the table in the SCOPE section above only as a lookup for Go
  homes. Do not maintain a second list anywhere: a duplicated table is how the
  skill and the rulings drifted apart for four cycles (2026-08-25), and a list
  of any kind is what the 2026-08-27 rewrite demoted — 12 of 16 scope rulings
  had moved a path INTO scope and no list survived that. If the table and a
  test disagree, the test wins, you port accordingly, and you fix the table in
  the same commit.
  Two path facts worth carrying that the table does not:
  - Catalog-only deltas live under `packages/ai/scripts`, but **not only in
    `generate-models.ts`** — `650e7a612` added a sibling
    (`scripts/openrouter-reasoning-options.ts`), so sweep the whole `scripts`
    tree rather than grepping one filename.
- **Host-seam commits are diffstat-dispatchable, and that is the point.** When
  a commit's excluded content is entirely host surface (E1), you owe no
  hunk-reading for it and no escalation — triage the non-host hunks and move on.
  Historically ~47% of mixed commits were mixed *only* because of host content,
  and reading them cost a cycle's attention to reach a foregone `n/a`.
- **E2 is NOT an exclusion.** Resolve a dependency question with the owner's
  standing formula: first establish what upstream does **at the pin**
  (`git show <pin>:<path>`) — three of the first three E2 consults rested on a
  premise already false. If upstream took no dependency, take none; if it took
  one, take the Go equivalent AND reproduce its containment. Open a `CONSULT`
  row only for cgo, a lost build target, or a shape change. Never `n/a` it, and
  never escalate it as a `decide`. (Bedrock's and codex's consults were both
  ANSWERED 2026-08-31.) A hunk *inside* one of them is still `port` when
  its Go home is a shared/generic function — the 2026-07-21 lesson applies
  unchanged.
- **A queued tree is `port-but-QUEUED`, never `n/a`.** Read the current rows in
  `docs/UPSTREAM.md` -> "Scope queue"; do not trust an enumeration written here,
  which has twice gone stale. Append the delta to the matching entry
  in `docs/UPSTREAM.md`. Marking one `n/a` re-creates exactly the drift the
  2026-08-27 rewrite closed.
- **A new project-local read must carry its gate.** Any change adding a read of
  `<cwd>/.pi/**` or an ancestor `.agents/**` ships with the project-trust gate.
  Ungated is a defect, not a scope question (2026-08-27).
- **One upstream fix can touch multiple sites that map to ONE Go file — or to a
  differently-structured Go file** (2026-07-21 lesson: `1942b260` "env section
  ignored" fixed BOTH `auth/helpers.ts` and `amazon-bedrock.ts`; the port landed
  the helpers half but missed the bedrock half, because in Go there is no
  `amazon-bedrock.ts` — ambient providers (bedrock/vertex/… anything not in
  `apiKeyEnvVars`) route through the GENERIC resolver in `builtins_models.go`
  `builtinProviderAuth`. Caught only by the parity review). When a change edits
  a shared helper AND a specific provider, list EVERY hunk in the commit and map
  each to its Go home — a provider-specific TS edit may collapse into a generic
  Go resolver rather than a same-named file. Don't assume one TS hunk = one Go
  edit, and don't assume a provider file has a 1:1 Go counterpart.
