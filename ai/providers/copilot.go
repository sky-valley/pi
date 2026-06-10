package providers

import "github.com/sky-valley/pi/ai"

// inferCopilotInitiator reports whether the request is user-initiated or
// agent-initiated (e.g. follow-up after assistant/tool messages). Ported from
// pi github-copilot-headers.ts: the last message being anything other than a
// user message (or there being no messages) means "agent"/"user" respectively.
func inferCopilotInitiator(messages []ai.Message) string {
	if len(messages) == 0 {
		return "user"
	}
	if messages[len(messages)-1].MessageRole() != ai.RoleUser {
		return "agent"
	}
	return "user"
}

// hasCopilotVisionInput reports whether any user or toolResult message
// contains an image block. Copilot requires the Copilot-Vision-Request header
// when sending images (github-copilot-headers.ts hasCopilotVisionInput).
func hasCopilotVisionInput(messages []ai.Message) bool {
	for _, msg := range messages {
		var content ai.ContentList
		switch m := msg.(type) {
		case ai.UserMessage:
			content = m.Content
		case *ai.UserMessage:
			content = m.Content
		case ai.ToolResultMessage:
			content = m.Content
		case *ai.ToolResultMessage:
			content = m.Content
		default:
			continue
		}
		for _, c := range content {
			if _, ok := c.(ai.ImageContent); ok {
				return true
			}
		}
	}
	return false
}

// buildCopilotDynamicHeaders builds the per-request GitHub Copilot headers
// (github-copilot-headers.ts buildCopilotDynamicHeaders): X-Initiator,
// Openai-Intent, and Copilot-Vision-Request when images are present.
func buildCopilotDynamicHeaders(messages []ai.Message, hasImages bool) map[string]string {
	headers := map[string]string{
		"X-Initiator":   inferCopilotInitiator(messages),
		"Openai-Intent": "conversation-edits",
	}
	if hasImages {
		headers["Copilot-Vision-Request"] = "true"
	}
	return headers
}
