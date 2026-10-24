package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/sky-valley/pi/ai"
)

// piMessagesEventsCaptureFile is written by
// testdata/pi-messages/capture-pi-messages-events.mts, which streams pi's
// pi-messages adapter from source at the named sha.
const piMessagesEventsCaptureFile = "testdata/pi-messages/pi-messages-events-f715a15c1.json"

type piMessagesEventsRow struct {
	Name string `json:"name"`
	// Status, when set, is the response's status, and SSE its JSON body.
	Status int    `json:"status"`
	SSE    string `json:"sse"`
	// SSEBase64 is the body instead when it is not valid UTF-8.
	SSEBase64 []byte `json:"sseBase64"`
	// V8Error is the frame data whose JSON.parse failure is pi's errorMessage.
	V8Error    string   `json:"v8Error"`
	ThrowAt    *int     `json:"throwAt"`
	AbortFirst bool     `json:"abortFirst"`
	Observed   []string `json:"observed"`
	// Pushed holds each pushed event's type; pi's null (no type) decodes to "".
	Pushed []string `json:"pushed"`
	// PushedIndex holds each pushed event's contentIndex; null for none.
	PushedIndex []json.RawMessage `json:"pushedIndex"`
	Message     struct {
		StopReason   ai.StopReason   `json:"stopReason"`
		ErrorMessage string          `json:"errorMessage"`
		ResponseID   string          `json:"responseId"`
		Content      json.RawMessage `json:"content"`
		// Usage is decoded into ai.Usage, so pi's explicit zero and the port's
		// omitted zero (omitempty) compare equal, as the in-memory values are.
		Usage       ai.Usage `json:"usage"`
		Diagnostics []struct {
			Type  string `json:"type"`
			Error *struct {
				Name    string          `json:"name"`
				Message string          `json:"message"`
				Code    json.RawMessage `json:"code"`
			} `json:"error"`
			// Details is pi's details as JSON.stringify wrote them, a
			// timestampMs recorded as 0.
			Details json.RawMessage `json:"details"`
		} `json:"diagnostics"`
	} `json:"message"`
}

func loadPiMessagesEventsCapture(t *testing.T) []piMessagesEventsRow {
	t.Helper()
	data, err := os.ReadFile(piMessagesEventsCaptureFile)
	if err != nil {
		t.Fatalf("read %s: %v (regenerate it with testdata/pi-messages/capture-pi-messages-events.mts)", piMessagesEventsCaptureFile, err)
	}
	var capture struct {
		Rows []piMessagesEventsRow `json:"rows"`
	}
	if err := json.Unmarshal(data, &capture); err != nil {
		t.Fatalf("decode %s: %v", piMessagesEventsCaptureFile, err)
	}
	if len(capture.Rows) == 0 {
		t.Fatalf("%s has no rows", piMessagesEventsCaptureFile)
	}
	return capture.Rows
}

// body is the row's exact body.
func (r piMessagesEventsRow) body() string {
	if r.SSEBase64 != nil {
		return string(r.SSEBase64)
	}
	return r.SSE
}

// piMessagesCaptureBaseURL is the capture model's baseUrl.
const piMessagesCaptureBaseURL = "http://pi-messages.invalid/v1"

// cannedDoer answers every request with status and body, as a fetch stub
// does: a 200 as an event stream, any other status as JSON. oneByte serves the
// body one byte per read.
type cannedDoer struct {
	status  int
	body    string
	oneByte bool
}

func (d cannedDoer) Do(req *http.Request) (*http.Response, error) {
	contentType := "application/json"
	if d.status == http.StatusOK {
		contentType = "text/event-stream"
	}
	var body io.Reader = strings.NewReader(d.body)
	if d.oneByte {
		body = iotest.OneByteReader(body)
	}
	return &http.Response{
		StatusCode: d.status,
		Status:     fmt.Sprintf("%d %s", d.status, http.StatusText(d.status)),
		Header:     http.Header{"Content-Type": {contentType}},
		Body:       io.NopCloser(body),
		Request:    req,
	}, nil
}

