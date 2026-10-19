package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/sky-valley/pi/ai"
)

// TestSetupErrorMatchesPi replays testdata/setup-error (pi-ai 1.3.0, captured
// with an empty environment): a compat streamSimple with no credentials fails
// its eager auth assertion before a stream exists, so pi's lazyApi builds the
// result with createSetupErrorMessage — errorMessage BEFORE timestamp — while
// the compat stream fails inside the adapter's own output object, errorMessage
// after timestamp. The port renders the eager throw as ai.ErrorStream (G3), so
// the two entry points end with pi's events and the same keys in the same
// order.
func TestSetupErrorMatchesPi(t *testing.T) {
	data, err := os.ReadFile("testdata/setup-error/setup-error-1.3.0.json")
	if err != nil {
		t.Fatal(err)
	}
	var capture struct {
		Rows []struct {
			Name         string   `json:"name"`
			Entry        string   `json:"entry"`
			Model        ai.Model `json:"model"`
			Events       []string `json:"events"`
			Keys         []string `json:"keys"`
			StopReason   string   `json:"stopReason"`
			ErrorMessage string   `json:"errorMessage"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(data, &capture); err != nil {
		t.Fatal(err)
	}
	if len(capture.Rows) == 0 {
		t.Fatal("the setup-error capture holds no rows")
	}
	// No provider may find a key, as in the capture's empty environment.
	for _, kv := range os.Environ() {
		key, _, _ := strings.Cut(kv, "=")
		t.Setenv(key, "")
		os.Unsetenv(key)
	}
	req := ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}
	for _, row := range capture.Rows {
		t.Run(row.Name, func(t *testing.T) {
			model := row.Model
			var stream *ai.AssistantMessageEventStream
			if row.Entry == "stream" {
				stream = ai.Stream(context.Background(), &model, req, &ai.StreamOptions{})
			} else {
				stream = ai.StreamSimple(context.Background(), &model, req, &ai.SimpleStreamOptions{Reasoning: ai.ThinkingHigh})
			}
			var events []string
			for event := range stream.Events() {
				events = append(events, string(event.Type))
			}
			result := stream.Result()
			if result == nil {
				t.Fatal("no result")
			}
			raw, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			if keys := topLevelKeys(t, raw); !slices.Equal(keys, row.Keys) {
				t.Errorf("result keys %q, pi %q", keys, row.Keys)
			}
			if !slices.Equal(events, row.Events) || string(result.StopReason) != row.StopReason || result.ErrorMessage != row.ErrorMessage {
				t.Errorf("events %q stop %s error %q, pi %q %s %q", events, result.StopReason, result.ErrorMessage, row.Events, row.StopReason, row.ErrorMessage)
			}
		})
	}
}

// topLevelKeys are a JSON object's keys in the order they were written.
func topLevelKeys(t *testing.T, raw []byte) []string {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if _, err := decoder.Token(); err != nil {
		t.Fatal(err)
	}
	var keys []string
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			t.Fatal(err)
		}
		keys = append(keys, token.(string))
		var skip json.RawMessage
		if err := decoder.Decode(&skip); err != nil {
			t.Fatal(err)
		}
	}
	return keys
}

// pi's azure provider resolves the endpoint inside its own lazyStream before
// dispatching to openai-completions' streamSimple, whose api-key assertion
// therefore comes second: an azure model with neither an endpoint nor a key
// reports the endpoint. With an endpoint, the missing key is reported.
func TestAzureSimpleStreamResolvesTheEndpointBeforeTheKey(t *testing.T) {
	t.Setenv("AZURE_OPENAI_BASE_URL", "")
	t.Setenv("AZURE_OPENAI_RESOURCE_NAME", "")
	t.Setenv("AZURE_OPENAI_API_KEY", "")
	model := &ai.Model{ID: "gpt-5", Api: ai.APIOpenAICompletions, Provider: azureProvider}
	req := ai.NormalizeContext(ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}})
	_, final := drainSimple(StreamSimpleOpenAICompletions(context.Background(), model, req, nil))
	if final.ErrorMessage != errAzureBaseURLRequired.Error() {
		t.Fatalf("without an endpoint or a key: error %q, want the endpoint's %q", final.ErrorMessage, errAzureBaseURLRequired)
	}
	model.BaseURL = "https://example.openai.azure.com"
	_, final = drainSimple(StreamSimpleOpenAICompletions(context.Background(), model, req, nil))
	if final.ErrorMessage != "No API key for provider: azure" {
		t.Fatalf("with an endpoint and no key: error %q", final.ErrorMessage)
	}
}
