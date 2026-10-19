package providers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sky-valley/pi/ai"
)

// The model authored these keys in this order, and pi replays them in it: a JS
// object keeps insertion order, so the transcript a model is conditioned on is
// byte-identical to what it wrote. Nested objects and objects inside arrays are
// part of the same guarantee.
const orderedArgsJSON = `{"path":"/tmp","depth":1,"filter":{"z":true,"a":null},"tags":[{"y":1,"b":2}]}`

const orderedToolCallMessage = `{
	"role": "assistant",
	"content": [{"type": "toolCall", "id": "call_1", "name": "list_dir", "arguments": ` + orderedArgsJSON + `}],
	"api": "openai-completions",
	"provider": "openai",
	"model": "gpt-4o-mini",
	"usage": {"input": 10, "output": 5, "cacheRead": 0, "cacheWrite": 0},
	"stopReason": "toolUse",
	"timestamp": 2
}`

func loadOrderedToolCallContext(t *testing.T) ai.Context {
	t.Helper()
	msg, err := ai.UnmarshalMessage([]byte(orderedToolCallMessage))
	if err != nil {
		t.Fatalf("UnmarshalMessage: %v", err)
	}
	return ai.Context{Messages: []ai.Message{ai.NewUserText("what is in /tmp?", 1), msg}}
}

func mustMarshal(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(raw)
}

// A session file is the only place the order is stored: it is the key order of
// the "arguments" object itself, exactly as pi writes it. Loading a message and
// writing it back must not reshuffle it.
func TestToolCallArgumentsSurviveSessionRoundTrip(t *testing.T) {
	msg, err := ai.UnmarshalMessage([]byte(orderedToolCallMessage))
	if err != nil {
		t.Fatalf("UnmarshalMessage: %v", err)
	}
	got := mustMarshal(t, msg)
	if !strings.Contains(got, `"arguments":`+orderedArgsJSON) {
		t.Fatalf("session round trip lost argument key order:\n got %s\nwant arguments %s", got, orderedArgsJSON)
	}
}

// openai-completions carries `arguments` as a STRING, so replaying a prior tool
// call in a different key order conditions the model on literally different
// text (and shifts the prompt-cache prefix).
func TestOpenAIReplaysToolCallArgumentsInModelOrder(t *testing.T) {
	model := &ai.Model{ID: "gpt-4o-mini", Api: ai.APIOpenAICompletions, Provider: "openai", MaxTokens: 1024}
	params, err := buildOpenAIParams(model, ai.NormalizeContext(loadOrderedToolCallContext(t)), &OpenAIOptions{})
	if err != nil {
		t.Fatalf("buildOpenAIParams: %v", err)
	}
	body := mustMarshal(t, params)
	want := `"arguments":` + mustMarshal(t, orderedArgsJSON)
	if !strings.Contains(body, want) {
		t.Fatalf("openai-completions body lost argument key order:\n got %s\nwant %s", body, want)
	}
}

func TestOpenAIResponsesReplaysToolCallArgumentsInModelOrder(t *testing.T) {
	model := &ai.Model{ID: "gpt-5", Api: ai.APIOpenAIResponses, Provider: "openai", MaxTokens: 1024}
	params, err := buildResponsesParams(model, ai.NormalizeContext(loadOrderedToolCallContext(t)), &OpenAIResponsesOptions{})
	if err != nil {
		t.Fatalf("buildResponsesParams: %v", err)
	}
	body := mustMarshal(t, params)
	want := `"arguments":` + mustMarshal(t, orderedArgsJSON)
	if !strings.Contains(body, want) {
		t.Fatalf("openai-responses body lost argument key order:\n got %s\nwant %s", body, want)
	}
}

func TestAnthropicReplaysToolCallArgumentsInModelOrder(t *testing.T) {
	model := &ai.Model{ID: "claude-sonnet-4", Api: ai.APIAnthropicMessages, Provider: "anthropic", MaxTokens: 1024}
	params, err := buildAnthropicParams(model, ai.NormalizeContext(loadOrderedToolCallContext(t)), false, &AnthropicOptions{}, false)
	if err != nil {
		t.Fatalf("buildAnthropicParams: %v", err)
	}
	body := mustMarshal(t, params.body)
	if !strings.Contains(body, `"input":`+orderedArgsJSON) {
		t.Fatalf("anthropic body lost argument key order:\n got %s\nwant input %s", body, orderedArgsJSON)
	}
}

