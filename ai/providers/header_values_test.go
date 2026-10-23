package providers

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sky-valley/pi/ai"
)

// Every request pi makes builds its headers in a fetch Headers object — the
// pi-messages adapter's fetch call, and the Headers each vendor SDK (openai,
// @anthropic-ai/sdk, @google/genai) assembles before it calls fetch — and
// Headers.append/set normalize a value by stripping leading and trailing HTTP
// whitespace (space, tab, CR, LF). net/http does neither: it refuses a value
// holding a CR or LF, failing the whole request, and its HTTP/2 encoder sends a
// value's leading and trailing spaces and tabs verbatim. Only its HTTP/1.1
// writer trims those, so the space and tab rows here run over HTTP/2, the
// protocol every TLS provider endpoint negotiates with Go.
//
// Every want was measured on pi's wire: upstream 8676a0dcd's src
// (packages/ai/src/api/*.ts) run under node v26.4.0 with openai 6.40.0,
// @anthropic-ai/sdk 0.124.0 and @google/genai 2.21.0 — the versions
// package-lock.json locks at that sha — against a raw socket that recorded the
// header lines as sent. openai 7.19.0 (pi-ai's since ab30693d6, measured at
// 2b0a123de) builds its headers the same way but for header names, which its
// buildHeaders checks against the token grammar itself (invalidNameError).

// wireAdapter drives one adapter against baseURL.
type wireAdapter struct {
	name string
	sse  string
	// auth is the header the api key reaches, and bearer whether the adapter
	// prefixes it (the prefix is joined before the value is normalized, so a
	// key's leading space survives inside "Bearer  k").
	auth   string
	bearer bool
	// http1 marks google: pi refuses a custom fetch there, so the port refuses
	// a custom client, and the default one cannot reach a self-signed HTTP/2
	// test server. Its requests go over HTTP/1.1, where only the CR and LF rows
	// are observable.
	http1 bool
	// run sends one request; client is nil for an http1 adapter.
	run func(baseURL string, client *http.Client, opts ai.StreamOptions) *ai.AssistantMessage
}

func wireAdapters() []wireAdapter {
	req := func() ai.TranscriptContext {
		return ai.NormalizeContext(ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}})
	}
	model := func(id string, api ai.Api, provider ai.ProviderId, baseURL string) *ai.Model {
		return &ai.Model{ID: id, Api: api, Provider: provider, BaseURL: baseURL, Input: []string{"text"}, MaxTokens: 4096}
	}
	return []wireAdapter{
		{name: "pi-messages", sse: piMessagesSSE(
			`{"type":"start"}`,
			`{"type":"done","reason":"stop","usage":`+piMessagesUsageJSON+`,"responseId":"resp_1"}`,
		), auth: "authorization", bearer: true, run: func(baseURL string, client *http.Client, opts ai.StreamOptions) *ai.AssistantMessage {
			opts.HTTPClient = client
			return StreamPiMessages(context.Background(), piMessagesTestModel(baseURL+"/v1"), req(),
				&PiMessagesOptions{StreamOptions: opts}).Result()
		}},
		{name: "google-generative-ai", sse: googleSSE, auth: "x-goog-api-key", http1: true, run: func(baseURL string, _ *http.Client, opts ai.StreamOptions) *ai.AssistantMessage {
			return StreamGoogle(context.Background(), model("gemini-2.5-flash", ai.APIGoogleGenerativeAI, "google", baseURL), req(),
				&GoogleOptions{StreamOptions: opts}).Result()
		}},
		{name: "openai-completions", sse: completionsDoneSSE, auth: "authorization", bearer: true, run: func(baseURL string, client *http.Client, opts ai.StreamOptions) *ai.AssistantMessage {
			opts.HTTPClient = client
			return StreamOpenAICompletions(context.Background(), model("gpt-test", ai.APIOpenAICompletions, "openai", baseURL), req(),
				&OpenAIOptions{StreamOptions: opts}).Result()
		}},
		{name: "openai-responses", sse: responsesSSE, auth: "authorization", bearer: true, run: func(baseURL string, client *http.Client, opts ai.StreamOptions) *ai.AssistantMessage {
			opts.HTTPClient = client
			return StreamOpenAIResponses(context.Background(), model("gpt-test", ai.APIOpenAIResponses, "openai", baseURL), req(),
				&OpenAIResponsesOptions{StreamOptions: opts}).Result()
		}},
		{name: "anthropic-messages", sse: anthropicSSE, auth: "x-api-key", run: func(baseURL string, client *http.Client, opts ai.StreamOptions) *ai.AssistantMessage {
			opts.HTTPClient = client
			return StreamAnthropic(context.Background(), model("claude-test", ai.APIAnthropicMessages, "anthropic", baseURL), req(),
				&AnthropicOptions{StreamOptions: opts}).Result()
		}},
	}
}