// streamPiMessagesRow streams a captured row: a status row's body is served
// with that status from the capture model's baseUrl, which the response
// failure's details carry, and any other row's through
// streamPiMessagesEvents.
func streamPiMessagesRow(t *testing.T, ctx context.Context, row piMessagesEventsRow, opts ai.StreamOptions) (*ai.Model, []ai.AssistantMessageEvent, *ai.AssistantMessage) {
	t.Helper()
	if row.Status == 0 {
		return streamPiMessagesEvents(t, ctx, row.body(), opts)
	}
	return streamPiMessagesCanned(ctx, cannedDoer{status: row.Status, body: row.body()}, opts)
}

// streamPiMessagesCanned streams what doer serves from the capture model's
// baseUrl.
func streamPiMessagesCanned(ctx context.Context, doer cannedDoer, opts ai.StreamOptions) (*ai.Model, []ai.AssistantMessageEvent, *ai.AssistantMessage) {
	model := piMessagesTestModel(piMessagesCaptureBaseURL)
	opts.APIKey = "test-key"
	opts.HTTPClient = doer
	stream := StreamSimplePiMessages(ctx, model, ai.NormalizeContext(piMessagesTestContext()), &ai.SimpleStreamOptions{StreamOptions: opts})
	var pushed []ai.AssistantMessageEvent
	for ev := range stream.Events() {
		pushed = append(pushed, ev)
	}
	return model, pushed, stream.Result()
}

// streamPiMessagesEvents streams body through StreamSimplePiMessages on the
// upstream suite's model and returns the model, every pushed event and the
// final message.
func streamPiMessagesEvents(t *testing.T, ctx context.Context, body string, opts ai.StreamOptions) (*ai.Model, []ai.AssistantMessageEvent, *ai.AssistantMessage) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, body)
	}))
	defer server.Close()
	model := piMessagesTestModel(server.URL + "/v1")
	opts.APIKey = "test-key"
	stream := StreamSimplePiMessages(ctx, model, ai.NormalizeContext(piMessagesTestContext()), &ai.SimpleStreamOptions{StreamOptions: opts})
	var pushed []ai.AssistantMessageEvent
	for ev := range stream.Events() {
		pushed = append(pushed, ev)
	}
	return model, pushed, stream.Result()
}

// assertPiMessagesPushedMatchesPi compares the pushed events with pi's: their
// types, and the contentIndex of each per-block event. pi's event carries the
// wire event's own contentIndex; the port's carries the slot that value
// addresses (see rawPropertyKey), or -1 when it addresses none, since an int
// cannot hold what pi's event then holds ("01", 1.5, undefined).
func assertPiMessagesPushedMatchesPi(t *testing.T, row piMessagesEventsRow, pushed []ai.AssistantMessageEvent) {
	t.Helper()
	types := make([]string, len(pushed))
	for i, ev := range pushed {
		types[i] = string(ev.Type)
	}
	if !reflect.DeepEqual(types, row.Pushed) {
		t.Errorf("pushed = %q, want %q", types, row.Pushed)
		return
	}
	if len(row.PushedIndex) != len(pushed) {
		t.Fatalf("capture has %d pushed indices for %d pushed events", len(row.PushedIndex), len(pushed))
	}
	for i, ev := range pushed {
		switch ev.Type {
		case ai.EventTextStart, ai.EventTextDelta, ai.EventTextEnd,
			ai.EventThinkingStart, ai.EventThinkingDelta, ai.EventThinkingEnd,
			ai.EventToolCallStart, ai.EventToolCallDelta, ai.EventToolCallEnd:
		default:
			continue
		}
		_, slot, isSlot, err := rawPropertyKey(row.PushedIndex[i])
		if err != nil {
			t.Fatalf("pushed event %d: pi's contentIndex %s: %v", i, row.PushedIndex[i], err)
		}
		if !isSlot {
			slot = -1
		}
		if ev.ContentIndex != slot {
			t.Errorf("pushed event %d (%s): contentIndex = %d, want %d for pi's %s", i, ev.Type, ev.ContentIndex, slot, row.PushedIndex[i])
		}
	}
}

