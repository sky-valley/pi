package coding

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"

	"github.com/sky-valley/pi/agent"
	"github.com/sky-valley/pi/ai"
)

// CompactionSettings configures automatic context-window compaction (port of
// pi's CompactionSettings / DEFAULT_COMPACTION_SETTINGS).
type CompactionSettings struct {
	Enabled          bool
	ReserveTokens    int
	KeepRecentTokens int
}

// DefaultCompactionSettings mirrors pi's defaults.
var DefaultCompactionSettings = CompactionSettings{
	Enabled:          true,
	ReserveTokens:    16384,
	KeepRecentTokens: 20000,
}

// estimatedImageChars mirrors pi's ESTIMATED_IMAGE_CHARS (compaction.ts:228):
// the per-image char estimate used by the token heuristic.
const estimatedImageChars = 4800

// toolResultMaxChars mirrors pi's TOOL_RESULT_MAX_CHARS (utils.ts:89): tool
// results are truncated to this many characters when serialized for summarization.
const toolResultMaxChars = 2000

// summarizationSystemPrompt is pi's SUMMARIZATION_SYSTEM_PROMPT (utils.ts:168),
// the dedicated system prompt for the summarization request. Byte-for-byte.
const summarizationSystemPrompt = `You are a context summarization assistant. Your task is to read a conversation between a user and an AI coding assistant, then produce a structured summary following the exact format specified.

Do NOT continue the conversation. Do NOT respond to any questions in the conversation. ONLY output the structured summary.`

const summarizationPrompt = `The messages above are a conversation to summarize. Create a structured context checkpoint summary that another LLM will use to continue the work.

Use this EXACT format:

## Goal
[What is the user trying to accomplish? Can be multiple items if the session covers different tasks.]

## Constraints & Preferences
- [Any constraints, preferences, or requirements mentioned by user]
- [Or "(none)" if none were mentioned]

## Progress
### Done
- [x] [Completed tasks/changes]

### In Progress
- [ ] [Current work]

### Blocked
- [Issues preventing progress, if any]

## Key Decisions
- **[Decision]**: [Brief rationale]

## Next Steps
1. [Ordered list of what should happen next]

## Critical Context
- [Any data, examples, or references needed to continue]
- [Or "(none)" if not applicable]

Keep each section concise. Preserve exact file paths, function names, and error messages.`

// EstimateMessageTokens estimates the token cost of a message (port of
// estimateTokens: char count / 4, rounded up).
func EstimateMessageTokens(m agent.AgentMessage) int {
	chars := 0
	switch v := m.(type) {
	case ai.UserMessage:
		chars = contentChars(v.Content)
	case *ai.AssistantMessage:
		chars = assistantChars(v)
	case ai.AssistantMessage:
		chars = assistantChars(&v)
	case ai.ToolResultMessage:
		chars = contentChars(v.Content)
	}
	return int(math.Ceil(float64(chars) / 4))
}

func assistantChars(a *ai.AssistantMessage) int {
	chars := 0
	for _, c := range a.Content {
		switch b := c.(type) {
		case ai.TextContent:
			chars += len(b.Text)
		case ai.ThinkingContent:
			chars += len(b.Thinking)
		case ai.ToolCall:
			args, _ := json.Marshal(b.Arguments)
			chars += len(b.Name) + len(args)
		}
	}
	return chars
}

func contentChars(content ai.ContentList) int {
	chars := 0
	for _, c := range content {
		switch b := c.(type) {
		case ai.TextContent:
			chars += len(b.Text)
		case ai.ImageContent:
			chars += estimatedImageChars // fixed estimate for an inline image
		}
	}
	return chars
}

// EstimateContextTokens sums estimated tokens across messages (pure heuristic).
func EstimateContextTokens(messages []agent.AgentMessage) int {
	total := 0
	for _, m := range messages {
		total += EstimateMessageTokens(m)
	}
	return total
}

// estimateContextTokensUsageAware blends the real token usage reported by the
// last assistant turn with a heuristic estimate of the trailing messages (port
// of pi's estimateContextTokens). This is far more accurate than the pure
// char/4 heuristic on large contexts (big repos), where it matters most.
func estimateContextTokensUsageAware(messages []agent.AgentMessage) int {
	lastIdx := -1
	var lastUsage ai.Usage
	for i, m := range messages {
		am, ok := messageAsAssistant(m)
		if !ok {
			continue
		}
		if contextTokensFromUsage(am.Usage) > 0 {
			lastIdx = i
			lastUsage = am.Usage
		}
	}
	if lastIdx == -1 {
		return EstimateContextTokens(messages)
	}
	total := contextTokensFromUsage(lastUsage)
	for i := lastIdx + 1; i < len(messages); i++ {
		total += EstimateMessageTokens(messages[i])
	}
	return total
}

func contextTokensFromUsage(u ai.Usage) int {
	if u.TotalTokens > 0 {
		return u.TotalTokens
	}
	return u.Input + u.Output + u.CacheRead + u.CacheWrite
}

