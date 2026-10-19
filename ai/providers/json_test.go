package providers

import (
	"encoding/json"
	"os"
	"reflect"
	"strconv"
	"testing"

	"github.com/sky-valley/pi/internal/jstext"
)

// D7d: a trailing comma INSIDE an open string is content, not a dangling
// token; completion must not strip it (vectors verified against partial-json
// 0.1.7, pi's streaming-JSON dependency).
func TestParseStreamingJSONTrailingCommaInsideString(t *testing.T) {
	cases := []struct {
		in   string
		want map[string]any
	}{
		{`{"a":"x,`, map[string]any{"a": "x,"}},
		// partial-json trims the whole input first, so trailing whitespace
		// after the comma is dropped even inside the open string.
		{`{"a":"x, `, map[string]any{"a": "x,"}},
		// Real dangling commas (outside strings) are still stripped.
		{`{"a":1,`, map[string]any{"a": float64(1)}},
		{`{"a":1, `, map[string]any{"a": float64(1)}},
		{`{"a":"x,","b`, map[string]any{"a": "x,"}},
		{`{"key":`, map[string]any{}},
	}
	for _, c := range cases {
		if got, _ := parseStreamingJSON(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("parseStreamingJSON(%q) = %#v want %#v", c.in, got, c.want)
		}
	}
}

// The arguments a streamed tool call replays keep the key order of the object
// pi's parseStreamingJson builds: array-index keys first, ascending, at every
// depth. Each want is node's JSON.stringify(parseStreamingJson(in)) at pi
// 002fc8385 (partial-json 0.1.7).
func TestParseStreamingJSONKeyOrderMatchesPi(t *testing.T) {
	cases := []struct{ in, want string }{
		{`{"b":1,"2":"x","1":"y"}`, `{"1":"y","2":"x","b":1}`},
		{`{"b":1,"2":"x","1":"y`, `{"1":"y","2":"x","b":1}`},
		{`{"cmd":"ls","10":{"b":0,"0":1},"9":[{"1":1,"0":0}]}`, `{"9":[{"0":0,"1":1}],"10":{"0":1,"b":0},"cmd":"ls"}`},
	}
	for _, c := range cases {
		_, ordered := parseStreamingJSON(c.in)
		got, err := json.Marshal(ordered)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != c.want {
			t.Errorf("parseStreamingJSON(%q) orders as %s, pi %s", c.in, got, c.want)
		}
	}
}

// A member whose key is complete but whose value has not started — `"key"` or
// `"key":` — is dropped with the comma before it, and the members before it
// stay, whatever the key holds (a comma, an escaped quote). Each want is
// node's JSON.stringify(parseStreamingJson(in)) at pi 002fc8385
// (partial-json 0.1.7).
func TestParseStreamingJSONDropsADanglingMemberLikePi(t *testing.T) {
	cases := []struct{ in, want string }{
		{`{"b":1,"a":"x","c":`, `{"b":1,"a":"x"}`},
		{`{"b":1,"a":"x","c" :`, `{"b":1,"a":"x"}`},
		{`{"b":1,"a":"x" , "c": `, `{"b":1,"a":"x"}`},
		{`{"key":`, `{}`},
		{`{"a":{"b":`, `{"a":{}}`},
		{`{"a":{"x":1,"b":`, `{"a":{"x":1}}`},
		{`{"x":1,"a,b":`, `{"x":1}`},
		{`{"x":1,"a\",b":`, `{"x":1}`},
		{`{"x":[1,{"y":`, `{"x":[1,{}]}`},
		{`{"x":"v:","y":`, `{"x":"v:"}`},
		{`{"b":1,"2":"x","1":`, `{"2":"x","b":1}`},
		{`{"b":1,"c"`, `{"b":1}`},
		{`{"b":1,"c" `, `{"b":1}`},
		// A string in value position is a value, however the text ends.
		{`{"a":"x"`, `{"a":"x"}`},
		{`{"a":["x"`, `{"a":["x"]}`},
		{`{"a":"x","b":"y"`, `{"a":"x","b":"y"}`},
		{`{"a":{"k":"v"`, `{"a":{"k":"v"}}`},
	}
	for _, c := range cases {
		_, ordered := parseStreamingJSON(c.in)
		got, err := json.Marshal(ordered)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != c.want {
			t.Errorf("parseStreamingJSON(%q) = %s, pi %s", c.in, got, c.want)
		}
	}
}

// streamingJSONCaptureFile is what testdata/streaming-json/capture.mts recorded
// from pi's parseStreamingJson at 4b994d5e5 with the partial-json its lockfile
// pins: the arguments pi reads out of every prefix of tool-argument texts, and
// out of single inputs.
const streamingJSONCaptureFile = "testdata/streaming-json/streaming-json-4b994d5e5.json"

