package delta

import (
	"errors"
	"math/rand"
	"reflect"
	"strings"
	"testing"
)

// Goldens marked "pi:" are the bytes pi's encoder/decoder produced for the
// same input under node (packages/chord/src/delta/index.ts at 64eeb82a4).
// Each was re-checked at 49681e1b7 by replaying pi's encoder() and decoder()
// over these inputs and 400 random streams against Encode and Decode.

// wireOps parses a JSON array of wire tuples through ParseWireOp — the batch a
// decoder is handed once each tuple's shape has been checked.
func wireOps(t *testing.T, literal string) []WireOp {
	t.Helper()
	raw, ok := tree(t, literal).([]any)
	if !ok {
		t.Fatalf("bad wire literal %s: not an array", literal)
	}
	out := make([]WireOp, 0, len(raw))
	for _, v := range raw {
		op, err := ParseWireOp(v)
		if err != nil {
			t.Fatalf("bad wire literal %s: %v", literal, err)
		}
		out = append(out, op)
	}
	return out
}

// wantWire compares an encoded batch with the tuples pi emits, given as a
// compact JSON literal — the bytes are the golden, so this is a byte compare.
func wantWire(t *testing.T, got []WireOp, literal string) {
	t.Helper()
	if got == nil {
		t.Fatalf("encode returned a nil batch; an empty batch is [] on the wire")
	}
	if g := jsonText(t, got); g != literal {
		t.Errorf("wire\n got %s\nwant %s", g, literal)
	}
}

// decode runs one decoder over a batch and fails on error.
func decode(t *testing.T, dec *Decoder, wire []WireOp) []Op {
	t.Helper()
	ops, err := dec.Decode(wire)
	if err != nil {
		t.Fatalf("Decode(%s): %v", jsonText(t, wire), err)
	}
	return ops
}

// roundTrip is upstream's roundTrip: one encoder/decoder pair over a stream
// of batches. It asserts the property every codec case shares — decode of
// encode is the identity, in Go value and in JSON — and returns the decoded
// batches for the caller's own checks.
func roundTrip(t *testing.T, batches [][]Op) [][]Op {
	t.Helper()
	var enc Encoder
	var dec Decoder
	out := make([][]Op, 0, len(batches))
	for i, ops := range batches {
		decoded := decode(t, &dec, enc.Encode(ops))
		if !reflect.DeepEqual(decoded, ops) || jsonText(t, decoded) != jsonText(t, ops) {
			t.Errorf("batch %d did not round-trip\n got %s\nwant %s", i, jsonText(t, decoded), jsonText(t, ops))
		}
		out = append(out, decoded)
	}
	return out
}

// cloneOps copies the payloads of a batch about to be applied, so two
// replicas fed the same in-memory stream never share a container. A real
// consumer gets its batch off the wire, already detached.
func cloneOps(ops []Op) []Op {
	out := make([]Op, len(ops))
	for i, op := range ops {
		switch op := op.(type) {
		case Replace:
			out[i] = Replace{Value: cloneJSON(op.Value)}
		case Set:
			out[i] = Set{Path: op.Path, Value: cloneJSON(op.Value)}
		case Splice:
			out[i] = Splice{Path: op.Path, Index: op.Index, Remove: op.Remove, Items: cloneItems(op.Items)}
		default:
			out[i] = op
		}
	}
	return out
}

// replayBoth folds a stream onto a replica twice — once from the original
// batches, once from the batches that came back through the codec — and
// checks both against want. The apply half of the round-trip property.
func replayBoth(t *testing.T, initial string, batches, decoded [][]Op, want any) {
	t.Helper()
	direct, viaCodec := tree(t, initial), tree(t, initial)
	var err error
	for i := range batches {
		if direct, err = Apply(direct, cloneOps(batches[i])); err != nil {
			t.Fatalf("apply batch %d: %v", i, err)
		}
		if viaCodec, err = Apply(viaCodec, cloneOps(decoded[i])); err != nil {
			t.Fatalf("apply decoded batch %d: %v", i, err)
		}
	}
	wantJSON(t, direct, want)
	wantJSON(t, viaCodec, want)
}

// ─── codec: path interning and arity omission ────────────────────────────────
//
// One pair per stream. The table spans a whole subscription or file, so a
// second consumer joining later needs its own encoder.

