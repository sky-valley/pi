package ai

import (
	"math"
	"unicode/utf16"

	"github.com/sky-valley/pi/internal/jstext"
)

// Context-token estimation, ported from pi packages/ai/src/utils/estimate.ts
// (upstream 09f10595, system messages 9e05370b2). The estimates here drive clampMaxTokensToContext, which
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
	// charsPerToken is 3.5 since upstream 27075fe07 (#10497): large new inputs
	// need more room than chars/4 left when limiting output. The divisions
	// below are float, as pi's Math.ceil(chars / CHARS_PER_TOKEN).
	charsPerToken       = 3.5
	estimatedImageChars = 4800
)

// jsStringLength returns the JS String.prototype.length of s — the number of
// UTF-16 code units, matching `text.length` in pi. This is neither byte length
// nor rune count: characters outside the BMP count as 2. Each byte of invalid
// UTF-8 is a U+FFFD, one unit. It counts without copying s.
func jsStringLength(s string) int {
	n := 0
	for _, r := range s {
		n += utf16.RuneLen(r)
	}
	return n
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
// fallback string when the value cannot be serialized. It is measured, so it
// must be JSON.stringify's text, not encoding/json's: the latter escapes <, >,
// &, U+2028 and U+2029 into six characters each.
func safeJSONStringify(value any) string {
	s, err := jstext.Stringify(value)
	if err != nil {
		return "[unserializable]"
	}
	return s
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

// estimateTextTokens mirrors pi's estimateTextTokens: ceil(length / 3.5) over the
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
	if system, ok := systemMessageOf(message); ok {
		return estimateTextTokens(GetSystemMessageText(system)) +
			estimateToolsTokens(system.ToolsAdded) +
			estimateToolsTokens(system.ToolsRemoved)
	}
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

// messageTimestamp returns the message's timestamp, mirroring pi where every
// Message carries a `timestamp`.
func messageTimestamp(m Message) int64 {
	if system, ok := systemMessageOf(m); ok {
		return system.Timestamp
	}
	switch msg := m.(type) {
	case UserMessage:
		return msg.Timestamp
	case AssistantMessage:
		return msg.Timestamp
	case ToolResultMessage:
		return msg.Timestamp
	default:
		return 0
	}
}

// getLastAssistantUsageInfo walks messages forward and returns the newest
// non-aborted/non-error assistant whose usage reports a positive token count AND
// still describes the current prefix. A later prefix message inserted after a
// response (e.g. a compaction summary) invalidates that response's usage, since
// the usage can no longer describe the reshaped prefix (#6464): such stale usage
// is skipped even though it is chronologically later in the list.
func getLastAssistantUsageInfo(messages []Message) (usage Usage, index int, found bool) {
	index = -1
	latestPrefixTimestamp := int64(math.MinInt64)
	for i := 0; i < len(messages); i++ {
		if assistant, ok := messages[i].(AssistantMessage); ok {
			usageAppliesToPrefix := assistant.Timestamp >= latestPrefixTimestamp
			if usageAppliesToPrefix &&
				assistant.StopReason != StopAborted &&
				assistant.StopReason != StopError &&
				calculateContextTokens(assistant.Usage) > 0 {
				usage, index, found = assistant.Usage, i, true
			}
		}
		latestPrefixTimestamp = max(latestPrefixTimestamp, messageTimestamp(messages[i]))
	}
	return usage, index, found
}

// estimateContextTokens mirrors pi's estimateContextTokens over a transcript
// (upstream 9e05370b2 dropped the Context overload: the system prompt and tool
// definitions now ride the transcript's system messages and are counted like any
// other message). It anchors on the last usage block when present, else sums
// every message.
func estimateContextTokens(messages []Message) ContextUsageEstimate {
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

// estimateToolsTokens mirrors pi's estimateToolsTokens: JSON-serialize a list
// (tool definitions or tool references) and estimate its text tokens (0 for an
// empty list).
func estimateToolsTokens[T any](tools []T) int {
	if len(tools) == 0 {
		return 0
	}
	return estimateTextTokens(safeJSONStringify(tools))
}
