package schema

import (
	"encoding/json"
	"math"
	"math/big"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sky-valley/pi/internal/jstext"
)

// shared.test.ts at 6536f8012 checks that a bare-type schema shares one
// module-level check (a Go func has no identity to compare) and that the
// pattern cache drops a RegExp once its schemas are collected (the port's
// cache keeps its patterns; see regexes). What carries over is the sharing.
func TestSharedRegexIsSharedBySource(t *testing.T) {
	// The cache is the process's: each run (go test -count=n) uses sources
	// no run before it has.
	prefix := "^unique-" + jstext.NumberToString(float64(sharedRegexRuns.Add(1))) + "-"
	before := sharedRegexCount()
	var validators []*Validator
	for i := range 50 {
		validators = append(validators, mustCompile(t, Object{{"type", "string"}, {"pattern", prefix + jstext.NumberToString(float64(i)) + "$"}}, Options{}))
	}
	again := mustCompile(t, Object{{"type", "string"}, {"pattern", prefix + "0$"}}, Options{})
	if got := sharedRegexCount(); got != before+50 {
		t.Errorf("cache holds %d, want %d", got, before+50)
	}
	value := prefix[1:] + "0"
	if !again.Check(value) || !validators[0].Check(value) || validators[1].Check(value) {
		t.Error("checks")
	}
	a, _ := sharedRegex(prefix + "0$")
	b, _ := sharedRegex(prefix + "0$")
	if a != b {
		t.Error("one source compiled twice")
	}
}

// sharedRegexRuns counts the runs of TestSharedRegexIsSharedBySource.
var sharedRegexRuns atomic.Int64

// sharedRegexCount is the number of cached sources.
func sharedRegexCount() int {
	n := 0
	regexes.Range(func(any, any) bool { n++; return true })
	return n
}

func TestDecodeReadsJSONAsJSONParseDoes(t *testing.T) {
	for _, c := range []struct{ in, out string }{
		{`{"b":1,"a":2,"10":3,"2":4,"01":5}`, `{"2":4,"10":3,"b":1,"a":2,"01":5}`},
		{`{"a":1,"b":2,"a":3}`, `{"a":3,"b":2}`},
		{`[1e400,-1e400,1e-400,-0,0.1]`, `[null,null,0,0,0.1]`},
		{`"𐀀 \ud800 \udc00\ud800 é\/"`, `"𐀀 \ud800 \udc00\ud800 é/"`},
		{` {"x" : [ true , false , null ] } `, `{"x":[true,false,null]}`},
	} {
		v, err := Decode([]byte(c.in))
		if err != nil {
			t.Errorf("%s: %v", c.in, err)
			continue
		}
		if got := stringify(v); got != c.out {
			t.Errorf("%s: %s, want %s", c.in, got, c.out)
		}
	}
	v, _ := Decode([]byte(`[1e400,-0]`))
	if list := v.([]any); !math.IsInf(list[0].(float64), 1) || !math.Signbit(list[1].(float64)) {
		t.Errorf("numbers %v", list)
	}
	for _, bad := range []string{``, `{`, `{"a":1,}`, `[1 2]`, `01`, `"\x01"`, `{"a":1} x`, `nul`} {
		if _, err := Decode([]byte(bad)); err == nil {
			t.Errorf("%q decoded", bad)
		}
	}
	if _, err := Decode([]byte(`{"a" 1}`)); err == nil || err.Error() == "" {
		t.Error("no syntax error")
	}
}

func TestMapsListKeysInJavaScriptOrder(t *testing.T) {
	m := map[string]any{"b": 1, "a": 2, "10": 3, "2": 4, "😀": 5, "￿": 6}
	got := ownKeys(m)
	want := []string{"2", "10", "a", "b", "😀", "￿"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("keys %q, want %q", got, want)
		}
	}
	wantJSON(t, m, `{"2":4,"10":3,"a":2,"b":1,"😀":5,"￿":6}`)
}