// assertPiMessagesMessageMatchesPi compares the final message with pi's. A
// v8Error row's message is V8's JSON.parse text for that frame's data, which
// the port does not reproduce (encoding/json words the failure its own way);
// assertPiMessagesFrameSyntaxError pins what is pi's there — that the read
// fails on that frame with a JSON syntax error.
func assertPiMessagesMessageMatchesPi(t *testing.T, row piMessagesEventsRow, final *ai.AssistantMessage) {
	t.Helper()
	want := row.Message
	if final.StopReason != want.StopReason {
		t.Errorf("stopReason = %s, want %s (%s)", final.StopReason, want.StopReason, final.ErrorMessage)
	}
	if row.V8Error != "" {
		if final.ErrorMessage == "" {
			t.Errorf("errorMessage is empty, want the JSON parse failure of %q (pi: %q)", row.V8Error, want.ErrorMessage)
		}
	} else if final.ErrorMessage != want.ErrorMessage {
		t.Errorf("errorMessage = %q, want %q", final.ErrorMessage, want.ErrorMessage)
	}
	if final.ResponseID != want.ResponseID {
		t.Errorf("responseId = %q, want %q", final.ResponseID, want.ResponseID)
	}
	gotContent, err := json.Marshal(final.Content)
	if err != nil {
		t.Fatalf("marshal content: %v", err)
	}
	var g, w any
	if err := json.Unmarshal(gotContent, &g); err != nil {
		t.Fatalf("decode content: %v", err)
	}
	if err := json.Unmarshal(want.Content, &w); err != nil {
		t.Fatalf("decode pi content: %v", err)
	}
	if !reflect.DeepEqual(g, w) {
		t.Errorf("content = %s, want %s", gotContent, want.Content)
	}
	if final.Usage != want.Usage {
		t.Errorf("usage = %+v, want %+v", final.Usage, want.Usage)
	}
	assertPiMessagesDiagnosticsMatchPi(t, row, final.Diagnostics)
	// The message persists as pi's does and reads back unchanged.
	persisted, err := json.Marshal(final)
	if err != nil {
		t.Fatalf("marshal message: %v", err)
	}
	back, err := ai.UnmarshalMessage(persisted)
	if err != nil {
		t.Fatalf("the persisted message does not read back: %v", err)
	}
	t.Run("readBack", func(t *testing.T) {
		assertPiMessagesDiagnosticsMatchPi(t, row, back.(ai.AssistantMessage).Diagnostics)
	})
}

// assertPiMessagesDiagnosticsMatchPi compares diagnostics with pi's: type,
// error and the details' JSON text, byte for byte — key order and numbers as
// JSON.stringify writes them, with a timestampMs taken as 0 as the capture
// records it. (No captured details hold <, > or &, which encoding/json would
// escape where JSON.stringify does not.)
func assertPiMessagesDiagnosticsMatchPi(t *testing.T, row piMessagesEventsRow, diagnostics []ai.Diagnostic) {
	t.Helper()
	want := row.Message.Diagnostics
	if len(diagnostics) != len(want) {
		t.Fatalf("diagnostics = %+v, want %d", diagnostics, len(want))
	}
	for i, d := range diagnostics {
		details := slices.Clone(d.Details)
		for j, f := range details {
			if f.Key == "timestampMs" {
				details[j].Value = 0
			}
		}
		gotDetails := []byte("null")
		if details != nil {
			var err error
			if gotDetails, err = json.Marshal(details); err != nil {
				t.Fatalf("marshal diagnostic details: %v", err)
			}
		}
		if d.Type != want[i].Type || string(gotDetails) != string(want[i].Details) {
			t.Errorf("diagnostic %d = %s %s, want %s %s", i, d.Type, gotDetails, want[i].Type, want[i].Details)
		}
		switch w := want[i].Error; {
		case w == nil && d.Error != nil:
			t.Errorf("diagnostic %d has error %+v, want none", i, d.Error)
		case w != nil && d.Error == nil:
			t.Errorf("diagnostic %d has no error, want %+v", i, w)
		case w != nil:
			code, err := json.Marshal(d.Error.Code)
			if err != nil {
				t.Fatal(err)
			}
			if d.Error.Code == nil {
				code = nil
			}
			if d.Error.Name != w.Name || d.Error.Message != w.Message || string(code) != string(w.Code) {
				t.Errorf("diagnostic %d error = {%q %q %s}, want {%q %q %s}", i, d.Error.Name, d.Error.Message, code, w.Name, w.Message, w.Code)
			}
		}
	}
}

