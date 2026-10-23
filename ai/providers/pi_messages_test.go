package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/sky-valley/pi/ai"
)

// piMessagesSSE builds an SSE body of `data: <json>\n\n` frames from raw event
// JSON strings, matching the upstream test server's framing.
func piMessagesSSE(events ...string) string {
	var b strings.Builder
	for _, e := range events {
		b.WriteString("data: ")
		b.WriteString(e)
		b.WriteString("\n\n")
	}
	return b.String()
}

func piMessagesTestModel(baseURL string) *ai.Model {
	return &ai.Model{
		ID: "auto", Name: "Radius Auto", Api: ai.APIPiMessages, Provider: "radius",
		BaseURL: baseURL, Input: []string{"text"},
		Cost:          ai.ModelCost{Input: 1, Output: 2, CacheRead: 0.1, CacheWrite: 0.2},
		ContextWindow: 128000, MaxTokens: 16384,
	}
}

func piMessagesTestContext() ai.Context {
	return ai.Context{Messages: []ai.Message{ai.NewUserText("Hello", 1)}}
}

const piMessagesUsageJSON = `{"input":10,"output":5,"cacheRead":0,"cacheWrite":0,"totalTokens":15,"cost":{"input":0.1,"output":0.2,"cacheRead":0,"cacheWrite":0,"total":0.3}}`

func piMessagesWantUsage() ai.Usage {
	return ai.Usage{
		Input: 10, Output: 5, CacheRead: 0, CacheWrite: 0, TotalTokens: 15,
		Cost: ai.CostBreakdown{Input: 0.1, Output: 0.2, CacheRead: 0, CacheWrite: 0, Total: 0.3},
	}
}

