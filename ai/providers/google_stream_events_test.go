package providers

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/sky-valley/pi/ai"
	"github.com/sky-valley/pi/internal/jstext"
)

// googleStreamCapture is testdata/google-stream-events/google-stream-events-*.json:
// what pi's google-generative-ai adapter, running on @google/genai, does with
// each of a table of raw HTTP responses. capture.mts beside it says how it is
// made.
type googleStreamCapture struct {
	Source    string                 `json:"source"`
	Scenarios []googleStreamScenario `json:"scenarios"`
}

type googleStreamScenario struct {
	Name string `json:"name"`
	// Status is the response's status line, "HTTP/1.1 200 OK" when empty.
	Status     string `json:"status"`
	Divergence string `json:"divergence"`
	// DivergentFields names the parts of the outcome, as replayGoogleScenario
	// keys them, where a divergence scenario differs from pi. Every other part
	// must still be pi's.
	DivergentFields []string `json:"divergentFields"`
	Framing         string   `json:"framing"` // "close" or "chunked"
	// Headers is the response head, one [name, value] per line.
	Headers [][2]string `json:"headers"`
	// Segments are the body, each written as its own body read.
	Segments []string `json:"segments"`
	// EncodedBody, when set, is the whole body as written instead (a gzip,
	// deflate or brotli encoding of the joined segments, whole or damaged).
	EncodedBody []byte `json:"encodedBody"`
	// ContentLength adds a Content-Length for the body as written, and
	// ContentLengthExtra that many bytes more, so the body ends short.
	ContentLength      bool `json:"contentLength"`
	ContentLengthExtra int  `json:"contentLengthExtra"`
	// OneWrite sends each segment as its own HTTP chunk, all in one write.
	OneWrite bool `json:"oneWrite"`
	// AbruptEnd drops the connection after the body, unterminated.
	AbruptEnd bool `json:"abruptEnd"`
	// RequestHeaders are the options.headers the caller passes.
	RequestHeaders map[string]string `json:"requestHeaders"`
	// HeadLatin1 writes the head one byte per character (latin1) instead of
	// as UTF-8, so a header can carry bytes that are not UTF-8, and
	// BodyLatin1 the segments, so the body can.
	HeadLatin1   bool   `json:"headLatin1"`
	BodyLatin1   bool   `json:"bodyLatin1"`
	ThrowOn      *int   `json:"throwOn"`
	ThrowMessage string `json:"throwMessage"`
	// AbortOn is the callback call that aborts the request's signal.
	AbortOn *int `json:"abortOn"`
	// ReadBoundariesMatter reports whether pi's outcome changes when the
	// segments arrive as one read.
	ReadBoundariesMatter bool `json:"readBoundariesMatter"`
	// Pi is pi's outcome. A mistyped wire field can leave pi holding a value
	// no Go field can (a numeric delta or response id, a string token
	// count), so those are kept as JSON and compared by value. Usage holds
	// the token counts and the cost calculateCost left.
	Pi struct {
		// AcceptEncoding is the accept-encoding header the server received.
		AcceptEncoding string   `json:"acceptEncoding"`
		Events         []string `json:"events"`
		SameModel      bool     `json:"sameModel"`
		Stream         []struct {
			Type  string          `json:"type"`
			Delta json.RawMessage `json:"delta"`
		} `json:"stream"`
		StopReason   string         `json:"stopReason"`
		ErrorMessage string         `json:"errorMessage"`
		ResponseID   any            `json:"responseId"`
		Content      string         `json:"content"`
		Usage        map[string]any `json:"usage"`
	} `json:"pi"`
}

// piStreamEvent is one of pi's stream events as the replay compares it: the
// type, and the delta when there is one. A delta that is not a string (pi
// pushes part.text as it came) is written as its JSON, which no Go delta
// can equal.
func piStreamEvent(typ string, delta json.RawMessage) string {
	if delta == nil {
		return typ
	}
	var text string
	if delta[0] == '"' && json.Unmarshal(delta, &text) == nil {
		return typ + " " + text
	}
	var compact bytes.Buffer
	json.Compact(&compact, delta)
	return typ + " <non-string delta " + compact.String() + ">"
}