// delta.test.ts (upstream 64eeb82a4) "codec" → "round-trips a stream
// exactly", plus the wire pi emits for that stream: inline on first use,
// define-then-reference on the second, bare ids from then on. The batches are
// the tracker's; testdata/upstream_delta.json's "codec-stream" scenario pins
// them against pi at 9a139c62b.
func TestCodecRoundTripsAStreamExactly(t *testing.T) {
	initial := `{"a": {"deep": ""}, "b": {"deep": ""}}`
	tr := mustTrack(t, initial)
	var batches [][]Op
	for i := range 6 {
		batches = append(batches, commit(t, tr, func(d *Draft) {
			concat(t, d.At("a"), "deep", "x"+string(rune('0'+i)))
			concat(t, d.At("b"), "deep", "y"+string(rune('0'+i)))
		}))
	}
	decoded := roundTrip(t, batches)
	replayBoth(t, initial, batches, decoded, tr.Value())

	// pi: the same six batches through one encoder.
	var enc Encoder
	for i, want := range []string{
		`[["a",["a","deep"],"x0"],["a",["b","deep"],"y0"]]`,
		`[["#",0,["a","deep"]],["a",0,"x1"],["#",1,["b","deep"]],["a",1,"y1"]]`,
		`[["a",0,"x2"],["a",1,"y2"]]`,
		`[["a",0,"x3"],["a",1,"y3"]]`,
		`[["a",0,"x4"],["a",1,"y4"]]`,
		`[["a",0,"x5"],["a",1,"y5"]]`,
	} {
		wantWire(t, enc.Encode(batches[i]), want)
	}
}

// delta.test.ts "codec" → "interns on second use, not first".
func TestCodecInternsOnSecondUseNotFirst(t *testing.T) {
	var enc Encoder
	first := enc.Encode(ops(t, `[["a", ["a", "deep"], "1"]]`))
	second := enc.Encode(ops(t, `[["a", ["a", "deep"], "2"]]`))
	wantWire(t, first, `[["a",["a","deep"],"1"]]`)            // inline
	wantWire(t, second, `[["#",0,["a","deep"]],["a",0,"2"]]`) // define, then use
	if len(second) != 2 {
		t.FailNow()
	}
	if !reflect.DeepEqual(second[0], Define{ID: 0, Path: Path{Key("a"), Key("deep")}}) {
		t.Errorf("definition = %#v", second[0])
	}
	if !reflect.DeepEqual(second[1], WireAppend{Ref: PathID(0), Text: "2"}) {
		t.Errorf("reference = %#v", second[1])
	}
}

// delta.test.ts "codec" → "omits the path when it repeats".
func TestCodecOmitsThePathWhenItRepeats(t *testing.T) {
	var enc Encoder
	got := enc.Encode(ops(t, `[["s", ["a"], 1], ["s", ["a"], 2]]`))
	wantWire(t, got, `[["s",["a"],1],["s",2]]`)
	if len(got) != 2 {
		t.FailNow()
	}
	if !reflect.DeepEqual(got[1], WireSet{Ref: nil, Value: float64(2)}) {
		t.Errorf("short form = %#v, want a nil Ref", got[1])
	}
}

// delta.test.ts "codec" → "does not collide paths containing null
// characters". The dictionary is keyed on the JSON form, where "a\u0000b" and
// ["a","b"] are distinct; a separator-joined key would make them one path.
func TestCodecDoesNotCollidePathsContainingNulCharacters(t *testing.T) {
	batches := [][]Op{
		ops(t, `[["s", ["a\u0000b"], 1], ["s", ["a", "b"], 2]]`),
		ops(t, `[["s", ["a\u0000b"], 3], ["s", ["a", "b"], 4]]`),
	}
	if got := batches[0][0].(Set).Path[0]; got != Key("a\x00b") {
		t.Fatalf("literal did not carry the NUL: %q", got)
	}
	roundTrip(t, batches)

	// pi: both paths intern separately, in first-use order.
	var enc Encoder
	wantWire(t, enc.Encode(batches[0]), `[["s",["a\u0000b"],1],["s",["a","b"],2]]`)
	wantWire(t, enc.Encode(batches[1]), `[["#",0,["a\u0000b"]],["s",0,3],["#",1,["a","b"]],["s",1,4]]`)
}

