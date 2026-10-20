# pi-vs-Go differential request-body harness

The strongest fidelity proof this project has: for identical inputs, capture the
**exact HTTP request body** that real pi builds and that the Go port builds,
canonicalize both, and diff.

Neither side needs an API key, and neither side touches the network — both
capture through a payload hook that halts before the first byte is sent:

- **pi**: `onPayload` in `@earendil-works/pi-ai` stream options; it throws.
- **Go**: `OnPayload` on `ai.StreamOptions`; it returns an error.

> **Where this lives, and why it moved.** It ran from `/tmp` (lost twice), then
> from `~/.cache/pi-diff` (durable, but unversioned and on exactly one disk —
> every ledger entry cited results from a rig with no history and no backup).
> It now lives in the repo it verifies, because that is the only place it can
> stay in lockstep: each sync re-pins it and flips scenarios based on the port's
> own state, and the two drifted whenever they were kept apart.
>
> The earlier note here said it must **not** live in the repo, because
> `github.com/sky-valley/pi` is published, public and MIT. That concern is
> resolved rather than overruled: `difftest/` carries its own `go.mod`, and a
> subdirectory with a `go.mod` is excluded from the parent module. Verified, not
> assumed — a module zip built with `golang.org/x/mod/zip`, the same library the
> module proxy uses, contains **zero** `difftest/` entries. Nobody running
> `go get github.com/sky-valley/pi` downloads any of this.
>
> Nothing here is sensitive: scenarios are synthetic, keys are the literal
> `"test-key"`, and the porting internals it exercises are already public in
> `docs/UPSTREAM.md`. Do not add a real key or a real transcript to a scenario.
> Scenarios aimed at OpenAI's own endpoint use `"sk-test-key"`: since upstream
> `02eed88fd`, both sides read any other credential sent there as a Sign in
> with ChatGPT token and drop `max_output_tokens`, `temperature` and the
> prompt-cache fields, so a plain `"test-key"` would stop covering them. The
> `responses-chatgpt-sign-in*` scenarios send a fake token on purpose.

## Run it

```
difftest/run.sh                    # everything; exits non-zero on any NEW mismatch
difftest/run.sh --only deepseek    # one scenario
difftest/run.sh --list             # scenario names
```

`run.sh` is self-healing: if the npm build or the extracted upstream source is
missing it rebuilds them before capturing. Nothing here is precious — deleting
`out/` or `pisrc/` costs one run.

It also re-verifies package authenticity every run: the `package-lock.json`
integrity hash must equal `npm view <pkg>@<version> dist.integrity`, or the run
aborts. (If the registry is unreachable the check is reported as SKIPPED, never
silently passed.)

## The four states, and the exit-code contract

A runner that can never exit 0 is a broken instrument. When a known,
escalated, pre-existing divergence makes scenarios fail, that would permanently
pin the exit code at 1, the exit code would carry no information, and the next
NEW regression would hide inside the noise — reviewers would learn to read
"3 FAIL" as normal. (The baseline is currently EMPTY: `known-divergences.json`
holds no entries, so a clean run is every scenario PASS, 0 KNOWN, 0 FAIL. The
scenario count is kept in one place, the snapshot under "Scenarios".)

**A run that never reaches the scenarios is DARK, not FAIL, and never PASS.**
`run.sh` executes the pi capture and the Go driver in one shot each *before* the
scenario list, and each aborts with **exit 1** — the same status `classify.py`
returns for "any FAIL" — while printing no scenario tally. Zero compared
scenarios is not a result. A slice that DELETES or RENAMES exported `ai` surface
must build `difftest/port` before any harness result from it is trusted; the pi
arm fails the same way on a stale npm cache or a failed `git archive`.

So every scenario lands in exactly one of four states:

| state | meaning |
| --- | --- |
| **PASS** | pi and Go are identical. |
| **KNOWN** | Differs, and **every** difference found matches a `known-divergences.json` entry exactly — same scenario, same JSON path, same kind of difference. Accepted, tracked debt. Not clean. |
| **FAIL** | Differs in any way the baseline does not cover. A port bug until proven otherwise. |
| **FIXED** | A baseline entry that **no longer fires**. The divergence was repaired; the entry is now stale. |

Exit codes:

| exit | meaning |
| --- | --- |
| `0` | every scenario is PASS or KNOWN, and every baseline entry still fires |
| `1` | at least one **FAIL** |
| `3` | no FAIL, but at least one **FIXED** (stale baseline entry) |

The summary line always prints the KNOWN count, and never renders as clean:

```
8 PASS, 3 KNOWN (tracked debt), 0 FAIL  [11 scenario(s)]
KNOWN is not clean: it is accepted, tracked debt. See known-divergences.json.
```

**Why FIXED fails the run (exit 3), deliberately.** A stale entry is worse than
no entry: it is a live, unattended permission slip. It says "a key-order
difference at `$.messages[2].tool_calls[0].function.arguments` in
`basic-tools-cache` is expected" — so the day someone reintroduces exactly that
regression, the harness reports KNOWN and exits 0, and the bug ships. The
window between "fixed" and "entry deleted" is precisely when that hole is open,
so the runner refuses to be green during it. The cost is one loud run for
whoever lands the fix; the alternative is a silent hole nobody is looking at.
FIXED gets its own code (`3`) rather than reusing `1` so that CI, or a human
skimming, can tell "the port got worse" from "the port got better and the
paperwork is behind" — but both are non-zero, because both need a human.

## Known divergences: `known-divergences.json`

**A baseline entry is tracked debt with a fixed home in the ledger — it is not
a permanent exemption, and it is not a statement that the Go behavior is
acceptable.** pi is still ground truth at every one of these paths. An entry
buys time on a divergence that is *already escalated and already written down
somewhere durable*; it does not close the bug, and it must never be added
merely to turn a red run green.

Each entry carries `id`, `description`, `symptom`, `root_cause`, `impact`,
`why_not_fixed`, `status`, `accepted_on`, `tracked_in` (which ledger, which
section), `retire_when`, and a list of `matches`.

### The matching rule is deliberately tight

A `matches` element excuses **exactly one difference**, keyed on all three of:

- **scenario** — the scenario name;
- **path** — the exact JSON path (`[*]` is accepted as an array-index wildcard,
  but prefer exact indices: a wildcard is looser than it looks);
- **kind** — the *nature* of the difference.

Kinds the classifier produces: `missing-key`, `extra-key`, `type`, `value`,
`array-length`, `embedded-json-key-order` (two JSON **strings** that parse to
the same content with different key order — how the divergence shows up on
openai-completions, where `arguments` is a string), `key-order` and `key-set`
(from the declared order-sensitive paths in `order.txt`), and
`order-path-missing`.

This is **not** "ignore these scenarios" and **not** "ignore these paths". If
`basic-tools-cache` starts differing in a different field, or the *same* field
differs by **value** instead of key order, or the key **set** changes rather
than the order, the classifier calls it FAIL — the entry does not absorb it.
That is the whole point: the harness exists to catch those.

A scenario is KNOWN only when *every* difference it exhibits is matched. One
unmatched difference makes the whole scenario FAIL (the matched ones are still
printed, as `(also: known …)`, so the noise does not obscure the signal).

### Adding an entry

Do not add one to make a failing run green. Add one only when all of these hold:

1. pi is confirmed right and Go is confirmed wrong (an entry is never a
   canonicalizer loosening, and never a change to the pi side).
2. The divergence is **already escalated and recorded in a durable ledger** —
   normally the pi repo's `docs/UPSTREAM.md` drift note for the cycle. Fill in
   `tracked_in` with the section, not just the filename.