// googleUsageDiffs describes each figure of the Go usage that is not pi's,
// keyed "usage.<field>" and "usage.cost.<field>". A token count must be a
// number equal to the int, where pi can also hold a string, a fraction or null
// (NaN, Infinity); pi's reasoning is absent until a chunk carries usage, which
// is Go's 0. pi's calculateCost leaves every cost a number even from a string
// count, so the costs compare exactly.
func googleUsageDiffs(u ai.Usage, pi map[string]any) map[string]string {
	cost, _ := pi["cost"].(map[string]any)
	reasoning, reported := pi["reasoning"]
	if !reported {
		reasoning = 0.0
	}
	figures := []struct {
		key  string
		got  float64
		want any
	}{
		{"usage.input", float64(u.Input), pi["input"]},
		{"usage.output", float64(u.Output), pi["output"]},
		{"usage.cacheRead", float64(u.CacheRead), pi["cacheRead"]},
		{"usage.cacheWrite", float64(u.CacheWrite), pi["cacheWrite"]},
		{"usage.reasoning", float64(u.Reasoning), reasoning},
		{"usage.totalTokens", float64(u.TotalTokens), pi["totalTokens"]},
		{"usage.cost.input", u.Cost.Input, cost["input"]},
		{"usage.cost.output", u.Cost.Output, cost["output"]},
		{"usage.cost.cacheRead", u.Cost.CacheRead, cost["cacheRead"]},
		{"usage.cost.cacheWrite", u.Cost.CacheWrite, cost["cacheWrite"]},
		{"usage.cost.total", u.Cost.Total, cost["total"]},
	}
	diffs := map[string]string{}
	for _, f := range figures {
		if want, ok := f.want.(float64); !ok || want != f.got {
			diffs[f.key] = fmt.Sprintf("%s %v, pi %#v", f.key, f.got, f.want)
		}
	}
	return diffs
}

// sameGoogleResponseID reports whether the Go response id is pi's: pi's
// absent or "" is Go's "", a string is itself, and anything else is a value
// Go's string cannot hold.
func sameGoogleResponseID(got string, pi any) bool {
	if pi == nil {
		return got == ""
	}
	want, ok := pi.(string)
	return ok && got == want
}

func loadGoogleStreamCapture(t *testing.T) googleStreamCapture {
	t.Helper()
	raw, err := os.ReadFile("testdata/google-stream-events/google-stream-events-f72f799a4.json")
	if err != nil {
		t.Fatal(err)
	}
	var c googleStreamCapture
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	for pointer, message := range causeDetailMessages(t, "google-stream-events") {
		var i int
		if !scanPointer(pointer, "/scenarios/%d/pi", &i) || i >= len(c.Scenarios) {
			t.Fatalf("the cause-detail overlay names %s", pointer)
		}
		overlayErrorMessage(t, pointer, &c.Scenarios[i].Pi.ErrorMessage, message)
	}
	return c
}

// readsReader returns one of its reads per Read call, so a body arrives in
// exactly the body reads a scenario prescribes.
type readsReader struct{ reads []string }

func (r *readsReader) Read(p []byte) (int, error) {
	if len(r.reads) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.reads[0])
	if r.reads[0] = r.reads[0][n:]; r.reads[0] == "" {
		r.reads = r.reads[1:]
	}
	return n, nil
}

// latin1 is text written one byte per character, as node's latin1 encoding
// writes it; every character must be one latin1 has. It fails the test
// otherwise, so it runs on the test's goroutine, never in a server handler.
func latin1(t testing.TB, text string) string {
	t.Helper()
	out := make([]byte, 0, len(text))
	for _, r := range text {
		if r > 0xff {
			t.Fatalf("%q is not a latin1 character", r)
		}
		out = append(out, byte(r))
	}
	return string(out)
}

// segments is the scenario's body segments as the bytes its server writes:
// each segment's UTF-8, or one byte per character with BodyLatin1.
func (sc googleStreamScenario) segments(t testing.TB) []string {
	t.Helper()
	if !sc.BodyLatin1 {
		return sc.Segments
	}
	out := make([]string, len(sc.Segments))
	for i, seg := range sc.Segments {
		out[i] = latin1(t, seg)
	}
	return out
}

// googleScenarioWrites is what the scenario's server writes for the body
// (before any chunk framing): the encoded body, else each segment when they
// share one write, else the joined segments — pi's outcome too for every
// scenario whose read boundaries do not matter.
func googleScenarioWrites(t testing.TB, sc googleStreamScenario) [][]byte {
	t.Helper()
	switch {
	case sc.EncodedBody != nil:
		return [][]byte{sc.EncodedBody}
	case sc.OneWrite:
		writes := make([][]byte, len(sc.Segments))
		for i, seg := range sc.segments(t) {
			writes[i] = []byte(seg)
		}
		return writes
	}
	return [][]byte{[]byte(strings.Join(sc.segments(t), ""))}
}