// A string-spelled index and an index are different paths — ["1"] addresses
// an object property, [1] an array element — and the codec keeps them apart
// the same way. pi: two ids.
func TestCodecKeepsSpelledAndNumericSegmentsApart(t *testing.T) {
	batches := [][]Op{
		ops(t, `[["s", ["1"], 1], ["s", [1], 2]]`),
		ops(t, `[["s", ["1"], 3], ["s", [1], 4]]`),
	}
	decoded := roundTrip(t, batches)
	if t.Failed() {
		t.FailNow()
	}
	if _, ok := decoded[1][0].(Set).Path[0].(Key); !ok {
		t.Errorf("decoded [\"1\"] as %T", decoded[1][0].(Set).Path[0])
	}
	if _, ok := decoded[1][1].(Set).Path[0].(Index); !ok {
		t.Errorf("decoded [1] as %T", decoded[1][1].(Set).Path[0])
	}
	var enc Encoder
	wantWire(t, enc.Encode(batches[0]), `[["s",["1"],1],["s",[1],2]]`)
	wantWire(t, enc.Encode(batches[1]), `[["#",0,["1"]],["s",0,3],["#",1,[1]],["s",1,4]]`)
}

// delta.test.ts "codec" → "rejects a short form without a previous path".
// pi: PathError "unresolvable path: []".
func TestCodecRejectsShortFormWithoutPreviousPath(t *testing.T) {
	for _, literal := range []string{
		`[["a", "x"]]`,
		`[["d"]]`,
		`[["s", 1]]`,
		`[["t", 1]]`,
		`[["p", 0, 0, []]]`,
		// A definition is not an op and sets no previous path.
		`[["#", 0, ["a"]], ["a", "x"]]`,
		// A replacement clears the previous path, as it clears the ids.
		`[["s", ["a"], 1], ["r", {}], ["s", 2]]`,
	} {
		var dec Decoder
		_, err := dec.Decode(wireOps(t, literal))
		var pe *PathError
		if !errors.As(err, &pe) {
			t.Fatalf("Decode(%s): got %v, want *PathError", literal, err)
		}
		if got := refString(pe.Ref); got != "[]" {
			t.Errorf("Decode(%s): PathError ref = %s, want []", literal, got)
		}
		if !strings.Contains(err.Error(), "unresolvable path: []") {
			t.Errorf("Decode(%s): error %q does not carry pi's text", literal, err)
		}
	}
}

// Path omission is scoped to a batch: the previous path does not carry into
// the next Decode, exactly as the encoder never emits a short form first.
// pi: PathError "unresolvable path: []".
func TestCodecShortFormDoesNotSpanBatches(t *testing.T) {
	var dec Decoder
	decode(t, &dec, wireOps(t, `[["s", ["a"], 1]]`))
	_, err := dec.Decode(wireOps(t, `[["s", 2]]`))
	var pe *PathError
	if !errors.As(err, &pe) {
		t.Fatalf("got %v, want *PathError", err)
	}
	var enc Encoder
	enc.Encode(ops(t, `[["s", ["a"], 1]]`))
	wantWire(t, enc.Encode(ops(t, `[["s", ["a"], 2]]`)), `[["#",0,["a"]],["s",0,2]]`)
}

// delta.test.ts "codec" → "clears decoder ids on a base batch".
// pi: PathError "unresolvable path: 0".
func TestCodecClearsDecoderIdsOnBaseBatch(t *testing.T) {
	var dec Decoder
	decode(t, &dec, wireOps(t, `[["#", 0, ["a"]], ["a", 0, "1"]]`))
	decode(t, &dec, wireOps(t, `[["r", {"a": ""}]]`))
	_, err := dec.Decode(wireOps(t, `[["a", 0, "2"]]`))
	var pe *PathError
	if !errors.As(err, &pe) {
		t.Fatalf("got %v, want *PathError", err)
	}
	if pe.Ref != PathID(0) {
		t.Errorf("PathError ref = %#v, want PathID(0)", pe.Ref)
	}
	if !strings.Contains(err.Error(), "unresolvable path: 0") {
		t.Errorf("error %q does not carry pi's text", err)
	}
}

// delta.test.ts "codec" → "resets the table on a base batch, so recovery is
// self-contained". A reader replays from the LAST base batch with a fresh
// decoder. Ids defined before it were never seen, so keeping them breaks
// recovery with an unresolvable path id.
func TestCodecResetsTheTableOnABaseBatch(t *testing.T) {
	var enc Encoder
	enc.Encode(ops(t, `[["a", ["a", "deep"], "1"]]`))
	enc.Encode(ops(t, `[["a", ["a", "deep"], "2"]]`)) // id 0 assigned
	base := enc.Encode(ops(t, `[["r", {"a": {"deep": "x"}}]]`))
	after := enc.Encode(ops(t, `[["a", ["a", "deep"], "3"]]`))
	wantWire(t, base, `[["r",{"a":{"deep":"x"}}]]`)
	wantWire(t, after, `[["a",["a","deep"],"3"]]`) // inline again

	var dec Decoder
	decode(t, &dec, base)
	wantOps(t, decode(t, &dec, after), `[["a", ["a", "deep"], "3"]]`)
}

