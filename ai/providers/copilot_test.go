package providers

import (
	"testing"

	"github.com/sky-valley/pi/ai"
)

func userImageMsg() ai.UserMessage {
	return ai.UserMessage{Content: ai.ContentList{ai.ImageContent{Data: "aGk=", MimeType: "image/png"}}}
}

func assistantTextMsg() ai.AssistantMessage {
	return ai.AssistantMessage{Content: ai.ContentList{ai.TextContent{Text: "hello"}}}
}

func TestInferCopilotInitiator(t *testing.T) {
	// Last message is a user message → "user".
	msgs := []ai.Message{assistantTextMsg(), ai.NewUserText("hi", 1)}
	if got := inferCopilotInitiator(msgs); got != "user" {
		t.Fatalf("initiator = %q, want user", got)
	}
	// Last message is assistant → "agent".
	msgs = []ai.Message{ai.NewUserText("hi", 1), assistantTextMsg()}
	if got := inferCopilotInitiator(msgs); got != "agent" {
		t.Fatalf("initiator = %q, want agent", got)
	}
	// Last message is toolResult → "agent".
	msgs = []ai.Message{ai.NewUserText("hi", 1), ai.ToolResultMessage{ToolCallID: "t1"}}
	if got := inferCopilotInitiator(msgs); got != "agent" {
		t.Fatalf("initiator = %q, want agent", got)
	}
	// No messages → "user" (pi: `last && last.role !== "user" ? "agent" : "user"`).
	if got := inferCopilotInitiator(nil); got != "user" {
		t.Fatalf("initiator = %q, want user for empty messages", got)
	}
}

func TestHasCopilotVisionInput(t *testing.T) {
	// Image in a user message.
	if !hasCopilotVisionInput([]ai.Message{userImageMsg()}) {
		t.Fatal("expected vision input for image in user message")
	}
	// Image in a toolResult message.
	tr := ai.ToolResultMessage{ToolCallID: "t1", Content: ai.ContentList{ai.ImageContent{Data: "aGk=", MimeType: "image/png"}}}
	if !hasCopilotVisionInput([]ai.Message{ai.NewUserText("hi", 1), tr}) {
		t.Fatal("expected vision input for image in toolResult message")
	}
	// Pointer message variants are also detected.
	um := userImageMsg()
	if !hasCopilotVisionInput([]ai.Message{&um}) {
		t.Fatal("expected vision input for image in *UserMessage")
	}
	// Text only → false.
	if hasCopilotVisionInput([]ai.Message{ai.NewUserText("hi", 1), assistantTextMsg()}) {
		t.Fatal("expected no vision input for text-only messages")
	}
	// Image content in an assistant message does not count (pi checks only
	// user and toolResult roles).
	am := ai.AssistantMessage{Content: ai.ContentList{ai.ImageContent{Data: "aGk=", MimeType: "image/png"}}}
	if hasCopilotVisionInput([]ai.Message{am}) {
		t.Fatal("assistant images must not trigger the vision flag")
	}
}

func TestBuildCopilotDynamicHeaders(t *testing.T) {
	// User-initiated, no images.
	h := buildCopilotDynamicHeaders([]ai.Message{ai.NewUserText("hi", 1)}, false)
	if h["X-Initiator"] != "user" {
		t.Fatalf("X-Initiator = %q, want user", h["X-Initiator"])
	}
	if h["Openai-Intent"] != "conversation-edits" {
		t.Fatalf("Openai-Intent = %q, want conversation-edits", h["Openai-Intent"])
	}
	if _, ok := h["Copilot-Vision-Request"]; ok {
		t.Fatal("Copilot-Vision-Request must be absent without images")
	}

	// Agent-initiated with images.
	msgs := []ai.Message{userImageMsg(), assistantTextMsg()}
	h = buildCopilotDynamicHeaders(msgs, hasCopilotVisionInput(msgs))
	if h["X-Initiator"] != "agent" {
		t.Fatalf("X-Initiator = %q, want agent", h["X-Initiator"])
	}
	if h["Copilot-Vision-Request"] != "true" {
		t.Fatalf("Copilot-Vision-Request = %q, want true", h["Copilot-Vision-Request"])
	}
}
