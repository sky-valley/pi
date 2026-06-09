package coding

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/sky-valley/pi/agent"
	"github.com/sky-valley/pi/ai"
	"github.com/sky-valley/pi/ai/providers"
)

// These tests hit real provider endpoints. They are skipped unless the relevant
// API key is present, so the normal `go test ./...` stays hermetic. To run them:
//
//	OPENAI_API_KEY=sk-...     go test ./coding/ -run Live -v
//	ANTHROPIC_API_KEY=sk-...  go test ./coding/ -run Live -v
//	GEMINI_API_KEY=...        go test ./coding/ -run Live -v

func liveModel(t *testing.T, spec, keyEnv string) (*ai.Model, string) {
	t.Helper()
	providers.RegisterBuiltins()
	key := os.Getenv(keyEnv)
	if key == "" {
		t.Skipf("skipping live test: %s not set", keyEnv)
	}
	model, err := ResolveModel(spec)
	if err != nil {
		t.Fatal(err)
	}
	return model, key
}

// TestLiveOpenAIToolCall drives a real end-to-end agent turn against OpenAI: the
// model is given a tool, asked a question that requires it, and must call the
// tool and answer. This exercises the actual wire format, SSE parsing, tool
// streaming, the agent loop, and usage/cost accounting against the live API.
func TestLiveOpenAIToolCall(t *testing.T) {
	model, key := liveModel(t, "openai/gpt-5-mini", "OPENAI_API_KEY")
	runLiveToolCall(t, model, key)
}

func TestLiveOpenAICompletions(t *testing.T) {
	model, key := liveModel(t, "openai/gpt-4.1-mini", "OPENAI_API_KEY")
	runLiveToolCall(t, model, key)
}

func TestLiveAnthropicToolCall(t *testing.T) {
	model, key := liveModel(t, "anthropic/claude-haiku-4-5", "ANTHROPIC_API_KEY")
	runLiveToolCall(t, model, key)
}

func TestLiveGoogleToolCall(t *testing.T) {
	model, key := liveModel(t, "google/gemini-2.5-flash", "GEMINI_API_KEY")
	runLiveToolCall(t, model, key)
}

func runLiveToolCall(t *testing.T, model *ai.Model, key string) {
	t.Helper()
	called := false
	weather := agent.AgentTool{
		Name:        "get_weather",
		Description: "Get the current weather for a city. Always use this for weather questions.",
		Parameters:  ai.Object(ai.Prop("city", ai.String("City name"))),
		Execute: func(ctx context.Context, id string, params map[string]any, onUpdate agent.ToolUpdateFunc) (agent.AgentToolResult, error) {
			called = true
			city, _ := params["city"].(string)
			return agent.AgentToolResult{Content: ai.ContentList{ai.TextContent{Text: "22C and sunny in " + city}}}, nil
		},
	}

	sess := NewSession(SessionOptions{
		Model:        model,
		Cwd:          t.TempDir(),
		Tools:        []agent.AgentTool{weather},
		SystemPrompt: "You are a helpful assistant. Use tools when relevant.",
		APIKey:       key,
		SessionID:    "live-test",
	})

	res, err := sess.Run(context.Background(), "What's the weather in Paris? Use the get_weather tool.")
	if err != nil {
		t.Fatalf("live run failed: %v", err)
	}
	if !called {
		t.Fatalf("model did not call the tool; final text: %q", res.Text)
	}
	if !strings.Contains(strings.ToLower(res.Text), "paris") && !strings.Contains(res.Text, "22") {
		t.Logf("warning: final text did not mention the weather result: %q", res.Text)
	}
	if res.Usage.TotalTokens == 0 {
		t.Fatalf("no token usage reported from live API: %+v", res.Usage)
	}
	t.Logf("OK %s/%s: tool called, %d in / %d out tokens, $%.5f, final: %q",
		model.Provider, model.ID, res.Usage.Input, res.Usage.Output, res.Usage.Cost.Total, truncate(res.Text, 120))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