// captureWireHeaders runs one request through adapter — over HTTP/2 unless the
// adapter is http1 — and returns the headers that reached the server.
func captureWireHeaders(t *testing.T, adapter wireAdapter, opts ai.StreamOptions) http.Header {
	t.Helper()
	got, final := runWire(t, adapter, opts)
	if final.StopReason == ai.StopError {
		t.Fatalf("stream failed: %s", final.ErrorMessage)
	}
	return got
}

// runWire runs one request through adapter — over HTTP/2 unless the adapter is
// http1 — and returns the headers that reached the server, nil when no request
// did, and the stream's final message.
func runWire(t *testing.T, adapter wireAdapter, opts ai.StreamOptions) (http.Header, *ai.AssistantMessage) {
	t.Helper()
	var got http.Header
	var proto string
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, proto = r.Header.Clone(), r.Proto
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, adapter.sse)
	}))
	var client *http.Client
	wantProto := "HTTP/1.1"
	if adapter.http1 {
		server.Start()
	} else {
		server.EnableHTTP2 = true
		server.StartTLS()
		client = server.Client()
		wantProto = "HTTP/2.0"
	}
	defer server.Close()
	final := adapter.run(server.URL, client, opts)
	if got != nil && proto != wantProto {
		t.Fatalf("the request went over %s, want %s", proto, wantProto)
	}
	return got, final
}

func wantOneValue(t *testing.T, h http.Header, name, want string) {
	t.Helper()
	if got := h.Values(name); len(got) != 1 || got[0] != want {
		t.Fatalf("%s = %q, want exactly [%q]", name, got, want)
	}
}

// A consumer or model header value reaches the wire trimmed, through the
// record path (pi-messages, google) and the SDK defaultHeaders path (openai,
// anthropic) alike.
func TestHeaderValuesAreTrimmedLikeFetch(t *testing.T) {
	for _, adapter := range wireAdapters() {
		for _, tc := range []struct {
			name  string
			value string
			want  string
			// edges marks a row whose value differs from want only by spaces
			// and tabs, which HTTP/1.1 trims on its own.
			edges bool
		}{
			// net/http fails the request outright on the CR or LF.
			{"trailing LF", "tok\n", "tok", false},
			{"trailing CRLF", "tok\r\n", "tok", false},
			{"spaces and tabs", " \ty\t ", "y", true},
			{"all whitespace", "  ", "", true},
			{"inner whitespace kept", "a  b", "a  b", false},
		} {
			if tc.edges && adapter.http1 {
				continue
			}
			t.Run(adapter.name+"/"+tc.name, func(t *testing.T) {
				h := captureWireHeaders(t, adapter, ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{
					APIKey: "test-key", Headers: ai.ProviderHeaders{"X-A": strPtr(tc.value)},
				}})
				wantOneValue(t, h, "x-a", tc.want)
			})
		}
	}
}

// The api key is normalized with the header it lands in: after the "Bearer "
// prefix where the adapter adds one, so only the ends of the joined value go.
// @google/genai appends x-goog-api-key to its Headers itself, which is why
// google's key is trimmed although no record carries it.
func TestAPIKeyHeaderIsTrimmedLikeFetch(t *testing.T) {
	for _, adapter := range wireAdapters() {
		for _, tc := range []struct {
			name, key, want, wantBearer string
			edges                       bool
		}{
			{"trailing LF", "k\n", "k", "Bearer k", false},
			{"surrounding spaces", " k ", "k", "Bearer  k", true},
		} {
			if tc.edges && adapter.http1 {
				continue
			}
			t.Run(adapter.name+"/"+tc.name, func(t *testing.T) {
				h := captureWireHeaders(t, adapter, ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: tc.key}})
				want := tc.want
				if adapter.bearer {
					want = tc.wantBearer
				}
				wantOneValue(t, h, adapter.auth, want)
			})
		}
	}
}

