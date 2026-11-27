package delta

import (
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// delta-diff.test.ts's ERNDL-1176 step 4 cases (upstream 8bbc119b9): a window
// moved by k items is found by identity. The golden pins every case's ops
// byte for byte against pi's ("shift-" in testdata/upstream_delta.json); these
// replay them, as upstream's expectDiff does.

// shiftRows is upstream's Array.from({ length }, (_, id) => ({ id })).
func shiftRows(length int) []any {
	rows := make([]any, length)
	for id := range rows {
		rows[id] = obj("id", float64(id))
	}
	return rows
}

// shiftRandomRound is one round of the random shift test: before and after.
type shiftRandomRound struct{ before, after []any }

// shiftRandomRounds is upstream's random test, round by round, with its
// linear congruential generator in float64 arithmetic as JavaScript runs it
// (the product passes 2^53, so it rounds as V8 rounds it; the conversion
// keeps Go from fusing the multiply and the add).
var shiftRandomRounds = sync.OnceValue(func() []shiftRandomRound {
	seed := 7.0
	random := func(below int) int {
		seed = math.Mod(float64(seed*1_103_515_245)+12_345, 2_147_483_648)
		return int(seed) % below
	}
	rounds := make([]shiftRandomRound, 500)
	for round := range rounds {
		length := 2 + random(30)
		rows := shiftRows(length)
		k := 1 + random(length-1)
		fresh := make([]any, k)
		for id := range fresh {
			fresh[id] = obj("id", float64(1_000+id))
		}
		// New rows, rows that left coming back, or one of them a copy (equal,
		// not the same object).
		came := [][]any{
			fresh,
			rows[length-k:],
			append(slices.Clone(fresh[1:]), maps.Clone(rows[0].(map[string]any))),
		}[random(3)]
		var after []any
		if random(2) == 0 {
			after = slices.Concat(came, rows[:length-k])
		} else {
			after = slices.Concat(rows[k:], came)
		}
		if random(4) == 0 {
			after[random(length)] = obj("id", -1.0)
		}
		rounds[round] = shiftRandomRound{before: rows, after: after}
	}
	return rounds
})

// shiftCase builds a "shift-" generated case exactly as capture.mts does.
func shiftCase(name string) (before, after any, ok bool) {
	rows := shiftRows(50)
	came := []any{obj("id", 100.0), obj("id", 101.0)}
	switch {
	case name == "shift-down-50":
		return obj("rows", rows), obj("rows", slices.Concat(came, rows[:48])), true
	case name == "shift-up-50":
		return obj("rows", rows), obj("rows", slices.Concat(rows[2:], came)), true
	case name == "shift-rotation-50":
		return obj("rows", rows), obj("rows", slices.Concat(rows[48:], rows[:48])), true
	case strings.HasPrefix(name, "shift-window-"):
		length, err := strconv.Atoi(strings.TrimPrefix(name, "shift-window-"))
		if err != nil {
			return nil, nil, false
		}
		window := shiftRows(length)
		return obj("rows", window), obj("rows", slices.Concat([]any{obj("id", -1.0)}, window[:length-1])), true
	case strings.HasPrefix(name, "shift-random-"):
		round, err := strconv.Atoi(strings.TrimPrefix(name, "shift-random-"))
		rounds := shiftRandomRounds()
		if err != nil || round < 0 || round >= len(rounds) {
			return nil, nil, false
		}
		return obj("rows", rounds[round].before), obj("rows", rounds[round].after), true
	}
	return nil, nil, false
}

// expectDiff is upstream's: the batch, and that it replays to after.
func expectDiff(t *testing.T, before, after any, literal string) {
	t.Helper()
	ops := DiffRevisions(before, after)
	wantOps(t, ops, literal)
	replica, err := ApplyImmutable(before, ops)
	if err != nil {
		t.Fatalf("replaying %v: %v", ops, err)
	}
	wantJSON(t, replica, after)
}

func TestDiffRevisionsFindsAShiftedWindowByIdentity(t *testing.T) {
	rows := shiftRows(50)
	came := []any{obj("id", 100.0), obj("id", 101.0)}
	// Newest first: two rows on top, the last two gone.
	expectDiff(t, obj("rows", rows), obj("rows", slices.Concat(came, rows[:48])),
		`[["p",["rows"],0,0,[{"id":100},{"id":101}]],["p",["rows"],50,2,[]]]`)
	// Oldest first: the first two gone, two rows behind.
	expectDiff(t, obj("rows", rows), obj("rows", slices.Concat(rows[2:], came)),
		`[["p",["rows"],0,2,[]],["p",["rows"],48,0,[{"id":100},{"id":101}]]]`)
	// The rows that left come back (a rotation): a permutation, as before.
	ops := DiffRevisions(obj("rows", rows), obj("rows", slices.Concat(rows[48:], rows[:48])))
	if _, ok := ops[0].(Permute); !ok {
		t.Errorf("a rotation diffed to %v, want a permutation", ops)
	}
}

// Upstream counts the items a shifted window's diff reads through a Proxy
// (at most 2n + 8 of a window of n); a Go slice has no read hook, so the port
// keeps its shape: one splice in front and one behind.
func TestDiffRevisionsShiftedWindowIsTwoSplices(t *testing.T) {
	for _, length := range []int{50, 500} {
		rows := shiftRows(length)
		ops := DiffRevisions(obj("rows", rows), obj("rows", slices.Concat([]any{obj("id", -1.0)}, rows[:length-1])))
		if len(ops) != 2 {
			t.Errorf("window of %d: %d ops %v, want 2", length, len(ops), ops)
		}
	}
}

func TestDiffRevisionsEveryShiftReplays(t *testing.T) {
	for round, r := range shiftRandomRounds() {
		before, after := obj("rows", r.before), obj("rows", r.after)
		ops := DiffRevisions(before, after)
		replica, err := ApplyImmutable(before, ops)
		if err != nil {
			t.Fatalf("round %d: replaying %v: %v", round, ops, err)
		}
		if g, w := jsonText(t, replica), jsonText(t, after); g != w {
			t.Fatalf("round %d: ops %v give\n %s\nwant %s", round, jsonText(t, ops), g, w)
		}
	}
}
