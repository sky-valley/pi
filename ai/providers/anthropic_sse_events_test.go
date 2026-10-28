package providers

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/sky-valley/pi/ai"
)

// anthropicSSEEventsCaptureFile is written by
// testdata/anthropic-sse/capture-anthropic-sse-events.mts, which streams pi's
// anthropic-messages adapter from source at the named sha.
const anthropicSSEEventsCaptureFile = "testdata/anthropic-sse/anthropic-sse-events-f715a15c1.json"

type anthropicSSEEventsRow struct {
	Name string `json:"name"`
	SSE  string `json:"sse"`
	// SSEBase64 is the body instead when it is not valid UTF-8.
	SSEBase64  []byte `json:"sseBase64"`
	ThrowAt    *int   `json:"throwAt"`
	AbortFirst bool   `json:"abortFirst"`
	// OAuth rows stream with an OAuth token and one current tool, Read.
	OAuth bool `json:"oauth"`
	// RawSeed rows end with a block member pi holds as a non-string — the
	// raw value it was seeded with, or a number `+` made of it — which the
	// port's string field cannot hold.
	RawSeed bool `json:"rawSeed"`
	// The outcome of the body whole, in one read.
	anthropicSSEOutcome
	// OneByte is the outcome of the body read one byte per read, when it is
	// not the whole body's.
	OneByte *anthropicSSEOutcome `json:"oneByte"`
}

// anthropicSSEOutcome is what pi made of a row's body read one way.
type anthropicSSEOutcome struct {
	V8Cause  bool     `json:"v8Cause"`
	Observed []string `json:"observed"`
	// Pushed holds each pushed event's type; pi's null (no type) decodes to "".
	Pushed  []string `json:"pushed"`
	Message struct {
		StopReason   ai.StopReason   `json:"stopReason"`
		ErrorMessage string          `json:"errorMessage"`
		ResponseID   string          `json:"responseId"`
		Content      json.RawMessage `json:"content"`
		// Usage is decoded into ai.Usage, so pi's explicit zero and the port's
		// omitted zero (omitempty) compare equal, as the in-memory values are.
		Usage       ai.Usage `json:"usage"`
		Diagnostics []struct {
			Type string `json:"type"`
			// Details is pi's details as JSON.stringify wrote them.
			Details json.RawMessage `json:"details"`
		} `json:"diagnostics"`
	} `json:"message"`
}

// body is the row's exact body.
func (r anthropicSSEEventsRow) body() string {
	if r.SSEBase64 != nil {
		return string(r.SSEBase64)
	}
	return r.SSE
}

func loadAnthropicSSEEventsCapture(t *testing.T) []anthropicSSEEventsRow {
	t.Helper()
	data, err := os.ReadFile(anthropicSSEEventsCaptureFile)
	if err != nil {
		t.Fatalf("read %s: %v (regenerate it with testdata/anthropic-sse/capture-anthropic-sse-events.mts)", anthropicSSEEventsCaptureFile, err)
	}
	var capture struct {
		Model string                  `json:"model"`
		Rows  []anthropicSSEEventsRow `json:"rows"`
	}
	if err := json.Unmarshal(data, &capture); err != nil {
		t.Fatalf("decode %s: %v", anthropicSSEEventsCaptureFile, err)
	}
	if capture.Model != "anthropic/claude-haiku-4-5" || len(capture.Rows) == 0 {
		t.Fatalf("%s: model %q, %d rows", anthropicSSEEventsCaptureFile, capture.Model, len(capture.Rows))
	}
	return capture.Rows
}

// streamAnthropicSSEEvents streams body on claude-haiku-4-5 and returns the
// model it streamed, the type of every pushed event and the final message.
// With oauth it streams as the capture's oauth rows do: with an OAuth token,
// and a leading system message that adds one tool, Read.
func streamAnthropicSSEEvents(t *testing.T, ctx context.Context, body string, oauth bool, opts ai.StreamOptions) (*ai.Model, []string, *ai.AssistantMessage) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(200)
		io.WriteString(w, body)
	}))
	defer server.Close()
	base := ai.GetModel("anthropic", "claude-haiku-4-5")
	if base == nil {
		t.Fatal("catalog has no anthropic/claude-haiku-4-5")
	}
	model := *base
	model.BaseURL = server.URL
	opts.APIKey = "k"
	messages := []ai.Message{ai.NewUserText("Hello", 1)}
	if oauth {
		opts.APIKey = "sk-ant-oat01-capture"
		system := ai.NewSystemText("You are a test.", 1)
		system.ToolsAdded = []ai.Tool{{Name: "Read", Description: "Read a file", Parameters: ai.Object()}}
		messages = append([]ai.Message{system}, messages...)
	}
	stream := StreamAnthropic(ctx, &model, ai.NormalizeContext(ai.Context{Messages: messages}), &AnthropicOptions{StreamOptions: opts})
	var pushed []string
	for ev := range stream.Events() {
		pushed = append(pushed, string(ev.Type))
	}
	return &model, pushed, stream.Result()
}

