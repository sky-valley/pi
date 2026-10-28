package providers

import (
	"encoding/json"
	"testing"
)

// rawJSON spells a member's JSON text; nil is an absent member (undefined).
func rawJSON(s string) json.RawMessage { return json.RawMessage(s) }

// TestRawToStringMatchesNode pins String(value) for the values a wire event can
// hold. want is node's String(JSON.parse(in)); nil is undefined.
func TestRawToStringMatchesNode(t *testing.T) {
	for _, tc := range []struct {
		in   json.RawMessage
		want string
	}{
		{nil, "undefined"},
		{rawJSON(`5`), "5"},
		{rawJSON(`5.0`), "5"},
		{rawJSON(`-0`), "0"},
		{rawJSON(`1e400`), "Infinity"},
		{rawJSON(`1e21`), "1e+21"},
		{rawJSON(`0.1`), "0.1"},
		{rawJSON(`"0"`), "0"},
		{rawJSON(`null`), "null"},
		{rawJSON(`true`), "true"},
		{rawJSON(`false`), "false"},
		{rawJSON(`[1,[2,null]]`), "1,2,"},
		{rawJSON(`[]`), ""},
		{rawJSON(`{}`), "[object Object]"},
	} {
		got, err := rawToString(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("rawToString(%s) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
	for _, in := range []string{`{"toString":1}`, `[{"toString":1}]`} {
		if _, err := rawToString(rawJSON(in)); err == nil || err.Error() != "Cannot convert object to primitive value" {
			t.Errorf("rawToString(%s) error = %v, want V8's TypeError", in, err)
		}
	}
}

// TestRawStrictEqualMatchesNode pins === between values of two parsed events.
// want is node's JSON.parse(a) === JSON.parse(b); nil is undefined.
func TestRawStrictEqualMatchesNode(t *testing.T) {
	for _, tc := range []struct {
		a, b json.RawMessage
		want bool
	}{
		{rawJSON(`0`), rawJSON(`-0`), true},
		{rawJSON(`1`), rawJSON(`1.0`), true},
		{rawJSON(`1e400`), rawJSON(`1e400`), true},
		{rawJSON(`"0"`), rawJSON(`0`), false},
		{rawJSON(`"a"`), rawJSON(`"a"`), true},
		{rawJSON(`"\u0030"`), rawJSON(`"0"`), true},
		{rawJSON(`null`), rawJSON(`null`), true},
		{rawJSON(`true`), rawJSON(`true`), true},
		{rawJSON(`true`), rawJSON(`false`), false},
		{rawJSON(`{}`), rawJSON(`{}`), false},
		{rawJSON(`[]`), rawJSON(`[]`), false},
		{nil, nil, true},
		{nil, rawJSON(`null`), false},
		{rawJSON(`null`), nil, false},
	} {
		if got := rawStrictEqual(tc.a, tc.b); got != tc.want {
			t.Errorf("rawStrictEqual(%s, %s) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

// TestRawCount pins which counts an int holds.
func TestRawCount(t *testing.T) {
	for _, tc := range []struct {
		in    json.RawMessage
		count int
		ok    bool
	}{
		{rawJSON(`10.0`), 10, true},
		{rawJSON(`5e0`), 5, true},
		{rawJSON(`-3`), -3, true},
		{rawJSON(`2.9`), 2, true},
		{rawJSON(`1e400`), 0, false},
		{rawJSON(`"10"`), 0, false},
		{rawJSON(`true`), 0, false},
		{nil, 0, false},
	} {
		count, ok := rawCount(tc.in)
		if count != tc.count || ok != tc.ok {
			t.Errorf("rawCount(%s) = (%d, %v), want (%d, %v)", tc.in, count, ok, tc.count, tc.ok)
		}
	}
}

// TestDecodeRawObjectMatchesEncodingJSON requires decodeRawObject, which splits
// an object itself, to read every object as json.Unmarshal into a
// map[string]json.RawMessage reads it: the same keys (escapes, surrogate
// pairs and invalid UTF-8 unquoted alike), each value's exact text, a
// repeated key's last value, and the same failure for text that is not an
// object.
func TestDecodeRawObjectMatchesEncodingJSON(t *testing.T) {
	for _, in := range []string{
		`{}`,
		` { } `,
		`{"a":1}`,
		`{ "a" : 1 , "b" : [ 1 , { "c" : "}" } ] }`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}`,
		`{"a":"x\"y","b":"\\","c":"{[","d":"]}","e":"\B""}`,
		`{"k\u0041":1,"\ud83d\ude00":2,"\"q":3,"\n":4}`,
		`{"dup":1,"x":2,"dup":{"n":3}}`,
		`{"n":-1.5e+10,"m":0,"t":true,"f":false,"z":null,"e":[],"o":{}}`,
		`{"deep":[[[{"a":[1,[2,{"b":"]"}]]}]]],"after":"ok"}`,
		"{\"u\":\"é🙂\",\"é\":\"x\"}",
		"{\n\t\"a\"\n:\r\n1\n,\"b\":2 }",
		"{\"bad\xff\":\"v\xfe\",\"k\xe2\x82\":1}",
		`[1]`, `null`, `5`, `"s"`, `{"a":}`, `{"a":1`, `{"a" 1}`, `{} x`, ``,
	} {
		want := rawObject{}
		wantErr := json.Unmarshal([]byte(in), &want)
		got, err := decodeRawObject([]byte(in))
		if (err != nil) != (wantErr != nil) || (err != nil && err.Error() != wantErr.Error()) {
			t.Errorf("%q: error = %v, want %v", in, err, wantErr)
			continue
		}
		if err != nil {
			continue
		}
		if len(got) != len(want) {
			t.Errorf("%q: %d members %q, want %d %q", in, len(got), got, len(want), want)
		}
		for k, v := range want {
			if g, ok := got[k]; !ok || string(g) != string(v) {
				t.Errorf("%q: member %q = %q (present %v), want %q", in, k, g, ok, v)
			}
		}
	}
}

// TestDecodeRawObjectMembersCannotBeAppendedInto requires a member's capacity
// to end with it, so appending to one cannot overwrite the text after it.
func TestDecodeRawObjectMembersCannotBeAppendedInto(t *testing.T) {
	data := []byte(`{"a":"x","b":"y"}`)
	o, err := decodeRawObject(data)
	if err != nil {
		t.Fatal(err)
	}
	_ = append(o["a"], '!')
	if string(data) != `{"a":"x","b":"y"}` || string(o["b"]) != `"y"` {
		t.Fatalf("appending to a member changed the text after it: %s", data)
	}
}
