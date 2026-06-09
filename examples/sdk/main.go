// Command sdk-example shows how to embed pi-go as an SDK in your own app,
// driving the OpenAI API with a custom tool and streaming events.
//
//	OPENAI_API_KEY=sk-... go run ./examples/sdk
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/sky-valley/pi/agent"
	"github.com/sky-valley/pi/ai"
	"github.com/sky-valley/pi/ai/providers"
	"github.com/sky-valley/pi/coding"
)

func main() {
	// 1. Register the real provider APIs (Anthropic, OpenAI completions+responses, Google).
	providers.RegisterBuiltins()

	// 2. Resolve a model from the embedded catalog. gpt-5 uses the Responses API.
	model, err := coding.ResolveModel("openai/gpt-5")
	if err != nil {
		fail(err)
	}
	apiKey := ai.GetEnvApiKey(model.Provider) // reads OPENAI_API_KEY
	if apiKey == "" {
		fail(fmt.Errorf("set OPENAI_API_KEY"))
	}

	// 3. Define an app-specific tool. Tools are plain values; an agent can call
	//    any action your app can perform.
	weather := agent.AgentTool{
		Name:        "get_weather",
		Description: "Get the current weather for a city.",
		Parameters:  ai.Object(ai.Prop("city", ai.String("City name"))),
		Execute: func(ctx context.Context, id string, params map[string]any, onUpdate agent.ToolUpdateFunc) (agent.AgentToolResult, error) {
			city, _ := params["city"].(string)
			text := fmt.Sprintf("It is 22°C and sunny in %s.", city)
			return agent.AgentToolResult{Content: ai.ContentList{ai.TextContent{Text: text}}}, nil
		},
	}

	// 4. Build a session. Pass your own tools (or coding.CreateAllTools(cwd) for
	//    the built-in file/bash toolset). A custom system prompt skips file/skill
	//    discovery; omit it to get the coding-agent prompt.
	cwd, _ := os.Getwd()
	sess := coding.NewSession(coding.SessionOptions{
		Model:        model,
		Cwd:          cwd,
		Tools:        []agent.AgentTool{weather},
		SystemPrompt: "You are a helpful assistant. Use tools when relevant.",
		APIKey:       apiKey,
		SessionID:    "demo-session", // enables OpenAI prompt caching across turns
	})

	// 5. (Optional) stream tokens/tool activity into your app UI.
	sess.Subscribe(func(ctx context.Context, e agent.AgentEvent) error {
		if e.Type == agent.EvMessageUpdate && e.AssistantMessageEvent != nil &&
			e.AssistantMessageEvent.Type == ai.EventTextDelta {
			fmt.Print(e.AssistantMessageEvent.Delta)
		}
		return nil
	})

	// 6. Run a turn and read a structured result (final text, tool calls, usage, cost).
	res, err := sess.Run(context.Background(), "What's the weather in Paris, and should I bring an umbrella?")
	if err != nil {
		fail(err)
	}
	fmt.Printf("\n\n--- result ---\n%s\n", res.Text)
	fmt.Printf("tool calls: %d · tokens in/out: %d/%d · cost: $%.4f\n",
		len(res.ToolCalls), res.Usage.Input, res.Usage.Output, res.Usage.Cost.Total)
}

func fail(err error) {
	fmt.Fprintf(os.Stderr, "error: %v\n", err)
	os.Exit(1)
}