// assertAnthropicMessageMatchesPi compares the final message with pi's in the
// outcome it names (the whole body's, or its one-byte reading's). Where pi's
// error embeds V8's JSON.parse text (v8Cause), which the port does not
// reproduce, the text on either side of it must still be pi's exactly.
func assertAnthropicMessageMatchesPi(t *testing.T, row anthropicSSEEventsRow, outcome anthropicSSEOutcome, final *ai.AssistantMessage) {
	t.Helper()
	want := outcome.Message
	if final.StopReason != want.StopReason {
		t.Errorf("stopReason = %s, want %s (%s)", final.StopReason, want.StopReason, final.ErrorMessage)
	}
	if outcome.V8Cause {
		gotHead, gotCause, gotTail, gotOK := splitAnthropicParseFailure(final.ErrorMessage)
		wantHead, _, wantTail, wantOK := splitAnthropicParseFailure(want.ErrorMessage)
		if !gotOK || !wantOK || gotHead != wantHead || gotTail != wantTail || gotCause == "" {
			t.Errorf("errorMessage = %q, want %q around the JSON.parse cause", final.ErrorMessage, want.ErrorMessage)
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
	if row.RawSeed {
		dropRawSeedMembers(g, w)
	}
	if !reflect.DeepEqual(g, w) {
		t.Errorf("content = %s, want %s", gotContent, want.Content)
	}
	if final.Usage != want.Usage {
		t.Errorf("usage = %+v, want %+v", final.Usage, want.Usage)
	}
	// Each diagnostic's details are pi's JSON text, byte for byte: key order
	// and numbers as JSON.stringify writes them. (No captured details hold <,
	// > or &, which encoding/json would escape where JSON.stringify does not.)
	if len(final.Diagnostics) != len(want.Diagnostics) {
		t.Fatalf("diagnostics = %+v, want %d", final.Diagnostics, len(want.Diagnostics))
	}
	for i, d := range final.Diagnostics {
		details, err := json.Marshal(d.Details)
		if err != nil {
			t.Fatalf("marshal diagnostic details: %v", err)
		}
		if d.Type != want.Diagnostics[i].Type || string(details) != string(want.Diagnostics[i].Details) {
			t.Errorf("diagnostic %d = %s %s, want %s %s", i, d.Type, details, want.Diagnostics[i].Type, want.Diagnostics[i].Details)
		}
	}
}

// dropRawSeedMembers removes, from both decoded contents, each block's text,
// thinking or thinkingSignature that pi holds as a non-string — the raw value
// content_block_start seeded it with, which no delta converted before the
// stream ended, or a number `+` made of it that no string delta followed.
// The port's string field cannot hold such a value; every other member is
// still compared.
func dropRawSeedMembers(got, want any) {
	gotBlocks, _ := got.([]any)
	wantBlocks, _ := want.([]any)
	for i, wb := range wantBlocks {
		wm, _ := wb.(map[string]any)
		if i >= len(gotBlocks) || wm == nil {
			continue
		}
		gm, _ := gotBlocks[i].(map[string]any)
		for _, key := range []string{"text", "thinking", "thinkingSignature"} {
			if v, ok := wm[key]; ok {
				if _, isString := v.(string); !isString {
					delete(wm, key)
					delete(gm, key)
				}
			}
		}
	}
}

// splitAnthropicParseFailure splits "Could not parse Anthropic SSE event <name>:
// <cause>; data=<data>; raw=<raw>" around its cause.
func splitAnthropicParseFailure(msg string) (head, cause, tail string, ok bool) {
	const lead = "Could not parse Anthropic SSE event "
	if !strings.HasPrefix(msg, lead) {
		return "", "", "", false
	}
	colon := strings.Index(msg[len(lead):], ": ")
	if colon < 0 {
		return "", "", "", false
	}
	headEnd := len(lead) + colon + 2
	data := strings.Index(msg[headEnd:], "; data=")
	if data < 0 {
		return "", "", "", false
	}
	return msg[:headEnd], msg[headEnd : headEnd+data], msg[headEnd+data:], true
}

// jsonStringify renders a value an observer received as JSON.stringify renders
// the value JSON.parse made (stringifyOrderedValue).
func jsonStringify(t *testing.T, v any) string {
	t.Helper()
	s, ok := stringifyOrderedValue(v)
	if !ok {
		// Errorf, not Fatalf: the observer runs on the stream's goroutine.
		t.Errorf("observer received %#v, which holds a type JSON.parse never produces", v)
	}
	return s
}

// providerStreamObserver records what OnProviderStreamEvent receives, and on
// the event at index throwAt (when set) cancels the request if abortFirst and
// returns "observer boom" — the capture's observer.
type providerStreamObserver struct {
	t          *testing.T
	throwAt    *int
	abortFirst bool
	cancel     context.CancelFunc
	observed   []string
	models     []*ai.Model
}

func (o *providerStreamObserver) observe(data any, model *ai.Model) error {
	o.observed = append(o.observed, jsonStringify(o.t, data))
	o.models = append(o.models, model)
	if o.throwAt != nil && *o.throwAt == len(o.observed)-1 {
		if o.abortFirst {
			o.cancel()
		}
		return errors.New("observer boom")
	}
	return nil
}

// assertModel requires every observed event to carry model, the one the
// stream was called with (pi passes its `model` argument).
func (o *providerStreamObserver) assertModel(model *ai.Model) {
	o.t.Helper()
	for i, m := range o.models {
		if m != model {
			o.t.Errorf("observed event %d carries model %p, want the streamed model %p", i, m, model)
		}
	}
}

// TestAnthropicSSEEventsMatchPi replays each captured body and requires pi's
// observed events, pushed events and final message. Named events whose data
// is JSON but not an object are observed and then passed over; a `null` event
// fails with pi's TypeError text before the observer sees it; a parse failure
// carries pi's data= and raw= suffix — raw being every line of the event,
// comments included, joined with a literal backslash-n; an event's members are
// read as pi reads them, so a mistyped one never fails the event while a
// property of a missing sub-object fails it with V8's TypeError; and an
// observer error fails the stream with its message, aborted when the request
// was cancelled.
func TestAnthropicSSEEventsMatchPi(t *testing.T) {
	for _, row := range loadAnthropicSSEEventsCapture(t) {
		t.Run(row.Name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			obs := &providerStreamObserver{t: t, throwAt: row.ThrowAt, abortFirst: row.AbortFirst, cancel: cancel}
			model, pushed, final := streamAnthropicSSEEvents(t, ctx, row.body(), row.OAuth, ai.StreamOptions{OnProviderStreamEvent: obs.observe})
			if !reflect.DeepEqual(obs.observed, row.Observed) && (len(obs.observed) > 0 || len(row.Observed) > 0) {
				t.Errorf("observed = %q, want %q", obs.observed, row.Observed)
			}
			obs.assertModel(model)
			if !reflect.DeepEqual(pushed, row.Pushed) {
				t.Errorf("pushed = %q, want %q", pushed, row.Pushed)
			}
			assertAnthropicMessageMatchesPi(t, row, row.anthropicSSEOutcome, final)
			// pi's pushed events and message do not depend on observing. How
			// the body arrives is another matter: pi splits lines per read, so
			// a "\r" that ends a read ends its line, and a "\r\n" split across
			// two reads is two line breaks — read one byte per read, a body
			// with CRLF line endings fails where the whole body in one read
			// does not (the row's oneByte outcome). A byte-order mark or a
			// multi-byte character split across reads changes nothing.
			if row.ThrowAt == nil {
				whole := ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{HTTPClient: cannedDoer{status: http.StatusOK, body: row.body()}}}
				_, pushed, final := streamAnthropicSSEEvents(t, context.Background(), row.body(), row.OAuth, whole)
				if !reflect.DeepEqual(pushed, row.Pushed) {
					t.Errorf("without an observer: pushed = %q, want %q", pushed, row.Pushed)
				}
				assertAnthropicMessageMatchesPi(t, row, row.anthropicSSEOutcome, final)
				want := row.anthropicSSEOutcome
				if row.OneByte != nil {
					want = *row.OneByte
				}
				oneByte := ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{HTTPClient: cannedDoer{status: http.StatusOK, body: row.body(), oneByte: true}}}
				_, pushed, final = streamAnthropicSSEEvents(t, context.Background(), row.body(), row.OAuth, oneByte)
				if !reflect.DeepEqual(pushed, want.Pushed) {
					t.Errorf("one byte per read: pushed = %q, want %q", pushed, want.Pushed)
				}
				assertAnthropicMessageMatchesPi(t, row, want, final)
			}
		})
	}
}

