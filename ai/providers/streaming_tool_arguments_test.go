package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/sky-valley/pi/ai"
	"github.com/sky-valley/pi/internal/jstext"
)

// streamingToolArgumentsCaptureFile is what testdata/streaming-tool-arguments/
// capture.mts recorded from pi's anthropic-messages, openai-completions,
// openai-responses, pi-messages and faux adapters at f715a15c1: the partial
// arguments on every toolcall event and the message each stream ends with.
const streamingToolArgumentsCaptureFile = "testdata/streaming-tool-arguments/streaming-tool-arguments-f715a15c1.json"

// jsArgumentsText is JSON.stringify of a value in DecodeOrderedValue's or
// encoding/json's shapes, lone surrogates (WTF-8) escaped as JS writes them.
func jsArgumentsText(v any) string {
	switch t := v.(type) {
	case nil:
		return "null"
	case bool:
		return fmt.Sprint(t)
	case float64:
		if math.IsInf(t, 0) || math.IsNaN(t) {
			return "null"
		}
		return jstext.NumberToString(t)
	case string:
		return jsQuote(t)
	case ai.OrderedObject:
		parts := make([]string, len(t))
		for i, f := range t {
			parts[i] = jsQuote(f.Key) + ":" + jsArgumentsText(f.Value)
		}
		return "{" + strings.Join(parts, ",") + "}"
	case map[string]any:
		// One member has one order; more have none recorded.
		if len(t) > 1 {
			panic(fmt.Sprintf("arguments %v have no recorded order", t))
		}
		for k, v := range t {
			return "{" + jsQuote(k) + ":" + jsArgumentsText(v) + "}"
		}
		return "{}"
	case []any:
		parts := make([]string, len(t))
		for i, e := range t {
			parts[i] = jsArgumentsText(e)
		}
		return "[" + strings.Join(parts, ",") + "]"
	}
	panic(fmt.Sprintf("jsArgumentsText: %T", v))
}