// shouldCompact reports whether the context exceeds the safe budget.
func shouldCompact(contextTokens, contextWindow int, s CompactionSettings) bool {
	if !s.Enabled || contextWindow <= 0 {
		return false
	}
	return contextTokens > contextWindow-s.ReserveTokens
}

// findCutIndex returns the first index to KEEP: walking from the end, accumulate
// tokens until KeepRecentTokens is reached, then snap back so a kept run never
// begins on a tool-result (which must stay attached to its assistant turn).
func findCutIndex(messages []agent.AgentMessage, keepRecent int) int {
	acc := 0
	cut := 0
	for i := len(messages) - 1; i >= 0; i-- {
		acc += EstimateMessageTokens(messages[i])
		if acc >= keepRecent {
			cut = i
			break
		}
	}
	for cut > 0 && messages[cut].MessageRole() == ai.RoleToolResult {
		cut--
	}
	return cut
}

type compactionState struct {
	mu        sync.Mutex
	settings  CompactionSettings
	prefixLen int    // number of older messages covered by the cached summary
	summary   string // cached summary text
}

// EnableCompaction installs an automatic compaction TransformContext on the
// session's agent using the given settings. When the estimated context exceeds
// the model's window minus ReserveTokens, older turns are summarized (via the
// session's model) into a single checkpoint message and recent turns are kept.
func (s *Session) EnableCompaction(settings CompactionSettings) {
	state := &compactionState{settings: settings}
	s.Agent.TransformContext = func(ctx context.Context, messages []agent.AgentMessage) []agent.AgentMessage {
		return s.compact(ctx, state, messages)
	}
}

func (s *Session) compact(ctx context.Context, state *compactionState, messages []agent.AgentMessage) []agent.AgentMessage {
	window := 0
	if s.Model != nil {
		window = s.Model.ContextWindow
	}
	tokens := estimateContextTokensUsageAware(messages)
	if !shouldCompact(tokens, window, state.settings) {
		return messages
	}
	cut := findCutIndex(messages, state.settings.KeepRecentTokens)
	if cut <= 0 {
		return messages // nothing safely summarizable
	}

	older := messages[:cut]
	recent := messages[cut:]

	state.mu.Lock()
	summary := state.summary
	reuse := state.prefixLen == len(older) && summary != ""
	state.mu.Unlock()

	if !reuse {
		summary = s.summarize(ctx, older, state.settings.ReserveTokens)
		if summary == "" {
			return messages // summarization failed; don't drop context
		}
		state.mu.Lock()
		state.prefixLen = len(older)
		state.summary = summary
		state.mu.Unlock()
	}

	// Same wrapper text pi uses for a compaction summary (core/messages.ts).
	checkpoint := compactionSummaryMessage(summary, nowMillisCoding())
	out := make([]agent.AgentMessage, 0, 1+len(recent))
	out = append(out, checkpoint)
	out = append(out, recent...)
	return out
}