// The anthropic SDK re-emits the params' `betas` as the per-request
// anthropic-beta header (betas.toString()), and its Headers trims the joined
// value's ends. Only an onPayload hook can put whitespace there: pi's own
// feature list is trimmed when it is parsed.
func TestAnthropicBetaHeaderIsTrimmedLikeFetch(t *testing.T) {
	var anthropic wireAdapter
	for _, adapter := range wireAdapters() {
		if adapter.name == "anthropic-messages" {
			anthropic = adapter
		}
	}
	for _, tc := range []struct {
		name  string
		betas any
		want  string
	}{
		{"string with trailing LF", " x\n", "x"},
		{"list with edge spaces", []any{" a", "b "}, "a,b"},
		{"list with a trailing tab", []any{"a\t"}, "a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := captureWireHeaders(t, anthropic, ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{
				APIKey: "test-key",
				OnPayload: func(payload any, _ *ai.Model) (any, error) {
					body := payload.(map[string]any)
					body["betas"] = tc.betas
					return body, nil
				},
			}})
			wantOneValue(t, h, "anthropic-beta", tc.want)
		})
	}
}

// A record entry spelled differently from an adapter literal is joined onto it
// (see applyAsRecord), and the value it contributes is normalized first, so an
// LF in it no longer fails the request. google is the one adapter left with
// literals outside its record (genai's Content-Type); pi-messages' fixed
// headers joined the record in upstream adc5fa9f8 (see
// TestPiMessagesRecordFoldsFixedHeaders).
func TestJoinedHeaderValueIsTrimmedLikeFetch(t *testing.T) {
	adapters := map[string]wireAdapter{}
	for _, adapter := range wireAdapters() {
		adapters[adapter.name] = adapter
	}
	for _, tc := range []struct {
		name    string
		adapter string
		headers ai.ProviderHeaders
		header  string
		want    string
		edges   bool
	}{
		{"LF in the joined value", "google-generative-ai", ai.ProviderHeaders{"content-type": strPtr(" text/plain\n")}, "content-type", "application/json, text/plain", false},
	} {
		adapter := adapters[tc.adapter]
		if tc.edges && adapter.http1 {
			t.Fatalf("%s/%s: an edges row needs an HTTP/2 adapter", tc.adapter, tc.name)
		}
		t.Run(tc.adapter+"/"+tc.name, func(t *testing.T) {
			h := captureWireHeaders(t, adapter, ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{
				APIKey: "test-key", Headers: tc.headers,
			}})
			wantOneValue(t, h, tc.header, tc.want)
		})
	}
}

// A fetch Headers object converts every header name and value it is given to a
// WebIDL ByteString before it normalizes anything (see byteString). The wants
// below were measured on pi's wire the same way as the rows above: upstream
// 8676a0dcd's src under node v26.4.0, with the package-lock versions of openai,
// @anthropic-ai/sdk and @google/genai, against a raw socket.

// byteStringError is the TypeError text node's fetch throws for a string it
// cannot convert. Every adapter reports it as the stream's error message.
func byteStringError(index, value int) string {
	return fmt.Sprintf("Cannot convert argument to a ByteString because the character at index %d has a value of %d which is greater than 255.", index, value)
}

// wantNotSent asserts that the stream failed with want before any request
// reached the server.
func wantNotSent(t *testing.T, h http.Header, final *ai.AssistantMessage, want string) {
	t.Helper()
	if h != nil {
		t.Fatalf("a request reached the server with headers %v, want none sent", h)
	}
	if final.StopReason != ai.StopError || final.ErrorMessage != want {
		t.Fatalf("stream = %s %q, want error %q", final.StopReason, final.ErrorMessage, want)
	}
}