// The reset happens AT the replacement, mid-batch: ids assigned before it in
// the same batch are gone, paths after it are first uses again, and the
// short form starts over from the replacement. pi: the third batch below.
func TestCodecResetsMidBatch(t *testing.T) {
	batches := [][]Op{
		ops(t, `[["s", ["p"], 1], ["s", ["q"], 2], ["s", ["p"], 3], ["s", ["p"], 4], ["s", ["q"], 5]]`),
		ops(t, `[["s", ["p"], 6], ["s", ["q"], 7], ["r", 1], ["s", ["p"], 8], ["s", ["p"], 9], ["s", ["q"], 10], ["s", ["q"], 11]]`),
		ops(t, `[["a", ["p"], "!"], ["d", ["q"]]]`),
	}
	roundTrip(t, batches)
	var enc Encoder
	wantWire(t, enc.Encode(batches[0]), `[["s",["p"],1],["s",["q"],2],["#",0,["p"]],["s",0,3],["s",4],["#",1,["q"]],["s",1,5]]`)
	wantWire(t, enc.Encode(batches[1]), `[["s",0,6],["s",1,7],["r",1],["s",["p"],8],["s",9],["s",["q"],10],["s",11]]`)
	wantWire(t, enc.Encode(batches[2]), `[["#",0,["p"]],["a",0,"!"],["#",1,["q"]],["d",1]]`)

	// The same path on both sides of a replacement goes inline after it: a
	// short form would reach back across the replacement, to a path pi's
	// decoder has forgotten ("unresolvable path: []"). pi at 49681e1b7.
	straddle := ops(t, `[["s", ["a"], 1], ["r", 0], ["s", ["a"], 2]]`)
	enc = Encoder{}
	wantWire(t, enc.Encode(straddle), `[["s",["a"],1],["r",0],["s",["a"],2]]`)
	roundTrip(t, [][]Op{straddle})
}

// Every verb has a short form, and the root path takes one too: a root "p"
// followed by another is ["p", index, remove, items]. pi.
func TestCodecShortFormOfEveryVerb(t *testing.T) {
	batch := ops(t, `[["p", [], 0, 0, [1]], ["p", [], 1, 0, [2]], ["d", ["x"]], ["t", ["x"], 1], ["a", ["x"], "z"], ["p", ["x"], 0, 0, []], ["p", ["x"], 1, 1, [null]]]`)
	roundTrip(t, [][]Op{batch})
	var enc Encoder
	wantWire(t, enc.Encode(batch), `[["p",[],0,0,[1]],["p",1,0,[2]],["d",["x"]],["t",1],["a","z"],["p",0,0,[]],["p",1,1,[null]]]`)

	// README "Sending or storing changes": the t/a pair of a rolling window,
	// then the id its path earns on the next batch.
	enc = Encoder{}
	wantWire(t, enc.Encode(ops(t, `[["t", ["output"], 200], ["a", ["output"], "next chunk"]]`)), `[["t",["output"],200],["a","next chunk"]]`)
	wantWire(t, enc.Encode(ops(t, `[["a", ["output"], "more"]]`)), `[["#",0,["output"]],["a",0,"more"]]`)
	wantWire(t, enc.Encode(ops(t, `[["a", ["output"], "again"]]`)), `[["a",0,"again"]]`)
}

// The root path interns like any other once a second explicit use arrives.
// pi: ["#", id, []] then ["p", id, ...].
func TestCodecInternsTheRootPath(t *testing.T) {
	batches := [][]Op{
		ops(t, `[["p", [], 0, 0, [1]], ["p", [], 1, 0, [2]]]`),
		ops(t, `[["p", [], 2, 0, [3]]]`),
	}
	decoded := roundTrip(t, batches)
	replayBoth(t, `[]`, batches, decoded, tree(t, `[1, 2, 3]`))
	var enc Encoder
	wantWire(t, enc.Encode(batches[0]), `[["p",[],0,0,[1]],["p",1,0,[2]]]`)
	wantWire(t, enc.Encode(batches[1]), `[["#",0,[]],["p",0,2,0,[3]]]`)
}

