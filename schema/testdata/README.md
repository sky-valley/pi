# schema test data

- `json-schema-test-suite/` and `meta-schemas/`: upstream pi's
  `packages/schema/test/fixtures/` at `6536f8012`, copied unmodified (the
  suite subset is JSON-Schema-Test-Suite `3f05891cac5d51ffa068d7076d1372130e00c829`
  under its MIT `LICENSE`; the meta-schemas are the JSON Schema organization's,
  AFL-3.0 or BSD-3-Clause — see each directory's README). `conformance_test.go`
  runs them as upstream's `test/conformance.test.ts` does.
- `golden/`: the differential golden. `capture.mjs` (usage in its header)
  runs upstream's `packages/schema/src` at `6536f8012` over a corpus and
  writes `capture-6536f8012.json`; `golden_test.go` replays it.
