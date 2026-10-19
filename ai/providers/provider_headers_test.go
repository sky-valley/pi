package providers

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/sky-valley/pi/ai"
)

// Header deletion markers (upstream a24fb9e96). pi distinguishes three states
// per header name — absent (send the provider default), a string (send it, even
// when empty), and null (suppress the default) — and these tests pin all three
// on every adapter that merges ProviderHeaders.

// captureOpenAIHeaders runs one openai-completions request against a local
// server and returns the headers that reached the wire.
func captureOpenAIHeaders(t *testing.T, model *ai.Model, opts ai.StreamOptions) http.Header {
	t.Helper()
	var got http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	model.BaseURL = server.URL
	final := StreamOpenAICompletions(context.Background(), model, ai.NormalizeContext(
		ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}),

		&OpenAIOptions{StreamOptions: opts}).
		Result()
	if final.StopReason == ai.StopError {
		t.Fatalf("stream failed: %s", final.ErrorMessage)
	}
	return got
}

func openAITestModel() *ai.Model {
	return &ai.Model{ID: "gpt-test", Api: ai.APIOpenAICompletions, Provider: "openai",
		Input: []string{"text"}, MaxTokens: 4096}
}

// The three states on a header the adapter itself would send.
func TestOpenAICompletionsHeaderStates(t *testing.T) {
	t.Run("absent keeps the default", func(t *testing.T) {
		h := captureOpenAIHeaders(t, openAITestModel(), ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "k"}})
		if got := h.Get("authorization"); got != "Bearer k" {
			t.Fatalf("authorization = %q, want the default Bearer k", got)
		}
	})
	t.Run("empty string replaces the default", func(t *testing.T) {
		// An empty Authorization leaves openai's client nothing to
		// authenticate with, and its validateHeaders refuses the request
		// before sending it (pi's capture: testdata/cloudflare-auth,
		// openai-completions key-empty-authorization).
		final := StreamOpenAICompletions(context.Background(), openAITestModel(), ai.NormalizeContext(
			ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}),
			&OpenAIOptions{StreamOptions: ai.StreamOptions{
				ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "k", Headers: ai.ProviderHeaders{"authorization": strPtr("")}},
			}}).Result()
		if final.StopReason != ai.StopError || final.ErrorMessage != errOpenAINoRequestAuth.Error() {
			t.Fatalf("stopped %s with %q, want openai's validateHeaders refusal", final.StopReason, final.ErrorMessage)
		}
	})
	t.Run("null suppresses the default", func(t *testing.T) {
		h := captureOpenAIHeaders(t, openAITestModel(), ai.StreamOptions{
			ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "k", Headers: ai.ProviderHeaders{"authorization": nil}},
		})
		if _, ok := h["Authorization"]; ok {
			t.Fatalf("a deletion marker must remove the header, got %q", h.Get("authorization"))
		}
	})
}

// A marker in the consumer's headers suppresses an attribution default, which
// pi allows because attribution sits at the bottom of the same merged object
// (sdk.ts mergeProviderAttributionHeaders).
func TestOpenAICompletionsMarkerSuppressesAttribution(t *testing.T) {
	t.Setenv("PI_TELEMETRY", "1")
	model := openAITestModel()
	model.Provider = "openrouter"
	h := captureOpenAIHeaders(t, model, ai.StreamOptions{
		ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "k", Headers: ai.ProviderHeaders{"HTTP-Referer": nil}},
	})
	if _, ok := h["Http-Referer"]; ok {
		t.Fatalf("attribution default must be suppressible, got %q", h.Get("HTTP-Referer"))
	}
	if got := h.Get("X-OpenRouter-Title"); got != "pi" {
		t.Fatalf("unrelated attribution headers must survive: X-OpenRouter-Title = %q", got)
	}
}