func TestGoogleReplaysToolCallArgumentsInModelOrder(t *testing.T) {
	model := &ai.Model{ID: "gemini-2.5-pro", Api: ai.APIGoogleGenerativeAI, Provider: "google", MaxTokens: 1024}
	params, err := buildGoogleParams(model, ai.NormalizeContext(loadOrderedToolCallContext(t)), &GoogleOptions{})
	if err != nil {
		t.Fatalf("buildGoogleParams: %v", err)
	}
	body := mustMarshal(t, params)
	if !strings.Contains(body, `"args":`+orderedArgsJSON) {
		t.Fatalf("google body lost argument key order:\n got %s\nwant args %s", body, orderedArgsJSON)
	}
}

// The order has to be captured where the arguments are first parsed, not just
// where they are reloaded: a tool call streamed in this turn is replayed in the
// next request of the same session.
func TestOpenAIStreamedToolCallKeepsArgumentOrder(t *testing.T) {
	sse := "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"function\":{\"name\":\"list_dir\",\"arguments\":" +
		mustMarshal(t, orderedArgsJSON) + "}}]}}]}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, sse)
	}))
	defer server.Close()

	model := &ai.Model{
		ID: "gpt-4o-mini", Api: ai.APIOpenAICompletions, Provider: "openai", BaseURL: server.URL,
		MaxTokens: 1024,
	}
	final := StreamOpenAICompletions(context.Background(), model, ai.NormalizeContext(
		ai.Context{Messages: []ai.Message{ai.NewUserText("what is in /tmp?", 1)}}),

		&OpenAIOptions{StreamOptions: ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "sk-test"}}}).
		Result()
	if final.StopReason != ai.StopToolUse {
		t.Fatalf("expected toolUse, got %s (%s)", final.StopReason, final.ErrorMessage)
	}

	params, err := buildOpenAIParams(model, ai.NormalizeContext(ai.Context{Messages: []ai.Message{final}}), &OpenAIOptions{})
	if err != nil {
		t.Fatalf("buildOpenAIParams: %v", err)
	}
	body := mustMarshal(t, params)
	want := `"arguments":` + mustMarshal(t, orderedArgsJSON)
	if !strings.Contains(body, want) {
		t.Fatalf("streamed tool call lost argument key order on replay:\n got %s\nwant %s", body, want)
	}
}

// Arguments stays authoritative: a caller that replaces it gets the values it
// set, not a stale ordered copy.
func TestOrderedArgumentsIgnoredWhenArgumentsReplaced(t *testing.T) {
	msg, err := ai.UnmarshalMessage([]byte(orderedToolCallMessage))
	if err != nil {
		t.Fatalf("UnmarshalMessage: %v", err)
	}
	assistant := msg.(ai.AssistantMessage)
	tc := assistant.Content[0].(ai.ToolCall)
	tc.Arguments = map[string]any{"path": "/redacted"}

	if got := mustMarshal(t, tc); !strings.Contains(got, `"arguments":{"path":"/redacted"}`) {
		t.Fatalf("stale order won over replaced arguments: %s", got)
	}
}

// The validation failure text is handed back to the model as a tool result, and
// pi builds it with JSON.stringify(toolCall.arguments) — insertion order.
func TestValidationErrorEchoesArgumentsInModelOrder(t *testing.T) {
	msg, err := ai.UnmarshalMessage([]byte(orderedToolCallMessage))
	if err != nil {
		t.Fatalf("UnmarshalMessage: %v", err)
	}
	tc := msg.(ai.AssistantMessage).Content[0].(ai.ToolCall)
	tool := ai.Tool{Name: "list_dir", Parameters: ai.Object(ai.Prop("missing", ai.String()))}

	_, err = ai.ValidateToolArguments(tool, tc)
	if err == nil {
		t.Fatal("expected validation to fail on the missing required property")
	}
	if !strings.Contains(err.Error(), "\"path\": \"/tmp\",\n  \"depth\": 1,") {
		t.Fatalf("validation error echoed arguments out of model order:\n%s", err)
	}
}