// wantSent asserts that the stream succeeded and that name reached the wire
// as exactly the bytes want.
func wantSent(t *testing.T, h http.Header, final *ai.AssistantMessage, name, want string) {
	t.Helper()
	if final.StopReason == ai.StopError {
		t.Fatalf("stream failed: %s", final.ErrorMessage)
	}
	wantOneValue(t, h, name, want)
}

const cjk = 0x65e5 // 日, one UTF-16 code unit above 0xFF

// A value's U+0080..U+00FF characters go out as one Latin-1 byte each, and a
// character above U+00FF fails the request before it is sent, on the record
// paths (pi-messages, google) and the SDK defaultHeaders path (openai,
// anthropic) alike. The index and value count UTF-16 code units, so an astral
// character reports its high surrogate, and they are counted before the value
// is normalized.
func TestHeaderValuesAreByteStringsLikeFetch(t *testing.T) {
	for _, adapter := range wireAdapters() {
		for _, tc := range []struct {
			name, value string
			wire        string // what reaches the wire when the request is sent
			err         string // the stream's error when it is not
			edges       bool
		}{
			{name: "latin-1", value: "caf" + string(rune(0xe9)), wire: "caf\xe9"},
			{name: "C1 control", value: "x" + string(rune(0x80)) + "y", wire: "x\x80y"},
			{name: "latin-1 inside trimmed edges", value: " \t" + string(rune(0xe9)) + "\t ", wire: "\xe9", edges: true},
			{name: "above U+00FF", value: string(rune(cjk)), err: byteStringError(0, cjk)},
			{name: "astral", value: "ab" + string(rune(0x1f600)), err: byteStringError(2, 0xd83d)},
			{name: "index counted before trimming", value: " " + string(rune(cjk)), err: byteStringError(1, cjk)},
			{name: "U+0100 after U+00FF", value: string([]rune{0xff, 0x100}), err: byteStringError(1, 0x100)},
		} {
			if tc.edges && adapter.http1 {
				continue
			}
			t.Run(adapter.name+"/"+tc.name, func(t *testing.T) {
				h, final := runWire(t, adapter, ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{
					APIKey: "test-key", Headers: ai.ProviderHeaders{"X-A": strPtr(tc.value)},
				}})
				if tc.err != "" {
					wantNotSent(t, h, final, tc.err)
					return
				}
				wantSent(t, h, final, "x-a", tc.wire)
			})
		}
	}
}

// A header name is converted as a value is. A marker's name is converted only
// where the marker reaches a Headers: the SDKs delete the name from theirs,
// while a record drops the marker before any Headers sees it. The openai SDK
// refuses such a name itself before any Headers converts it (see
// invalidNameError).
func TestHeaderNamesAreByteStringsLikeFetch(t *testing.T) {
	name := "X-" + string(rune(cjk))
	want := func(adapter wireAdapter) string {
		if strings.HasPrefix(adapter.name, "openai-") {
			return invalidNameError(adapter.name, name)
		}
		return byteStringError(2, cjk)
	}
	for _, adapter := range wireAdapters() {
		t.Run(adapter.name+"/value", func(t *testing.T) {
			h, final := runWire(t, adapter, ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{
				APIKey: "test-key", Headers: ai.ProviderHeaders{name: strPtr("v")},
			}})
			wantNotSent(t, h, final, want(adapter))
		})
		t.Run(adapter.name+"/marker", func(t *testing.T) {
			h, final := runWire(t, adapter, ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{
				APIKey: "test-key", Headers: ai.ProviderHeaders{name: nil},
			}})
			if adapter.name == "pi-messages" || adapter.name == "google-generative-ai" {
				if final.StopReason == ai.StopError || h == nil {
					t.Fatalf("stream = %s %q, want the request sent", final.StopReason, final.ErrorMessage)
				}
				return
			}
			wantNotSent(t, h, final, want(adapter))
		})
	}
}