// Precedence is unchanged by markers: model headers outrank attribution, and
// the consumer's headers outrank both — in either direction.
func TestOpenAICompletionsMarkerPrecedence(t *testing.T) {
	model := openAITestModel()
	model.Headers = ai.ProviderHeaders{"X-Both": nil}
	h := captureOpenAIHeaders(t, model, ai.StreamOptions{
		ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "k", Headers: ai.ProviderHeaders{"X-Both": strPtr("consumer")}},
	})
	if got := h.Get("X-Both"); got != "consumer" {
		t.Fatalf("consumer headers merge last: X-Both = %q, want consumer", got)
	}

	model = openAITestModel()
	model.Headers = ai.ProviderHeaders{"X-Both": strPtr("model")}
	h = captureOpenAIHeaders(t, model, ai.StreamOptions{
		ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "k", Headers: ai.ProviderHeaders{"X-Both": nil}},
	})
	if _, ok := h["X-Both"]; ok {
		t.Fatalf("a consumer marker must cancel the model header, got %q", h.Get("X-Both"))
	}
}

// clientAPIKey: pi's hasHeader requires `value !== null`, so a marker is not a
// credential — the request must fail exactly as if the header were absent.
func TestClientAPIKeyIgnoresDeletionMarkers(t *testing.T) {
	_, err := clientAPIKey("custom", "", ai.ProviderHeaders{"Authorization": nil, "cf-aig-authorization": nil})
	if err == nil || err.Error() != "No API key for provider: custom" {
		t.Fatalf("deletion markers must not count as auth, got %v", err)
	}
}

// google builds its own request, so pi runs the merged headers through
// providerHeadersToRecord: a marker cancels earlier ProviderHeaders sources but
// cannot unset a header the adapter owns.
func TestGoogleHeaderStates(t *testing.T) {
	t.Setenv("PI_TELEMETRY", "1")
	var got http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, googleSSE)
	}))
	defer server.Close()
	model := &ai.Model{ID: "gemini-2.5-flash", Api: ai.APIGoogleGenerativeAI,
		Provider: "cloudflare-workers-ai", BaseURL: server.URL,
		Headers: ai.ProviderHeaders{"X-Model": strPtr("from-model"), "X-Empty": strPtr("")}}
	final := StreamGoogle(context.Background(), model, ai.NormalizeContext(
		ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}),

		&GoogleOptions{StreamOptions: ai.StreamOptions{
			ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "g-key", Headers: ai.ProviderHeaders{
				"X-Model":        nil,
				"User-Agent":     nil,
				"x-goog-api-key": nil,
			}},
		}}).
		Result()
	if final.StopReason == ai.StopError {
		t.Fatalf("stream failed: %s", final.ErrorMessage)
	}
	if _, ok := got["X-Model"]; ok {
		t.Fatalf("a marker must cancel the model header before the record is built, got %q", got.Get("X-Model"))
	}
	// net/http supplies its own User-Agent once ours is gone, exactly as fetch
	// does for pi; what must not survive is the attribution value.
	if got := got.Get("User-Agent"); got == "pi-coding-agent" {
		t.Fatal("a marker must cancel the attribution default")
	}
	if got.Get("X-Empty") != "" {
		t.Fatalf("X-Empty = %q, want an empty value", got.Get("X-Empty"))
	}
	if _, ok := got["X-Empty"]; !ok {
		t.Fatal("an empty string must still be sent")
	}
	// pi's providerHeadersToRecord only drops entries; the SDK-owned api-key
	// header is not part of the merged object and survives.
	if got.Get("x-goog-api-key") != "g-key" {
		t.Fatalf("x-goog-api-key = %q, want the adapter's own value", got.Get("x-goog-api-key"))
	}
}

