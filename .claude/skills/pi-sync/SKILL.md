---
name: pi-sync
description: Daily upstream-sync job for the pi Go port — fetch upstream pi, triage every change since the recorded pin, port what's in scope, verify idiomatic + parity via independent reviews, update the ledger, and push. Use for "sync with upstream", "porting job", or as the scheduled daily run.
---

# pi-sync — the daily porting job

Orchestrates one sync cycle. State lives in `docs/UPSTREAM.md` (the pin + the
ledger); this skill is restartable — a half-finished ledger resumes where it
stopped.

## 0. Preflight
- Repo: clean working tree on `main`, `git pull` first. Full gate must be
  green BEFORE starting (`go build ./... && go vet ./... && go test ./...`);
  never start a sync on a broken base.
- Upstream clone at `$PI_UPSTREAM_DIR` (default `~/.cache/pi-upstream`);
  clone if missing, `git fetch origin main`.
- Read the pin from `docs/UPSTREAM.md`. Delta = first-parent main-line
  changes `pin..origin/main` (a merged PR = one unit). If empty: record the
  check date in UPSTREAM.md and stop.
- If the delta contains a release tag: refresh the npm reference build
  (`npm i @earendil-works/pi-ai@<ver> @earendil-works/pi-coding-agent@<ver>`
  in the scratch dir) so parity review compares against what now ships.

## 1. Triage (skill: pi-triage)
Run pi-triage over the whole delta (subagent). Append all rows to the ledger
in `docs/UPSTREAM.md` with their verdicts. `n/a` rows are done. `decide` rows:
STOP and surface to the user — never silently expand or shrink the port's
scope.

## 2. Port (per `port`-verdict change, chronological)
- One subagent per change (or small coherent batch touching the same files).
  Input: the triage row (WHY/WHAT/file mapping), the upstream diff, and the
  standing rules: faithful to pi, npm build wins on drift, byte-exact
  model-visible strings, every behavior change test-locked, JS semantics
  ported deliberately (UTF-16 lengths, ??-semantics via pointers, Math.round).
- One Go commit per upstream change; message references the upstream sha:
  `port(<area>): <subject> (upstream <sha>)`.

## 3. Review gates (independent subagents — never the porter)
- **pi-go-review** on the ported diff → fix findings before proceeding.
- **pi-parity-review** on the ported diff vs the upstream change → fix
  divergences. If it says a golden must change, regenerate it from the npm
  build, never by hand.

## 4. Final gate
- `gofmt -l` clean, `go build ./... && go vet ./...`,
  `go test -race ./... -count=1` green.
- If anything under `ai/providers/openai*.go` changed: re-run the 6-scenario
  request diff (see pi-parity-review §3) — must be 6/6.

## 5. Record + ship
- Fill ledger rows (status, Go commit, notes). Move the **Current pin** to the
  new upstream sha; note the date and the new npm version if it changed.
- Commit the ledger update; push everything to
  `https://github.com/sky-valley/pi.git main` (HTTPS — SSH signing is not
  available to automation on this machine).
- Report: N changes — X ported (with commits), Y n/a, Z escalated; any test
  count change; any new deliberate divergence added to UPSTREAM.md.

## Hard rules
- Anything that would change the **public Go API** or the deliberate non-port
  boundary → escalate, don't ship.
- A port without a test does not ship. A parity divergence "fixed" by editing
  the assertion to match our output does not ship — goldens come from pi.
- If the cycle can't finish (e.g. blocked on a decision), ship the completed
  prefix of the ledger; never leave the repo red.