// TestAnthropicForwardsProviderStreamEventsInOrder transliterates upstream's
// 'forwards parsed provider stream events in order' (002fc8385): every message
// event reaches the observer, in order, with the model the stream was called
// with.
func TestAnthropicForwardsProviderStreamEventsInOrder(t *testing.T) {
	sse := `event: message_start
data: {"type":"message_start","message":{"id":"msg_test","usage":{"input_tokens":12,"output_tokens":0,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":12,"output_tokens":5,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}

event: message_stop
data: {"type":"message_stop"}
`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, sse)
	}))
	defer server.Close()
	base := ai.GetModel("anthropic", "claude-haiku-4-5")
	if base == nil {
		t.Fatal("catalog has no anthropic/claude-haiku-4-5")
	}
	model := *base
	model.BaseURL = server.URL
	var types []string
	var models []*ai.Model
	result := StreamAnthropic(context.Background(), &model, ai.NormalizeContext(ai.Context{Messages: []ai.Message{ai.NewUserText("Hello", 1)}}),
		&AnthropicOptions{StreamOptions: ai.StreamOptions{
			ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "k"},
			OnProviderStreamEvent: func(data any, eventModel *ai.Model) error {
				event, _ := data.(ai.OrderedObject)
				typ, _ := event.Plain()["type"].(string)
				types = append(types, typ)
				models = append(models, eventModel)
				return nil
			},
		}}).Result()

	if result.StopReason != ai.StopStop {
		t.Fatalf("stopReason = %s, want stop (%s)", result.StopReason, result.ErrorMessage)
	}
	want := []string{"message_start", "content_block_start", "content_block_delta", "content_block_stop", "message_delta", "message_stop"}
	if !reflect.DeepEqual(types, want) {
		t.Errorf("event types = %q, want %q", types, want)
	}
	if len(models) != len(want) {
		t.Fatalf("observer called %d times, want %d", len(models), len(want))
	}
	for i, m := range models {
		if m != &model {
			t.Errorf("event %d: observer got model %p, want the streamed model %p", i, m, &model)
		}
	}
}