// googleScenarioHead is the scenario's response head, as its server writes
// it.
func googleScenarioHead(t testing.TB, sc googleStreamScenario) string {
	t.Helper()
	var head strings.Builder
	status := sc.Status
	if status == "" {
		status = "HTTP/1.1 200 OK"
	}
	head.WriteString(status + "\r\n")
	for _, h := range sc.Headers {
		fmt.Fprintf(&head, "%s: %s\r\n", h[0], h[1])
	}
	if sc.ContentLength {
		n := sc.ContentLengthExtra
		for _, w := range googleScenarioWrites(t, sc) {
			n += len(w)
		}
		fmt.Fprintf(&head, "Content-Length: %d\r\n", n)
	}
	if sc.Framing == "chunked" {
		head.WriteString("Transfer-Encoding: chunked\r\n")
	}
	head.WriteString("\r\n")
	if !sc.HeadLatin1 {
		return head.String()
	}
	return latin1(t, head.String())
}

// googleScenarioResponse is the *http.Response Go's client makes of the
// scenario's head, read by net/http itself.
func googleScenarioResponse(t *testing.T, sc googleStreamScenario) *http.Response {
	t.Helper()
	resp, err := http.ReadResponse(bufio.NewReader(strings.NewReader(googleScenarioHead(t, sc))), nil)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// TestGoogleSSEReadChunksMatchPi replays, read by read, every captured
// scenario whose outcome the SDK's read loop decides — a bare JSON read that
// throws ApiError, a tail left unconsumed, a read rejected by an abort — and
// requires pi's error and the chunks pi observed before it. The SDK checks
// each body read, before buffering it, for a bare JSON {"error":{...}}
// payload (processStreamResponse), so where the reads fall is part of the
// outcome; here each captured segment is one read, as it was for pi (see
// iterateGoogleSSE on where Go's reads fall instead).
func TestGoogleSSEReadChunksMatchPi(t *testing.T) {
	ran := 0
	for _, sc := range loadGoogleStreamCapture(t).Scenarios {
		msg := sc.Pi.ErrorMessage
		if sc.Divergence != "" || sc.EncodedBody != nil || !(strings.HasPrefix(msg, "got status: ") || msg == "Incomplete JSON segment at the end" || msg == "This operation was aborted") {
			continue
		}
		ran++
		t.Run(sc.Name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			headers := googleSDKResponseHeaders(googleScenarioResponse(t, sc))
			events := []string{}
			observe := func(data any) error {
				text, err := jstext.Stringify(googleGenerateContentResponse(data, headers))
				if sc.AbortOn != nil && *sc.AbortOn == len(events) {
					cancel()
				}
				events = append(events, text)
				return err
			}
			body := fetchBody{ctx, &readsReader{reads: slices.Clone(sc.segments(t))}}
			err := iterateGoogleSSE(body, observe, func(any) error { return nil })
			if err == nil || err.Error() != msg {
				t.Fatalf("error %v\npi:   %s", err, msg)
			}
			if !slices.Equal(events, sc.Pi.Events) {
				t.Fatalf("observed %q\npi:       %q", events, sc.Pi.Events)
			}
		})
	}
	if ran < 10 {
		t.Fatalf("only %d read-loop scenarios in the capture", ran)
	}
}

// googleCaptureScenario returns the named scenario of the capture.
func googleCaptureScenario(t *testing.T, name string) googleStreamScenario {
	t.Helper()
	for _, sc := range loadGoogleStreamCapture(t).Scenarios {
		if sc.Name == name {
			return sc
		}
	}
	t.Fatalf("no scenario %q in the capture", name)
	return googleStreamScenario{}
}

