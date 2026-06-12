---
name: pi-parity-review
description: Adversarially verify that a ported change is faithful to the original pi implementation (TS source + published npm build). Use after porting upstream pi changes, or standalone on any area of this repo ("is X faithful to pi?").
---

# pi-parity-review — is the port true to pi?

Input: the upstream change (sha in the upstream clone) and our ported diff.
You are independent of the porter: assume the port is wrong until the diff
proves otherwise. Self-authored tests are circular — they pin what the porter
believed, not what pi does. Every sweep of this project found real bugs ONLY
via comparison against real pi.

## References
- Upstream TS: `$PI_UPSTREAM_DIR` (default `~/.cache/pi-upstream`), checked out
  at the relevant sha (`git -C dir show <sha>:<path>` reads without checkout).
- Published npm build: install/refresh `@earendil-works/pi-ai` +
  `pi-coding-agent` at the matching release into a scratch dir. **When the TS
  source and the shipped build disagree, the BUILD wins** — it's what real pi
  runs and what all goldens come from.
- Goldens in-repo: `coding/testdata/sessparity/`, `coding/testdata/imgparity/`,
  the default-system-prompt golden, and the tool output-string tests.

## Method
1. Read pi's change (the full first-parent diff) until you can state its exact
   observable behavior: strings, constants, ordering, error surfaces, request
   fields, edge cases.
2. Read our ported diff and try to BREAK the correspondence: byte-compare
   every model-visible string (prompts, tool outputs, wrapper text, request
   bodies); check JS semantics ported correctly (UTF-16 `.length`, `??` vs
   `||`, `Math.round`, Number() coercion, insertion order, truthiness of `{}`);
   check the change's edge cases (empty, zero, absent-vs-null, astral chars).
3. **If the change touches request building** (`ai/providers/openai*.go`
   especially): re-run the differential request diff — scenarios + pi goldens
   + canon scripts live in `/tmp/pi-diff/`, Go harness pattern in
   `/tmp/diffreq2/` (go.mod `replace` → this repo; captures via OnPayload
   returning an error to halt pre-network). If those scratch dirs are gone,
   regenerate the pi side from the npm build first.
4. If the change touches session format / image decisions / system prompt:
   regenerate or extend the corresponding golden FROM THE NPM BUILD (node
   against the installed package), never by hand.
5. Check whether our existing tests pinned the OLD behavior — a faithful port
   often must update tests; flag any test that now asserts non-pi behavior.

## Generated data / catalog & release changes
- npm reference builds live at `~/.cache/pi-npm/<version>/` (npm i the exact
  version there). Before trusting one, verify authenticity: package-lock
  integrity == `npm view <pkg>@<ver> dist.integrity`.
- **Never verify against the porter's intermediates** — regenerate the
  comparison artifact YOURSELF from the build (the circularity rule applies to
  scratch files, not just tests).
- Canonical catalog: `ai/models_catalog.json` = `JSON.stringify(MODELS)` from
  the matching build's `dist/models.generated.js` (single line, insertion
  order). Review = re-derive and `cmp`.
- **Endpoint pinning** (strongest technique for generated files): show old
  file ≡ upstream data at `<sha>^` AND new file ≡ data at `<sha>` ⇒ the ported
  diff equals the upstream diff exactly.
- Schema drift: enumerate the new artifact's JSON keys/value types against the
  Go struct tags (ai/types.go Model) — unknown keys are silently dropped and a
  type mismatch silently aborts the whole load.
- Release commits carry no portable behavior beyond regenerated artifacts;
  the real changes live in the commits between releases.
- Post-check: `go test ./ai/...` + grep code/tests for removed model ids
  (orphaned defaults, e.g. coding/resolve.go).
- Tip: `node --experimental-strip-types` can execute upstream generated `.ts`
  directly when you need data at a specific source sha.

## Output
Verdict per change: `faithful` / `diverges` (with file:line both sides, the
exact byte/behavior difference, and the failing scenario) / `unverifiable`
(say what's missing — e.g. needs a live key). List any new golden added.
Review-only unless asked to fix.