// The api key is converted with the header it lands in: after the "Bearer "
// prefix where the adapter adds one, so the index counts the prefix.
func TestAPIKeyHeaderIsAByteStringLikeFetch(t *testing.T) {
	for _, adapter := range wireAdapters() {
		t.Run(adapter.name+"/latin-1", func(t *testing.T) {
			h, final := runWire(t, adapter, ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{
				APIKey: "k" + string(rune(0xe9)),
			}})
			want := "k\xe9"
			if adapter.bearer {
				want = "Bearer k\xe9"
			}
			wantSent(t, h, final, adapter.auth, want)
		})
		t.Run(adapter.name+"/above U+00FF", func(t *testing.T) {
			h, final := runWire(t, adapter, ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{
				APIKey: "k" + string(rune(cjk)),
			}})
			want := byteStringError(1, cjk)
			if adapter.bearer {
				want = byteStringError(8, cjk)
			}
			wantNotSent(t, h, final, want)
		})
	}
}

// With two values that cannot be converted, the one pi's Headers sees first
// decides the error. The SDKs append their auth header before any
// `defaultHeaders` entry, and pi-messages' Headers is built from
// `{authorization, ...record}`, so the key fails first there. genai appends its
// key after the record, so on google the consumer header fails first.
func TestHeaderConversionFailsInPisOrder(t *testing.T) {
	for _, adapter := range wireAdapters() {
		want := byteStringError(2, cjk) // the key "kk日" in x-api-key
		switch {
		case adapter.name == "google-generative-ai":
			want = byteStringError(0, cjk) // the X-A value
		case adapter.bearer:
			want = byteStringError(9, cjk) // the key in "Bearer kk日"
		}
		t.Run(adapter.name, func(t *testing.T) {
			h, final := runWire(t, adapter, ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{
				APIKey: "kk" + string(rune(cjk)), Headers: ai.ProviderHeaders{"X-A": strPtr(string(rune(cjk)))},
			}})
			wantNotSent(t, h, final, want)
		})
	}
}

// Whether the key is converted at all depends on whether the object pi hands a
// Headers still holds it. The openai and anthropic SDKs build their auth header
// from their own options and append it before `defaultHeaders`, so a key they
// cannot convert fails the request whatever the headers say (sdkHeaders.auth).
// pi-messages' key is an entry of its record (upstream adc5fa9f8): an
// override replaces it unconverted, and a marker deletes it, so the request
// goes out with no authorization at all. genai appends its key only when the
// record carries none.
func TestAPIKeyOverrideDecidesWhetherTheKeyIsConverted(t *testing.T) {
	key := "k" + string(rune(cjk))
	override := ai.ProviderHeaders{"authorization": strPtr("ok"), "x-api-key": strPtr("ok"), "x-goog-api-key": strPtr("ok")}
	marker := ai.ProviderHeaders{"authorization": nil, "x-api-key": nil, "x-goog-api-key": nil}
	for _, adapter := range wireAdapters() {
		keyErr := byteStringError(1, cjk)
		if adapter.bearer {
			keyErr = byteStringError(8, cjk)
		}
		t.Run(adapter.name+"/exact override", func(t *testing.T) {
			h, final := runWire(t, adapter, ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{
				APIKey: key, Headers: override,
			}})
			if adapter.name == "pi-messages" || adapter.name == "google-generative-ai" {
				wantSent(t, h, final, adapter.auth, "ok")
				return
			}
			wantNotSent(t, h, final, keyErr)
		})
		t.Run(adapter.name+"/marker", func(t *testing.T) {
			h, final := runWire(t, adapter, ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{
				APIKey: key, Headers: marker,
			}})
			if adapter.name == "pi-messages" {
				if final.StopReason == ai.StopError && h == nil {
					t.Fatalf("stream failed before sending: %s", final.ErrorMessage)
				}
				if values, ok := h[http.CanonicalHeaderKey(adapter.auth)]; ok {
					t.Fatalf("%s = %q, want it deleted by the marker", adapter.auth, values)
				}
				return
			}
			wantNotSent(t, h, final, keyErr)
		})
	}
}

