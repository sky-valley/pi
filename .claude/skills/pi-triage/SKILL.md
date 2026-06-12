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
  `git fetch origin main` first.
- The authoritative non-port list and current pin live in `docs/UPSTREAM.md`
  ("Current pin" section). Read it before judging anything.

## Per change, produce:
1. **WHY** — intent, from the commit/PR message (`git log -1 --format=%B`) and
   any linked issue number in it.
2. **WHAT** — read the actual diff (`git diff <sha>^1..<sha> --stat` then the
   hunks for non-trivial files). Note behavior, constants, model-visible
   strings, new/changed tests.
3. **SCOPE** — verdict, one of:
   - `port` — touches behavior we ported: `packages/ai/src` (except oauth,
     images, cli, bedrock/vertex/mistral/azure/codex providers),
     `packages/agent/src`, `packages/coding-agent/src/core|main|sdk` (except
     extensions, trust-manager, bun, modes/tui).
   - `n/a` — only touches non-ported surface (TUI, extensions runtime, OAuth,
     project trust, unported providers, docs, CI, examples, packaging) — give
     the specific reason.
   - `decide` — changes the *boundary* itself (e.g. a feature that makes a
     non-ported area load-bearing for the SDK, a new provider, a new tool).
     Escalate to the user instead of deciding silently.
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
- **Release commits are `port`**: they regenerate `packages/ai/src/models.generated.ts`,
  which IS ported surface (→ `ai/models_catalog.json`, regenerated from the
  matching npm build). The version bump/changelog parts are noise; ALSO note
  the release tag so /pi-sync refreshes the npm reference build.
  `image-models.generated.ts` is excluded (images unported).
- `packages/coding-agent/src/utils/` is in scope only if a ported core file
  consumes it — judge by the consumer, not the path.
- `.pi/` files (upstream repo's own agent config/extensions) are always `n/a`.
- Generated/data files count as ported surface when we embed their derivative
  (currently: models.generated.ts → ai/models_catalog.json — the only one).
- A follow-on commit to a pending `decide` inherits that escalation: mark it
  `decide (rider on <sha>)` and batch into ONE question to the user.
- Once the user rules on a `decide`, record the ruling in docs/UPSTREAM.md
  ("Current pin" section's non-port list or a Rulings note) so future triage
  doesn't re-ask.
- Cost control: changes whose `--stat` touches only docs/CHANGELOG/.github/
  scripts/.pi/packages/tui may be dispatched from the diffstat alone; read
  hunks only when any path is in or near ported surface.