// pi-messages spreads providerHeadersToRecord(options.headers) into an object
// literal that already holds its three fixed headers, so a marker there means
// "not sent" and cannot remove the authorization the adapter just wrote.
func TestPiMessagesHeaderStates(t *testing.T) {
	var got http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, piMessagesSSE(
			`{"type":"start"}`,
			`{"type":"done","reason":"stop","usage":`+piMessagesUsageJSON+`,"responseId":"resp_1"}`,
		))
	}))
	defer server.Close()
	final := StreamPiMessages(context.Background(), piMessagesTestModel(server.URL+"/v1"), ai.NormalizeContext(
		piMessagesTestContext()),
		&PiMessagesOptions{StreamOptions: ai.StreamOptions{
			ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "test-key", Headers: ai.ProviderHeaders{
				"authorization": nil,
				"x-custom":      nil,
				"x-empty":       strPtr(""),
			}},
		}}).
		Result()
	if final.StopReason == ai.StopError {
		t.Fatalf("stream failed: %s", final.ErrorMessage)
	}
	if got.Get("authorization") != "Bearer test-key" {
		t.Fatalf("authorization = %q; a marker must not unset the adapter's own header", got.Get("authorization"))
	}
	if _, ok := got["X-Custom"]; ok {
		t.Fatalf("a marker must not be sent as a header, got %q", got.Get("x-custom"))
	}
	if _, ok := got["X-Empty"]; !ok {
		t.Fatal("an empty string must still be sent")
	}
}

// capturePiMessagesHeaders runs one pi-messages request against a local server
// and returns the headers that reached the wire.
func capturePiMessagesHeaders(t *testing.T, headers ai.ProviderHeaders) http.Header {
	t.Helper()
	var got http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, piMessagesSSE(
			`{"type":"start"}`,
			`{"type":"done","reason":"stop","usage":`+piMessagesUsageJSON+`,"responseId":"resp_1"}`,
		))
	}))
	defer server.Close()
	final := StreamPiMessages(context.Background(), piMessagesTestModel(server.URL+"/v1"), ai.NormalizeContext(
		piMessagesTestContext()),
		&PiMessagesOptions{StreamOptions: ai.StreamOptions{
			ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "test-key", Headers: headers},
		}}).
		Result()
	if final.StopReason == ai.StopError {
		t.Fatalf("stream failed: %s", final.ErrorMessage)
	}
	return got
}

// A marker deletes a differently-spelled value inside the same source too:
// providerHeadersToRecord folds {"X-A": "1", "x-a": null} to nothing (upstream
// a328aa89a; measured on pi's wire through fetch — no x-a header). Within one
// Go map the slot order is sorted name order, and "x-a" sorts last, as it is
// last in the JS literal.
func TestPiMessagesCaseVariantMarkerDeletesWithinSource(t *testing.T) {
	h := capturePiMessagesHeaders(t, ai.ProviderHeaders{"X-A": strPtr("1"), "x-a": nil})
	if values, present := h["X-A"]; present {
		t.Fatalf("X-A = %q, want it absent: the marker in the last slot deletes the earlier spelling", values)
	}
	if got := h.Get("authorization"); got != "Bearer test-key" {
		t.Fatalf("authorization = %q, want the adapter's own value", got)
	}
}

// pi-messages hands fetch `{authorization, accept, "content-type", ...record}`.
// A record entry spelled exactly like a fixed header replaces it in the JS
// object; one spelled differently is a second key, and fetch's Headers.append
// joins the two, fixed header first, after trimming each value. Every want is
// what node's fetch put on the wire for the same record (a328aa89a's
// providerHeadersToRecord, parsed by node's http server — which also trims the
// trailing space undici sends after an empty joined value).
func TestPiMessagesRecordJoinsCaseVariantFixedHeaders(t *testing.T) {
	for _, tc := range []struct {
		name    string
		headers ai.ProviderHeaders
		header  string
		want    string
	}{
		{"case-variant authorization joins", ai.ProviderHeaders{"Authorization": strPtr("x")}, "authorization", "Bearer test-key, x"},
		{"upper-case authorization joins", ai.ProviderHeaders{"AUTHORIZATION": strPtr("x")}, "authorization", "Bearer test-key, x"},
		{"exact spelling replaces", ai.ProviderHeaders{"authorization": strPtr("x")}, "authorization", "x"},
		{"accept joins", ai.ProviderHeaders{"Accept": strPtr("application/json")}, "accept", "text/event-stream, application/json"},
		{"content-type joins", ai.ProviderHeaders{"Content-Type": strPtr("text/plain")}, "content-type", "application/json, text/plain"},
		{"each joined value is trimmed", ai.ProviderHeaders{"Authorization": strPtr("  x  ")}, "authorization", "Bearer test-key, x"},
		{"each joined value is tab-trimmed", ai.ProviderHeaders{"Authorization": strPtr("\tx\t")}, "authorization", "Bearer test-key, x"},
		{"an empty value still joins", ai.ProviderHeaders{"Authorization": strPtr("")}, "authorization", "Bearer test-key,"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := capturePiMessagesHeaders(t, tc.headers)
			if got := h.Values(tc.header); len(got) != 1 || got[0] != tc.want {
				t.Fatalf("%s = %q, want exactly [%q]", tc.header, got, tc.want)
			}
		})
	}
}