// The same-path short form is decided before the dictionary is touched: a
// path repeated within one batch is used once as far as interning goes, so
// the definition waits for the next explicit use. pi.
func TestCodecShortFormDoesNotCountAsAUse(t *testing.T) {
	var enc Encoder
	wantWire(t, enc.Encode(ops(t, `[["s", ["p"], 1], ["s", ["p"], 2]]`)), `[["s",["p"],1],["s",2]]`)
	wantWire(t, enc.Encode(ops(t, `[["s", ["p"], 3]]`)), `[["#",0,["p"]],["s",0,3]]`)
	wantWire(t, enc.Encode(ops(t, `[["s", ["p"], 4]]`)), `[["s",0,4]]`)
	wantWire(t, enc.Encode(ops(t, `[["d", ["k", 0]], ["d", ["k", 0]], ["d", ["k", 0]]]`)), `[["d",["k",0]],["d"],["d"]]`)
}

// An empty batch encodes and decodes to an empty batch, never nil, and a
// definition alone decodes to nothing. pi: [] in each case.
func TestCodecEmptyBatches(t *testing.T) {
	var enc Encoder
	wantWire(t, enc.Encode([]Op{}), `[]`)
	wantWire(t, enc.Encode(nil), `[]`)
	wantWire(t, enc.Encode(ops(t, `[["s", ["p"], 1]]`)), `[["s",["p"],1]]`)

	var dec Decoder
	for _, literal := range []string{`[]`, `[["#", 0, ["a"]]]`} {
		got := decode(t, &dec, wireOps(t, literal))
		if got == nil || len(got) != 0 {
			t.Errorf("Decode(%s) = %#v, want an empty non-nil batch", literal, got)
		}
	}
	// ... and the definition took: the id resolves in the next batch.
	wantOps(t, decode(t, &dec, wireOps(t, `[["s", 0, 1]]`)), `[["s", ["a"], 1]]`)
}

// delta.test.ts (upstream 64eeb82a4) "codec" → "survives recovery from the
// last base batch": the wire pi writes for the stream, rebased at i == 5, and
// the replica a fresh decoder rebuilds from that base batch on. The immutable
// tracker has no rebase; a producer publishes a base batch by sending the
// committed revision as one Replace, as pi-durable's buffered watch does when
// it rebaselines (packages/durable/src/session/observation.ts; first pico3's
// legacy tracker, upstream 9a139c62b, which 7fd478a2e deleted). That Replace
// IS revision 5, which the replay must not modify: pi's recovery, run the same
// way with apply, throws "TypeError Cannot redefine property: deep" on the
// frozen revision.
func TestCodecSurvivesRecoveryFromTheLastBaseBatch(t *testing.T) {
	var enc Encoder
	tr := mustTrack(t, `{"a": {"deep": ""}, "b": {"deep": ""}}`)
	var wire [][]WireOp
	var retained any // the committed revision the base batch carries
	for i := range 8 {
		batch := commit(t, tr, func(d *Draft) {
			concat(t, d.At("a"), "deep", "x"+string(rune('0'+i)))
			concat(t, d.At("b"), "deep", "y"+string(rune('0'+i)))
		})
		if i == 5 {
			retained = tr.Value()
			batch = []Op{Replace{Value: retained}}
		}
		wire = append(wire, enc.Encode(batch))
	}
	for i, want := range []string{
		`[["a",["a","deep"],"x0"],["a",["b","deep"],"y0"]]`,
		`[["#",0,["a","deep"]],["a",0,"x1"],["#",1,["b","deep"]],["a",1,"y1"]]`,
		`[["a",0,"x2"],["a",1,"y2"]]`,
		`[["a",0,"x3"],["a",1,"y3"]]`,
		`[["a",0,"x4"],["a",1,"y4"]]`,
		`[["r",{"a":{"deep":"x0x1x2x3x4x5"},"b":{"deep":"y0y1y2y3y4y5"}}]]`,
		`[["a",["a","deep"],"x6"],["a",["b","deep"],"y6"]]`,
		`[["#",0,["a","deep"]],["a",0,"x7"],["#",1,["b","deep"]],["a",1,"y7"]]`,
	} {
		wantWire(t, wire[i], want)
	}

	lastBase := -1
	for i, w := range wire {
		if IsBase(w) {
			lastBase = i
		}
	}
	if lastBase != 5 {
		t.Fatalf("last base batch at %d, want 5", lastBase)
	}
	// In-process, the decoded batches still hold the tracker's revisions, so
	// the replica applies them immutably, as pi's replicas do; pi's apply would
	// throw on the frozen revision, and Go's would write into it.
	var dec Decoder
	var replica any
	for _, w := range wire[lastBase:] {
		var err error
		if replica, err = ApplyImmutable(replica, decode(t, &dec, w)); err != nil {
			t.Fatal(err)
		}
	}
	wantJSON(t, replica, tr.Value())
	wantJSON(t, replica, tree(t, `{"a": {"deep": "x0x1x2x3x4x5x6x7"}, "b": {"deep": "y0y1y2y3y4y5y6y7"}}`))
	// The replay read the base batch's payload - the tracker's revision 5 -
	// and left it as it was.
	wantJSON(t, retained, tree(t, `{"a": {"deep": "x0x1x2x3x4x5"}, "b": {"deep": "y0y1y2y3y4y5"}}`))
}