func TestPiMessagesStreamsTextAndToolCalls(t *testing.T) {
	var gotBody map[string]any
	var gotURL, gotAuth, gotCustom, gotAccept string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		gotURL = r.URL.RequestURI()
		gotAuth = r.Header.Get("authorization")
		gotCustom = r.Header.Get("x-custom")
		gotAccept = r.Header.Get("accept")
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, piMessagesSSE(
			`{"type":"start"}`,
			`{"type":"text_start","contentIndex":0}`,
			`{"type":"text_delta","contentIndex":0,"delta":"Hel"}`,
			`{"type":"text_delta","contentIndex":0,"delta":"lo"}`,
			`{"type":"text_end","contentIndex":0,"content":"Hello"}`,
			`{"type":"toolcall_start","contentIndex":1,"id":"call_1","toolName":"read"}`,
			// A truncated delta stream: parseStreamingJSON would complete this to
			// {"path":"a.tx"}, so the terminal toolcall_end must overwrite it with
			// the authoritative {"path":"a.txt"} (pi's Object.assign).
			`{"type":"toolcall_delta","contentIndex":1,"delta":"{\"path\":"}`,
			`{"type":"toolcall_delta","contentIndex":1,"delta":"\"a.tx"}`,
			`{"type":"toolcall_end","contentIndex":1,"toolCall":{"type":"toolCall","id":"call_1","name":"read","arguments":{"path":"a.txt"}}}`,
			`{"type":"done","reason":"toolUse","usage":`+piMessagesUsageJSON+`,"responseId":"resp_1","responseModel":"claude-opus-5-5"}`,
		))
	}))
	defer server.Close()

	model := piMessagesTestModel(server.URL + "/v1")
	ctxReq := piMessagesTestContext()
	es := StreamPiMessages(context.Background(), model, ai.NormalizeContext(ctxReq), &PiMessagesOptions{
		StreamOptions: ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "test-key", Headers: ai.ProviderHeaders{"x-custom": strPtr("1")}}, SessionID: "session-1", MaxTokens: intp(100)},
		ToolChoice:    "auto",
	})

	var sawTextDelta, toolEndCount int
	var startStop ai.StopReason
	sawStart := false
	for ev := range es.Events() {
		if ev.Type == ai.EventStart && ev.Partial != nil {
			sawStart = true
			startStop = ev.Partial.StopReason
		}
		if ev.Type == ai.EventTextDelta {
			sawTextDelta++
		}
		if ev.Type == ai.EventToolCallEnd {
			toolEndCount++
		}
	}
	final := es.Result()

	// Upstream f9a49869: the in-flight partial carries a pending stop reason
	// until the terminal `done` event resolves it.
	if !sawStart || startStop != ai.StopPending {
		t.Fatalf("in-flight partial should be pending, got start=%v stop=%q", sawStart, startStop)
	}
	if final.StopReason != ai.StopToolUse {
		t.Fatalf("stopReason = %s, want toolUse (%s)", final.StopReason, final.ErrorMessage)
	}
	if final.Usage != piMessagesWantUsage() {
		t.Errorf("usage = %+v, want %+v", final.Usage, piMessagesWantUsage())
	}
	if final.ResponseID != "resp_1" {
		t.Errorf("responseId = %q, want resp_1", final.ResponseID)
	}
	if final.Model != "auto" || final.Provider != "radius" {
		t.Errorf("model/provider = %q/%q", final.Model, final.Provider)
	}
	// The physical model behind the virtual "auto" (upstream 14c811841).
	if final.ResponseModel != "claude-opus-5-5" {
		t.Errorf("responseModel = %q, want claude-opus-5-5", final.ResponseModel)
	}
	if len(final.Content) != 2 {
		t.Fatalf("content len = %d, want 2", len(final.Content))
	}
	txt, ok := final.Content[0].(ai.TextContent)
	if !ok || txt.Text != "Hello" {
		t.Errorf("content[0] = %#v, want text Hello", final.Content[0])
	}
	tc, ok := final.Content[1].(ai.ToolCall)
	if !ok || tc.ID != "call_1" || tc.Name != "read" || tc.Arguments["path"] != "a.txt" {
		t.Errorf("content[1] = %#v, want toolCall read{path:a.txt}", final.Content[1])
	}
	if sawTextDelta == 0 {
		t.Errorf("expected text_delta events")
	}
	if toolEndCount != 1 {
		t.Errorf("toolcall_end count = %d, want 1", toolEndCount)
	}

	// Request shape.
	if gotURL != "/v1/messages" {
		t.Errorf("url = %q, want /v1/messages", gotURL)
	}
	if gotAuth != "Bearer test-key" {
		t.Errorf("authorization = %q", gotAuth)
	}
	if gotCustom != "1" {
		t.Errorf("x-custom = %q", gotCustom)
	}
	if gotAccept != "text/event-stream" {
		t.Errorf("accept = %q", gotAccept)
	}
	if gotBody["model"] != "auto" {
		t.Errorf("body.model = %v", gotBody["model"])
	}
	opts, _ := gotBody["options"].(map[string]any)
	wantOpts := map[string]any{"maxTokens": float64(100), "sessionId": "session-1", "toolChoice": "auto"}
	if len(opts) != len(wantOpts) {
		t.Errorf("options keys = %v, want %v", opts, wantOpts)
	}
	for k, v := range wantOpts {
		if opts[k] != v {
			t.Errorf("options[%q] = %v, want %v", k, opts[k], v)
		}
	}
	// The posted context is the normalized transcript (pi posts the
	// TranscriptContext it was handed): a single user message with content
	// "Hello". Normalize both through a map so key ordering doesn't affect the
	// comparison; TestPiMessagesPostsTheNormalizedTranscript pins the bytes.
	gotCtx, _ := json.Marshal(gotBody["context"])
	wantRaw, _ := json.Marshal(ai.NormalizeContext(ctxReq))
	var wantMap map[string]any
	_ = json.Unmarshal(wantRaw, &wantMap)
	wantCtx, _ := json.Marshal(wantMap)
	if string(gotCtx) != string(wantCtx) {
		t.Errorf("context = %s, want %s", gotCtx, wantCtx)
	}
}

func TestPiMessagesDebugAndOnResponse(t *testing.T) {
	var gotURL string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURL = r.URL.RequestURI()
		w.Header().Set("x-pi-gateway-upstream-provider", "anthropic")
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, piMessagesSSE(`{"type":"done","reason":"stop","usage":`+piMessagesUsageJSON+`}`))
	}))
	defer server.Close()

	model := piMessagesTestModel(server.URL + "/v1")
	var observed map[string]string
	final := StreamSimplePiMessages(context.Background(), model, ai.NormalizeContext(piMessagesTestContext()), &ai.SimpleStreamOptions{
		StreamOptions: ai.StreamOptions{
			ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "test-key", OnResponse: func(resp ai.ProviderResponse, m *ai.Model) error { observed = resp.Headers; return nil }},
		},
	}).
		Result()

	// debug is only reachable via *PiMessagesOptions; drive it through the full
	// stream to lock the ?debug=1 behavior.
	_ = final

	dbg := StreamPiMessages(context.Background(), model, ai.NormalizeContext(piMessagesTestContext()), &PiMessagesOptions{
		StreamOptions: ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "test-key"}},
		Debug:         true,
	}).
		Result()
	if dbg.StopReason != ai.StopStop {
		t.Fatalf("stopReason = %s, want stop (%s)", dbg.StopReason, dbg.ErrorMessage)
	}
	if gotURL != "/v1/messages?debug=1" {
		t.Errorf("url = %q, want /v1/messages?debug=1", gotURL)
	}
	if observed["x-pi-gateway-upstream-provider"] != "anthropic" {
		t.Errorf("onResponse headers = %v", observed)
	}
}

func TestPiMessagesBackendErrorResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"error":{"message":"Token expired","code":"unauthorized"}}`)
	}))
	defer server.Close()

	model := piMessagesTestModel(server.URL + "/v1")
	final := StreamPiMessages(context.Background(), model, ai.NormalizeContext(piMessagesTestContext()), &PiMessagesOptions{
		StreamOptions: ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "stale"}},
	}).
		Result()

	if final.StopReason != ai.StopError {
		t.Fatalf("stopReason = %s, want error", final.StopReason)
	}
	for _, want := range []string{"401", "Token expired", "unauthorized"} {
		if !strings.Contains(final.ErrorMessage, want) {
			t.Errorf("errorMessage %q missing %q", final.ErrorMessage, want)
		}
	}
	if len(final.Diagnostics) != 1 {
		t.Fatalf("diagnostics len = %d, want 1", len(final.Diagnostics))
	}
	d := final.Diagnostics[0]
	if d.Type != "pi_messages_response_failure" {
		t.Errorf("diagnostic type = %q", d.Type)
	}
	if status, _ := d.Details.Get("status"); status != 401 {
		t.Errorf("diagnostic details.status = %v, want 401", status)
	}
}

func TestPiMessagesServerSentError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, piMessagesSSE(
			`{"type":"start"}`,
			`{"type":"error","reason":"error","usage":`+piMessagesUsageJSON+`,"errorMessage":"Upstream failed","responseModel":"claude-opus-5-5"}`,
		))
	}))
	defer server.Close()

	model := piMessagesTestModel(server.URL + "/v1")
	final := StreamPiMessages(context.Background(), model, ai.NormalizeContext(piMessagesTestContext()), &PiMessagesOptions{
		StreamOptions: ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "test-key"}},
	}).
		Result()

	if final.StopReason != ai.StopError {
		t.Fatalf("stopReason = %s, want error", final.StopReason)
	}
	if final.ErrorMessage != "Upstream failed" {
		t.Errorf("errorMessage = %q, want Upstream failed", final.ErrorMessage)
	}
	if final.Usage != piMessagesWantUsage() {
		t.Errorf("usage = %+v, want %+v", final.Usage, piMessagesWantUsage())
	}
	if final.ResponseModel != "claude-opus-5-5" {
		t.Errorf("responseModel = %q, want claude-opus-5-5", final.ResponseModel)
	}
}

// A terminal event's responseModel is copied onto the message whenever it is
// present, even when it equals the requested model (upstream 14c811841), and an
// absent one leaves the message's own standing, as providerThinkingLevel's does.
func TestPiMessagesResponseModel(t *testing.T) {
	for _, tc := range []struct {
		name     string
		terminal string
		existing string
		want     string
	}{
		{"done carries it", `{"type":"done","reason":"stop","usage":` + piMessagesUsageJSON + `,"responseModel":"claude-opus-5-5"}`, "", "claude-opus-5-5"},
		{"error carries it", `{"type":"error","reason":"error","usage":` + piMessagesUsageJSON + `,"errorMessage":"boom","responseModel":"gpt-6"}`, "", "gpt-6"},
		{"equal to the requested model is kept", `{"type":"done","reason":"stop","usage":` + piMessagesUsageJSON + `,"responseModel":"auto"}`, "", "auto"},
		{"present replaces an existing one", `{"type":"done","reason":"stop","usage":` + piMessagesUsageJSON + `,"responseModel":"claude-opus-5-5"}`, "older", "claude-opus-5-5"},
		{"absent on done leaves it standing", `{"type":"done","reason":"stop","usage":` + piMessagesUsageJSON + `}`, "older", "older"},
		{"absent on error leaves it standing", `{"type":"error","reason":"error","usage":` + piMessagesUsageJSON + `,"errorMessage":"boom"}`, "older", "older"},
		{"absent leaves it unset", `{"type":"done","reason":"stop","usage":` + piMessagesUsageJSON + `}`, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newPiMessagesConverter(piMessagesTestModel("http://unused"))
			c.partial.ResponseModel = tc.existing
			ev, _, err := decodePiMessagesEvent(tc.terminal)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := c.convert(ev); err != nil {
				t.Fatal(err)
			}
			if c.partial.ResponseModel != tc.want {
				t.Fatalf("responseModel = %q, want %q", c.partial.ResponseModel, tc.want)
			}
		})
	}
}

func TestPiMessagesMissingAPIKey(t *testing.T) {
	model := piMessagesTestModel("http://127.0.0.1:1/v1")
	final := StreamPiMessages(context.Background(), model, ai.NormalizeContext(piMessagesTestContext()), nil).Result()
	if final.StopReason != ai.StopError {
		t.Fatalf("stopReason = %s, want error", final.StopReason)
	}
	if !strings.Contains(final.ErrorMessage, "No API key provided") {
		t.Errorf("errorMessage = %q, want No API key provided", final.ErrorMessage)
	}
}

func TestPiMessagesNoTerminalEvent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, piMessagesSSE(
			`{"type":"start"}`,
			`{"type":"text_start","contentIndex":0}`,
			`{"type":"text_delta","contentIndex":0,"delta":"partial"}`,
		))
	}))
	defer server.Close()

	model := piMessagesTestModel(server.URL + "/v1")
	final := StreamPiMessages(context.Background(), model, ai.NormalizeContext(piMessagesTestContext()), &PiMessagesOptions{
		StreamOptions: ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "test-key"}},
	}).
		Result()

	if final.StopReason != ai.StopError {
		t.Fatalf("stopReason = %s, want error", final.StopReason)
	}
	if !strings.Contains(final.ErrorMessage, "stream ended without a terminal event") {
		t.Errorf("errorMessage = %q", final.ErrorMessage)
	}
}

func TestPiMessagesRegisteredBuiltin(t *testing.T) {
	if _, ok := ai.GetApiProvider(ai.APIPiMessages); !ok {
		t.Fatalf("pi-messages api provider not registered")
	}
}

// A non-conformant backend that sends a *_delta for a contentIndex it never
// started must surface a terminal error event — mirroring pi's throw being
// caught into createErrorEvent — not panic the host process. Without the
// goroutine recover, the out-of-range slice index crashes the test binary.
func TestPiMessagesMalformedStreamDoesNotPanic(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, piMessagesSSE(
			`{"type":"start"}`,
			// contentIndex 3 was never started: partial.Content is empty here.
			`{"type":"text_delta","contentIndex":3,"delta":"boom"}`,
			`{"type":"done","reason":"stop","usage":`+piMessagesUsageJSON+`}`,
		))
	}))
	defer server.Close()

	model := piMessagesTestModel(server.URL + "/v1")
	final := StreamPiMessages(context.Background(), model, ai.NormalizeContext(piMessagesTestContext()), &PiMessagesOptions{
		StreamOptions: ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "test-key"}},
	}).
		Result()

	if final.StopReason != ai.StopError {
		t.Fatalf("stopReason = %s, want error", final.StopReason)
	}
}

// OnResponse returning a non-nil error must fail the stream (pi: a rejected
// onResponse promise propagates to the catch), symmetric with OnPayload.
func TestPiMessagesOnResponseErrorFailsStream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, piMessagesSSE(`{"type":"done","reason":"stop","usage":`+piMessagesUsageJSON+`}`))
	}))
	defer server.Close()

	model := piMessagesTestModel(server.URL + "/v1")
	final := StreamPiMessages(context.Background(), model, ai.NormalizeContext(piMessagesTestContext()), &PiMessagesOptions{
		StreamOptions: ai.StreamOptions{
			ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "test-key", OnResponse: func(resp ai.ProviderResponse, m *ai.Model) error { return fmt.Errorf("hook rejected") }},
		},
	}).
		Result()

	if final.StopReason != ai.StopError {
		t.Fatalf("stopReason = %s, want error", final.StopReason)
	}
	if !strings.Contains(final.ErrorMessage, "hook rejected") {
		t.Errorf("errorMessage = %q, want it to contain the hook error", final.ErrorMessage)
	}
}

// chunkedReader returns its payload one predetermined chunk per Read, letting a
// test place a read boundary at an exact byte offset.
type chunkedReader struct {
	chunks [][]byte
	i      int
}

func (c *chunkedReader) Read(p []byte) (int, error) {
	if c.i >= len(c.chunks) {
		return 0, io.EOF
	}
	n := copy(p, c.chunks[c.i])
	c.i++
	return n, nil
}

// A "\r\n" split across two reads must still collapse to "\n" so the frame
// boundary is found — the whole-buffer normalization, matching pi. With the
// buggy per-chunk normalization the seam "\r"+"\n" survives, the first frame
// boundary is missed, and the terminal done event is lost.
func TestPiMessagesCRLFSeamAcrossReads(t *testing.T) {
	// Frame separator is CRLF-CRLF; the boundary between the two frames is split
	// mid-"\r\n" across the chunk seam.
	frame1 := "data: {\"type\":\"start\"}\r\n\r"
	frame2 := "\ndata: {\"type\":\"done\",\"reason\":\"stop\",\"usage\":" + piMessagesUsageJSON + "}\r\n\r\n"
	reader := &chunkedReader{chunks: [][]byte{[]byte(frame1), []byte(frame2)}}

	var types []string
	err := readPiMessagesEvents(reader, nil, func(ev piMessagesEvent) (bool, error) {
		types = append(types, ev.Type)
		return ev.Type != "done", nil
	})
	if err != nil {
		t.Fatalf("readPiMessagesEvents error: %v", err)
	}
	if len(types) != 2 || types[0] != "start" || types[1] != "done" {
		t.Fatalf("parsed frames = %v, want [start done]", types)
	}
}

func intp(i int) *int { return &i }

// A pi-messages backend that fronts a managed-effort Anthropic model reports the
// effort the turn actually ran at, and the converter carries it onto the final
// message so the next request can replay it (upstream 4e69b0c28). An event that
// omits the field must leave the message's level alone rather than blanking it.
func TestPiMessagesProviderThinkingLevel(t *testing.T) {
	cases := []struct {
		name      string
		terminal  string
		wantLevel string
		wantStop  ai.StopReason
	}{
		{
			name:      "done carries the level",
			terminal:  `{"type":"done","reason":"stop","usage":` + piMessagesUsageJSON + `,"responseId":"r1","providerThinkingLevel":"xhigh"}`,
			wantLevel: "xhigh",
			wantStop:  ai.StopStop,
		},
		{
			name:      "error carries the level",
			terminal:  `{"type":"error","reason":"error","usage":` + piMessagesUsageJSON + `,"errorMessage":"boom","providerThinkingLevel":"max"}`,
			wantLevel: "max",
			wantStop:  ai.StopError,
		},
		{
			name:      "absent leaves it unset",
			terminal:  `{"type":"done","reason":"stop","usage":` + piMessagesUsageJSON + `}`,
			wantLevel: "",
			wantStop:  ai.StopStop,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("content-type", "text/event-stream")
				io.WriteString(w, piMessagesSSE(`{"type":"start"}`, tc.terminal))
			}))
			defer server.Close()

			opts := &PiMessagesOptions{}
			opts.APIKey = "k"
			got := StreamPiMessages(context.Background(), piMessagesTestModel(server.URL), ai.NormalizeContext(piMessagesTestContext()), opts).Result()
			if got.StopReason != tc.wantStop {
				t.Fatalf("stopReason = %q, want %q", got.StopReason, tc.wantStop)
			}
			if got.ProviderThinkingLevel != tc.wantLevel {
				t.Fatalf("providerThinkingLevel = %q, want %q", got.ProviderThinkingLevel, tc.wantLevel)
			}
		})
	}
}

// The pointer on piMessagesEvent.ProviderThinkingLevel exists to keep pi's
// `if (event.providerThinkingLevel !== undefined)` distinction: an absent field
// leaves whatever the partial already carries standing, it does not blank it.
// StreamPiMessages never seeds the partial with a level, so the stream-level
// cases above cannot tell "left standing" from "blanked to empty"; this pins
// the distinction where it is observable, on the converter itself.
func TestPiMessagesAbsentProviderThinkingLevelLeavesExistingLevel(t *testing.T) {
	for _, tc := range []struct {
		name     string
		terminal string
	}{
		{"done", `{"type":"done","reason":"stop","usage":` + piMessagesUsageJSON + `}`},
		{"error", `{"type":"error","reason":"error","usage":` + piMessagesUsageJSON + `,"errorMessage":"boom"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newPiMessagesConverter(piMessagesTestModel("http://unused"))
			c.partial.ProviderThinkingLevel = "xhigh"
			ev, _, err := decodePiMessagesEvent(tc.terminal)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := c.convert(ev); err != nil {
				t.Fatal(err)
			}
			if c.partial.ProviderThinkingLevel != "xhigh" {
				t.Fatalf("providerThinkingLevel = %q, want the existing %q left standing by an absent field", c.partial.ProviderThinkingLevel, "xhigh")
			}
		})
	}
}