3. `why_not_fixed` states a real, specific blocker (e.g. "needs a public Go API
   change"), not "not got to it yet".
4. `retire_when` states the observable condition that makes the entry stale.
5. The `matches` are as narrow as you can make them — exact scenario, exact
   path, exact kind.

Then `./run.sh` and confirm the scenario reports KNOWN, not PASS and not FAIL.

### Retiring an entry

When the runner prints **FIXED**, the work is not "make the harness quiet" — it
is:

1. Delete that `matches` element from `known-divergences.json`. If the entry
   has no `matches` left, delete the whole entry.
2. Close it out where it is tracked (the `tracked_in` ledger section): the drift
   note should say it is fixed, not leave a reader believing it is still open.
3. Re-run. The scenario should now be PASS and the run should exit 0.

Retire an entry the moment it stops firing. Do not "keep it around just in
case" — that is exactly the stale permission slip described above.

## What is compared, and against which pi

Configuration lives in `config.env`. There are two pi backends, because the Go
port is currently **ahead of the last npm release**:

| backend | source | when to use |
| --- | --- | --- |
| `dist` | `~/.cache/pi-npm/<version>/` — the published `@earendil-works/pi-ai` build | anything that has shipped. Strongest ground truth: it is what real pi actually runs. |
| `src` | `~/.cache/pi-upstream` at `PI_UPSTREAM_SHA`, extracted read-only via `git archive` into `pisrc/<sha>/` and executed directly under Node's TypeScript type-stripping. Third-party imports resolve from the published build's `node_modules`; the `@earendil-works` workspace packages pi-ai depends on (`pi-telemetry`) come from the same sha, extracted beside it and shimmed to their `src` by `pi/srclink.mjs`, because src imports what the workspace has there, not what the last release shipped | changes ported **before** they were released. The published build does not contain them at all, so it cannot be the reference. |

A `"backend": "src"` scenario says in its `note` which change the published build
lacks.

**pi is ground truth.** A mismatch is a port bug until proven otherwise. Do not
"fix" a mismatch by editing the pi side, by loosening the canonicalizer, or by
adding a `known-divergences.json` entry (see "Known divergences" above — an
entry records escalated debt, it does not excuse a fresh mismatch).

## Canonicalization: what it normalizes, and what it deliberately does not

Each side writes three files per scenario into `out/<side>/`:

- `<name>.raw.json` — the wire bytes, exactly as serialized for the request.
- `<name>.body.json` — canonical form, **object keys sorted**, used for the content diff.
- `<name>.order.txt` — one line per object, `<path>\t<keys in ORIGINAL order>`.

`pi/canon.mjs` and `port/canon.go` are deliberate twins. Change one, change both.

**Normalized** (differences here are hidden, on purpose):

- **Object key order** in `body.json` — keys are sorted before diffing. A JSON
  object is unordered by specification, and Go marshals `map[string]any` with
  sorted keys unconditionally, so a raw order comparison would be pure noise on
  every scenario. This is why `order.txt` exists: **order is never discarded,
  only moved to a separate assertion** (see below).
- **String escaping** — both sides use one JSON-string writer matched to
  `JSON.stringify` (ES2019 well-formed). Go's `encoding/json` escapes `<`, `>`,
  `&`, U+2028 and U+2029 by default and JS does not; that is a serializer
  artifact, not a request difference, so the Go side disables HTML escaping and
  both sides re-encode through the same routine.
- **Indentation / whitespace.**

**Not normalized** (differences here will fail a scenario, on purpose):

- **Number formatting.** Numbers keep their original literal text on both sides
  (`json.Decoder.UseNumber` in Go, `JSON.parse` source access in JS), so `1024`
  vs `1024.0` vs `1e3` is a visible failure rather than a float round-trip.
- **Array order** — never sorted, ever.
- **`null` versus absent** — a present `null` is a key; an absent key is not.
  This is the distinction most pi porting bugs live in.
- **Key sets** — an extra or missing key always fails.
- **Key order at paths a scenario declares order-sensitive.**

### Order-sensitive paths

A scenario may declare `orderSensitivePaths`. Those paths are compared for key
order against `order.txt`, in addition to the content diff. Use it wherever
pi's insertion order is genuinely observable to the model or the provider —
the repo has an `orderedJSONObject` mechanism for exactly these places.

Currently declared:

- `$.chat_template_args` (Baseten) — fed into a chat template, where order
  changes the rendered prompt.
- `functionCall.args` on the google scenarios — the arguments a model authored.
- `$.messages[i].reasoning_details[j]` on the `reasoning-details-replay-*`
  scenarios — OpenRouter wants the sequence back unmodified, and the order is
  the parsed object's own-property order (array-index keys first, ascending,
  then creation order), not the provider's byte order.

### Known limits (things this harness does NOT prove)

- **`google-generative-ai` is only partially covered.** pi's `onPayload` sees
  the `@google/genai` SDK *call params* (`{model, contents, config}`) and the
  SDK builds the REST body internally; pi explicitly rejects a custom `fetch`,
  so pi's real REST body is not observable at all. The Go port has no such SDK
  and builds the REST body directly. The two hooks therefore sit at different
  layers. Only `$.contents` is the same artifact on both sides, so the google
  scenarios set `comparePaths: ["$.contents"]` and compare nothing else —
  `generationConfig`, `tools` and `systemInstruction` are **out of reach**.
- Request **headers**, URLs and auth are not compared here (the in-repo
  `ai/providers/differential_pi_test.go` covers some header behavior).
- Only the request is compared. Response parsing is not in scope.

## Scenarios

`scenarios/*.json` — one self-contained file each: model, context, options.
Both sides read the same file, so neither can quietly diverge on inputs.

