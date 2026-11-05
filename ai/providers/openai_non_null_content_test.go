package providers

import (
	"reflect"
	"testing"

	"github.com/sky-valley/pi/ai"
)

// Upstream 14b804d80 (pi test/openai-completions-non-null-assistant-content.test.ts):
// compat.requiresNonNullAssistantContent sends "" instead of null for a
// tool-call-only assistant turn, without the extra assistant message that
// requiresAssistantAfterToolResult inserts after tool results.
func TestOpenAIRequiresNonNullAssistantContent(t *testing.T) {
	ctx := ai.Context{Messages: []ai.Message{
		ai.NewUserText("Run the command", 1),
		ai.AssistantMessage{
			Content: ai.ContentList{ai.ToolCall{ID: "tool-1", Name: "bash", Arguments: map[string]any{"command": "true"}}},
			Api:     ai.APIOpenAICompletions, Provider: "openai", Model: "gpt-5", StopReason: ai.StopToolUse, Timestamp: 2,
		},
		ai.ToolResultMessage{ToolCallID: "tool-1", ToolName: "bash", Content: ai.ContentList{ai.TextContent{Text: "ok"}}, Timestamp: 3},
		ai.NewUserText("Thanks", 4),
	}}
	tests := []struct {
		name      string
		compat    string
		wantRoles []string
		// wantContent is the content of each assistant message in order; nil
		// means JSON null.
		wantContent []any
	}{
		{"null content by default", "",
			[]string{"user", "assistant", "tool", "user"}, []any{nil}},
		{"empty string, no inserted assistant", `{"requiresNonNullAssistantContent":true}`,
			[]string{"user", "assistant", "tool", "user"}, []any{""}},
		{"requiresAssistantAfterToolResult still inserts its message", `{"requiresAssistantAfterToolResult":true}`,
			[]string{"user", "assistant", "tool", "assistant", "user"}, []any{"", "I have processed the tool results."}},
		{"explicit false keeps null", `{"requiresNonNullAssistantContent":false}`,
			[]string{"user", "assistant", "tool", "user"}, []any{nil}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body := mustBuildOpenAIParams(t, openAICompletionsCompatModel(tc.compat), ctx, &OpenAIOptions{})
			msgs, _ := body["messages"].([]map[string]any)
			var roles []string
			var content []any
			for _, m := range msgs {
				roles = append(roles, m["role"].(string))
				if m["role"] == "assistant" {
					content = append(content, m["content"])
				}
			}
			if !reflect.DeepEqual(roles, tc.wantRoles) {
				t.Fatalf("roles = %v, want %v", roles, tc.wantRoles)
			}
			if !reflect.DeepEqual(content, tc.wantContent) {
				t.Fatalf("assistant content = %#v, want %#v", content, tc.wantContent)
			}
		})
	}
}