// delta.test.ts (upstream 64eeb82a4) "codec" → "round-trips random streams",
// with the apply half of the property on top: the replica built from the
// decoded batches is the producer's value. A rebase is a base batch of the
// committed revision, as above.
func TestCodecRoundTripsRandomStreams(t *testing.T) {
	rng := rand.New(rand.NewSource(0x5eed11))
	initial := `{"a": {"p": "", "q": ""}, "b": [], "c": 0}`
	for round := range 300 {
		tr := mustTrack(t, initial)
		var batches [][]Op
		for i := range 8 {
			r := rng.Float64()
			ops := commit(t, tr, func(d *Draft) {
				switch {
				case r < 0.3:
					concat(t, d.At("a"), "p", "x")
				case r < 0.5:
					concat(t, d.At("a"), "q", "y")
				case r < 0.65:
					_, err := d.At("b").Push(float64(i))
					must(t, err)
				case r < 0.8:
					must(t, d.Set("c", float64(i)))
				case r < 0.9:
					must(t, d.Delete("c"))
				}
			})
			if r >= 0.9 {
				ops = []Op{Replace{Value: tr.Value()}}
			}
			if len(ops) > 0 {
				batches = append(batches, ops)
			}
		}
		decoded := roundTrip(t, batches)
		replayBoth(t, initial, batches, decoded, tr.Value())
		if t.Failed() {
			t.Fatalf("round %d", round)
		}
	}
}

// ─── decoder checks ──────────────────────────────────────────────────────────

// delta.test.ts "flush" → "rejects a forbidden path reached through an
// interned id". ParseWireOp refuses the definition on the wire (op_test.go);
// the decoder refuses it again on a typed value, so a Go producer that builds
// wire ops by hand cannot smuggle one past a TypeScript replica.
func TestCodecRejectsForbiddenPathThroughInternedId(t *testing.T) {
	var dec Decoder
	_, err := dec.Decode([]WireOp{
		Define{ID: 0, Path: Path{Key("__proto__"), Key("w")}},
		WireSet{Ref: PathID(0), Value: true},
	})
	var unsafe *UnsafePathError
	if !errors.As(err, &unsafe) {
		t.Fatalf("got %v, want *UnsafePathError", err)
	}
	if unsafe.Segment != Key("__proto__") {
		t.Errorf("segment = %#v", unsafe.Segment)
	}
	// Nothing was defined: the id does not resolve afterwards either.
	_, err = dec.Decode([]WireOp{WireSet{Ref: PathID(0), Value: true}})
	var pe *PathError
	if !errors.As(err, &pe) {
		t.Errorf("after the refusal: got %v, want *PathError", err)
	}
}

// delta.test.ts "safety: op structure" → "rejects negative truncation", the
// decoder half: decoder().decode([["t", ["a"], -1]]) throws.
func TestCodecDecoderRejectsNegativeTruncation(t *testing.T) {
	var dec Decoder
	for _, wire := range [][]WireOp{
		{WireTruncate{Ref: Path{Key("a")}, Count: -1}},
		{WireSet{Ref: Path{Key("a")}, Value: 1}, WireTruncate{Ref: nil, Count: -1}},
	} {
		_, err := dec.Decode(wire)
		if !errors.Is(err, ErrInvalidOp) {
			t.Errorf("Decode(%s): got %v, want ErrInvalidOp", jsonText(t, wire), err)
		}
	}
}

