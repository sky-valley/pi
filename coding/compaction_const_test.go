package coding

import (
	"testing"

	"github.com/sky-valley/pi/ai"
)

// TestSummarizationSystemPromptByteForByte pins SUMMARIZATION_SYSTEM_PROMPT
// (utils.ts:168) exactly.
func TestSummarizationSystemPromptByteForByte(t *testing.T) {
	// Matches the built/shipped JS (dist utils.d.ts): "AI coding assistant".
	// The TS source reads "AI assistant" but the distributed artifact — the
	// cross-check source of truth — includes "coding".
	want := "You are a context summarization assistant. Your task is to read a conversation between a user and an AI coding assistant, then produce a structured summary following the exact format specified.\n\nDo NOT continue the conversation. Do NOT respond to any questions in the conversation. ONLY output the structured summary."
	if summarizationSystemPrompt != want {
		t.Fatalf("SUMMARIZATION_SYSTEM_PROMPT drift:\n got: %q\nwant: %q", summarizationSystemPrompt, want)
	}
}

// TestEstimateImageChars verifies the per-image token estimate uses pi's
// ESTIMATED_IMAGE_CHARS = 4800 (compaction.ts:228), exercised via contentChars
// and EstimateMessageTokens.
func TestEstimateImageChars(t *testing.T) {
	if estimatedImageChars != 4800 {
		t.Fatalf("estimatedImageChars = %d, want 4800", estimatedImageChars)
	}

	// contentChars: 4800 (image) + 5 (text "hello").
	content := ai.ContentList{
		ai.ImageContent{Data: "abc", MimeType: "image/png"},
		ai.TextContent{Text: "hello"},
	}
	if got := contentChars(content); got != 4805 {
		t.Fatalf("contentChars with one image+text = %d, want 4805", got)
	}

	// EstimateMessageTokens on a user message with a single image:
	// ceil(4800/4) = 1200 tokens.
	msg := ai.UserMessage{Content: ai.ContentList{ai.ImageContent{Data: "x", MimeType: "image/png"}}, Timestamp: 1}
	if got := EstimateMessageTokens(msg); got != 1200 {
		t.Fatalf("EstimateMessageTokens(image) = %d, want 1200 (ceil(4800/4))", got)
	}
}
