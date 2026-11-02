package schema

import (
	"math"
	"testing"

	"github.com/sky-valley/pi/internal/jstext"
)

// shared.test.ts at 6536f8012 checks that a bare-type schema shares one
// module-level check (a Go func has no identity to compare) and that the
// pattern cache drops a RegExp once its schemas are collected (the port's
// cache keeps its patterns; see regexes). What carries over is the sharing.
func TestSharedRegexIsSharedBySource(t *testing.T) {
	before := sharedRegexCount()
	var validators []*Validator
	for i := range 50 {
		validators = append(validators, mustCompile(t, Object{{"type", "string"}, {"pattern", "^unique-" + jstext.NumberToString(float64(i)) + "$"}}, Options{}))
	}
	again := mustCompile(t, Object{{"type", "string"}, {"pattern", "^unique-0$"}}, Options{})
	if got := sharedRegexCount(); got != before+50 {
		t.Errorf("cache holds %d, want %d", got, before+50)
	}
	if !again.Check("unique-0") || !validators[0].Check("unique-0") || validators[1].Check("unique-0") {
		t.Error("checks")
	}
	a, _ := sharedRegex("^unique-0$")
	b, _ := sharedRegex("^unique-0$")
	if a != b {
		t.Error("one source compiled twice")
	}
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
	b, err := Object{{"z", 1.0}, {"a", Object{{"y", "x"}}}}.MarshalJSON()
	if err != nil || string(b) != `{"z":1,"a":{"y":"x"}}` {
		t.Errorf("MarshalJSON %s %v", b, err)
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
