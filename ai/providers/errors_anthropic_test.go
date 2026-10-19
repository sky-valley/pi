package providers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sky-valley/pi/ai"
)

// TestAnthropicSDKErrorMessage locks the Anthropic SDK's APIError message shape.
// Both SDKs share a byte-identical makeMessage; they diverge in
// APIError.generate, which decides what makeMessage receives as `error`: openai
// passes errorResponse["error"], anthropic passes the WHOLE body. So a
// conformant anthropic body has no top-level `message` and falls through to
// JSON.stringify(body).
//
// Expectations are captured verbatim from the real @anthropic-ai/sdk 0.91.1
// shipped in @earendil-works/pi-coding-agent 0.82.0, by calling
// APIError.generate(429, JSON.parse(body), …).message.
func TestAnthropicSDKErrorMessage(t *testing.T) {
	cases := []struct {
		body string
		want string
	}{
		{
			`{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`,
			`429 {"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`,
		},
		// Key order is the parse order, not sorted — encoding/json would sort.
		{
			`{"zebra":1,"apple":2,"middle":{"z":1,"a":2}}`,
			`429 {"zebra":1,"apple":2,"middle":{"z":1,"a":2}}`,
		},
		// JS re-escapes: é becomes a literal é, and " stays escaped.
		{
			`{"type":"error","error":{"type":"x","message":"unicode é and \"q\""}}`,
			`429 {"type":"error","error":{"type":"x","message":"unicode é and \"q\""}}`,
		},
		// JS does NOT escape <, >, & or U+2028; encoding/json escapes all four.
		{
			"{\"a\":\"<script>&\\u2028\"}",
			"429 {\"a\":\"<script>& \"}",
		},
		// JS number canonicalization: 1.0->1, 1e2->100, 1e21->1e+21, -0->0.
		{
			`{"n":[1.0,1e2,0.5,1e-8,1e21,-0]}`,
			`429 {"n":[1,100,0.5,1e-8,1e+21,0]}`,
		},
		// A truthy top-level `message` short-circuits the stringify.
		{`{"message":"top level"}`, `429 top level`},
		// Non-JSON falls back to the raw text.
		{`not json at all`, `429 not json at all`},
		{``, `429 status code (no body)`},
		{
			`{"ok":true,"nil":null,"arr":[1,"two",false]}`,
			`429 {"ok":true,"nil":null,"arr":[1,"two",false]}`,
		},
	}
	for _, c := range cases {
		t.Run(c.body, func(t *testing.T) {
			if got := anthropicSDKErrorMessage(429, []byte(c.body)); got != c.want {
				t.Errorf("anthropicSDKErrorMessage\n got: %q\nwant: %q", got, c.want)
			}
		})
	}
}

// TestAnthropicAndOpenAIMessagesDiverge pins the reason this renderer exists at
// all: for the same conformant anthropic error body the two SDKs produce
// different messages, so a single shared renderer cannot be faithful to both.
func TestAnthropicAndOpenAIMessagesDiverge(t *testing.T) {
	body := []byte(`{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`)
	anth := anthropicSDKErrorMessage(429, body)
	oai := openaiSDKErrorMessage(429, body)
	if anth == oai {
		t.Fatalf("expected the two SDK shapes to differ, both gave %q", anth)
	}
	if oai != "429 slow down" {
		t.Errorf("openai unwraps error.message, got %q", oai)
	}
	if !strings.HasPrefix(anth, `429 {"type":"error"`) {
		t.Errorf("anthropic stringifies the whole body, got %q", anth)
	}
}

// TestJSStringifyPreservesKeyOrder is the property encoding/json cannot provide.
func TestJSStringifyPreservesKeyOrder(t *testing.T) {
	raw := []byte(`{"z":1,"m":{"b":2,"a":3},"a":[{"y":1,"x":2}]}`)
	want := `{"z":1,"m":{"b":2,"a":3},"a":[{"y":1,"x":2}]}`
	got, ok := jsStringify(raw)
	if !ok {
		t.Fatal("jsStringify rejected well-formed JSON")
	}
	if got != want {
		t.Errorf("jsStringify\n got: %s\nwant: %s", got, want)
	}
}