// The decoder validates every typed value the way ParseWireOp validates the
// tree — a bad id, a negative splice bound, a reserved segment on any verb —
// and names the offending op's index.
func TestCodecDecoderValidatesTypedValues(t *testing.T) {
	cases := []struct {
		name string
		wire []WireOp
		want error
	}{
		{"bad id", []WireOp{WireSet{Ref: PathID(-1), Value: 1}}, ErrInvalidOp},
		{"negative splice index", []WireOp{WireSplice{Ref: Path{Key("xs")}, Index: -1}}, ErrInvalidOp},
		{"reserved inline segment", []WireOp{WireDelete{Ref: Path{Key("constructor")}}}, &UnsafePathError{}},
		{"reserved defined segment", []WireOp{Define{ID: 0, Path: Path{Key("prototype")}}}, &UnsafePathError{}},
		{"nil op", []WireOp{WireSet{Ref: Path{Key("a")}, Value: 1}, nil}, ErrInvalidOp},
	}
	for _, tc := range cases {
		var dec Decoder
		_, err := dec.Decode(tc.wire)
		switch want := tc.want.(type) {
		case *UnsafePathError:
			if !errors.As(err, &want) {
				t.Errorf("%s: got %v, want *UnsafePathError", tc.name, err)
			}
		default:
			if !errors.Is(err, want) {
				t.Errorf("%s: got %v, want %v", tc.name, err, want)
			}
		}
		if err == nil || !strings.Contains(err.Error(), "wire["+string(rune('0'+len(tc.wire)-1))+"]") {
			t.Errorf("%s: error %v does not name the op's index", tc.name, err)
		}
	}
}

// Only "p" may address the root, inline, through an id, or by the short
// form; s/d/a/t on an empty path are refused by the decoder before an
// applier could see them. pi: PathError [] for the refusals, the "p" and "m"
// ops decode.
func TestCodecRootPathIsLegalOnlyForSpliceAndPermute(t *testing.T) {
	for _, literal := range []string{
		`[["s", [], 1]]`,
		`[["#", 0, []], ["s", 0, 1]]`,
		`[["p", [], 0, 0, []], ["d"]]`,
		`[["#", 0, []], ["a", 0, "x"]]`,
	} {
		var dec Decoder
		_, err := dec.Decode(wireOps(t, literal))
		var pe *PathError
		if !errors.As(err, &pe) || refString(pe.Ref) != "[]" {
			t.Errorf("Decode(%s): got %v, want *PathError []", literal, err)
		}
	}
	var dec Decoder
	wantOps(t, decode(t, &dec, wireOps(t, `[["#", 0, []], ["p", 0, 0, 0, [1]]]`)), `[["p", [], 0, 0, [1]]]`)
	wantOps(t, decode(t, &dec, wireOps(t, `[["p", [], 0, 0, [1]], ["p", 1, 0, [2]]]`)), `[["p", [], 0, 0, [1]], ["p", [], 1, 0, [2]]]`)
	// pi at 10d1ad621: [["m",[],[1,0]],["m",[],[0,1]]].
	wantOps(t, decode(t, &dec, wireOps(t, `[["m", [], [1, 0]], ["m", [0, 1]]]`)), `[["m", [], [1, 0]], ["m", [], [0, 1]]]`)
}

// An id reference sets the previous path, a definition leaves it alone, and
// an id may be redefined. pi: the decoded batches below.
func TestCodecDecoderPreviousPathAndRedefinition(t *testing.T) {
	var dec Decoder
	wantOps(t, decode(t, &dec, wireOps(t, `[["#", 0, ["a"]], ["s", 0, 1], ["s", 2], ["#", 1, ["b"]], ["s", 1, 3], ["s", 4]]`)),
		`[["s", ["a"], 1], ["s", ["a"], 2], ["s", ["b"], 3], ["s", ["b"], 4]]`)
	wantOps(t, decode(t, &dec, wireOps(t, `[["s", ["a"], 1], ["#", 0, ["b"]], ["a", "x"]]`)),
		`[["s", ["a"], 1], ["a", ["a"], "x"]]`)
	wantOps(t, decode(t, &dec, wireOps(t, `[["#", 0, ["a"]], ["#", 0, ["b"]], ["s", 0, 1]]`)),
		`[["s", ["b"], 1]]`)
	// Ids persist across batches; the redefinition above is what 0 means now.
	wantOps(t, decode(t, &dec, wireOps(t, `[["d", 0]]`)), `[["d", ["b"]]]`)
}