// TestGoogleToolCallArgumentsKeepModelOrder: pi's tool-call arguments are the
// parsed functionCall.args object, so JSON.stringify of them (the
// toolcall_delta) and every later replay keep the model's key order, nested
// objects included.
func TestGoogleToolCallArgumentsKeepModelOrder(t *testing.T) {
	sc := googleCaptureScenario(t, "thinking, text and a function call")
	stream := googleServe(t, "gemini-2.5-flash", strings.Join(sc.segments(t), ""))
	var deltas []string
	for ev := range stream.Events() {
		if ev.Type == ai.EventToolCallDelta {
			deltas = append(deltas, ev.Delta)
		}
	}
	var piDeltas []string
	for _, ev := range sc.Pi.Stream {
		if ev.Type == "toolcall_delta" {
			piDeltas = append(piDeltas, strings.TrimPrefix(piStreamEvent(ev.Type, ev.Delta), ev.Type+" "))
		}
	}
	if !slices.Equal(deltas, piDeltas) {
		t.Fatalf("toolcall_delta %q\npi:            %q", deltas, piDeltas)
	}
	content, err := jstext.Stringify(stream.Result().Content)
	if err != nil {
		t.Fatal(err)
	}
	if content != sc.Pi.Content {
		t.Fatalf("content %s\npi:      %s", content, sc.Pi.Content)
	}
}

// serveGoogleScenario answers every request with the scenario's raw
// response — the captured head line for line, then its body
// (googleScenarioWrites, each write one HTTP chunk when chunked), all in one
// write — and reports the Accept-Encoding the last request carried.
func serveGoogleScenario(t *testing.T, sc googleStreamScenario) (baseURL string, acceptEncoding func() string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	var resp bytes.Buffer
	resp.WriteString(googleScenarioHead(t, sc))
	for _, w := range googleScenarioWrites(t, sc) {
		if sc.Framing == "chunked" {
			fmt.Fprintf(&resp, "%x\r\n%s\r\n", len(w), w)
		} else {
			resp.Write(w)
		}
	}
	if sc.Framing == "chunked" && !sc.AbruptEnd {
		resp.WriteString("0\r\n\r\n")
	}
	var mu sync.Mutex
	var gotAcceptEncoding string
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				req, err := http.ReadRequest(bufio.NewReader(conn))
				if err != nil {
					return
				}
				mu.Lock()
				gotAcceptEncoding = strings.Join(req.Header.Values("Accept-Encoding"), ", ")
				mu.Unlock()
				io.Copy(io.Discard, req.Body)
				conn.Write(resp.Bytes())
			}()
		}
	}()
	return "http://" + ln.Addr().String(), func() string {
		mu.Lock()
		defer mu.Unlock()
		return gotAcceptEncoding
	}
}

// googleCaptureModel is the literal model capture.mts streams with.
func googleCaptureModel(baseURL string) *ai.Model {
	return &ai.Model{
		ID: "gemini-2.5-flash", Name: "Gemini 2.5 Flash", Api: ai.APIGoogleGenerativeAI, Provider: "google",
		BaseURL: baseURL, Reasoning: true, Input: []string{"text", "image"},
		Cost:          ai.ModelCost{Input: 0.3, Output: 2.5, CacheRead: 0.03},
		ContextWindow: 1048576, MaxTokens: 65536,
	}
}

