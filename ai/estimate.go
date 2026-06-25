package ai

import (
	"encoding/json"
	"math"
	"unicode/utf16"
)

// Context-token estimation, ported from pi packages/ai/src/utils/estimate.ts
// (upstream 09f10595). The estimates here drive clampMaxTokensToContext, which
// caps streamSimple max-token defaults so providers that count input and output
// against a single context window do not reject long requests.

// ContextUsageEstimate mirrors pi's ContextUsageEstimate.
type ContextUsageEstimate struct {
	// Tokens is the estimated total context tokens.
	Tokens int
	// UsageTokens are the tokens reported by the most recent assistant usage block.
	UsageTokens int
	// TrailingTokens are the estimated tokens after the most recent assistant
	// usage block.
	TrailingTokens int
	// LastUsageIndex is the index of the message that provided usage, or -1 when
	// none exists (pi uses `number | null`; we use -1 for "null").
	LastUsageIndex int
}

const (
	charsPerToken       = 4
	estimatedImageChars = 4800
)

// jsStringLength returns the JS String.prototype.length of s — the number of
// UTF-16 code units, matching `text.length` in pi. This is neither byte length
// nor rune count: characters outside the BMP count as 2.
func jsStringLength(s string) int {
	return len(utf16.Encode([]rune(s)))
}

// calculateContextTokens mirrors pi's calculateContextTokens: prefer the
// provider-reported total, else sum the component counts.
func calculateContextTokens(usage Usage) int {
	if usage.TotalTokens != 0 {
		return usage.TotalTokens
	}
	return usage.Input + usage.Output + usage.CacheRead + usage.CacheWrite
}

// safeJSONStringify mirrors pi's safeJsonStringify: JSON.stringify with a
// fallback string when the value cannot be serialized.
func safeJSONStringify(value any) string {
	b, err := json.Marshal(value)
	if err != nil {
		return "[unserializable]"
	}
	return string(b)
}

// estimateTextAndImageContentChars sums the UTF-16 length of text blocks and a
// flat per-image character budget, mirroring pi.
func estimateTextAndImageContentChars(content ContentList) int {
	chars := 0
	for _, block := range content {
		switch b := block.(type) {
		case TextContent:
			chars += jsStringLength(b.Text)
		case ImageContent:
			chars += estimatedImageChars
		default:
			// pi's estimateTextAndImageContentChars only ever sees text/image
			// blocks (user and toolResult content). Other block types are not
			// expected here; ignore them, as pi's typing excludes them.
		}
	}
	return chars
}

// estimateTextTokens mirrors pi's estimateTextTokens: ceil(length / 4) over the
// UTF-16 length.
func estimateTextTokens(text string) int {
	return int(math.Ceil(float64(jsStringLength(text)) / charsPerToken))
}

// estimateTextAndImageContentTokens mirrors pi's function of the same name.
func estimateTextAndImageContentTokens(content ContentList) int {
	return int(math.Ceil(float64(estimateTextAndImageContentChars(content)) / charsPerToken))
}

// estimateMessageTokens mirrors pi's estimateMessageTokens.
func estimateMessageTokens(message Message) int {
	switch m := message.(type) {
	case UserMessage:
		return estimateTextAndImageContentTokens(m.Content)
	case ToolResultMessage:
		return estimateTextAndImageContentTokens(m.Content)
	case AssistantMessage:
		chars := 0
		for _, block := range m.Content {
			switch b := block.(type) {
			case TextContent:
				chars += jsStringLength(b.Text)
			case ThinkingContent:
				chars += jsStringLength(b.Thinking)
			case ToolCall:
				chars += jsStringLength(b.Name) + jsStringLength(safeJSONStringify(b.Arguments))
			}
		}
		return int(math.Ceil(float64(chars) / charsPerToken))
	default:
		return 0
	}
}

// getLastAssistantUsageInfo walks messages backwards and returns the first
// non-aborted/non-error assistant whose usage reports a positive token count.
func getLastAssistantUsageInfo(messages []Message) (usage Usage, index int, found bool) {
	for i := len(messages) - 1; i >= 0; i-- {
		assistant, ok := messages[i].(AssistantMessage)
		if !ok {
			continue
		}
		if assistant.StopReason == StopAborted || assistant.StopReason == StopError {
			continue
		}
		if calculateContextTokens(assistant.Usage) > 0 {
			return assistant.Usage, i, true
		}
	}
	return Usage{}, -1, false
}

// estimateMessages mirrors pi's estimateMessages: anchor on the last usage
// block when present, else sum every message.
func estimateMessages(messages []Message) ContextUsageEstimate {
	usage, index, found := getLastAssistantUsageInfo(messages)
	if found {
		usageTokens := calculateContextTokens(usage)
		trailingTokens := 0
		for i := index + 1; i < len(messages); i++ {
			trailingTokens += estimateMessageTokens(messages[i])
		}
		return ContextUsageEstimate{
			Tokens:         usageTokens + trailingTokens,
			UsageTokens:    usageTokens,
			TrailingTokens: trailingTokens,
			LastUsageIndex: index,
		}
	}

	tokens := 0
	for _, message := range messages {
		tokens += estimateMessageTokens(message)
	}
	return ContextUsageEstimate{Tokens: tokens, UsageTokens: 0, TrailingTokens: tokens, LastUsageIndex: -1}
}

// estimateContextTokens mirrors pi's Context overload of estimateContextTokens:
// estimate the messages, and when there is no usage anchor add prefix tokens for
// the system prompt and tool definitions.
func estimateContextTokens(context Context) ContextUsageEstimate {
	estimate := estimateMessages(context.Messages)
	if estimate.LastUsageIndex != -1 {
		return estimate
	}

	prefixTokens := 0
	if context.SystemPrompt != "" {
		prefixTokens += estimateTextTokens(context.SystemPrompt)
	}
	if len(context.Tools) > 0 {
		prefixTokens += estimateTextTokens(safeJSONStringify(context.Tools))
	}

	return ContextUsageEstimate{
		Tokens:         estimate.Tokens + prefixTokens,
		UsageTokens:    estimate.UsageTokens,
		TrailingTokens: estimate.TrailingTokens + prefixTokens,
		LastUsageIndex: estimate.LastUsageIndex,
	}
}