// @google/genai spreads the record over its own defaults and builds the
// request with Headers.append, so Content-Type — the one genai default the
// port sends — joins with a record entry spelled differently and is replaced
// by one spelled exactly "Content-Type". The api key genai adds only when the
// record lacks one, so a record entry of any spelling replaces it. Every want
// was measured on the wire through @google/genai 2.21.0.
func TestGoogleRecordOverGenaiDefaults(t *testing.T) {
	for _, tc := range []struct {
		name    string
		headers ai.ProviderHeaders
		header  string
		want    string
	}{
		{"case-variant content-type joins", ai.ProviderHeaders{"content-type": strPtr("text/plain")}, "content-type", "application/json, text/plain"},
		{"exact Content-Type replaces", ai.ProviderHeaders{"Content-Type": strPtr("text/plain")}, "content-type", "text/plain"},
		{"api key replaced in genai's spelling", ai.ProviderHeaders{"x-goog-api-key": strPtr("other")}, "x-goog-api-key", "other"},
		{"api key replaced in any spelling", ai.ProviderHeaders{"X-Goog-Api-Key": strPtr("other")}, "x-goog-api-key", "other"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model := &ai.Model{ID: "gemini-2.5-flash", Api: ai.APIGoogleGenerativeAI, Provider: "google",
				Input: []string{"text"}, MaxTokens: 4096}
			h := captureGoogleHeaders(t, model, ai.StreamOptions{
				ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "g-key", Headers: tc.headers},
			})
			if got := h.Values(tc.header); len(got) != 1 || got[0] != tc.want {
				t.Fatalf("%s = %q, want exactly [%q]", tc.header, got, tc.want)
			}
		})
	}
}