// replayGoogleScenario streams the scenario over a real connection, with an
// OnProviderStreamEvent that records what it receives (and throws where the
// scenario says), and describes every way the outcome differs from pi's,
// keyed by the part that differs: "events", each value the callback
// received, as JSON.stringify writes it (key order included); "sameModel",
// whether each call got the model the stream was called with; "stream", the
// assistant stream event by event (with deltas); "stop", the stop reason and
// error message; "responseId"; "content", as JSON; each usage figure
// (googleUsageDiffs); and "acceptEncoding", what the request asked for. No
// differences means the port reproduces pi.
func replayGoogleScenario(t *testing.T, sc googleStreamScenario) map[string]string {
	t.Helper()
	baseURL, acceptEncoding := serveGoogleScenario(t, sc)
	model := googleCaptureModel(baseURL)
	req := ai.NormalizeContext(ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}})
	events := []string{}
	sameModel, calls := true, 0
	var headers ai.ProviderHeaders
	for name, value := range sc.RequestHeaders {
		if headers == nil {
			headers = ai.ProviderHeaders{}
		}
		headers[name] = ai.HeaderValue(value)
	}
	stream := StreamGoogle(context.Background(), model, req, &GoogleOptions{StreamOptions: ai.StreamOptions{
		ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "test-api-key", Headers: headers},
		OnProviderStreamEvent: func(data any, eventModel *ai.Model) error {
			if eventModel != model {
				sameModel = false
			}
			if sc.ThrowOn != nil && *sc.ThrowOn == calls {
				return errors.New(sc.ThrowMessage)
			}
			calls++
			text, err := jstext.Stringify(data)
			events = append(events, text)
			return err
		},
	}})
	var got, want []string
	diffs := map[string]string{}
	for ev := range stream.Events() {
		switch ev.Type {
		case ai.EventTextDelta, ai.EventThinkingDelta, ai.EventToolCallDelta:
			got = append(got, string(ev.Type)+" "+ev.Delta)
		default:
			got = append(got, string(ev.Type))
		}
	}
	for _, ev := range sc.Pi.Stream {
		want = append(want, piStreamEvent(ev.Type, ev.Delta))
	}
	if !slices.Equal(got, want) {
		diffs["stream"] = fmt.Sprintf("stream %q\npi:     %q", got, want)
	}
	final := stream.Result()
	if !slices.Equal(events, sc.Pi.Events) {
		diffs["events"] = fmt.Sprintf("observed %q\npi:       %q", events, sc.Pi.Events)
	}
	if !sameModel || !sc.Pi.SameModel {
		diffs["sameModel"] = fmt.Sprintf("observer's model: same %v, pi same %v", sameModel, sc.Pi.SameModel)
	}
	if string(final.StopReason) != sc.Pi.StopReason || final.ErrorMessage != sc.Pi.ErrorMessage {
		diffs["stop"] = fmt.Sprintf("stop %s %q, pi %s %q", final.StopReason, final.ErrorMessage, sc.Pi.StopReason, sc.Pi.ErrorMessage)
	}
	if !sameGoogleResponseID(final.ResponseID, sc.Pi.ResponseID) {
		diffs["responseId"] = fmt.Sprintf("responseId %q, pi %#v", final.ResponseID, sc.Pi.ResponseID)
	}
	if content, _ := jstext.Stringify(final.Content); content != sc.Pi.Content {
		diffs["content"] = fmt.Sprintf("content %s\npi:      %s", content, sc.Pi.Content)
	}
	maps.Copy(diffs, googleUsageDiffs(final.Usage, sc.Pi.Usage))
	if got := acceptEncoding(); got != sc.Pi.AcceptEncoding {
		diffs["acceptEncoding"] = fmt.Sprintf("request accept-encoding %q, pi %q", got, sc.Pi.AcceptEncoding)
	}
	return diffs
}

// TestGoogleStreamEventsMatchPi replays each captured scenario whose outcome
// does not hang on where the reads fall, over a real connection, and
// requires pi's result (replayGoogleScenario).
func TestGoogleStreamEventsMatchPi(t *testing.T) {
	ran := 0
	for _, sc := range loadGoogleStreamCapture(t).Scenarios {
		if sc.Divergence != "" || sc.ReadBoundariesMatter {
			continue
		}
		ran++
		t.Run(sc.Name, func(t *testing.T) {
			diffs := replayGoogleScenario(t, sc)
			for _, part := range slices.Sorted(maps.Keys(diffs)) {
				t.Error(diffs[part])
			}
		})
	}
	if ran < 15 {
		t.Fatalf("only %d replayable scenarios in the capture", ran)
	}
}

// TestGoogleDivergencesStillDiffer is the tripwire for the captured
// scenarios tagged `divergence`: measured differences the port carries and
// the ledger records. Each is replayed the way TestGoogleStreamEventsMatchPi
// replays the rest. It must differ from pi in exactly the parts its
// divergentFields name and match pi everywhere else: a named part that has
// come to match pi fails here until it is dropped from the list (and, with
// the last one, the tag and its ledger row are retired), and a part that
// differs without being named is a regression. It asserts only that the
// named parts are not pi's, never what the port does instead.
func TestGoogleDivergencesStillDiffer(t *testing.T) {
	ran := 0
	for _, sc := range loadGoogleStreamCapture(t).Scenarios {
		if sc.Divergence == "" {
			continue
		}
		ran++
		t.Run(sc.Name, func(t *testing.T) {
			if len(sc.DivergentFields) == 0 {
				t.Fatalf("the scenario names no divergentFields; list the parts where the port differs from pi (%s)", sc.Divergence)
			}
			diffs := replayGoogleScenario(t, sc)
			for _, part := range sc.DivergentFields {
				if _, differs := diffs[part]; !differs {
					t.Errorf("FIXED: %s now matches pi; drop it from the scenario's divergentFields, and with the last one its divergence tag and ledger row (%s)", part, sc.Divergence)
				}
			}
			for _, part := range slices.Sorted(maps.Keys(diffs)) {
				if !slices.Contains(sc.DivergentFields, part) {
					t.Errorf("differs from pi outside its recorded divergence: %s", diffs[part])
				}
			}
		})
	}
	if ran == 0 {
		t.Fatal("no divergence scenarios in the capture")
	}
}