// A record entry joined onto a literal is converted on its own, before the
// join: its index counts from its own first character, and its Latin-1 bytes
// follow the literal's. pi-messages' entry replaces its fixed header now
// (upstream adc5fa9f8), so its bytes stand alone.
func TestJoinedHeaderValueIsAByteStringLikeFetch(t *testing.T) {
	adapters := map[string]wireAdapter{}
	for _, adapter := range wireAdapters() {
		adapters[adapter.name] = adapter
	}
	for _, tc := range []struct {
		name, adapter string
		headers       ai.ProviderHeaders
		header        string
		wire, err     string
	}{
		{"latin-1", "pi-messages", ai.ProviderHeaders{"Authorization": strPtr(string(rune(0xe9)))}, "authorization", "\xe9", ""},
		{"above U+00FF", "pi-messages", ai.ProviderHeaders{"Authorization": strPtr(" " + string(rune(cjk)))}, "authorization", "", byteStringError(1, cjk)},
		{"latin-1", "google-generative-ai", ai.ProviderHeaders{"content-type": strPtr(string(rune(0xe9)))}, "content-type", "application/json, \xe9", ""},
		{"above U+00FF", "google-generative-ai", ai.ProviderHeaders{"content-type": strPtr(string(rune(cjk)))}, "content-type", "", byteStringError(0, cjk)},
	} {
		t.Run(tc.adapter+"/"+tc.name, func(t *testing.T) {
			h, final := runWire(t, adapters[tc.adapter], ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{
				APIKey: "test-key", Headers: tc.headers,
			}})
			if tc.err != "" {
				wantNotSent(t, h, final, tc.err)
				return
			}
			wantSent(t, h, final, tc.header, tc.wire)
		})
	}
}

// The anthropic SDK converts the anthropic-beta value it re-emits from the
// params' `betas` (betas.toString()) like any other.
func TestAnthropicBetaHeaderIsAByteStringLikeFetch(t *testing.T) {
	var anthropic wireAdapter
	for _, adapter := range wireAdapters() {
		if adapter.name == "anthropic-messages" {
			anthropic = adapter
		}
	}
	for _, tc := range []struct {
		name      string
		betas     any
		wire, err string
	}{
		{"latin-1", []any{"a" + string(rune(0xe9))}, "a\xe9", ""},
		{"above U+00FF", []any{string(rune(cjk))}, "", byteStringError(0, cjk)},
		{"index across the joined list", []any{"x", " " + string(rune(cjk))}, "", byteStringError(3, cjk)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, final := runWire(t, anthropic, ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{
				APIKey: "test-key",
				OnPayload: func(payload any, _ *ai.Model) (any, error) {
					body := payload.(map[string]any)
					body["betas"] = tc.betas
					return body, nil
				},
			}})
			if tc.err != "" {
				wantNotSent(t, h, final, tc.err)
				return
			}
			wantSent(t, h, final, "anthropic-beta", tc.wire)
		})
	}
}

// invalidValueError is the TypeError undici's Headers throws for a value that
// still holds a NUL, CR or LF once normalized; it quotes the normalized value.
func invalidValueError(value string) string {
	return fmt.Sprintf("Headers.append: \"%s\" is an invalid header value.", value)
}

// invalidNameError is the TypeError a name that is not an HTTP token fails
// adapter's request with. openai 7.19.0's buildHeaders checks every name
// against the token grammar itself, before anything converts it, so a name
// with a character above U+00FF fails there too. The other adapters leave it
// to undici's Headers, whose TypeError names the method that refused the
// name: Headers.delete on anthropic's SDK path, whose buildHeaders deletes a
// name before it first appends one, and Headers.append on the record paths.
func invalidNameError(adapter, name string) string {
	switch adapter {
	case "openai-completions", "openai-responses":
		return fmt.Sprintf("Header name must be a valid HTTP token [\"%s\"]", name)
	case "anthropic-messages":
		return fmt.Sprintf("Headers.delete: \"%s\" is an invalid header name.", name)
	}
	return fmt.Sprintf("Headers.append: \"%s\" is an invalid header name.", name)
}

// sdkPath reports whether adapter hands its headers to a vendor SDK as
// `defaultHeaders`.
func sdkPath(adapter wireAdapter) bool {
	return adapter.name != "pi-messages" && adapter.name != "google-generative-ai"
}

