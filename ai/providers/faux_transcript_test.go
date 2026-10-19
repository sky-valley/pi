package providers

import (
	"context"
	"strings"
	"testing"

	"github.com/sky-valley/pi/ai"
)

// faux.ts at upstream 9e05370b2 serializes the normalized transcript alone —
// `role:text` per message joined by a blank line — and renders a system message
// as getSystemMessageText plus `tool-:<json>` / `tool+:<json>` lines. The
// prompt text drives the usage estimate, so these usages were captured by
// streaming the same contexts through pi's createFauxCore under node at the sha.

func fauxUsageFor(t *testing.T, reg *FauxProviderRegistration, req ai.Context, sessionID string) ai.Usage {
	t.Helper()
	opts := &ai.SimpleStreamOptions{}
	opts.SessionID = sessionID
	final := ai.StreamSimple(context.Background(), reg.GetModel(), req, opts).Result()
	if final.StopReason != ai.StopStop {
		t.Fatalf("faux stream ended %s: %q", final.StopReason, final.ErrorMessage)
	}
	return final.Usage
}

func TestFauxUsageSerializesSystemMessagesInPlace(t *testing.T) {
	reg := RegisterFauxProvider(RegisterFauxProviderOptions{})
	defer reg.Unregister()
	reg.SetResponses([]FauxResponseStep{FauxStatic(FauxAssistantMessage(ai.ContentList{FauxText("ok")}, ai.StopStop))})

	update := ai.NewSystemText("updated guidance", 2)
	update.Sections = ai.SystemSections{{Name: "docs", Value: foldStr("<docs/>")}}
	update.ToolsRemoved = []ai.ToolReference{{Name: "base_tool"}}
	update.ToolsAdded = []ai.Tool{transcriptTool("late_tool")}
	req := ai.Context{
		SystemPrompt: "You are faux.",
		Tools:        []ai.Tool{transcriptTool("base_tool")},
		Messages:     []ai.Message{ai.NewUserText("hi", 1), update, ai.NewUserText("again", 3)},
	}
	usage := fauxUsageFor(t, reg, req, "")
	if usage.Input != 78 || usage.Output != 1 || usage.CacheRead != 0 || usage.CacheWrite != 0 || usage.TotalTokens != 79 {
		t.Fatalf("usage = %+v, want {Input:78 Output:1 CacheRead:0 CacheWrite:0 TotalTokens:79}", usage)
	}
}

// The prompt text behind that usage, byte for byte: faux.ts's
// serializeContext/messageToText at 9e05370b2 run on the same normalized context
// under node. Since fe11328b0 pi keeps one text per message and joins them only
// in measuring; the joined text is unchanged.
func TestFauxSerializeContextBytes(t *testing.T) {
	update := ai.NewSystemText("updated guidance", 2)
	update.Sections = ai.SystemSections{{Name: "docs", Value: foldStr("<docs/>")}}
	update.ToolsRemoved = []ai.ToolReference{{Name: "base_tool"}}
	update.ToolsAdded = []ai.Tool{transcriptTool("late_tool")}
	req := ai.NormalizeContext(ai.Context{
		SystemPrompt: "You are faux.",
		Tools:        []ai.Tool{transcriptTool("base_tool")},
		Messages:     []ai.Message{ai.NewUserText("hi", 1), update, ai.NewUserText("again", 3)},
	})
	const want = "system:You are faux.\n" +
		`tool+:{"name":"base_tool","description":"base_tool tool","parameters":{"type":"object","properties":{}}}` +
		"\n\nuser:hi\n\nsystem:updated guidance\n\n<docs/>\n" +
		`tool-:{"name":"base_tool"}` + "\n" +
		`tool+:{"name":"late_tool","description":"late_tool tool","parameters":{"type":"object","properties":{}}}` +
		"\n\nuser:again"
	if got := strings.Join(promptTexts(req), "\n\n"); got != want {
		t.Fatalf("joined prompt\n got %q\nwant %q", got, want)
	}
}

// The estimate and the cached prefix are measured in UTF-16 code units, as JS
// string lengths and indices are.
func TestFauxUsageCountsUTF16CodeUnits(t *testing.T) {
	reg := RegisterFauxProvider(RegisterFauxProviderOptions{})
	defer reg.Unregister()
	reg.SetResponses([]FauxResponseStep{
		FauxStatic(FauxAssistantMessage(ai.ContentList{FauxText("ok")}, ai.StopStop)),
		FauxStatic(FauxAssistantMessage(ai.ContentList{FauxText("😀 réponse")}, ai.StopStop)),
	})

	first := ai.Context{SystemPrompt: "Réponds brièvement 😀.", Messages: []ai.Message{ai.NewUserText("héllo wörld", 1)}}
	if usage := fauxUsageFor(t, reg, first, "s"); usage.Input != 12 || usage.Output != 1 || usage.CacheRead != 0 || usage.CacheWrite != 12 || usage.TotalTokens != 25 {
		t.Fatalf("first usage = %+v, want {Input:12 Output:1 CacheRead:0 CacheWrite:12 TotalTokens:25}", usage)
	}
	second := ai.Context{SystemPrompt: "Réponds brièvement 😀.", Messages: []ai.Message{
		ai.NewUserText("héllo wörld", 1),
		ai.AssistantMessage{Content: ai.ContentList{ai.TextContent{Text: "ok"}}, Api: "x", Provider: "x", Model: "x", StopReason: ai.StopStop, Timestamp: 2},
		ai.NewUserText("ça va? 🎉🎉", 3),
	}}
	if usage := fauxUsageFor(t, reg, second, "s"); usage.Input != 8 || usage.Output != 3 || usage.CacheRead != 12 || usage.CacheWrite != 8 || usage.TotalTokens != 31 {
		t.Fatalf("second usage = %+v, want {Input:8 Output:3 CacheRead:12 CacheWrite:8 TotalTokens:31}", usage)
	}
}

// toolResultToText is `[toolName, ...content.map(block => contentToText([block]))].join("\n")`:
// a result with no content is its tool name alone, with no trailing newline.
func TestFauxToolResultWithoutContentIsItsName(t *testing.T) {
	if got := messageToText(ai.ToolResultMessage{ToolName: "bash", ToolCallID: "x"}); got != "bash" {
		t.Fatalf("messageToText(empty tool result) = %q, want %q", got, "bash")
	}
}