// TestJSStringifyReproducesObjectOwnPropertyOrder pins the two things a parsed
// JS object does to its keys that a streaming re-emit does not: a repeated key
// is one property (first position, last value) and array-index keys are listed
// first, ascending. Want values captured from node via
// `JSON.stringify(JSON.parse(input))`.
func TestJSStringifyReproducesObjectOwnPropertyOrder(t *testing.T) {
	cases := []struct{ in, want string }{
		{`{"a":1,"b":2,"a":3}`, `{"a":3,"b":2}`},
		{`{"a":{"x":1},"a":{"y":2}}`, `{"a":{"y":2}}`},
		{`{"type":"x","0":"y"}`, `{"0":"y","type":"x"}`},
		{`{"2":"c","10":"j","1":"b","z":1}`, `{"1":"b","2":"c","10":"j","z":1}`},
		// Only the canonical spelling of a uint32 below 2^32-1 is an index.
		{`{"01":"a","-1":"b","1.0":"c","4294967295":"d","4294967294":"e"}`,
			`{"4294967294":"e","01":"a","-1":"b","1.0":"c","4294967295":"d"}`},
		{`{"":"empty","0":"zero"}`, `{"0":"zero","":"empty"}`},
		{`{"a":{"1":1,"b":2,"0":0}}`, `{"a":{"0":0,"1":1,"b":2}}`},
	}
	for _, c := range cases {
		got, ok := jsStringify([]byte(c.in))
		if !ok {
			t.Errorf("jsStringify(%s) rejected well-formed JSON", c.in)
			continue
		}
		if got != c.want {
			t.Errorf("jsStringify(%s)\n got: %s\nwant: %s", c.in, got, c.want)
		}
	}
}

func TestJSStringifyRejectsMalformed(t *testing.T) {
	for _, raw := range []string{`{"a":1`, `{"a":1} trailing`, ``, `{,}`} {
		if _, ok := jsStringify([]byte(raw)); ok {
			t.Errorf("jsStringify(%q) should have failed", raw)
		}
	}
}

// TestJSNumberExponentialThreshold pins JS's fixed-vs-exponential boundary,
// which is 1e-6 and NOT 1e-7: String(1e-6) is "0.000001" but String(9.9e-7) is
// "9.9e-7". Values captured from node.
func TestJSNumberExponentialThreshold(t *testing.T) {
	cases := []struct{ in, want string }{
		{"0.00001", "0.00001"},
		{"0.000001", "0.000001"}, // 1e-6 — last fixed value
		{"0.0000015", "0.0000015"},
		{"9.9e-7", "9.9e-7"}, // just below 1e-6 — exponential
		{"5e-7", "5e-7"},
		{"1e-7", "1e-7"},
		{"1e-8", "1e-8"},
		{"1e21", "1e+21"},
		{"1e20", "100000000000000000000"},
		{"-0", "0"},
		{"1.0", "1"},
		{"1e2", "100"},
		// Out of float64 range is not malformed: JSON.parse saturates to
		// ±Infinity or a signed zero, and JSON.stringify writes Infinity as
		// `null`. A literal that is not a number at all still passes through.
		{"1e400", "null"},
		{"-1e400", "null"},
		{"1e-400", "0"},
		{"-1e-400", "0"},
		{"12345678901234567890", "12345678901234567000"},
		{"not-a-number", "not-a-number"},
	}
	for _, c := range cases {
		if got := jsNumber(c.in); got != c.want {
			t.Errorf("jsNumber(%s) = %s, want %s", c.in, got, c.want)
		}
	}
}

// TestAnthropicHTTPErrorMatchesPi drives every captured body through the
// adapter and compares the terminal ErrorMessage with pi's byte for byte: pi
// surfaces the Anthropic SDK's APIError message as is (`${status} ${msg}`, the
// whole body stringified when it has no top-level message, never capped), over
// the text fetch decoded the body to.
func TestAnthropicHTTPErrorMatchesPi(t *testing.T) {
	rows, byteRows, _ := loadHTTPErrorCaptureWithBytes(t)
	req := ai.NormalizeContext(ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}})
	for _, row := range append(rows, byteRows...) {
		t.Run(row.Name, func(t *testing.T) {
			want, ok := row.ErrorMessage["anthropic-messages"]
			if !ok {
				t.Fatalf("oracle row %q has no anthropic-messages column; re-capture it", row.Name)
			}
			body := []byte(row.Body)
			if row.BodyBase64 != nil {
				body = row.BodyBase64
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(row.Status)
				w.Write(body)
			}))
			defer server.Close()
			m := anthropicUAModel()
			m.BaseURL = server.URL
			opts := &AnthropicOptions{StreamOptions: ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "sk"}}}
			final := StreamAnthropic(t.Context(), m, req, opts).Result()
			if final.StopReason != ai.StopError || final.ErrorMessage != want {
				t.Fatalf("stream = %s %q, want pi's %q", final.StopReason, final.ErrorMessage, want)
			}
		})
	}
}