// assertPiMessagesFrameSyntaxError requires reading a v8Error row's body to
// fail with a JSON syntax error, as pi's JSON.parse throws a SyntaxError.
func assertPiMessagesFrameSyntaxError(t *testing.T, row piMessagesEventsRow) {
	t.Helper()
	err := readPiMessagesEvents(strings.NewReader(row.body()), nil, func(piMessagesEvent) (bool, error) { return true, nil })
	var syntaxErr *json.SyntaxError
	if !errors.As(err, &syntaxErr) {
		t.Errorf("reading the body returned %v, want the JSON syntax error of frame data %q", err, row.V8Error)
	}
}

// TestPiMessagesEventsMatchPi replays each captured body and requires pi's
// observed events, pushed events and final message: a JS-falsy frame is
// skipped unobserved, any other value that is not an object is observed and
// converts to an event with no type, an object converts whatever its members
// hold, a frame that is not JSON fails the stream, the terminal done is
// observed before it converts, and an observer error fails the stream with its
// message — aborted when the request was cancelled, and never done even when
// thrown on the done event.
//
// A row whose observer does not throw is replayed with no observer too: pi's
// pushed events and message do not depend on observing, and without an
// observer the port decodes each frame once, never into the observer's value.
// It is replayed one byte per read as well: what a body yields does not depend
// on how it arrives (a byte-order mark, a CRLF or a multi-byte character split
// across reads included).
func TestPiMessagesEventsMatchPi(t *testing.T) {
	for _, row := range loadPiMessagesEventsCapture(t) {
		t.Run(row.Name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			obs := &providerStreamObserver{t: t, throwAt: row.ThrowAt, abortFirst: row.AbortFirst, cancel: cancel}
			model, pushed, final := streamPiMessagesRow(t, ctx, row, ai.StreamOptions{OnProviderStreamEvent: obs.observe})
			if !reflect.DeepEqual(obs.observed, row.Observed) && (len(obs.observed) > 0 || len(row.Observed) > 0) {
				t.Errorf("observed = %q, want %q", obs.observed, row.Observed)
			}
			obs.assertModel(model)
			assertPiMessagesPushedMatchesPi(t, row, pushed)
			assertPiMessagesMessageMatchesPi(t, row, final)
			if row.V8Error != "" {
				assertPiMessagesFrameSyntaxError(t, row)
			}
			if row.ThrowAt == nil {
				_, pushed, final := streamPiMessagesRow(t, context.Background(), row, ai.StreamOptions{})
				t.Run("withoutObserver", func(t *testing.T) {
					assertPiMessagesPushedMatchesPi(t, row, pushed)
					assertPiMessagesMessageMatchesPi(t, row, final)
				})
				status := row.Status
				if status == 0 {
					status = http.StatusOK
				}
				_, pushed, final = streamPiMessagesCanned(context.Background(), cannedDoer{status: status, body: row.body(), oneByte: true}, ai.StreamOptions{})
				t.Run("oneByteReads", func(t *testing.T) {
					assertPiMessagesPushedMatchesPi(t, row, pushed)
					assertPiMessagesMessageMatchesPi(t, row, final)
				})
			}
		})
	}
}