// A "#" id is an address, as the path id that refers to it is, so an id past
// 2^31 names the same path on a 32-bit build. pi at 9a139c62b: the decoded
// batch below.
func TestCodecDefineIDPastInt32(t *testing.T) {
	op, err := ParseWireOp(tree(t, `["#", 3e9, ["a"]]`))
	if err != nil {
		t.Fatalf(`["#", 3e9, ["a"]]: %v`, err)
	}
	if id := op.(Define).ID; id != 3000000000 {
		t.Errorf(`["#", 3e9, ["a"]]: ID = %d, want 3000000000`, id)
	}
	var dec Decoder
	wantOps(t, decode(t, &dec, wireOps(t, `[["#", 3e9, ["x"]], ["s", 3e9, 1]]`)), `[["s", ["x"], 1]]`)
}

// A decode error terminates the batch where it happens: ops before it are
// not returned, so a consumer cannot half-apply a batch it must discard.
func TestCodecDecodeErrorReturnsNoOps(t *testing.T) {
	var dec Decoder
	got, err := dec.Decode(wireOps(t, `[["s", ["a"], 1], ["s", 0, 2]]`))
	if err == nil {
		t.Fatal("unknown id accepted")
	}
	if got != nil {
		t.Errorf("Decode returned %s alongside the error", jsonText(t, got))
	}
}

// The encoder does not validate: a reserved segment or a negative count
// passes through as-is, and the decoder on the other side is what refuses
// it. Encoding is a producer-side operation on a batch a tracker prepared.
func TestCodecEncoderDoesNotValidate(t *testing.T) {
	var enc Encoder
	wire := enc.Encode([]Op{Set{Path: Path{Key("__proto__")}, Value: 1}, Truncate{Path: Path{Key("s")}, Count: -1}})
	wantWire(t, wire, `[["s",["__proto__"],1],["t",["s"],-1]]`)
	var dec Decoder
	if _, err := dec.Decode(wire); err == nil {
		t.Error("decoder accepted what the encoder passed through")
	}
	// A nil op is a producer bug, not bad input.
	if err := mustPanic(t, func() { enc.Encode([]Op{nil}) }); !strings.Contains(err.Error(), "nil") {
		t.Errorf("panic %v does not name the nil element", err)
	}
}

// A decoded batch carries the same slices its wire form did — a Splice's
// items and a Set's value are adopted, not copied — so a consumer that
// serialised nothing owns what it decoded, as with Apply.
func TestCodecDecodeAdoptsPayloads(t *testing.T) {
	items := []any{float64(1)}
	value := map[string]any{"k": float64(1)}
	var enc Encoder
	var dec Decoder
	got := decode(t, &dec, enc.Encode([]Op{
		Splice{Path: Path{Key("xs")}, Items: items},
		Set{Path: Path{Key("o")}, Value: value},
	}))
	if got[0].(Splice).Items[0] != items[0] || cap(got[0].(Splice).Items) != cap(items) {
		t.Error("splice items were copied")
	}
	if reflect.ValueOf(got[1].(Set).Value).Pointer() != reflect.ValueOf(value).Pointer() {
		t.Error("set value was copied")
	}
}

// state-diff.test.ts (upstream 10d1ad621) "validates and encodes
// permutations", the codec half, extended past one batch: the short form on a
// repeated path, the second-use definition and reference, and a root "m".
// pi at 10d1ad621 encoded this batch to exactly the wire below.
func TestCodecPermute(t *testing.T) {
	batch := ops(t, `[["m", ["values"], [2, 0, 1]], ["m", ["values"], [1, 2, 0]], ["s", ["x"], 1], ["m", ["values"], [0, 2, 1]], ["m", [], [1, 0]]]`)
	var enc Encoder
	wantWire(t, enc.Encode(batch), `[["m",["values"],[2,0,1]],["m",[1,2,0]],["s",["x"],1],["#",0,["values"]],["m",0,[0,2,1]],["m",[],[1,0]]]`)
	roundTrip(t, [][]Op{batch})

	// With nothing before it, the short form resolves nothing. pi: PathError [].
	var dec Decoder
	_, err := dec.Decode(wireOps(t, `[["m", [1, 0]]]`))
	var pe *PathError
	if !errors.As(err, &pe) || refString(pe.Ref) != "[]" {
		t.Errorf("short m with no previous path: got %v, want *PathError []", err)
	}
}