// TestClaudeCodeNameOfReadsTheNameOnlyWithTools pins pi's fromClaudeCodeName
// guard (anthropic-messages.ts at 8676a0dcd): `name.toLowerCase()` runs only
// when there are current tools, so with none a name that is not a string is
// passed through (the port holds it as "") instead of throwing. The thrown
// texts are the oauth rows' of the capture.
func TestClaudeCodeNameOfReadsTheNameOnlyWithTools(t *testing.T) {
	tools := []ai.Tool{{Name: "Read", Description: "Read a file", Parameters: ai.Object()}}
	for _, tc := range []struct {
		raw  json.RawMessage
		want string
	}{
		{nil, "Cannot read properties of undefined (reading 'toLowerCase')"},
		{json.RawMessage(`null`), "Cannot read properties of null (reading 'toLowerCase')"},
		{json.RawMessage(`5`), "name.toLowerCase is not a function"},
	} {
		if _, err := claudeCodeNameOf(tc.raw, tools); err == nil || err.Error() != tc.want {
			t.Errorf("claudeCodeNameOf(%s) with tools: error %v, want %q", tc.raw, err, tc.want)
		}
		if name, err := claudeCodeNameOf(tc.raw, nil); err != nil || name != "" {
			t.Errorf("claudeCodeNameOf(%s) without tools = %q, %v; want the name passed through", tc.raw, name, err)
		}
	}
	if name, err := claudeCodeNameOf(json.RawMessage(`"read"`), tools); err != nil || name != "Read" {
		t.Errorf(`claudeCodeNameOf("read") = %q, %v; want "Read"`, name, err)
	}
}