// TestPiMessagesForwardsWireEventsInOrder transliterates upstream's 'forwards
// parsed wire events in order before converting them' (002fc8385): the
// observer receives each wire event as parsed — unknown fields and the
// terminal done included — with the model the stream was called with.
func TestPiMessagesForwardsWireEventsInOrder(t *testing.T) {
	wireEvents := []string{
		`{"type":"start"}`,
		`{"type":"text_start","contentIndex":0}`,
		`{"type":"text_delta","contentIndex":0,"delta":"Hello","gatewayField":"upstream-value"}`,
		`{"type":"text_end","contentIndex":0,"content":"Hello"}`,
		`{"type":"done","reason":"stop","usage":` + piMessagesUsageJSON + `,"responseId":"resp_1"}`,
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, piMessagesSSE(wireEvents...))
	}))
	defer server.Close()
	model := piMessagesTestModel(server.URL + "/v1")
	var received []any
	var models []*ai.Model
	message := StreamSimplePiMessages(context.Background(), model, ai.NormalizeContext(piMessagesTestContext()), &ai.SimpleStreamOptions{
		StreamOptions: ai.StreamOptions{
			ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "test-key"},
			OnProviderStreamEvent: func(data any, eventModel *ai.Model) error {
				received = append(received, data)
				models = append(models, eventModel)
				return nil
			},
		},
	}).Result()

	want := make([]any, len(wireEvents))
	for i, e := range wireEvents {
		v, err := ai.DecodeOrderedValue([]byte(e))
		if err != nil {
			t.Fatal(err)
		}
		want[i] = v
	}
	if !reflect.DeepEqual(received, want) {
		t.Errorf("received = %v, want %v", received, want)
	}
	if len(models) != len(wireEvents) {
		t.Fatalf("observer called %d times, want %d", len(models), len(wireEvents))
	}
	for i, m := range models {
		if m != model {
			t.Errorf("event %d: observer got model %p, want %p", i, m, model)
		}
	}
	if message.StopReason != ai.StopStop {
		t.Fatalf("stopReason = %s, want stop (%s)", message.StopReason, message.ErrorMessage)
	}
	if message.ResponseID != "resp_1" {
		t.Errorf("responseId = %q, want resp_1", message.ResponseID)
	}
	content, _ := json.Marshal(message.Content)
	if string(content) != `[{"type":"text","text":"Hello"}]` {
		t.Errorf("content = %s, want [{\"type\":\"text\",\"text\":\"Hello\"}]", content)
	}
}

// TestPiMessagesDecodesFramesOnceWithoutAnObserver locks the cost of the
// observer seam (spec: decode for the observer only when there is one): the
// value an observer receives is its own decode of the frame
// (ai.DecodeOrderedValue), so reading a body without an observer must save at
// least those allocations over reading it with one.
func TestPiMessagesDecodesFramesOnceWithoutAnObserver(t *testing.T) {
	frames := []string{
		`{"type":"start"}`,
		`{"type":"text_start","contentIndex":0}`,
		`{"type":"text_delta","contentIndex":0,"delta":"Hello there","gatewayField":"upstream-value"}`,
		`{"type":"text_end","contentIndex":0,"content":"Hello there"}`,
	}
	body := piMessagesSSE(frames...)
	keepReading := func(piMessagesEvent) (bool, error) { return true, nil }
	read := func(onEvent func(any) error) float64 {
		return testing.AllocsPerRun(50, func() {
			if err := readPiMessagesEvents(strings.NewReader(body), onEvent, keepReading); err != nil {
				t.Fatal(err)
			}
		})
	}
	without := read(nil)
	with := read(func(any) error { return nil })
	observerValues := testing.AllocsPerRun(50, func() {
		for _, f := range frames {
			if _, err := ai.DecodeOrderedValue([]byte(f)); err != nil {
				t.Fatal(err)
			}
		}
	})
	if without > with-observerValues*0.9 {
		t.Fatalf("reading without an observer allocates %.0f times, with one %.0f; the observer's values alone take %.0f, so frames are being decoded for an observer that is not there", without, with, observerValues)
	}
}
