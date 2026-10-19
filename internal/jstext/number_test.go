package jstext

import (
	"math"
	"testing"
)

func TestNumberToString(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{1000000, "1000000"},
		{0.0000001, "1e-7"},
		{1e21, "1e+21"},
		{1e20, "100000000000000000000"},
		{1e-6, "0.000001"},
		{1e-7, "1e-7"},
		{123.456, "123.456"},
		{-0.5, "-0.5"},
		{0, "0"},
		{math.Copysign(0, -1), "0"},
		{42, "42"},
		{1.5e22, "1.5e+22"},
		{1234567890123456789, "1234567890123456800"},
		{math.Inf(1), "Infinity"},
		{math.Inf(-1), "-Infinity"},
		{math.NaN(), "NaN"},
	}
	for _, c := range cases {
		if got := NumberToString(c.in); got != c.want {
			t.Errorf("NumberToString(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

// String(JSON.parse(text)) in node, for each text; throws marks the texts
// whose String() throws "TypeError: Cannot convert object to primitive value".
func TestToStringMatchesNode(t *testing.T) {
	for _, tc := range []struct {
		text, want string
		throws     bool
	}{
		{text: "null", want: "null"},
		{text: "true", want: "true"},
		{text: "false", want: "false"},
		{text: "5", want: "5"},
		{text: "5.0", want: "5"},
		{text: "-0", want: "0"},
		{text: "1e21", want: "1e+21"},
		{text: "0.1", want: "0.1"},
		{text: "1e400", want: "Infinity"},
		{text: "-1e400", want: "-Infinity"},
		{text: `"x"`, want: "x"},
		{text: "[]", want: ""},
		{text: "[null]", want: ""},
		{text: `[null,"x",["y",2]]`, want: ",x,y,2"},
		{text: "[[],[[]]]", want: ","},
		{text: "{}", want: "[object Object]"},
		{text: `{"x":1}`, want: "[object Object]"},
		{text: "[{}]", want: "[object Object]"},
		{text: `{"valueOf":1}`, want: "[object Object]"},
		{text: `{"a":{"toString":1}}`, want: "[object Object]"},
		{text: `{"__proto__":{"toString":1}}`, want: "[object Object]"},
		{text: `{"toString":1}`, throws: true},
		{text: `{"toString":1,"valueOf":2}`, throws: true},
		{text: `[{"toString":"x"}]`, throws: true},
		{text: `[1,{"toString":1}]`, throws: true},
		{text: `[[{"toString":null}]]`, throws: true},
	} {
		v, err := Parse([]byte(tc.text))
		if err != nil {
			t.Fatalf("Parse(%s): %v", tc.text, err)
		}
		got, ok := ToString(v)
		switch {
		case tc.throws && ok:
			t.Errorf("ToString(%s) = %q, but node's String() throws", tc.text, got)
		case !tc.throws && !ok:
			t.Errorf("ToString(%s) has no string form, node says %q", tc.text, tc.want)
		case got != tc.want:
			t.Errorf("ToString(%s) = %q, node says %q", tc.text, got, tc.want)
		}
	}
}

// String(Number(JSON.parse(text))) in node, for each text; throws marks the
// texts whose Number() throws "TypeError: Cannot convert object to primitive
// value".
func TestToNumberMatchesNode(t *testing.T) {
	for _, tc := range []struct {
		text, want string
		throws     bool
	}{
		{text: "null", want: "0"},
		{text: "true", want: "1"},
		{text: "false", want: "0"},
		{text: "5", want: "5"},
		{text: "1e400", want: "Infinity"},
		{text: `"500"`, want: "500"},
		{text: `" 500 "`, want: "500"},
		{text: `"0x1F"`, want: "31"},
		{text: `""`, want: "0"},
		{text: `"abc"`, want: "NaN"},
		{text: `"1e3"`, want: "1000"},
		{text: `"Infinity"`, want: "Infinity"},
		{text: "[]", want: "0"},
		{text: "[429]", want: "429"},
		{text: `["429"]`, want: "429"},
		{text: "[[429]]", want: "429"},
		{text: "[1,2]", want: "NaN"},
		{text: "[null]", want: "0"},
		{text: "{}", want: "NaN"},
		{text: `{"valueOf":1}`, want: "NaN"},
		{text: `{"code":500}`, want: "NaN"},
		{text: `{"toString":1}`, throws: true},
		{text: `[{"toString":1}]`, throws: true},
	} {
		v, err := Parse([]byte(tc.text))
		if err != nil {
			t.Fatalf("Parse(%s): %v", tc.text, err)
		}
		n, ok := ToNumber(v)
		switch {
		case tc.throws && ok:
			t.Errorf("ToNumber(%s) = %v, but node's Number() throws", tc.text, n)
		case !tc.throws && !ok:
			t.Errorf("ToNumber(%s) has no number, node says %s", tc.text, tc.want)
		case ok && NumberToString(n) != tc.want:
			t.Errorf("ToNumber(%s) = %s, node says %s", tc.text, NumberToString(n), tc.want)
		}
	}
}

// Expected strings from node v26.4.0: [x, x.toFixed(6), x.toFixed(0),
// x.toFixed(2)].
func TestToFixedMatchesNode(t *testing.T) {
	for _, tc := range []struct {
		x              float64
		six, zero, two string
	}{
		{0, "0.000000", "0", "0.00"},
		{math.Copysign(0, -1), "0.000000", "0", "0.00"},
		{0.0078125, "0.007813", "0", "0.01"},
		{1.0000005, "1.000001", "1", "1.00"},
		{123.456, "123.456000", "123", "123.46"},
		{0.30000000000000004, "0.300000", "0", "0.30"},
		{1e21, "1e+21", "1e+21", "1e+21"},
		{-1e21, "-1e+21", "-1e+21", "-1e+21"},
		{2.5e-7, "0.000000", "0", "0.00"},
		{-2.5e-7, "-0.000000", "-0", "-0.00"},
		{-1e-7, "-0.000000", "-0", "-0.00"},
		{5e-7, "0.000000", "0", "0.00"},
		{999999.9999995, "999999.999999", "1000000", "1000000.00"},
		{0.1234565, "0.123456", "0", "0.12"},
		{math.NaN(), "NaN", "NaN", "NaN"},
		{math.Inf(1), "Infinity", "Infinity", "Infinity"},
		{math.Inf(-1), "-Infinity", "-Infinity", "-Infinity"},
		{1.5, "1.500000", "2", "1.50"},
		{-1.5, "-1.500000", "-2", "-1.50"},
		{0.000001, "0.000001", "0", "0.00"},
		{12345678901234567890, "12345678901234567168.000000", "12345678901234567168", "12345678901234567168.00"},
	} {
		for _, c := range []struct {
			digits int
			want   string
		}{{6, tc.six}, {0, tc.zero}, {2, tc.two}} {
			if got := ToFixed(tc.x, c.digits); got != c.want {
				t.Errorf("(%v).toFixed(%d) = %q, node says %q", tc.x, c.digits, got, c.want)
			}
		}
	}
}