// After it normalizes a value, a Headers object refuses a value that still
// holds a NUL, CR or LF, and a name that is not an HTTP token. The request fails
// at once, before anything is sent, where net/http failed the round trip with
// its own text (and, with MaxRetries set, the retry loop tried it again).
func TestHeaderValuesAreRefusedLikeFetch(t *testing.T) {
	for _, adapter := range wireAdapters() {
		for _, tc := range []struct{ name, value, err string }{
			{"LF inside", "a\nb", invalidValueError("a\nb")},
			{"CR inside", "a\rb", invalidValueError("a\rb")},
			{"NUL inside", "a\x00b", invalidValueError("a\x00b")},
			{"quoted normalized", " a\nb\t", invalidValueError("a\nb")},
			{"quoted as text", string(rune(0xe9)) + "\nb", invalidValueError(string(rune(0xe9)) + "\nb")},
		} {
			t.Run(adapter.name+"/"+tc.name, func(t *testing.T) {
				h, final := runWire(t, adapter, ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{
					APIKey: "test-key", Headers: ai.ProviderHeaders{"X-A": strPtr(tc.value)},
				}})
				wantNotSent(t, h, final, tc.err)
			})
		}
	}
}

func TestHeaderNamesAreRefusedLikeFetch(t *testing.T) {
	aboveLatin1 := "X-" + string(rune(0x100))
	// wantAboveLatin1 is what a name holding U+0100 fails with: openai's own
	// check refuses it before undici converts it (see invalidNameError).
	wantAboveLatin1 := func(adapter wireAdapter) string {
		if strings.HasPrefix(adapter.name, "openai-") {
			return invalidNameError(adapter.name, aboveLatin1)
		}
		return byteStringError(2, 0x100)
	}
	for _, adapter := range wireAdapters() {
		for _, name := range []string{"", "X A", "X-" + string(rune(0xe9)), aboveLatin1} {
			t.Run(adapter.name+"/"+name, func(t *testing.T) {
				h, final := runWire(t, adapter, ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{
					APIKey: "test-key", Headers: ai.ProviderHeaders{name: strPtr("v")},
				}})
				want := invalidNameError(adapter.name, name)
				if name == aboveLatin1 {
					want = wantAboveLatin1(adapter)
				}
				wantNotSent(t, h, final, want)
			})
		}
		// A marker's name reaches a Headers only on the SDK path.
		for _, name := range []string{"X-" + string(rune(0xe9)), aboveLatin1} {
			t.Run(adapter.name+"/marker "+name, func(t *testing.T) {
				h, final := runWire(t, adapter, ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{
					APIKey: "test-key", Headers: ai.ProviderHeaders{name: nil},
				}})
				if !sdkPath(adapter) {
					if final.StopReason == ai.StopError || h == nil {
						t.Fatalf("stream = %s %q, want the request sent", final.StopReason, final.ErrorMessage)
					}
					return
				}
				want := invalidNameError(adapter.name, name)
				if name == aboveLatin1 {
					want = wantAboveLatin1(adapter)
				}
				wantNotSent(t, h, final, want)
			})
		}
	}
}

// The api key and the anthropic-beta value are refused like any other value,
// the key with the "Bearer " prefix it is joined to.
func TestAPIKeyAndBetaValuesAreRefusedLikeFetch(t *testing.T) {
	for _, adapter := range wireAdapters() {
		t.Run(adapter.name+"/key", func(t *testing.T) {
			h, final := runWire(t, adapter, ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "k\nk"}})
			want := invalidValueError("k\nk")
			if adapter.bearer {
				want = invalidValueError("Bearer k\nk")
			}
			wantNotSent(t, h, final, want)
		})
		if adapter.name != "anthropic-messages" {
			continue
		}
		t.Run(adapter.name+"/anthropic-beta", func(t *testing.T) {
			h, final := runWire(t, adapter, ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{
				APIKey: "test-key",
				OnPayload: func(payload any, _ *ai.Model) (any, error) {
					body := payload.(map[string]any)
					body["betas"] = "a\nb"
					return body, nil
				},
			}})
			wantNotSent(t, h, final, invalidValueError("a\nb"))
		})
	}
}

// A contribution joined onto a literal is refused on its own.
func TestJoinedHeaderValueIsRefusedLikeFetch(t *testing.T) {
	for _, adapter := range wireAdapters() {
		if adapter.name != "pi-messages" {
			continue
		}
		h, final := runWire(t, adapter, ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{
			APIKey: "test-key", Headers: ai.ProviderHeaders{"Authorization": strPtr("a\nb")},
		}})
		wantNotSent(t, h, final, invalidValueError("a\nb"))
	}
}