// TestGoogleAbortFromCallbackFailsNextRead is the captured scenario "an abort
// from the callback fails the next read" end to end: the server sends the
// first segment and holds the connection, the callback cancels, the chunk in
// hand is still normalized, and the stream ends aborted with undici's
// AbortError message and no text_end.
func TestGoogleAbortFromCallbackFailsNextRead(t *testing.T) {
	sc := googleCaptureScenario(t, "an abort from the callback fails the next read")
	first := sc.segments(t)[0]
	release := make(chan struct{})
	defer close(release)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, first)
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	model := googleCaptureModel(server.URL)
	req := ai.NormalizeContext(ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}})
	stream := StreamGoogle(ctx, model, req, &GoogleOptions{StreamOptions: ai.StreamOptions{
		ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "test-api-key"},
		OnProviderStreamEvent: func(any, *ai.Model) error {
			cancel()
			return nil
		},
	}})
	var got, want []string
	for ev := range stream.Events() {
		got = append(got, string(ev.Type))
	}
	for _, ev := range sc.Pi.Stream {
		want = append(want, ev.Type)
	}
	final := stream.Result()
	if !slices.Equal(got, want) {
		t.Errorf("stream %v, pi %v", got, want)
	}
	if string(final.StopReason) != sc.Pi.StopReason || final.ErrorMessage != sc.Pi.ErrorMessage {
		t.Errorf("stop %s %q, pi %s %q", final.StopReason, final.ErrorMessage, sc.Pi.StopReason, sc.Pi.ErrorMessage)
	}
	if content, _ := jstext.Stringify(final.Content); content != sc.Pi.Content {
		t.Errorf("content %s, pi %s", content, sc.Pi.Content)
	}
}

// abortingReader cancels its context from inside Read and fails the read, the
// way an http body read does when its request's context ends mid-read.
type abortingReader struct{ cancel context.CancelFunc }

func (r abortingReader) Read([]byte) (int, error) {
	r.cancel()
	return 0, context.Canceled
}

// TestGoogleAbortDuringReadIsAbortError: a read of the body StreamGoogle
// reads (fetchBody) that fails because the request was aborted surfaces as
// undici's AbortError, not the transport's error text.
func TestGoogleAbortDuringReadIsAbortError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := iterateGoogleSSE(fetchBody{ctx, abortingReader{cancel}}, nil, func(any) error { return nil })
	if err == nil || err.Error() != "This operation was aborted" {
		t.Fatalf("error %v, want This operation was aborted", err)
	}
}

// TestGoogleEachChunkGetsItsOwnHeaderRecord: @google/genai builds the
// sdkHttpResponse.headers record afresh for every chunk (a new HttpResponse
// per data: event), so an observer that writes to one chunk's record leaves
// the next chunk's as pi's capture has it.
func TestGoogleEachChunkGetsItsOwnHeaderRecord(t *testing.T) {
	sc := googleCaptureScenario(t, "upstream two chunks")
	baseURL, _ := serveGoogleScenario(t, sc)
	model := googleCaptureModel(baseURL)
	req := ai.NormalizeContext(ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}})
	var events []string
	stream := StreamGoogle(context.Background(), model, req, &GoogleOptions{StreamOptions: ai.StreamOptions{
		ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "test-api-key"},
		OnProviderStreamEvent: func(data any, _ *ai.Model) error {
			text, err := jstext.Stringify(data)
			events = append(events, text)
			// Write into this chunk's record, after recording it.
			for _, f := range data.(ai.OrderedObject) {
				if f.Key == "sdkHttpResponse" {
					headers := f.Value.(ai.OrderedObject)[0].Value.(ai.OrderedObject)
					for i := range headers {
						headers[i].Value = "written by the observer"
					}
				}
			}
			return err
		},
	}})
	for range stream.Events() {
	}
	if !slices.Equal(events, sc.Pi.Events) {
		t.Fatalf("observed %q\npi:       %q", events, sc.Pi.Events)
	}
}