// summarize asks the model to produce a structured checkpoint of older messages.
// It builds the summarization request faithfully to pi's generateSummary
// (compaction.ts:558-620): the conversation is serialized to text, wrapped in
// <conversation>...</conversation>, sent with the dedicated
// SUMMARIZATION_SYSTEM_PROMPT and a capped maxTokens, and the read/modified file
// lists are appended to the resulting summary text.
func (s *Session) summarize(ctx context.Context, older []agent.AgentMessage, reserveTokens int) string {
	// Convert to LLM messages first (handles role filtering).
	var llmMessages []ai.Message
	for _, m := range older {
		switch m.MessageRole() {
		case ai.RoleUser, ai.RoleAssistant, ai.RoleToolResult:
			llmMessages = append(llmMessages, m)
		}
	}

	conversationText := serializeConversation(llmMessages)
	promptText := "<conversation>\n" + conversationText + "\n</conversation>\n\n" + summarizationPrompt

	summarizationMessages := []ai.Message{ai.NewUserText(promptText, nowMillisCoding())}

	// maxTokens = min(floor(0.8 * reserveTokens), model.maxTokens if > 0).
	maxTokens := int(math.Floor(0.8 * float64(reserveTokens)))
	if s.Model != nil && s.Model.MaxTokens > 0 && s.Model.MaxTokens < maxTokens {
		maxTokens = s.Model.MaxTokens
	}

	opts := &ai.SimpleStreamOptions{StreamOptions: ai.StreamOptions{APIKey: s.apiKey, MaxTokens: &maxTokens}}

	streamFn := s.Agent.StreamFn
	if streamFn == nil {
		streamFn = ai.StreamSimple
	}
	stream := streamFn(ctx, s.Model, ai.Context{SystemPrompt: summarizationSystemPrompt, Messages: summarizationMessages}, opts)
	msg := stream.Result()
	if msg == nil || msg.StopReason == ai.StopError || msg.StopReason == ai.StopAborted {
		return ""
	}
	var b strings.Builder
	for _, c := range msg.Content {
		if tc, ok := c.(ai.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	summary := b.String()

	// Compute file lists and append to the summary (compaction.ts:817-819).
	readFiles, modifiedFiles := computeFileLists(older)
	summary += formatFileOperations(readFiles, modifiedFiles)

	return summary
}

// serializeConversation serializes LLM messages to text for summarization so the
// model treats it as content to summarize, not a conversation to continue (port
// of utils.ts serializeConversation). Tool results are truncated to
// toolResultMaxChars.
func serializeConversation(messages []ai.Message) string {
	var parts []string
	for _, m := range messages {
		switch msg := m.(type) {
		case ai.UserMessage:
			content := textOf(msg.Content)
			if content != "" {
				parts = append(parts, "[User]: "+content)
			}
		case ai.AssistantMessage:
			parts = append(parts, serializeAssistant(&msg)...)
		case *ai.AssistantMessage:
			parts = append(parts, serializeAssistant(msg)...)
		case ai.ToolResultMessage:
			content := textOf(msg.Content)
			if content != "" {
				parts = append(parts, "[Tool result]: "+truncateForSummary(content, toolResultMaxChars))
			}
		}
	}
	return strings.Join(parts, "\n\n")
}

func serializeAssistant(a *ai.AssistantMessage) []string {
	var textParts, thinkingParts, toolCalls []string
	for _, c := range a.Content {
		switch b := c.(type) {
		case ai.TextContent:
			textParts = append(textParts, b.Text)
		case ai.ThinkingContent:
			thinkingParts = append(thinkingParts, b.Thinking)
		case ai.ToolCall:
			var entries []string
			// JS Object.entries preserves insertion order; map iteration is
			// non-deterministic in Go, so sort keys for a stable serialization.
			keys := make([]string, 0, len(b.Arguments))
			for k := range b.Arguments {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				v, _ := json.Marshal(b.Arguments[k])
				entries = append(entries, k+"="+string(v))
			}
			toolCalls = append(toolCalls, b.Name+"("+strings.Join(entries, ", ")+")")
		}
	}
	var parts []string
	if len(thinkingParts) > 0 {
		parts = append(parts, "[Assistant thinking]: "+strings.Join(thinkingParts, "\n"))
	}
	if len(textParts) > 0 {
		parts = append(parts, "[Assistant]: "+strings.Join(textParts, "\n"))
	}
	if len(toolCalls) > 0 {
		parts = append(parts, "[Assistant tool calls]: "+strings.Join(toolCalls, "; "))
	}
	return parts
}

func textOf(content ai.ContentList) string {
	var b strings.Builder
	for _, c := range content {
		if tc, ok := c.(ai.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

// truncateForSummary truncates text to maxChars, appending a marker (utils.ts).
func truncateForSummary(text string, maxChars int) string {
	if len(text) <= maxChars {
		return text
	}
	truncated := len(text) - maxChars
	return text[:maxChars] + fmt.Sprintf("\n\n[... %d more characters truncated]", truncated)
}

// computeFileLists derives the read-only and modified file lists from read/edit/
// write tool calls in the older messages (port of extractFileOpsFromMessage +
// computeFileLists). readFiles excludes any file that was also modified.
func computeFileLists(messages []agent.AgentMessage) (readFiles, modifiedFiles []string) {
	read := map[string]bool{}
	modified := map[string]bool{}
	for _, m := range messages {
		am, ok := messageAsAssistant(m)
		if !ok {
			continue
		}
		for _, c := range am.Content {
			tc, ok := c.(ai.ToolCall)
			if !ok {
				continue
			}
			path, _ := tc.Arguments["path"].(string)
			if path == "" {
				continue
			}
			switch tc.Name {
			case "read":
				read[path] = true
			case "write", "edit":
				modified[path] = true
			}
		}
	}
	for f := range read {
		if !modified[f] {
			readFiles = append(readFiles, f)
		}
	}
	for f := range modified {
		modifiedFiles = append(modifiedFiles, f)
	}
	sort.Strings(readFiles)
	sort.Strings(modifiedFiles)
	return readFiles, modifiedFiles
}

// formatFileOperations formats read/modified file lists as XML tags appended to
// the summary (port of utils.ts formatFileOperations).
func formatFileOperations(readFiles, modifiedFiles []string) string {
	var sections []string
	if len(readFiles) > 0 {
		sections = append(sections, "<read-files>\n"+strings.Join(readFiles, "\n")+"\n</read-files>")
	}
	if len(modifiedFiles) > 0 {
		sections = append(sections, "<modified-files>\n"+strings.Join(modifiedFiles, "\n")+"\n</modified-files>")
	}
	if len(sections) == 0 {
		return ""
	}
	return "\n\n" + strings.Join(sections, "\n\n")
}
