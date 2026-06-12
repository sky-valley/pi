---
name: pi-go-review
description: Review ported Go code for idiomatic quality — that the port maximizes Go rather than transliterating TypeScript. Use after porting upstream pi changes, or standalone on any diff in this repo.
---

# pi-go-review — is the port real Go?

Input: a diff (commit range, or "the uncommitted changes"). Review ONLY the
changed code, in context. You are independent of whoever wrote it; do not
trust its comments or its tests' assertions.

## Charter
The port's promise is "faithful, idiomatic Go that leans into Go's strengths".
Faithfulness is the other reviewer's job (pi-parity-review); yours is the Go.
A finding must cite file:line and say what idiomatic form replaces it.

## Hunt for
- **TS transliteration smells**: `map[string]any` where a struct fits; `any`
  plumbing; closures captured where a method belongs; promise-style callback
  shapes where a channel/context fits; manual index loops over runes/UTF-16
  where the stdlib has it (careful: UTF-16 code-unit math is often DELIBERATE
  parity — check the comment before flagging).
- **Error discipline**: errors returned not panicked (panic only for the
  loop's emitPanic protocol); `%w` wrapping where callers may unwrap; no
  swallowed errors (`_ =` needs a justifying comment).
- **Concurrency**: every goroutine has an exit path; locks released on all
  paths (defer or tight func scope); no shared-map mutation without the
  documented serialization discipline (see agent/loop.go serialMu pattern);
  anything new must pass `-race`.
- **API shape**: zero-values useful; options structs over long params;
  pointers only for optional/ownership (`*int` = "absent" is a deliberate
  pi-nullish pattern — keep); exported things documented; no stutter
  (`coding.CodingTool`).
- **Resource hygiene**: Close/cleanup on error paths, contexts respected,
  bounded buffers for unbounded input (see the OutputAccumulator port).
- **Tests**: table-driven where repetitive; `t.TempDir`/`t.Context`; no
  sleeps-as-sync; race-clean.

## Regenerated embedded artifacts (e.g. ai/models_catalog.json)
The unit of review is the STRUCTURAL diff vs the previous embed, not the
textual diff (these files are minified single lines). Reading the consuming
Go code (e.g. ai/catalog.go, ai/types.go) is required, not out of scope.
Validate the artifact against its consuming types: field/enum unions,
duplicate JSON keys (encoding/json silently last-wins), null-vs-absent
semantics; grep the repo for IDs the regen removed (orphaned defaults/tests);
run the consuming package's tests under -race; confirm which upstream version
generated it. Cite findings by JSON path, not file:line.

## Mandatory checks
`gofmt -l`, `go vet ./...`, and the changed packages' tests under `-race`.

## Output
Findings list `[HIGH/MED/LOW] file:line — issue — idiomatic fix`, then a
verdict: ship / fix-first (with the fix list). Do not edit code unless the
caller asked you to fix; default is review-only.