// headerObject.record is pi's providerHeadersToRecord since upstream a328aa89a.
// Each case is google's one spread object, built source by source; every want
// is the output of a328aa89a's providerHeadersToRecord executed under node
// over the same sources in the same order.
func TestHeaderObjectRecordMatchesPi(t *testing.T) {
	ua := "pi (x)"
	for _, tc := range []struct {
		name    string
		sources []ai.ProviderHeaders
		want    []recordEntry
	}{
		{"lowercase override",
			[]ai.ProviderHeaders{{"User-Agent": &ua}, {"user-agent": strPtr("custom")}},
			[]recordEntry{{"user-agent", "custom"}}},
		{"lowercase marker",
			[]ai.ProviderHeaders{{"User-Agent": &ua}, {"user-agent": nil}},
			nil},
		{"cross-source marker",
			[]ai.ProviderHeaders{{"User-Agent": &ua}, {"X-Trace": strPtr("model")}, {"x-trace": nil}},
			[]recordEntry{{"User-Agent", ua}}},
		// JS spread updates "User-Agent" in its first slot, so "user-agent"
		// still holds the last slot and wins.
		{"exact re-spelling keeps its slot",
			[]ai.ProviderHeaders{{"User-Agent": &ua}, {"user-agent": strPtr("m")}, {"User-Agent": strPtr("o")}},
			[]recordEntry{{"user-agent", "m"}}},
		{"re-set moves to the end",
			[]ai.ProviderHeaders{{"A": strPtr("1")}, {"B": strPtr("2")}, {"a": strPtr("3")}},
			[]recordEntry{{"B", "2"}, {"a", "3"}}},
		{"empty string kept",
			[]ai.ProviderHeaders{{"X-Empty": strPtr("")}},
			[]recordEntry{{"X-Empty", ""}}},
		{"marker then value",
			[]ai.ProviderHeaders{{"X-A": nil}, {"x-a": strPtr("v")}},
			[]recordEntry{{"x-a", "v"}}},
		// The fold is JavaScript's toLowerCase: "X-\u0130D" lowers to
		// "x-i\u0307d" and "X\u03a3" to "x\u03c2", so neither marker reaches
		// the earlier name.
		{"dotted capital I marker",
			[]ai.ProviderHeaders{{"x-id": strPtr("v")}, {"X-" + dottedI + "D": nil}},
			[]recordEntry{{"x-id", "v"}}},
		{"final sigma marker",
			[]ai.ProviderHeaders{{"x" + smallSig: strPtr("v")}, {"X" + capitalSig: nil}},
			[]recordEntry{{"x" + smallSig, "v"}}},
		{"dotted capital I within one source",
			[]ai.ProviderHeaders{{"X-" + dottedI: strPtr("v"), "x-i": nil}},
			[]recordEntry{{"X-" + dottedI, "v"}}},
		{"final sigma within one source",
			[]ai.ProviderHeaders{{"X" + capitalSig: strPtr("v"), "x" + smallSig: nil}},
			[]recordEntry{{"X" + capitalSig, "v"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := &headerObject{}
			for _, source := range tc.sources {
				o.merge(source)
			}
			if got := o.record(); !slices.Equal(got, tc.want) {
				t.Fatalf("record = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// Merging one ProviderHeaders source must not depend on Go map iteration order.
// Two names that differ only by case are distinct ProviderHeaders keys but
// canonicalize to one http.Header key, so a marker and a value can collide on
// it; sorted name order is the tie-break (see headerObject.merge), matching
// mergeHeaders in ai/models_runtime.go.
//
// Across sources the winner is decided by slot order and needs no tie-break, so
// this is now the whole remaining reach of the sorted rule: two spellings
// inside ONE ai.ProviderHeaders literal — a catalog entry, or a consumer map.
func TestHeaderObjectCaseCollisionIsDeterministic(t *testing.T) {
	headers := ai.ProviderHeaders{
		// "Authorization" sorts before "authorization" (ASCII), so the Del runs
		// first and the value wins.
		"Authorization": nil,
		"authorization": strPtr("Bearer x"),
		// The mirror case: the value sorts first, so the marker runs last and
		// wins. This is also what separates sorted order from "apply markers
		// first", which would have kept the value here.
		"X-Trace": strPtr("on"),
		"x-trace": nil,
		"X-Keep":  strPtr("keep"),
	}
	// One run cannot tell a sorted implementation from a lucky one: Go
	// randomizes map iteration per range statement, so repeat enough that an
	// order-dependent implementation is overwhelmingly likely to disagree with
	// itself at least once.
	for i := range 200 {
		h := http.Header{}
		o := &headerObject{}
		o.merge(headers)
		if err := o.applyAsDefaultHeaders(h, nil, undiciDeleteHeaderName); err != nil {
			t.Fatal(err)
		}
		if got := h.Get("authorization"); got != "Bearer x" {
			t.Fatalf("run %d: authorization = %q, want %q — the name sorting last must win", i, got, "Bearer x")
		}
		if _, present := h["X-Trace"]; present {
			t.Fatalf("run %d: X-Trace = %q, want it deleted — the marker sorts last", i, h.Get("X-Trace"))
		}
		if got := h.Get("X-Keep"); got != "keep" {
			t.Fatalf("run %d: X-Keep = %q, want keep", i, got)
		}
	}
}