| scenario | backend | what it pins |
| --- | --- | --- |
| `basic-tools-cache` | dist | multi-turn transcript with a tool call + tool result, tool schemas, session prompt caching |
| `reasoning` | dist | unified thinking level → `reasoning_effort`, thinking blocks replayed |
| `long-cache` | dist | `cacheRetention: long` → `prompt_cache_retention: "24h"`, 64-char session-id clamp |
| `together` | dist | `thinkingFormat: "together"`, `supportsReasoningEffort: false` |
| `deepseek` | dist | `thinkingFormat: "deepseek"`, `requiresReasoningContentOnAssistantMessages` |
| `openrouter` | dist | `thinkingFormat: "openrouter"` + `cacheControlFormat: "anthropic"` breakpoints |
| `sampling-params` | dist | model+request `samplingParams` merged per key, applied **last** so they override named fields |
| `sampling-params-stream` | dist | the same inputs through the native `stream()` entry: the adapter merges the model's defaults, so a direct call gets them too (upstream `c01f687e5`; 0.87.1 merges only in `streamSimple`) |
| `sampling-params-stream-responses` | dist | the openai-responses twin, where a sampling key overrides `max_output_tokens` |
| `responses-chatgpt-sign-in` | dist | a credential sent to OpenAI's own endpoint that is not an `sk-` key is a ChatGPT sign-in token: `max_output_tokens`, `temperature` and `prompt_cache_retention` are omitted, `prompt_cache_key` still sent (upstream `02eed88fd`) |
| `responses-chatgpt-sign-in-explicit-cache` | dist | the same for a model with `supportsExplicitPromptCacheMode`: `prompt_cache_options` omitted too |
| `baseten-thinking` | dist | `thinkingFormat: "baseten"` → `chat_template_args` + `reasoning_effort` via `thinkingLevelMap` |
| `baseten-thinking-off` | dist | the same, thinking OFF: `omitWhenOff`, `thinkingLevelMap.off` |
| `gemini3-tool-ids` | dist | gemini major ≥ 3 ⇒ `functionCall`/`functionResponse` carry `id` |
| `gemini25-tool-ids` | dist | control: gemini 2.x stays below the threshold, no `id` |
| `tool-choice-without-tools-present` | dist | `tool_choice` is sent even when the request carries no tools (upstream `6b36eb592` reverted the `params.tools?.length` guard) |
| `tool-choice-with-tools-control` | dist | control for the row above: with tools present the key is sent too, so arm one passes on the guard's absence rather than on the key never being written |
| `tools-only-openai-completions` | dist | tools and no `systemPrompt`: the normalized leading system message has empty text and sends no system message (upstream `9e05370b2`; the published build takes the raw context, which renders the same) |
| `kimi-k3-anchored-tool-additions` | dist | openai-completions Kimi K3 with `supportsMidConvoSystemMessages` + `supportsMidConvoToolAdditions`: `tools` holds the leading message's tools, and a later system message adding a tool is sent in place as a Kimi `{role:"system", tools}` message followed by its rendered update (upstream `9e05370b2`) |
| `anthropic-native-tool-changes` | dist | anthropic-messages with `supportsMidConvoSystemMessages` + `supportsMidConvoToolChanges` (upstream `9e05370b2`; the 1.3.0 shape, `b271b0a52`): request `tools` holds only the leading message's tools (with the cache breakpoint) and a stable `__pi_deferred_placeholder__`; each later system message is held and sent as a `{role:"system"}` block carrying its rendered update, the first with a `tool_addition` holding an inline `tool_definition`, the closing one with a `tool_removal`, under the `inline-tools-2026-09-15` beta. Red against 0.99.2's `mid-conversation-tool-changes-2026-07-01` shape from `bcbea91` until the 1.3.0 re-pin (`82de5d9`) |
| `responses-midconvo-additional-tools` | src | native mid-conversation system messages (upstream `9e05370b2`) with `supportsAdditionalTools`: every tool is declared in `additional_tools` items (upstream `caa6967b6`), so there is no body.tools; the leading message's tools go in one at the head of the input, and `late_tool` in a second one, preceded by the incremental tools notice, before the in-place developer update. `src` until a release ships `caa6967b6` (1.3.0 predates it) |
| `responses-midconvo-tool-search` | dist | additive tool history with `supportsToolSearch`: body.tools keeps the leading message's tools, and `late_tool` loads through a completed client `tool_search_call`/`tool_search_output` pair, `call_id` `pi_tool_load_<shortHash("system:<msgIndex>:<names>")>` with a msgIndex the leading system message does not advance |
| `responses-midconvo-fallback` | src | the same model with a removal: no body.tools and no fallback to the current tool state (upstream `caa6967b6`); the update renders its section changes followed by the removal notice ("The following tools are no longer available. Do not call them:"), and the incremental tools notice before `late_tool` does not list the tool this update removes (upstream `530015d93`). `src` until a release ships both |
| `responses-midconvo-off` | dist | native system messages without additional_tools or tool search: every current tool in body.tools, the update still sent in place |