// parseStreamingJSON reads every prefix of a tool call's argument text as pi's
// parseStreamingJson does — JSON.parse, its repair, partial-json 0.1.7's
// parse of both — to the byte of JSON.stringify: a partial literal, number,
// string, escape or key, a repaired control character or escape, array-index
// keys, "__proto__", and a non-finite number as null. Two kinds of result are
// counted, not compared, since a Go value cannot hold them: one pi holds as a
// non-object (the capture records null), which is the port's {}, and one
// holding half a surrogate pair — a \uD83D escape whose other half has not
// streamed yet — which JSON.stringify writes as that escape and the port
// holds as U+FFFD.
func TestParseStreamingJSONLikePi(t *testing.T) {
	data, err := os.ReadFile(streamingJSONCaptureFile)
	if err != nil {
		t.Fatal(err)
	}
	var capture struct {
		PartialJSON string `json:"partialJSON"`
		Documents   []struct {
			Doc     string    `json:"doc"`
			Results []*string `json:"results"`
		} `json:"documents"`
		Cases []struct {
			Input  string  `json:"input"`
			Result *string `json:"result"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(data, &capture); err != nil {
		t.Fatalf("%s: %v; rerun capture.mts", streamingJSONCaptureFile, err)
	}
	if capture.PartialJSON != "0.1.7" {
		t.Fatalf("%s was captured with partial-json %s; 4b994d5e5's package-lock.json locks 0.1.7", streamingJSONCaptureFile, capture.PartialJSON)
	}
	compared, nonObject, loneSurrogate := 0, 0, 0
	check := func(input string, want *string) {
		t.Helper()
		if want == nil {
			nonObject++
			return
		}
		if hasLoneSurrogateEscape(*want) {
			loneSurrogate++
			return
		}
		compared++
		args, ordered := parseStreamingJSON(input)
		// The map holds the same members, and encoding/json writes it —
		// in its own key order — as JSON.stringify writes pi's value.
		mapJSON, err := json.Marshal(args)
		if err != nil {
			t.Errorf("parseStreamingJSON(%q)'s map does not marshal: %v", input, err)
		} else {
			var gotValue, wantValue any
			_ = json.Unmarshal(mapJSON, &gotValue)
			_ = json.Unmarshal([]byte(*want), &wantValue)
			if !reflect.DeepEqual(gotValue, wantValue) {
				t.Errorf("parseStreamingJSON(%q)'s map = %s, pi %s", input, mapJSON, *want)
			}
		}
		var got string
		if ordered == nil {
			got = "{}"
		} else if got, err = jstext.Stringify(ordered); err != nil {
			t.Fatalf("parseStreamingJSON(%q) has no JSON form: %v", input, err)
		}
		if got != *want {
			t.Errorf("parseStreamingJSON(%q) = %s, pi %s", input, got, *want)
		}
	}
	for _, doc := range capture.Documents {
		runes := []rune(doc.Doc)
		if len(runes) != len(doc.Results) {
			t.Fatalf("%q has %d code points and %d results; rerun capture.mts", doc.Doc, len(runes), len(doc.Results))
		}
		for n := range runes {
			check(string(runes[:n+1]), doc.Results[n])
		}
	}
	for _, c := range capture.Cases {
		check(c.Input, c.Result)
	}
	if compared < 5000 {
		t.Fatalf("compared %d results; the capture records over 5000", compared)
	}
	t.Logf("%d results compared; not compared: %d non-objects, %d holding half a surrogate pair", compared, nonObject, loneSurrogate)
}

// hasLoneSurrogateEscape reports whether text, JSON that JSON.stringify
// wrote, escapes a surrogate code unit, which it does only for half a pair.
func hasLoneSurrogateEscape(text string) bool {
	for i := 0; i+1 < len(text); i++ {
		if text[i] != '\\' {
			continue
		}
		if text[i+1] == 'u' && i+5 < len(text) {
			if unit, err := strconv.ParseUint(text[i+2:i+6], 16, 16); err == nil && unit >= 0xd800 && unit <= 0xdfff {
				return true
			}
		}
		i++ // the escaped character
	}
	return false
}

// D7g: unpaired surrogates are DELETED (pi replaces them with ""), not
// substituted with U+FFFD; properly paired surrogates are preserved.
func TestSanitizeSurrogatesDeletesUnpaired(t *testing.T) {
	loneHigh := "Text \xed\xa0\xbd here"       // U+D83D unpaired (WTF-8)
	loneLow := "lo\xed\xb9\x88w"               // U+DE48 unpaired (WTF-8)
	paired := "go \xed\xa0\xbd\xed\xb9\x88 go" // U+D83D U+DE48 = 🙈 as WTF-8 pair

	if got := sanitizeSurrogates(loneHigh); got != "Text  here" {
		t.Errorf("lone high surrogate: %q want %q", got, "Text  here")
	}
	if got := sanitizeSurrogates(loneLow); got != "low" {
		t.Errorf("lone low surrogate: %q want %q", got, "low")
	}
	if got := sanitizeSurrogates(paired); got != "go 🙈 go" {
		t.Errorf("paired surrogates must be preserved: %q", got)
	}
	// Valid UTF-8 (including real emoji and U+FFFD characters) passes through.
	for _, s := range []string{"Hello 🙈 World", "kept � char", ""} {
		if got := sanitizeSurrogates(s); got != s {
			t.Errorf("valid string mutated: %q -> %q", s, got)
		}
	}
}