func TestStringifyWritesWhatJSONStringifyWrites(t *testing.T) {
	for _, c := range []struct {
		v    any
		want string
	}{
		{"a\"\\\b\f\n\r\t\x01 <>&", `"a\"\\\b\f\n\r\t\u0001` + " <>&\""},
		{jstext.CodeUnit(0xDC01), `"\udc01"`},
		{[]any{math.Inf(1), math.NaN(), math.Copysign(0, -1), 1e21, 1e-7, undefined}, `[null,null,0,1e+21,1e-7,null]`},
		{Object{{"a", undefined}, {"b", []byte{1, 2}}}, `{"b":{"0":1,"1":2}}`},
		{int64(7), `7`},
	} {
		if got := stringify(c.v); got != c.want {
			t.Errorf("stringify %#v = %s, want %s", c.v, got, c.want)
		}
	}
	b, err := Object{{"z", 1.0}, {"a", Object{{"y", "x"}}}, {"u", Undefined}}.MarshalJSON()
	if err != nil || string(b) != `{"z":1,"a":{"y":"x"}}` {
		t.Errorf("MarshalJSON %s %v", b, err)
	}
	// JSON.stringify of a bigint throws V8's TypeError.
	if b, err := (Object{{"a", big.NewInt(1)}}).MarshalJSON(); err == nil || err.Error() != "Do not know how to serialize a BigInt" {
		t.Errorf("MarshalJSON of a bigint = %s, %v", b, err)
	}
	// encoding/json writes Undefined as null, as JSON.stringify writes an
	// undefined array item; a map keeps an Undefined member's key.
	for _, c := range []struct {
		v    any
		want string
	}{
		{[]any{Undefined}, `[null]`},
		{map[string]any{"a": Undefined}, `{"a":null}`},
		{Undefined, `null`},
	} {
		if b, err := json.Marshal(c.v); err != nil || string(b) != c.want {
			t.Errorf("json.Marshal %#v = %s, %v, want %s", c.v, b, err, c.want)
		}
	}
}

func TestDeepEqualAndIdentity(t *testing.T) {
	if !deepEqual(Object{{"a", 1.0}, {"b", []any{2.0}}}, map[string]any{"b": []any{2}, "a": 1}) {
		t.Error("an Object and a map with the same members differ")
	}
	if deepEqual([]any{1.0}, Object{{"0", 1.0}}) || deepEqual(Object{{"a", 1.0}}, Object{{"a", 1.0}, {"b", 2.0}}) {
		t.Error("unequal values equal")
	}
	if !deepEqual([]byte{1}, Object{{"0", 1.0}}) {
		t.Error("bytes compare as objects of index keys")
	}
	a := []any{1.0}
	if !strictEquals(a, a) || strictEquals(a, []any{1.0}) || !strictEquals(json6(), 6.0) || strictEquals(math.NaN(), math.NaN()) {
		t.Error("strictEquals")
	}
	if !sameValue(math.NaN(), math.NaN()) || sameValue(0.0, math.Copysign(0, -1)) {
		t.Error("sameValue")
	}
}

func json6() any { return int8(6) }

// TestDecodeLiteralsDoNotCopyTheInput: reading a literal (null, true, false)
// compares in place; a copy of the rest of the input per literal made an
// array of 80k nulls take seconds.
func TestDecodeLiteralsDoNotCopyTheInput(t *testing.T) {
	data := []byte("[" + strings.Repeat("null,true,false,", 1000) + "null]")
	allocs := testing.AllocsPerRun(5, func() {
		if _, err := Decode(data); err != nil {
			t.Fatal(err)
		}
	})
	if allocs > 100 {
		t.Errorf("decoding 3001 literals allocates %v times", allocs)
	}
}

func BenchmarkDecodeLiterals(b *testing.B) {
	data := []byte("[" + strings.Repeat("null,", 80000) + "null]")
	for b.Loop() {
		if _, err := Decode(data); err != nil {
			b.Fatal(err)
		}
	}
}

// TestDecodeOrdersIndexKeys: array-index keys come first, ascending over
// their whole uint32 range (a difference of two cast to a 32-bit int would
// wrap), and the other keys keep their order.
func TestDecodeOrdersIndexKeys(t *testing.T) {
	v, err := Decode([]byte(`{"b":0,"4294967294":1,"x":2,"0":3,"2147483648":4,"5":5,"4294967295":6}`))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := stringify(v), `{"0":3,"5":5,"2147483648":4,"4294967294":1,"b":0,"x":2,"4294967295":6}`; got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

// equal.test.ts (since af79b0c3d): Equal is JSON equality, a drop-in for
// TypeBox's Value.Equal; the wants are Value.Equal's.
func TestEqualComparesJSONValuesStructurally(t *testing.T) {
	for _, c := range []struct {
		a, b any
		want bool
	}{
		{1.0, 1.0, true},
		{"a", "b", false},
		{nil, nil, true},
		{nil, Undefined, false},
		{js(t, `{"a":1,"b":[1,{"c":2}]}`), js(t, `{"b":[1,{"c":2}],"a":1}`), true},
		{js(t, `{"a":1}`), js(t, `{"a":1,"b":2}`), false},
		{js(t, `[1,2]`), js(t, `[2,1]`), false},
		{js(t, `[1]`), js(t, `{"0":1}`), false},
		{js(t, `{}`), js(t, `[]`), false},
		{Object{{"a", Undefined}}, Object{}, false},
	} {
		if got := Equal(c.a, c.b); got != c.want {
			t.Errorf("Equal(%s, %s) = %v", stringify(c.a), stringify(c.b), got)
		}
	}
}
