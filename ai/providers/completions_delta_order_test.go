package providers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"testing"

	"github.com/sky-valley/pi/ai"
)

// completionsDeltaOrderCaptureFile is what testdata/completions-delta-order/
// capture.mjs recorded from pi-ai 1.3.0's openai-completions stream: bodies
// whose deltas carry several of reasoning, reasoning_details, content and
// tool_calls at once, with every event's type and contentIndex and the final
// content.
const completionsDeltaOrderCaptureFile = "testdata/completions-delta-order/delta-order-1.3.0.json"

// A completions delta is handled in a fixed order — the reasoning fields, then
// reasoning_details, then content, then tool_calls (upstream 63035734f) — so a
// proxy that sends reasoning and answer in one delta (LiteLLM, #10926) still
// gets the thinking block before the text block.
func TestOpenAICompletionsDeltaOrderMatchesPi(t *testing.T) {
	data, err := os.ReadFile(completionsDeltaOrderCaptureFile)
	if err != nil {
		t.Fatal(err)
	}
	var capture struct {
		Rows []struct {
			Name         string   `json:"name"`
			Body         string   `json:"body"`
			Events       [][2]any `json:"events"`
			StopReason   string   `json:"stopReason"`
			ErrorMessage *string  `json:"errorMessage"`
			Content      string   `json:"content"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(data, &capture); err != nil {
		t.Fatalf("%s: %v; rerun capture.mjs", completionsDeltaOrderCaptureFile, err)
	}
	if len(capture.Rows) == 0 {
		t.Fatalf("%s holds no rows; rerun capture.mjs", completionsDeltaOrderCaptureFile)
	}
	for _, row := range capture.Rows {
		t.Run(row.Name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("content-type", "text/event-stream")
				io.WriteString(w, row.Body)
			}))
			defer server.Close()
			model := &ai.Model{ID: "gpt-x", Api: ai.APIOpenAICompletions, Provider: "openai", BaseURL: server.URL,
				Reasoning: true, Input: []string{"text"}, ContextWindow: 100000, MaxTokens: 100}
			stream := StreamOpenAICompletions(context.Background(), model,
				ai.NormalizeContext(ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 0)}}),
				&OpenAIOptions{StreamOptions: ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "k"}}})
			var events [][2]any
			for e := range stream.Events() {
				var index any
				switch e.Type {
				case ai.EventStart, ai.EventDone, ai.EventError:
				default:
					index = float64(e.ContentIndex)
				}
				events = append(events, [2]any{string(e.Type), index})
			}
			final := stream.Result()
			content, err := json.Marshal(final.Content)
			if err != nil {
				t.Fatal(err)
			}
			if string(final.StopReason) != row.StopReason || string(content) != row.Content {
				t.Errorf("ended %s with %s\npi    %s with %s", final.StopReason, content, row.StopReason, row.Content)
			}
			if !reflect.DeepEqual(events, row.Events) {
				t.Errorf("events %v\npi     %v", events, row.Events)
			}
		})
	}
}
