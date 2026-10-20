package jstext

import (
	"encoding/json"
	"math"
	"os"
	"testing"
)

// TestStringifyMatchesNode compares Stringify with node's JSON.stringify for
// testdata/stringify-node.json, captured by testdata/capture-stringify.mjs.
// The cases cover the escapes where encoding/json and JSON.stringify disagree
// (<, >, &, U+2028, U+2029), an escaped backslash right before "u2028", and
// the escapes they already share.
func TestStringifyMatchesNode(t *testing.T) {
	data, err := os.ReadFile("testdata/stringify-node.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Input any    `json:"input"`
		JSON  string `json:"json"`
	}
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) == 0 {
		t.Fatal("no cases in testdata/stringify-node.json")
	}
	for _, c := range cases {
		got, err := Stringify(c.Input)
		if err != nil {
			t.Fatalf("Stringify(%v): %v", c.Input, err)
		}
		if got != c.JSON {
			t.Errorf("Stringify = %q, node = %q", got, c.JSON)
		}
	}
}

// htmlMarshaler returns text json.Marshal has already HTML-escaped, the way a
// MarshalJSON method that delegates to json.Marshal does.
type htmlMarshaler struct{ S string }

func (h htmlMarshaler) MarshalJSON() ([]byte, error) { return json.Marshal(h.S) }

func TestStringifyUndoesEscapesFromMarshalJSON(t *testing.T) {
	got, err := Stringify(map[string]any{"v": htmlMarshaler{"a<b>&c\u2028"}})
	if err != nil {
		t.Fatal(err)
	}
	if want := "{\"v\":\"a<b>&c\u2028\"}"; got != want {
		t.Fatalf("Stringify = %q, want %q", got, want)
	}
}

// JSON.stringify writes negative zero as 0 (it cannot reach the node-captured
// cases above: node's own oracle file already reads 0). Only a number is
// rewritten; "-0" inside a string, -0.5 and -1 are not.
func TestStringifyNegativeZero(t *testing.T) {
	negZero := math.Copysign(0, -1)
	got, err := Stringify(map[string]any{"a": []any{negZero, 0.0, -0.5, -1.0, negZero}, "s": "-0 x", "z": negZero})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"a":[0,0,-0.5,-1,0],"s":"-0 x","z":0}`; got != want {
		t.Fatalf("Stringify = %s, want %s", got, want)
	}
}

// JSON.stringify writes a number that is not finite as null, where
// encoding/json refuses it (node: JSON.stringify({a: NaN, b: [Infinity]}) is
// {"a":null,"b":[null]}). The value handed in is not changed.
func TestStringifyNonFiniteIsNull(t *testing.T) {
	in := map[string]any{"a": math.NaN(), "b": []any{math.Inf(1), 1.5, map[string]any{"c": math.Inf(-1)}}, "d": "Infinity"}
	got, err := Stringify(in)
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"a":null,"b":[null,1.5,{"c":null}],"d":"Infinity"}`; got != want {
		t.Fatalf("Stringify = %s, want %s", got, want)
	}
	if !math.IsInf(in["b"].([]any)[0].(float64), 1) {
		t.Fatalf("Stringify changed its argument: %v", in)
	}
	if got, err := Stringify(math.Inf(-1)); err != nil || got != "null" {
		t.Fatalf("Stringify(-Inf) = %q, %v; want null", got, err)
	}
}

// JSON.stringify spells a parsed number by its value, not its literal (node
// v26.4.0: JSON.stringify(JSON.parse('[1.50,1e308,1e400,-0,100e-2]')) is
// `[1.5,1e+308,null,0,1]`).
func TestStringifySpellsParsedNumbersLikeJS(t *testing.T) {
	v, err := Parse([]byte(`[1.50,1e308,1e400,-0,100e-2,{"n":2E1}]`))
	if err != nil {
		t.Fatal(err)
	}
	got, err := Stringify(v)
	if err != nil {
		t.Fatal(err)
	}
	if want := `[1.5,1e+308,null,0,1,{"n":20}]`; got != want {
		t.Fatalf("Stringify = %s, want %s", got, want)
	}
}

type celsius float64

type marshaled float64

func (marshaled) MarshalJSON() ([]byte, error) { return []byte(`"own"`), nil }

// Floats of any type, and slices and arrays of any element type, are seen as
// JavaScript sees them; a type with its own MarshalJSON keeps it.
func TestStringifyTypedFloatsAndSlices(t *testing.T) {
	for _, c := range []struct {
		v    any
		want string
	}{
		{float32(math.NaN()), "null"},
		{float32(0.1), "0.10000000149011612"},
		{celsius(math.Inf(1)), "null"},
		{[]float64{math.NaN(), math.Copysign(0, -1), 1e21}, "[null,0,1e+21]"},
		{[2]float32{1, float32(math.Inf(-1))}, "[1,null]"},
		{[]int{1, 2}, "[1,2]"},
		{[]byte("hi"), `"aGk="`},
		{marshaled(math.NaN()), `"own"`},
		{[]marshaled{1}, `["own"]`},
	} {
		got, err := Stringify(c.v)
		if err != nil || got != c.want {
			t.Errorf("Stringify(%#v) = %q, %v; want %q", c.v, got, err, c.want)
		}
	}
}
