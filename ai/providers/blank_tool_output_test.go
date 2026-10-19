package providers

import (
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"testing"

	"github.com/sky-valley/pi/ai"
)

// blankToolOutputCaptureFile is what testdata/blank-tool-output/capture.mts
// recorded from pi's source at 4dd2b149d: the messages / input / contents the
// openai-completions, openai-responses and google-generative-ai adapters
// build for a tool result whose text blocks are blank, whitespace or text,
// with and without an image, for text-only and image models.
const blankToolOutputCaptureFile = "testdata/blank-tool-output/blank-tool-output-4dd2b149d.json"

// A tool result's text counts only when it is not blank by JS trim (upstream
// 4dd2b149d, #8720): a bare "\r\n" from bash gets the empty-output
// placeholder, "(see attached image)" beside an image, and Google's empty
// output, as an empty text does. U+200B and U+0085 are not JS whitespace.
func TestBlankToolOutputMatchesPi(t *testing.T) {
	data, err := os.ReadFile(blankToolOutputCaptureFile)
	if err != nil {
		t.Fatal(err)
	}
	var capture struct {
		Rows []struct {
			API     ai.Api          `json:"api"`
			Input   []string        `json:"input"`
			IsError bool            `json:"isError"`
			Content ai.ContentList  `json:"content"`
			Request json.RawMessage `json:"request"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(data, &capture); err != nil {
		t.Fatalf("%s: %v; rerun capture.mts", blankToolOutputCaptureFile, err)
	}
	if len(capture.Rows) == 0 {
		t.Fatalf("%s holds no rows; rerun capture.mts", blankToolOutputCaptureFile)
	}
	for _, row := range capture.Rows {
		model := &ai.Model{ID: "gpt-x", Api: row.API, Provider: "openai", BaseURL: "https://api.openai.com/v1",
			Input: row.Input, ContextWindow: 100000, MaxTokens: 100}
		stream := StreamSimpleOpenAICompletions
		key := "messages"
		switch row.API {
		case ai.APIOpenAIResponses:
			stream, key = StreamSimpleOpenAIResponses, "input"
		case ai.APIGoogleGenerativeAI:
			model.ID, model.Provider, model.BaseURL = "gemini-3-pro-preview", "google", "https://generativelanguage.googleapis.com/v1beta"
			stream, key = StreamSimpleGoogle, "contents"
		}
		req := ai.Context{Messages: []ai.Message{
			ai.NewUserText("run", 0),
			ai.AssistantMessage{Api: row.API, Provider: model.Provider, Model: model.ID, StopReason: ai.StopToolUse,
				Content: ai.ContentList{ai.ToolCall{ID: "call_1", Name: "bash", Arguments: map[string]any{"command": "true"}}}},
			ai.ToolResultMessage{ToolCallID: "call_1", ToolName: "bash", Content: row.Content, IsError: row.IsError},
		}}
		var payload any
		opts := &ai.SimpleStreamOptions{}
		opts.APIKey = "k"
		opts.OnPayload = func(p any, _ *ai.Model) (any, error) {
			payload = p
			return nil, errors.New("captured")
		}
		final := stream(t.Context(), model, ai.NormalizeContext(req), opts).Result()
		if payload == nil {
			t.Fatalf("%s: no payload (%s %q)", row.API, final.StopReason, final.ErrorMessage)
		}
		body, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]any
		_ = json.Unmarshal(body, &fields)
		var request string
		var want any
		if err := json.Unmarshal(row.Request, &request); err != nil {
			t.Fatal(err)
		}
		_ = json.Unmarshal([]byte(request), &want)
		if !reflect.DeepEqual(fields[key], want) {
			got, _ := json.Marshal(fields[key])
			content, _ := json.Marshal(row.Content)
			t.Errorf("%s %v isError %v, tool result %s:\n%s\npi\n%s", row.API, row.Input, row.IsError, content, got, request)
		}
	}
}