// TestPiMessagesHeadersTimeoutFailsFetch: pi-messages calls fetch itself, so
// no timeoutMs applies, and what bounds the wait for headers is fetch's own
// headersTimeout, undici's 300s default; its expiry fails as every other
// no-response failure does: "fetch failed", no start event. Measured against
// pi's stream at 49681e1b7 (node v26.4.0): a server that reads the request
// and never answers → [error] "fetch failed" after 301s, timeoutMs 50.
func TestPiMessagesHeadersTimeoutFailsFetch(t *testing.T) {
	shortenUndiciHeadersTimeout(t, 50)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
	}))
	defer server.Close()
	opts := ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{TimeoutMs: 60_000}}
	events, final := drain(streamAbortAdapter(t, context.Background(), string(ai.APIPiMessages), server.URL, opts))
	if fmt.Sprint(events) != "[error]" || final.StopReason != ai.StopError || final.ErrorMessage != "fetch failed" {
		t.Fatalf("stream %v, stop %s %q; pi: [error], error \"fetch failed\"", events, final.StopReason, final.ErrorMessage)
	}
}

// piMessagesArgumentsErrorCaptureFile is what
// testdata/pi-messages/capture-pi-messages-arguments-error.mjs recorded from
// pi-ai 1.3.0.
const piMessagesArgumentsErrorCaptureFile = "testdata/pi-messages/pi-messages-arguments-error-1.3.0.json"