// TestStreamedToolCallArgumentsLikePi replays each capture row through the
// port's adapter and compares every toolcall event — type, content index,
// delta and the call's partial arguments — and the final message.
func TestStreamedToolCallArgumentsLikePi(t *testing.T) {
	data, err := os.ReadFile(streamingToolArgumentsCaptureFile)
	if err != nil {
		t.Fatal(err)
	}
	var capture struct {
		Rows []struct {
			API        string            `json:"api"`
			Name       string            `json:"name"`
			Body       string            `json:"body"`
			Events     []json.RawMessage `json:"events"`
			Arguments  string            `json:"arguments"`
			ToolEvents []struct {
				Type         string          `json:"type"`
				ContentIndex int             `json:"contentIndex"`
				Delta        json.RawMessage `json:"delta"`
				Arguments    *string         `json:"arguments"`
			} `json:"toolEvents"`
			StopReason   string  `json:"stopReason"`
			ErrorMessage *string `json:"errorMessage"`
			Content      string  `json:"content"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(data, &capture); err != nil {
		t.Fatalf("%s: %v; rerun capture.mts", streamingToolArgumentsCaptureFile, err)
	}
	if len(capture.Rows) < 60 {
		t.Fatalf("%s holds %d rows; rerun capture.mts", streamingToolArgumentsCaptureFile, len(capture.Rows))
	}
	// nonStringDeltas are the rows whose pi delta is not a string (see below).
	var nonStringDeltas []string
	defer func() {
		want := "anthropic-messages/number delta,openai-completions/number arguments,openai-responses/delta number,pi-messages/number delta"
		if got := strings.Join(nonStringDeltas, ","); got != want {
			t.Errorf("rows with a delta that is not a string: %s, want %s", got, want)
		}
	}()
	for _, row := range capture.Rows {
		t.Run(row.API+"/"+row.Name, func(t *testing.T) {
			body := row.Body
			if row.API == "openai-responses" {
				var b strings.Builder
				for _, e := range row.Events {
					var compact bytes.Buffer
					if err := json.Compact(&compact, e); err != nil {
						t.Fatal(err)
					}
					fmt.Fprintf(&b, "data: %s\n\n", compact.Bytes())
				}
				body = b.String()
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("content-type", "text/event-stream")
				io.WriteString(w, body)
			}))
			defer server.Close()
			req := ai.NormalizeContext(ai.Context{Messages: []ai.Message{ai.NewUserText("write", 0)}})
			base := ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "k"}}
			var stream *ai.AssistantMessageEventStream
			switch row.API {
			case "anthropic-messages":
				model := &ai.Model{ID: "claude-test", Api: ai.APIAnthropicMessages, Provider: "anthropic", BaseURL: server.URL, Input: []string{"text"}, ContextWindow: 200000, MaxTokens: 64000}
				stream = StreamAnthropic(context.Background(), model, req, &AnthropicOptions{StreamOptions: base})
			case "openai-completions":
				model := &ai.Model{ID: "gpt-test", Api: ai.APIOpenAICompletions, Provider: "openai", BaseURL: server.URL, Input: []string{"text"}, ContextWindow: 200000, MaxTokens: 64000}
				stream = StreamOpenAICompletions(context.Background(), model, req, &OpenAIOptions{StreamOptions: base})
			case "openai-responses":
				model := &ai.Model{ID: "gpt-test", Api: ai.APIOpenAIResponses, Provider: "openai", BaseURL: server.URL, Input: []string{"text"}, ContextWindow: 200000, MaxTokens: 64000}
				stream = StreamOpenAIResponses(context.Background(), model, req, &OpenAIResponsesOptions{StreamOptions: base})
			case "pi-messages":
				stream = StreamPiMessages(context.Background(), piMessagesTestModel(server.URL+"/v1"), req, &PiMessagesOptions{StreamOptions: base})
			case "faux":
				args, order, err := ai.DecodeOrderedObject([]byte(row.Arguments))
				if err != nil {
					t.Fatal(err)
				}
				call := FauxToolCall("write", args, "call_1")
				call.ArgumentsOrder = order
				reg := RegisterFauxProvider(RegisterFauxProviderOptions{MinTokenSize: 1, MaxTokenSize: 1})
				defer reg.Unregister()
				reg.SetResponses([]FauxResponseStep{FauxStatic(FauxAssistantMessage(ai.ContentList{call}, ai.StopToolUse))})
				stream = ai.StreamSimple(context.Background(), reg.GetModel(), ai.Context{Messages: []ai.Message{ai.NewUserText("write", 0)}}, nil)
			default:
				t.Fatalf("row for api %q; rerun capture.mts", row.API)
			}
			i := 0
			for e := range stream.Events() {
				switch e.Type {
				case ai.EventToolCallStart, ai.EventToolCallDelta, ai.EventToolCallEnd:
				default:
					continue
				}
				if i >= len(row.ToolEvents) {
					t.Errorf("extra %s event at %d", e.Type, e.ContentIndex)
					continue
				}
				want := row.ToolEvents[i]
				i++
				got := struct {
					typ      string
					index    int
					delta    string
					hasDelta bool
					args     string
					hasArgs  bool
				}{typ: string(e.Type), index: e.ContentIndex, delta: e.Delta, hasDelta: e.Type == ai.EventToolCallDelta}
				if e.Partial != nil && e.ContentIndex < len(e.Partial.Content) {
					if tc, ok := e.Partial.Content[e.ContentIndex].(ai.ToolCall); ok {
						got.args, got.hasArgs = jsArgumentsText(tc.OrderedArguments()), true
					}
				}
				wantDelta, wantArgs := "", ""
				if want.Delta != nil {
					// Read as JSON.parse reads it: half a surrogate pair is kept.
					v, err := ai.DecodeOrderedValueKeepingSurrogates(want.Delta)
					if err != nil {
						t.Fatal(err)
					}
					if text, isString := v.(string); isString {
						wantDelta = text
					} else {
						// KNOWN: pi pushes a delta that is not a string as itself;
						// the port's Delta is a string and carries String(delta).
						nonStringDeltas = append(nonStringDeltas, row.API+"/"+row.Name)
						wantDelta, _ = jsToString(v)
					}
				}
				if want.Arguments != nil {
					wantArgs = *want.Arguments
				}
				if got.typ != want.Type || got.index != want.ContentIndex || got.hasDelta != (want.Delta != nil) || got.delta != wantDelta ||
					got.hasArgs != (want.Arguments != nil) || got.args != wantArgs {
					t.Errorf("event %d: %s[%d] delta %q arguments %s\n                pi %s[%d] delta %q arguments %s",
						i-1, got.typ, got.index, got.delta, got.args, want.Type, want.ContentIndex, wantDelta, wantArgs)
				}
			}
			if i != len(row.ToolEvents) {
				t.Errorf("%d toolcall events, pi %d", i, len(row.ToolEvents))
			}
			final := stream.Result()
			content, err := json.Marshal(final.Content)
			if err != nil {
				t.Fatal(err)
			}
			wantErr := ""
			if row.ErrorMessage != nil {
				wantErr = *row.ErrorMessage
			}
			if string(final.StopReason) != row.StopReason || final.ErrorMessage != wantErr || string(content) != row.Content {
				t.Errorf("ended %s %q with %s\n   pi %s %q with %s", final.StopReason, final.ErrorMessage, content, row.StopReason, wantErr, row.Content)
			}
		})
	}
}