// Which failure wins follows the order pi's Headers meets them in. The SDKs
// check one entry's name and value before the next entry's, and genai does too.
// fetch converts pi-messages' whole init before it checks any entry, so there
// a later entry that does not convert beats an earlier one that is refused.
func TestHeaderRefusalsFollowPisOrder(t *testing.T) {
	for _, adapter := range wireAdapters() {
		for _, tc := range []struct {
			name    string
			headers ai.ProviderHeaders
			want    string
		}{
			{"refused value, then a value that does not convert",
				ai.ProviderHeaders{"X-B": strPtr("a\nb"), "X-C": strPtr(string(rune(cjk)))},
				invalidValueError("a\nb")},
			{"refused name, then a value that does not convert",
				ai.ProviderHeaders{"X A": strPtr("v"), "X-C": strPtr(string(rune(cjk)))},
				invalidNameError(adapter.name, "X A")},
		} {
			if adapter.name == "pi-messages" {
				tc.want = byteStringError(0, cjk)
			}
			t.Run(adapter.name+"/"+tc.name, func(t *testing.T) {
				h, final := runWire(t, adapter, ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{
					APIKey: "test-key", Headers: tc.headers,
				}})
				wantNotSent(t, h, final, tc.want)
			})
		}
	}
}

// countingDoer counts the requests an adapter hands its transport, and fails
// each one.
type countingDoer struct{ calls int }

func (d *countingDoer) Do(*http.Request) (*http.Response, error) {
	d.calls++
	return nil, fmt.Errorf("transport call %d", d.calls)
}

// A refused header never reaches the transport, so there is nothing to retry:
// pi's SDKs throw from buildHeaders, outside their retry loop. Measured with a
// counting fetch and maxRetries 2 on openai-completions, openai-responses and
// anthropic-messages: zero fetch calls. google takes no custom transport.
func TestRefusedHeaderNeverReachesTheTransport(t *testing.T) {
	req := ai.NormalizeContext(ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}})
	const baseURL = "http://127.0.0.1:1"
	model := func(api ai.Api, provider ai.ProviderId) *ai.Model {
		return &ai.Model{ID: "m", Api: api, Provider: provider, BaseURL: baseURL, Input: []string{"text"}, MaxTokens: 4096}
	}
	for _, tc := range []struct {
		name string
		run  func(ai.StreamOptions) *ai.AssistantMessage
	}{
		{"pi-messages", func(o ai.StreamOptions) *ai.AssistantMessage {
			return StreamPiMessages(context.Background(), piMessagesTestModel(baseURL+"/v1"), req, &PiMessagesOptions{StreamOptions: o}).Result()
		}},
		{"openai-completions", func(o ai.StreamOptions) *ai.AssistantMessage {
			return StreamOpenAICompletions(context.Background(), model(ai.APIOpenAICompletions, "openai"), req, &OpenAIOptions{StreamOptions: o}).Result()
		}},
		{"openai-responses", func(o ai.StreamOptions) *ai.AssistantMessage {
			return StreamOpenAIResponses(context.Background(), model(ai.APIOpenAIResponses, "openai"), req, &OpenAIResponsesOptions{StreamOptions: o}).Result()
		}},
		{"anthropic-messages", func(o ai.StreamOptions) *ai.AssistantMessage {
			return StreamAnthropic(context.Background(), model(ai.APIAnthropicMessages, "anthropic"), req, &AnthropicOptions{StreamOptions: o}).Result()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doer := &countingDoer{}
			final := tc.run(ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{
				APIKey: "test-key", HTTPClient: doer, MaxRetries: 2, Headers: ai.ProviderHeaders{"X-A": strPtr("a\nb")},
			}})
			if doer.calls != 0 {
				t.Fatalf("transport calls = %d, want 0: the request must fail before it is sent", doer.calls)
			}
			if want := invalidValueError("a\nb"); final.StopReason != ai.StopError || final.ErrorMessage != want {
				t.Fatalf("stream = %s %q, want error %q", final.StopReason, final.ErrorMessage, want)
			}
		})
	}
}