The `backend` column above is a snapshot. Scenarios flip `src` -> `dist` as releases
ship the surface they cover, so re-read `scenarios/*.json` rather than this table when
the distinction matters. **As of 2026-10-20 the suite is 66 scenarios against
1.3.0 and upstream `fdce48eb4`, all `dist` but two**: `responses-midconvo-additional-tools`
and `responses-midconvo-fallback` went back to `src` when the port took
`caa6967b6`/`530015d93`, which 1.3.0 predates; flip them to `dist` at the first
release that ships both. (The previous two `src` scenarios, `sampling-params-stream`
and `sampling-params-stream-responses` (`c01f687e5`), flipped when 0.99.1 shipped it.)

From `9e05370b2` on, the api adapters take a normalized transcript that only
`normalizeContext` produces. Both arms normalize the scenario's `context` before
calling the adapter — `capture.mjs` whenever the backend has `utils/transcript`
(every build from 0.86.1 on, and `src` at or after that sha), `port/main.go` always.

## Adding a scenario

1. Drop a new `scenarios/<name>.json`. Fields:
   - `name`, `note` — `note` is documentation; say what the scenario pins and
     cite the upstream sha if it is a recent port.
   - `backend` — `"dist"` (shipped) or `"src"` (unreleased; see the table above).
   - `api` — `openai-completions`, `google-generative-ai`, `anthropic-messages`,
     `openai-responses`.
   - `entry` — `"stream"` or `"streamSimple"`. Some behavior only exists on one:
     the `maxTokens` default and its clamp to the context window, for instance,
     happen in pi's `simple-options.ts` and so are `streamSimple`-only.
   - `model`, `context` — pi's own wire shapes. Go decodes them through
     `ai.Model` / `ai.UnmarshalMessage`, so they must be valid pi JSON.
   - `options` — pi-shaped camelCase.
   - optional `orderSensitivePaths`, `comparePaths`.
   - optional `catalogModel` — `"<provider>/<id>"` when `model` is a verbatim
     copy of that catalog entry (`dist` only). `pi/capture.mjs` refuses the
     scenario unless `JSON.stringify(model)` equals the pinned build's entry,
     key order included, and prints the entry to paste. A regen can move
     fields the request never reads, so without the check a stale copy passes.
2. `./run.sh --only <name>`.

If an option key is not yet mapped, the Go side fails loudly (it decodes with
`DisallowUnknownFields`) and tells you to add the field to `scenarioOptions` in
`port/main.go` — it will never silently ignore an input, which would make both
sides agree for the wrong reason. Adding a new `api`/`entry` combination means
adding a case to `capture()` in `port/main.go`.

## Re-pointing at a new npm version / upstream sha

Edit `config.env`:

- `PI_NPM_VERSION` — `run.sh` installs it into `~/.cache/pi-npm/<version>/` on
  the next run and verifies its integrity against the registry.
- `PI_UPSTREAM_SHA` — set to the sha the port is synced to (the "TS source fully
  reviewed/ported" row of the repo's `docs/UPSTREAM.md`). `run.sh` extracts it
  read-only with `git archive`; `~/.cache/pi-upstream` is never modified,
  never checked out.

When a release finally ships a change that a `"src"` scenario covers, flip that
scenario to `"backend": "dist"` — the published build is the better reference.

A `PI_NPM_VERSION` bump re-checks every `catalogModel` scenario against the new
build's catalog; refresh each one the pi side refuses.

## Layout

```
config.env          versions, shas, paths
run.sh              THE runner: rebuild both sides, then classify
classify.py         structural diff + PASS/KNOWN/FAIL/FIXED + exit code
known-divergences.json   accepted-divergence baseline (tracked debt, not policy)
scenarios/*.json    inputs, shared verbatim by both sides
pi/capture.mjs      pi side (dist or src backend)
pi/srclink.mjs      builds pisrc/<sha>/node_modules (workspace packages from the sha)
pi/canon.mjs        canonicalizer
port/main.go          Go side; go.mod has replace -> the pi repo
port/canon.go         canonicalizer, twin of canon.mjs
out/{pi,go}/        generated captures (disposable)
pisrc/<sha>/        upstream TS extracted read-only (disposable)
```
Build note: `go vet ./...` and `go build -o /dev/null ./port` are the checks to
run here. Bare `go build ./...` fails with `build output "port" already exists
and is a directory` — generic Go behaviour for a `main` package in a
subdirectory, since the binary is named after its directory. `run.sh` uses
`go run`, so nothing in the normal path hits it. The repo's own gate never does
either: `difftest/` has its own `go.mod`, so the root module's `go build ./...`
does not descend into it.