// toolcall_end is pi's Object.assign(block, event.toolCall): every key of the
// gateway's tool call lands on the block, argumentsError (upstream 87f9a3641)
// included, so arguments the gateway could not parse fail validation on the
// client — `Invalid arguments for tool "read": …` — instead of running the
// tool with {}. A falsy one validates as absent; a truthy non-string is
// written as JavaScript's template literal writes it.
func TestPiMessagesToolCallKeepsArgumentsErrorLikePi(t *testing.T) {
	data, err := os.ReadFile(piMessagesArgumentsErrorCaptureFile)
	if err != nil {
		t.Fatal(err)
	}
	var capture struct {
		Rows []struct {
			Name       string `json:"name"`
			Body       string `json:"body"`
			StopReason string `json:"stopReason"`
			ToolCall   struct {
				ArgumentsError json.RawMessage `json:"argumentsError"`
			} `json:"toolCall"`
			Validation *string `json:"validation"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(data, &capture); err != nil {
		t.Fatalf("%s: %v; rerun the capture", piMessagesArgumentsErrorCaptureFile, err)
	}
	if len(capture.Rows) == 0 {
		t.Fatalf("%s holds no rows; rerun the capture", piMessagesArgumentsErrorCaptureFile)
	}
	tool := ai.Tool{Name: "read", Description: "read", Parameters: ai.Object(ai.Opt("path", ai.String()))}
	for _, row := range capture.Rows {
		t.Run(row.Name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, row.Body)
			}))
			defer server.Close()
			opts := &PiMessagesOptions{StreamOptions: ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "k"}}}
			final := StreamPiMessages(t.Context(), piMessagesTestModel(server.URL+"/v1"), ai.NormalizeContext(ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 0)}}), opts).Result()
			if string(final.StopReason) != row.StopReason || len(final.Content) != 1 {
				t.Fatalf("stream ended %s (%q) with %d blocks; pi %s", final.StopReason, final.ErrorMessage, len(final.Content), row.StopReason)
			}
			tc, ok := final.Content[0].(ai.ToolCall)
			if !ok {
				t.Fatalf("block is %T, want a tool call", final.Content[0])
			}
			if want, isString := rawString(row.ToolCall.ArgumentsError); isString && tc.ArgumentsError != want {
				t.Errorf("argumentsError %q, pi %q", tc.ArgumentsError, want)
			}
			_, verr := ai.ValidateToolArguments(tool, tc)
			switch {
			case row.Validation == nil && verr != nil:
				t.Errorf("validation failed (%v); pi's passes", verr)
			case row.Validation != nil && (verr == nil || verr.Error() != *row.Validation):
				t.Errorf("validation = %v, pi's = %q", verr, *row.Validation)
			}
		})
	}
}
