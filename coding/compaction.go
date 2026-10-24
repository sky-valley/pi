package coding

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/sky-valley/pi/agent"
	"github.com/sky-valley/pi/ai"
	"github.com/sky-valley/pi/internal/jstext"
)

// CompactionSettings configures automatic context-window compaction (port of
// pi's CompactionSettings / DEFAULT_COMPACTION_SETTINGS).
type CompactionSettings struct {
	Enabled bool
	// ReserveTokens is the headroom kept free of the context window: a
	// context past ContextWindow-ReserveTokens compacts. It only decides when
	// compaction starts; summary requests set no output limit of their own
	// (upstream 6fcb48670).
	ReserveTokens    int
	KeepRecentTokens int
	// SessionID, when set, is the routing session ID summarization requests
	// reuse instead of minting a fresh one per request; cache retention stays
	// "none" either way (pi compact()'s optional sessionId param, upstream
	// 58302d34e "support compaction routing sessions").
	SessionID string
	// Retry, when set and enabled, retries a summary request that fails with
	// a transient error, with backoff (pi compact()'s retry param:
	// completeSummarization wraps each call in retryAssistantCall, upstream
	// 8e53e0e49). Nil sends each request once, as pi's compact() does
	// without the param. pi's AgentSession always passes its settings.retry;
	// DefaultCompactionSettings carries that setting's default.
	Retry *ai.RetryPolicy
	// RetryCallbacks observe those retries (pi compact()'s callbacks).
	RetryCallbacks *ai.RetryCallbacks
}

// DefaultCompactionSettings returns pi's defaults: SETTINGS_DEFAULTS.compaction,
// and the retry policy pi's AgentSession compacts under unless configured
// (SETTINGS_DEFAULTS.retry: enabled, 3 retries, a 2s base backoff capped at
// DefaultMaxAgentRetryDelayMs). Each call returns new settings with a retry
// policy of their own, so a caller may change any field.
func DefaultCompactionSettings() *CompactionSettings {
	return &CompactionSettings{
		Enabled:          true,
		ReserveTokens:    16384,
		KeepRecentTokens: 20000,
		Retry:            &ai.RetryPolicy{Enabled: true, MaxRetries: 3, BaseDelayMs: 2000},
	}
}

// estimatedImageChars mirrors pi's ESTIMATED_IMAGE_CHARS (compaction.ts):
// the per-image char estimate used by the token heuristic.
const estimatedImageChars = 4800

// toolResultMaxChars mirrors pi's TOOL_RESULT_MAX_CHARS (utils.ts): tool
// results are truncated to this many characters when serialized for summarization.
const toolResultMaxChars = 2000

// summarizationSystemPrompt is pi's SUMMARIZATION_SYSTEM_PROMPT (utils.ts),
// the dedicated system prompt for the summarization request. Byte-for-byte; it
// names a neutral "AI assistant" so non-coding agents summarize correctly too
// (pi #5401, upstream 72fd91135).
const summarizationSystemPrompt = `You are a context summarization assistant. Your task is to read a conversation between a user and an AI assistant, then produce a structured summary following the exact format specified.

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

// updateSummarizationPrompt is pi's UPDATE_SUMMARIZATION_PROMPT (compaction.ts),
// used when a previous compaction summary exists. Byte-for-byte from the npm build.
const updateSummarizationPrompt = `The messages above are NEW conversation messages to incorporate into the existing summary provided in <previous-summary> tags.

Update the existing structured summary with new information. RULES:
- PRESERVE all existing information from the previous summary
- ADD new progress, decisions, and context from the new messages
- UPDATE the Progress section: move items from "In Progress" to "Done" when completed
- UPDATE "Next Steps" based on what was accomplished
- PRESERVE exact file paths, function names, and error messages
- If something is no longer relevant, you may remove it

Use this EXACT format:

## Goal
[Preserve existing goals, add new ones if the task expanded]

## Constraints & Preferences
- [Preserve existing, add new ones discovered]

## Progress
### Done
- [x] [Include previously done items AND newly completed items]

### In Progress
- [ ] [Current work - update based on progress]

### Blocked
- [Current blockers - remove if resolved]

## Key Decisions
- **[Decision]**: [Brief rationale] (preserve all previous, add new)

## Next Steps
1. [Update based on current state]

## Critical Context
- [Preserve important context, add new if needed]

Keep each section concise. Preserve exact file paths, function names, and error messages.`

// turnPrefixSummarizationPrompt is pi's TURN_PREFIX_SUMMARIZATION_PROMPT
// (compaction.ts), used for the prefix of a split turn. Byte-for-byte.
// Its continuation wording, with the Markdown boundary generateTurnPrefixSummary
// puts around the conversation, avoids the reasoning-extraction false positive
// that made Claude Fable 5.1 refuse split-turn summaries (pi #9652, upstream
// d192bd6dc).
const turnPrefixSummarizationPrompt = `The messages above are earlier context from an ongoing conversation. Later messages are stored separately and do not need to be reconstructed.

Create a concise checkpoint of the user's request and the progress shown above. This checkpoint will be placed before the later messages so the conversation can continue with the necessary context.

## Original Request
[What did the user ask for?]

## Progress So Far
- [Key decisions and work completed in these messages]

## Context Needed to Continue
- [Information from these messages needed to understand the later work]

Only summarize information explicitly present above. Do not infer or recreate later messages.`

// EstimateMessageTokens estimates the token cost of a message (port of
// estimateTokens: char count / 4, rounded up).
func EstimateMessageTokens(m agent.AgentMessage) int {
	chars := 0
	switch v := m.(type) {
	case ai.SystemMessage:
		chars = systemChars(&v)
	case *ai.SystemMessage:
		chars = systemChars(v)
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

// systemChars counts a system message's content, each non-empty section and
// JSON.stringify(toolsAdded); tool removals carry no weight (pi estimateTokens,
// upstream 466db0fec).
func systemChars(m *ai.SystemMessage) int {
	chars := contentChars(m.Content)
	for _, section := range m.Sections.Entries() {
		if section.Value != nil {
			chars += jstext.UTF16Length(*section.Value)
		}
	}
	if m.ToolsAdded != nil {
		chars += jsonStringifyLength(m.ToolsAdded)
	}
	return chars
}

func assistantChars(a *ai.AssistantMessage) int {
	chars := 0
	for _, c := range a.Content {
		switch b := c.(type) {
		case ai.TextContent:
			chars += jstext.UTF16Length(b.Text)
		case ai.ThinkingContent:
			chars += jstext.UTF16Length(b.Thinking)
		case ai.ToolCall:
			chars += jstext.UTF16Length(b.Name) + jsonStringifyLength(b.Arguments)
		}
	}
	return chars
}

func contentChars(content ai.ContentList) int {
	chars := 0
	for _, c := range content {
		switch b := c.(type) {
		case ai.TextContent:
			chars += jstext.UTF16Length(b.Text)
		case ai.ImageContent:
			chars += estimatedImageChars // fixed estimate for an inline image
		}
	}
	return chars
}

// jsonStringifyLength is JSON.stringify(v).length: the UTF-16 length of the
// text JSON.stringify writes, which is not encoding/json's (see jstext.Stringify).
func jsonStringifyLength(v any) int {
	s, err := jstext.Stringify(v)
	if err != nil {
		return 0
	}
	return jstext.UTF16Length(s)
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
	i, usage, ok := lastAssistantUsage(messages)
	if !ok {
		return EstimateContextTokens(messages)
	}
	return usageAnchoredTokens(messages, i, usage)
}

// estimateProjectedContextTokens is pi's estimateProjectedContextTokens: the
// usage-anchored estimate when the last usage was recorded at or after
// trustedFrom (after the latest compaction or context edit), and otherwise a
// pure estimate that counts the current system message once, however many
// system messages built it, plus every other message.
func estimateProjectedContextTokens(messages []agent.AgentMessage, trustedFrom int) int {
	if i, usage, ok := lastAssistantUsage(messages); ok && i >= trustedFrom {
		return usageAnchoredTokens(messages, i, usage)
	}
	tokens := 0
	if system, ok := ai.GetCurrentSystemMessage(messages); ok {
		tokens = EstimateMessageTokens(system)
	}
	for _, m := range messages {
		if m.MessageRole() != ai.RoleSystem {
			tokens += EstimateMessageTokens(m)
		}
	}
	return tokens
}

// lastAssistantUsage is pi's getLastAssistantUsageInfo: the last message
// getAssistantUsage accepts, an assistant message that is not aborted, not an
// error, and has non-zero usage (upstream cd95c274 added the
// calculateContextTokens(usage) > 0 guard so a malformed all-zero usage
// response is not trusted as the anchor).
func lastAssistantUsage(messages []agent.AgentMessage) (index int, usage ai.Usage, ok bool) {
	for i := len(messages) - 1; i >= 0; i-- {
		am, isAssistant := messageAsAssistant(messages[i])
		if !isAssistant || am.StopReason == ai.StopAborted || am.StopReason == ai.StopError {
			continue
		}
		if contextTokensFromUsage(am.Usage) > 0 {
			return i, am.Usage, true
		}
	}
	return -1, ai.Usage{}, false
}

// usageAnchoredTokens is the usage messages[i] reported plus an estimate of
// every message after it.
func usageAnchoredTokens(messages []agent.AgentMessage, i int, usage ai.Usage) int {
	total := contextTokensFromUsage(usage)
	for _, m := range messages[i+1:] {
		total += EstimateMessageTokens(m)
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

// cutPointResult mirrors pi's CutPointResult (compaction.ts).
type cutPointResult struct {
	// firstKeptIndex is the index of the first message to keep.
	firstKeptIndex int
	// turnStartIndex is the user message starting the turn being split, or -1.
	turnStartIndex int
	// isSplitTurn is true when the cut lands mid-turn (not on a user message).
	isSplitTurn bool
}

// isCutPointMessage is pi's isCutPointMessage over the port's in-memory roles:
// a kept run may start on a user or assistant message, never on a tool result
// (it must follow its tool call) or a system message (prompt state, which the
// compaction replays instead).
func isCutPointMessage(m agent.AgentMessage) bool {
	switch m.MessageRole() {
	case ai.RoleUser, ai.RoleAssistant:
		return true
	}
	return false
}

// findCutPoint ports pi's findCutPoint + findValidCutPoints (compaction.ts) to a
// flat message list. Walking backwards from the newest message, estimated tokens
// accumulate until KeepRecentTokens is reached — a message estimated at zero
// tokens (a system message) never crosses the budget — and the cut then snaps
// FORWARD to the first cut point at or after the crossing index (so a boundary
// tool result goes into the summarized portion), or back to the LAST cut point
// when the crossing lands past every one of them — trailing tool results big
// enough to blow the budget alone. If the budget is never reached, the cut
// defaults to the first cut point (keep everything). Only messages in
// [startIndex, endIndex) are considered.
func findCutPoint(messages []agent.AgentMessage, startIndex, endIndex, keepRecentTokens int) cutPointResult {
	var cutPoints []int
	for i := startIndex; i < endIndex; i++ {
		if isCutPointMessage(messages[i]) {
			cutPoints = append(cutPoints, i)
		}
	}
	if len(cutPoints) == 0 {
		return cutPointResult{firstKeptIndex: startIndex, turnStartIndex: -1}
	}

	// Walk backwards from newest, accumulating estimated message sizes.
	acc := 0
	cutIndex := cutPoints[0] // default: keep from first message
	for i := endIndex - 1; i >= startIndex; i-- {
		tokens := EstimateMessageTokens(messages[i])
		if tokens == 0 {
			continue
		}
		acc += tokens
		if acc >= keepRecentTokens {
			// Prefer the closest valid cut point at or after this index. When the
			// trailing tool results cross the budget on their own there is none, and
			// the cut falls back to the LAST cut point — the assistant message that
			// issued those calls — instead of the first, so older history can still
			// be summarized (pi 8bdcd4498).
			cutIndex = cutPoints[len(cutPoints)-1]
			for _, c := range cutPoints {
				if c >= i {
					cutIndex = c
					break
				}
			}
			break
		}
	}

	// Determine if this is a split turn (pi findCutPoint): a cut not on
	// a user message splits the turn started by the nearest preceding user message.
	isUser := messages[cutIndex].MessageRole() == ai.RoleUser
	turnStart := -1
	if !isUser {
		for i := cutIndex; i >= startIndex; i-- {
			if messages[i].MessageRole() == ai.RoleUser {
				turnStart = i
				break
			}
		}
	}
	return cutPointResult{
		firstKeptIndex: cutIndex,
		turnStartIndex: turnStart,
		isSplitTurn:    !isUser && turnStart != -1,
	}
}

// compactionPreparation is pi's CompactionPreparation over the in-memory
// transcript.
type compactionPreparation struct {
	// firstKeptIndex is the first message kept after the summary.
	firstKeptIndex int
	// messagesToSummarize are summarized and dropped.
	messagesToSummarize []agent.AgentMessage
	// turnPrefixMessages are the split turn's prefix, summarized on their own.
	turnPrefixMessages []agent.AgentMessage
	// isSplitTurn reports whether the cut lands mid-turn.
	isSplitTurn bool
}

// prepareCompaction is pi's prepareCompaction: it cuts the transcript from
// boundaryStart (the previous compaction's first kept message) and collects the
// messages to summarize. System messages are prompt state, not conversation, so
// neither list carries them (pi getMessageFromEntryForCompaction); the
// compaction replays them instead. It reports false when there is nothing to
// summarize.
func prepareCompaction(messages []agent.AgentMessage, boundaryStart, keepRecentTokens int) (compactionPreparation, bool) {
	cp := findCutPoint(messages, boundaryStart, len(messages), keepRecentTokens)
	historyEnd := cp.firstKeptIndex
	if cp.isSplitTurn {
		historyEnd = cp.turnStartIndex
	}
	preparation := compactionPreparation{
		firstKeptIndex:      cp.firstKeptIndex,
		messagesToSummarize: withoutSystemMessages(messages[boundaryStart:historyEnd]),
		isSplitTurn:         cp.isSplitTurn,
	}
	if cp.isSplitTurn {
		preparation.turnPrefixMessages = withoutSystemMessages(messages[cp.turnStartIndex:cp.firstKeptIndex])
	}
	if len(preparation.messagesToSummarize) == 0 && len(preparation.turnPrefixMessages) == 0 {
		return compactionPreparation{}, false
	}
	return preparation, true
}

// withoutSystemMessages returns the messages that are not system messages, in
// a fresh slice.
func withoutSystemMessages(messages []agent.AgentMessage) []agent.AgentMessage {
	var out []agent.AgentMessage
	for _, m := range messages {
		if m.MessageRole() != ai.RoleSystem {
			out = append(out, m)
		}
	}
	return out
}

type compactionState struct {
	mu       sync.Mutex
	settings CompactionSettings
	// checkpoint is the newest compaction (pi: the compaction entry on the
	// branch), nil when there is none. An empty summary is still a checkpoint:
	// pi persists the empty summary a model's empty reply produces, and reads it
	// back as a defined previousSummary "". A published checkpoint is never
	// modified; a new compaction replaces the pointer.
	checkpoint *compactionCheckpoint
	// usageFrom indexes the transcript: an assistant usage recorded before it
	// predates the latest compaction or context edit, and measured a context
	// that has since changed. pi's estimateProjectedContextTokens trusts only
	// usage recorded after both. 0 trusts every usage.
	usageFrom int
}

// EnableCompaction fills the compaction stage of the session's per-request
// transform chain (installTransformContext) with the given settings. When the
// estimated context exceeds the model's window minus ReserveTokens, older turns
// are summarized (via the session's model) into a single checkpoint message and
// recent turns are kept.
//
// It does NOT assign Agent.TransformContext, so calling it after NewSession
// keeps the chain — including a wrapper an embedder installed on the field —
// and keeps the forced-prompt projection last.
//
// Re-enabling compaction updates the settings and KEEPS the existing
// checkpoint, because compaction is permanent (see compact below): a fresh
// state would drop the summary and let dropped turns reappear.
func (s *Session) EnableCompaction(settings CompactionSettings) {
	if s.compactState == nil {
		s.compactState = &compactionState{}
	}
	state := s.compactState
	state.mu.Lock()
	state.settings = settings
	state.mu.Unlock()
	s.compactTransform = func(ctx context.Context, messages []agent.AgentMessage) []agent.AgentMessage {
		return s.compact(ctx, state, messages)
	}
}

// setCompaction replaces the session's compaction state: none for a new
// transcript (pi's /new starts a fresh SessionManager, with no compaction
// entry), or what a resumed branch carries. The settings stay. A session that
// never enabled compaction keeps the state for a later EnableCompaction.
func (s *Session) setCompaction(c *compactionCheckpoint, usageFrom int) {
	if s.compactState == nil {
		if c == nil && usageFrom == 0 {
			return
		}
		s.compactState = &compactionState{}
	}
	s.compactState.mu.Lock()
	s.compactState.checkpoint = c
	s.compactState.usageFrom = usageFrom
	s.compactState.mu.Unlock()
}

// resumedCompaction is the compaction state a resumed branch carries: its
// newest compaction as a checkpoint, or nil, and the index its trusted usage
// starts from.
//
// pi's prepareCompaction takes the first projected entry as the previous
// compaction when it is a compaction that still contributes messages; a
// context edit that omitted it leaves none, and pi then has no previous
// compaction. Its summary is the previous summary, its stored system message
// the replay, and its details' file lists (only when an extension did not
// produce it) the lists to merge.
//
// The projection's messages open with the compaction's own: the replayed
// system message when it stored one, then the summary, which fall before
// prefixLen, where the next cut search starts (pi boundaryStart). The messages
// it kept follow, then the messages recorded after it, from compactedLen: the
// first of those entries is the compaction entry's child on the path, and from
// it on the entries keep the path's order. Applying the checkpoint rebuilds the
// same view.
//
// pi's estimateProjectedContextTokens trusts a usage only when its entry comes
// after the branch's latest compaction or context_edit, omitted or not, so
// usageFrom is the first message after the later of the two.
func resumedCompaction(p BranchProjection) (checkpoint *compactionCheckpoint, usageFrom int) {
	var compaction *SessionEntry
	if len(p.Entries) > 0 && p.Entries[0].SourceEntry.Type == "compaction" {
		compaction = p.Entries[0].SourceEntry
	}
	// Without a compaction every entry is in path order.
	after := compaction == nil
	compactedLen := len(p.Messages)
	offset := 0
	for i, entry := range p.Entries {
		if !after && i > 0 && entry.SourceEntry.ParentID == compaction.ID {
			after = true
			compactedLen = offset
			usageFrom = offset
		}
		if after && entry.SourceEntry.Type == "context_edit" {
			usageFrom = offset + len(entry.Messages)
		}
		offset += len(entry.Messages)
	}
	if !after {
		// Nothing was recorded after the compaction.
		usageFrom = compactedLen
	}
	if compaction == nil || len(p.Entries[0].Messages) == 0 {
		return nil, usageFrom
	}
	checkpoint = &compactionCheckpoint{
		prefixLen:     1,
		compactedLen:  compactedLen,
		systemMessage: compaction.SystemMessage,
		summary:       compaction.Summary,
	}
	if compaction.SystemMessage != nil {
		checkpoint.prefixLen++
	}
	if !compaction.FromHook {
		checkpoint.readFiles = detailsFileList(compaction.Details, "readFiles")
		checkpoint.modifiedFiles = detailsFileList(compaction.Details, "modifiedFiles")
	}
	return checkpoint, usageFrom
}

// detailsFileList reads one file list from a compaction's details, as pi's
// extractFileOperations does: only when the member is an array, and every
// element as JSON.parse returns it. pi's own compactions write paths, but
// appendCompaction (public SDK API) stores whatever details it is given.
func detailsFileList(details json.RawMessage, key string) []fileListItem {
	var members map[string]json.RawMessage
	if json.Unmarshal(details, &members) != nil {
		return nil
	}
	list, err := jstext.Parse(members[key])
	elements, isArray := list.([]any)
	if err != nil || !isArray {
		return nil
	}
	items := make([]fileListItem, len(elements))
	for i, v := range elements {
		items[i] = detailsItem(v)
	}
	return items
}

// compactionCheckpoint is one compaction as pi's session file records it.
type compactionCheckpoint struct {
	// prefixLen indexes the ORIGINAL message list: the first kept message (pi
	// firstKeptEntryId).
	prefixLen int
	// compactedLen is the length of the original message list when the
	// checkpoint was taken, where pi appends the compaction entry: kept
	// messages before it drop their system messages, which the replay already
	// holds; messages from it on come after the compaction and keep theirs.
	compactedLen int
	// systemMessage is the prompt and tool state replayed at the checkpoint (pi
	// CompactionEntry.systemMessage); nil when the transcript had none.
	systemMessage *ai.SystemMessage
	// summary is the summary text, file-ops appendix included (pi
	// CompactionEntry.summary).
	summary string
	// readFiles and modifiedFiles are the file lists the compaction recorded
	// (pi CompactionEntry.details), merged into the next compaction's (pi
	// extractFileOperations).
	readFiles     []fileListItem
	modifiedFiles []fileListItem
}

// apply builds the compacted view in pi buildSessionContext's order: the
// replayed system message, the summary, the kept messages without their system
// messages, then every message after the compaction.
func (c *compactionCheckpoint) apply(messages []agent.AgentMessage) []agent.AgentMessage {
	// Defensive: the transcript was replaced or shrunk.
	prefixLen := min(c.prefixLen, len(messages))
	compactedLen := min(max(c.compactedLen, prefixLen), len(messages))
	out := make([]agent.AgentMessage, 0, 2+len(messages)-prefixLen)
	if c.systemMessage != nil {
		out = append(out, *c.systemMessage)
	}
	out = append(out, compactionSummaryMessage(c.summary, nowMillisCoding()))
	out = append(out, withoutSystemMessages(messages[prefixLen:compactedLen])...)
	return append(out, messages[compactedLen:]...)
}

// compact is the per-request TransformContext. pi semantics: compaction is
// PERMANENT — once a summary checkpoint exists it is always applied (pi persists
// a compaction entry; dropped turns never come back). The shouldCompact check
// only decides whether to EXTEND the compaction by summarizing a larger prefix,
// merging via the <previous-summary> update flow (pi prepareCompaction/compact).
//
// The checkpoint's prefixLen and compactedLen index the ORIGINAL message list,
// which the agent only ever grows by appending, so they stay valid across turns
// and re-compactions. Replacing the transcript replaces the checkpoint
// (setCompaction).
func (s *Session) compact(ctx context.Context, state *compactionState, messages []agent.AgentMessage) []agent.AgentMessage {
	window := 0
	if s.Model != nil {
		window = s.Model.ContextWindow
	}

	state.mu.Lock()
	settings := state.settings
	previous := state.checkpoint
	usageFrom := state.usageFrom
	state.mu.Unlock()

	// Always re-apply the checkpoint first (permanence), even one whose summary
	// is empty. A new compaction extends it from its first kept message (pi
	// boundaryStart) with its summary as the previous one.
	current := messages
	boundaryStart := 0
	previousSummary := ""
	if previous != nil {
		current = previous.apply(messages)
		boundaryStart = min(previous.prefixLen, len(messages))
		previousSummary = previous.summary
	}

	tokens := estimateContextTokensUsageAware(current)
	if usageFrom > 0 {
		// usageFrom is never before the checkpoint's compactedLen, and the view
		// ends with every message from compactedLen on, so it indexes the
		// view's tail.
		tokens = estimateProjectedContextTokens(current, len(current)-max(len(messages)-usageFrom, 0))
	}
	if !shouldCompact(tokens, window, settings) {
		return current
	}
	// pi's prepareCompaction finds nothing to compact when the branch's last
	// entry is the compaction itself.
	if previous != nil && len(messages) <= previous.compactedLen {
		return current
	}

	preparation, ok := prepareCompaction(messages, boundaryStart, settings.KeepRecentTokens)
	if !ok {
		return current // nothing new safely summarizable
	}
	history, turnPrefix := preparation.messagesToSummarize, preparation.turnPrefixMessages

	// Generate summaries (pi compact). Sequential, as upstream is since f58c1156.
	// generateSummary takes previousSummary as is: pi picks the update prompt on
	// `previousSummary ? ... : ...`, and an empty summary is falsy like an
	// absent one.
	var newSummary string
	if preparation.isSplitTurn && len(turnPrefix) > 0 {
		// pi: `previousSummary ?? "No prior history."`. Only a missing
		// compaction falls back; an empty previous summary stays empty.
		historyResult := "No prior history."
		if previous != nil {
			historyResult = previous.summary
		}
		if len(history) > 0 {
			hr, ok := s.generateSummary(ctx, history, previousSummary, settings)
			if !ok {
				return current // summarization failed; keep current view
			}
			historyResult = hr
		}
		tp, ok := s.generateTurnPrefixSummary(ctx, turnPrefix, settings)
		if !ok {
			return current
		}
		newSummary = historyResult + "\n\n---\n\n**Turn Context (split turn):**\n\n" + tp
	} else {
		ns, ok := s.generateSummary(ctx, history, previousSummary, settings)
		if !ok {
			return current
		}
		newSummary = ns
	}
	// An empty summary is still a compaction: pi's compact() returns it and the
	// session appends it. Only an abort discards the result, whatever text it
	// produced: _runAutoCompaction calls signal.throwIfAborted() after compact()
	// and before appendCompaction.
	if ctx.Err() != nil {
		return current
	}

	// Merge file ops from the previous compaction's lists plus the newly
	// summarized messages (pi extractFileOperations + split-turn extraction).
	var ops fileOps
	if previous != nil {
		for _, f := range previous.readFiles {
			ops.read.add(f)
		}
		for _, f := range previous.modifiedFiles {
			ops.edited.add(f)
		}
	}
	for _, m := range history {
		extractFileOpsFromMessage(m, &ops)
	}
	for _, m := range turnPrefix {
		extractFileOpsFromMessage(m, &ops)
	}
	readFiles, modifiedFiles := ops.lists()
	// A previous compaction's details value with no string form makes pi's
	// sort or join throw, and the compaction fails after its summaries.
	if !stringForms(readFiles) || !stringForms(modifiedFiles) {
		return current
	}
	newSummary += formatFileOperations(readFiles, modifiedFiles)

	// pi appendCompaction stores the prompt and tool state the context replays
	// at this point, stamped with the compaction's time.
	next := &compactionCheckpoint{
		prefixLen:     preparation.firstKeptIndex,
		compactedLen:  len(messages),
		summary:       newSummary,
		readFiles:     readFiles,
		modifiedFiles: modifiedFiles,
	}
	if replay, ok := ai.GetCurrentSystemMessage(current); ok {
		replay.Timestamp = nowMillisCoding()
		next.systemMessage = &replay
	}

	state.mu.Lock()
	state.checkpoint = next
	state.usageFrom = next.compactedLen
	state.mu.Unlock()

	return next.apply(messages)
}

// summarize asks the model to produce a structured checkpoint of older messages
// with no previous summary, appending the read/modified file lists computed from
// those messages (the non-split path of pi compact).
// Branch-summary-style callers carry no routing session ID (pi generateSummary's
// optional sessionId stays undefined for them), so each request gets a fresh one.
func (s *Session) summarize(ctx context.Context, older []agent.AgentMessage) string {
	text, ok := s.generateSummary(ctx, older, "", CompactionSettings{})
	if !ok {
		return ""
	}
	readFiles, modifiedFiles := computeFileLists(older)
	return text + formatFileOperations(readFiles, modifiedFiles)
}

// generateSummary ports pi's generateSummaryWithUsage: the conversation is
// serialized to text, wrapped in <conversation>...</conversation> (followed by
// <previous-summary>...</previous-summary> and the update prompt variant when a
// previous summary is non-empty), and sent with the dedicated
// SUMMARIZATION_SYSTEM_PROMPT. Only this history request
// keeps the tag wrapper; a split turn's prefix is framed in Markdown (see
// generateTurnPrefixSummary). Returns ok=false where pi fails the summarization
// (stopReason "error" or "length"); an aborted response returns the text
// produced so far, like pi.
func (s *Session) generateSummary(ctx context.Context, older []agent.AgentMessage, previousSummary string, settings CompactionSettings) (string, bool) {
	conversationText := serializeConversation(messagesAsLlm(older))

	promptText := "<conversation>\n" + conversationText + "\n</conversation>\n\n"
	if previousSummary != "" {
		promptText += "<previous-summary>\n" + previousSummary + "\n</previous-summary>\n\n"
		promptText += updateSummarizationPrompt
	} else {
		promptText += summarizationPrompt
	}

	return s.completeSummarization(ctx, promptText, settings)
}

// generateTurnPrefixSummary ports pi's generateTurnPrefixSummary: the prefix of
// a split turn is summarized with the dedicated turn-prefix prompt. The
// serialized prefix goes under a
// "# Conversation" heading and the prompt under "# Instructions" — not the
// history request's <conversation> tags (pi #9652, upstream d192bd6dc).
func (s *Session) generateTurnPrefixSummary(ctx context.Context, messages []agent.AgentMessage, settings CompactionSettings) (string, bool) {
	conversationText := serializeConversation(messagesAsLlm(messages))
	promptText := "# Conversation\n" + conversationText + "\n\n# Instructions\n" + turnPrefixSummarizationPrompt
	return s.completeSummarization(ctx, promptText, settings)
}

// messagesAsLlm filters agent messages down to LLM roles (pi convertToLlm, which
// passes system messages since upstream 9e05370b2; serializeConversation has
// no system arm, so they never reach a summary).
func messagesAsLlm(messages []agent.AgentMessage) []ai.Message {
	var llmMessages []ai.Message
	for _, m := range messages {
		switch m.MessageRole() {
		case ai.RoleSystem, ai.RoleUser, ai.RoleAssistant, ai.RoleToolResult:
			llmMessages = append(llmMessages, m)
		}
	}
	return llmMessages
}

// summarizationRequest is the model and auth a summarization request is sent
// with (pi AgentSession._getSummarizationRequestAuth, upstream 18d65de62 /
// #6768): the auth the session's Models resolves for its model — the API
// key, the headers less the ones the auth deletes, and the env (a Cloudflare
// account id, for one, which fills the endpoint) — and the model rebuilt on
// the base URL the auth carries.
//
// A provider can carry its endpoint in the credential rather than in the
// catalog: GitHub Copilot's Business/Enterprise base URL comes from the stored
// OAuth credential's toAuth(). The request that follows carries the resolved
// key explicitly, and an explicit key short-circuits Models' resolution to the
// api-key path (auth_resolve.go: "overrides?.apiKey !== undefined"), which
// never reaches the stored OAuth credential — so the base URL must be taken
// here, or a Copilot compaction request goes to the Individual endpoint on a
// Business/Enterprise account.
//
// Resolution is best-effort, as pi's is: when it fails or resolves nothing,
// the request goes to the session's own model with no auth of its own, and
// the stream resolves it. A session without Models has no runtime to ask, and
// its compat stream resolves each request's key from the environment.
func (s *Session) summarizationRequest(ctx context.Context) (*ai.Model, summarizationAuth) {
	if s.models == nil || s.Model == nil {
		return s.Model, summarizationAuth{}
	}
	result, err := s.models.GetAuth(ctx, s.Model, nil)
	if err != nil || result == nil {
		return s.Model, summarizationAuth{}
	}
	auth := summarizationAuth{apiKey: result.Auth.APIKey, headers: withoutDeletedHeaders(result.Auth.Headers), env: result.Env}
	if result.Auth.BaseURL == "" {
		return s.Model, auth
	}
	requestModel := *s.Model
	requestModel.BaseURL = result.Auth.BaseURL
	return &requestModel, auth
}

// summarizationAuth is the auth summarizationRequest resolved.
type summarizationAuth struct {
	apiKey  string
	headers ai.ProviderHeaders
	env     map[string]string
}

// withoutDeletedHeaders is pi's withoutDeletedHeaders (agent-session.ts): the
// headers less the ones an auth deletes (nil values).
func withoutDeletedHeaders(headers ai.ProviderHeaders) ai.ProviderHeaders {
	if headers == nil {
		return nil
	}
	kept := make(ai.ProviderHeaders, len(headers))
	for name, value := range headers {
		if value != nil {
			kept[name] = value
		}
	}
	return kept
}

// withSessionHeaders lays the session's Headers over a summary's auth
// headers, as they lie over every request the session makes; a session
// header replaces an auth header of the same name in any case. pi has no
// session headers.
func withSessionHeaders(auth, session ai.ProviderHeaders) ai.ProviderHeaders {
	if len(session) == 0 {
		return auth
	}
	merged := make(ai.ProviderHeaders, len(auth)+len(session))
	for name, value := range auth {
		merged[name] = value
	}
	for name, value := range session {
		for existing := range merged {
			if jstext.ToLower(existing) == jstext.ToLower(name) {
				delete(merged, existing)
			}
		}
		merged[name] = value
	}
	return merged
}

// summarizationFailed reports whether a summarization response is unsafe to
// persist as a checkpoint (pi getSummarizationFailure, compaction.ts, upstream
// 97fa14e39 / #7048). A "length" stop means generation hit the token cap, so the
// summary is truncated mid-thought and must not become a session checkpoint —
// only "error" used to fail. An abort is not a failure here: the summarizer
// returns the text produced so far, and compact then discards the aborted
// compaction as a whole, as pi's session does.
//
// pi builds a "<label> failed: <reason>" message here, with a different label at
// each of its three call sites (Summarization, Turn prefix summarization, Branch
// summarization). The port has one summarization choke point and no channel for
// those strings — the same reason its tool-call guard below carries none — so it
// ports the predicate, which is the whole of the observable behavior.
func summarizationFailed(reason ai.StopReason) bool {
	return reason == ai.StopError || reason == ai.StopLength
}

// completeSummarization sends one summarization request (pi completeSummarization
// + createSummarizationOptions): the auth summarizationRequest resolved (its
// key, headers and env), the session's headers, and — when the model supports
// reasoning and the session's thinking level is set and not off — the thinking
// level are passed through. It sets no maxTokens (upstream
// 6fcb48670): the provider sizes the output from the actual request, the
// model's output limit reduced to fit the remaining context, as for normal
// turns, so ReserveTokens only decides when compaction starts. The request
// runs under settings.Retry (pi retryAssistantCall): a transient error such as
// a dropped stream is retried with backoff, while a deterministic error or an
// abort returns at once; settings.SessionID is the routing session. Returns ok=false where pi fails the
// summarization (see summarizationFailed); otherwise the response's text blocks
// joined with "\n" (pi-ai contentText), which for an aborted response is the
// text produced so far.
func (s *Session) completeSummarization(ctx context.Context, promptText string, settings CompactionSettings) (string, bool) {
	summarizationMessages := []ai.Message{ai.NewUserText(promptText, nowMillisCoding())}

	// Avoid cache writes for one-off summaries (pi 9b3a2059): retention "none".
	// Reuse caller-supplied routing when available; callers without a session
	// ID, including branch summaries, receive a fresh routing ID (pi
	// `options.sessionId ?? uuidv7()`, upstream 58302d34e). pi throws here on
	// generator exhaustion; the Must form panics for the same reason, because
	// completeSummarization's (string, bool) return has nowhere to carry it.
	sessionID := settings.SessionID
	if sessionID == "" {
		sessionID = mustUUIDv7()
	}
	requestModel, auth := s.summarizationRequest(ctx)
	opts := &ai.SimpleStreamOptions{StreamOptions: ai.StreamOptions{
		ProviderRequestOptions: ai.ProviderRequestOptions{
			APIKey:  auth.apiKey,
			Headers: withSessionHeaders(auth.headers, s.Agent.Headers),
			Env:     auth.env,
		},
		CacheRetention: ai.CacheNone,
		SessionID:      sessionID,
	}}
	// pi 6b36eb592 withdrew the `toolChoice: "none"` 90305d90a had forced here:
	// summarization leaves the option absent and lets the provider default apply.
	// A summary is still text, never a tool call — the guard below enforces that
	// on the response instead of on the request.
	//
	// pi ed867e909 withdrew getAnthropicSummarizationFallback: server-side refusal
	// fallback is no longer requested per call site. The Anthropic provider derives
	// it from the model's catalog compat, so a summarization request gets it on the
	// same terms as every other request.
	level := s.Agent.State().ThinkingLevel
	if s.Model != nil && s.Model.Reasoning && level != "" && level != agent.ThinkOff {
		opts.Reasoning = ai.ThinkingLevel(level)
	}

	streamFn := s.Agent.StreamFn
	if streamFn == nil {
		streamFn = func(ctx context.Context, model *ai.Model, req ai.TranscriptContext, opts *ai.SimpleStreamOptions) *ai.AssistantMessageEventStream {
			return ai.StreamSimple(ctx, model, ai.Context{Messages: req.Messages}, opts)
		}
	}
	// pi buildSummarizationContext: the summarization prompt rides the leading
	// system message of a normalized transcript.
	summarizationContext := ai.NormalizeContext(ai.Context{SystemPrompt: summarizationSystemPrompt, Messages: summarizationMessages})
	produce := func() *ai.AssistantMessage {
		if msg := streamFn(ctx, requestModel, summarizationContext, opts).Result(); msg != nil {
			return msg
		}
		// pi awaits a result that never comes. Here a stream that ends
		// without one fails the request like a dropped stream, which the
		// retry policy may retry.
		return ai.NewErrorMessage(requestModel.Api, requestModel.Provider, requestModel.ID, ai.StopError, "summarization stream ended without a result")
	}
	msg := ai.RetryAssistantCall(ctx, produce, settings.Retry, settings.RetryCallbacks)
	if summarizationFailed(msg.StopReason) {
		return "", false
	}
	// A model that called a tool anyway did not produce a summary, so the
	// compaction fails rather than checkpointing whatever text came with it. pi
	// puts this guard in each of completeSummarization's callers, differing only
	// in the message it throws; the port has one place to put it and no channel
	// for those strings — a failed summarization surfaces as "keep the current
	// view" either way.
	var texts []string
	for _, c := range msg.Content {
		switch v := c.(type) {
		case ai.ToolCall:
			return "", false
		case ai.TextContent:
			texts = append(texts, v.Text)
		}
	}
	return strings.Join(texts, "\n"), true
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

// orderedArguments returns a tool call's arguments as key/value pairs in the
// order the model wrote them, falling back to sorted keys when no order was
// recorded (a tool call built in Go rather than decoded from a model).
func orderedArguments(tc ai.ToolCall) ai.OrderedObject {
	if ordered, ok := tc.OrderedArguments().(ai.OrderedObject); ok {
		return ordered
	}
	keys := make([]string, 0, len(tc.Arguments))
	for k := range tc.Arguments {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	fields := make(ai.OrderedObject, 0, len(keys))
	for _, k := range keys {
		fields = append(fields, ai.OrderedField{Key: k, Value: tc.Arguments[k]})
	}
	return fields
}

// serializeAssistant is serializeConversation's assistant arm. Thinking is
// omitted (upstream 15e7bdc14): providers return summaries of the real
// reasoning, and replayed thinking is often not part of the model's live
// context — a block that does not match its request is dropped — so it could
// make the summarization request larger than the context it summarizes.
// Visible text, tool calls and tool results record what happened.
func serializeAssistant(a *ai.AssistantMessage) []string {
	var textParts, toolCalls []string
	for _, c := range a.Content {
		switch b := c.(type) {
		case ai.TextContent:
			textParts = append(textParts, b.Text)
		case ai.ToolCall:
			var entries []string
			// pi's Object.entries walks the arguments in the order the model
			// wrote them; Go map iteration is unordered, so follow the recorded
			// order and fall back to sorted keys when there is none.
			for _, f := range orderedArguments(b) {
				v, _ := jstext.Stringify(f.Value)
				entries = append(entries, f.Key+"="+v)
			}
			toolCalls = append(toolCalls, b.Name+"("+strings.Join(entries, ", ")+")")
		}
	}
	var parts []string
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

// truncateForSummary truncates text to maxChars UTF-16 code units, appending a
// marker (utils.ts truncateForSummary; JS .length/.slice count UTF-16 units).
// Unlike JS slice, a surrogate pair on the boundary is dropped whole rather than
// split, so the output is always valid UTF-8.
func truncateForSummary(text string, maxChars int) string {
	length := jstext.UTF16Length(text)
	if length <= maxChars {
		return text
	}
	return sliceUTF16(text, maxChars) + fmt.Sprintf("\n\n[... %d more characters truncated]", length-maxChars)
}

// sliceUTF16 returns the longest prefix of s holding at most n UTF-16 code
// units without splitting a rune (an astral rune counts as 2 units and is
// excluded entirely when it straddles the boundary).
func sliceUTF16(s string, n int) string {
	units := 0
	for i, r := range s {
		w := 1
		if r > 0xFFFF {
			w = 2
		}
		if units+w > n {
			return s[:i]
		}
		units += w
	}
	return s
}

// fileOps mirrors pi's FileOperations (utils.ts createFileOps): three Sets.
// The zero value is empty and ready to use.
type fileOps struct {
	read, written, edited fileSet
}

// fileSet is a JavaScript Set of file-list values: insertion-ordered, each
// primitive held once (SameValueZero), each object or array on its own.
type fileSet struct {
	items []fileListItem
	ids   map[string]bool
}

func (s *fileSet) add(item fileListItem) {
	if item.id != "" {
		if s.ids[item.id] {
			return
		}
		if s.ids == nil {
			s.ids = map[string]bool{}
		}
		s.ids[item.id] = true
	}
	s.items = append(s.items, item)
}

func (s *fileSet) has(item fileListItem) bool { return item.id != "" && s.ids[item.id] }

// fileListItem is one value of a compaction's file lists. A tool call
// contributes the path it names. A compaction's details, which pi's
// appendCompaction stores unchecked, can hold any JSON value, and pi's
// extractFileOperations adds each element to its Set as it is.
type fileListItem struct {
	// id is the value's identity under SameValueZero, which a Set uses: its
	// type and value for a primitive; empty for an object or array, each of
	// which is its own element.
	id string
	// text is String(value), which Array.prototype.sort compares.
	text string
	// null marks JSON null, which sorts as "null" and joins as "".
	null bool
	// noString marks a value String() throws on (jstext.ToString), so pi's
	// sort and join of a list holding it throw.
	noString bool
}

// stringForms reports whether every item has a string form, as pi's
// computeFileLists sort and formatFileOperations join need.
func stringForms(items []fileListItem) bool {
	return !slices.ContainsFunc(items, func(f fileListItem) bool { return f.noString })
}

// filePath is the path a tool call names.
func filePath(path string) fileListItem { return fileListItem{id: "s" + path, text: path} }

// detailsItem is one element of a details file list, as JSON.parse returns it
// (jstext.Parse).
func detailsItem(v any) fileListItem {
	text, ok := jstext.ToString(v)
	if !ok {
		return fileListItem{noString: true}
	}
	switch x := v.(type) {
	case nil:
		return fileListItem{id: "null", text: text, null: true}
	case bool:
		return fileListItem{id: "b" + text, text: text}
	case json.Number:
		// String(number) tells two numbers apart exactly when SameValueZero
		// does: 5.0 is 5, and -0 is 0.
		return fileListItem{id: "n" + text, text: text}
	case string:
		return filePath(x)
	}
	return fileListItem{text: text}
}

// extractFileOpsFromMessage collects file paths from read/write/edit tool calls
// in an assistant message, or from the nested calls recorded on a tool result
// (port of utils.ts extractFileOpsFromMessage; the nested calls since upstream
// 8562bcf66).
func extractFileOpsFromMessage(m agent.AgentMessage, ops *fileOps) {
	if trm, ok := messageAsToolResult(m); ok {
		// Calls made from codemode scripts are recorded on the script's result.
		if trm.NestedCalls != nil {
			for _, call := range trm.NestedCalls.Calls {
				addFileOp(call.Name, nestedCallArgs(call.Arguments), ops)
			}
		}
		return
	}
	am, ok := messageAsAssistant(m)
	if !ok {
		return
	}
	for _, c := range am.Content {
		if tc, ok := c.(ai.ToolCall); ok {
			addFileOp(tc.Name, tc.Arguments, ops)
		}
	}
}

// nestedCallArgs reads a nested call's recorded arguments as an object. pi's
// reader is `typeof args?.path === "string"`, so arguments of any other shape
// — a string, an array, a null, or none — carry no file operation: the failed
// decode leaves args nil, which is exactly that.
func nestedCallArgs(raw json.RawMessage) map[string]any {
	var args map[string]any
	if json.Unmarshal(raw, &args) != nil {
		return nil
	}
	return args
}

func addFileOp(toolName string, args map[string]any, ops *fileOps) {
	path, _ := args["path"].(string)
	if path == "" {
		return
	}
	switch toolName {
	case "read":
		ops.read.add(filePath(path))
	case "write":
		ops.written.add(filePath(path))
	case "edit":
		ops.edited.add(filePath(path))
	}
}

// lists computes the final sorted file lists (port of utils.ts computeFileLists):
// modified = edited + written; readFiles excludes any file that was also modified.
func (ops *fileOps) lists() (readFiles, modifiedFiles []fileListItem) {
	var modified fileSet
	for _, f := range ops.edited.items {
		modified.add(f)
	}
	for _, f := range ops.written.items {
		modified.add(f)
	}
	for _, f := range ops.read.items {
		if !modified.has(f) {
			readFiles = append(readFiles, f)
		}
	}
	modifiedFiles = modified.items
	sortFileList(readFiles)
	sortFileList(modifiedFiles)
	return readFiles, modifiedFiles
}

// sortFileList is Array.prototype.sort's default order: by String(value), in
// UTF-16 code units, and stable, so equal texts keep their Set order.
func sortFileList(items []fileListItem) {
	slices.SortStableFunc(items, func(a, b fileListItem) int { return jstext.CompareUTF16(a.text, b.text) })
}

// computeFileLists derives the read-only and modified file lists from read/edit/
// write tool calls in the given messages.
func computeFileLists(messages []agent.AgentMessage) (readFiles, modifiedFiles []fileListItem) {
	var ops fileOps
	for _, m := range messages {
		extractFileOpsFromMessage(m, &ops)
	}
	return ops.lists()
}

// formatFileOperations formats read/modified file lists as XML tags appended to
// the summary (port of utils.ts formatFileOperations).
func formatFileOperations(readFiles, modifiedFiles []fileListItem) string {
	var sections []string
	if len(readFiles) > 0 {
		sections = append(sections, "<read-files>\n"+joinFileList(readFiles)+"\n</read-files>")
	}
	if len(modifiedFiles) > 0 {
		sections = append(sections, "<modified-files>\n"+joinFileList(modifiedFiles)+"\n</modified-files>")
	}
	if len(sections) == 0 {
		return ""
	}
	return "\n\n" + strings.Join(sections, "\n\n")
}

// joinFileList is Array.prototype.join("\n"), which writes null as "".
func joinFileList(items []fileListItem) string {
	texts := make([]string, len(items))
	for i, f := range items {
		if !f.null {
			texts[i] = f.text
		}
	}
	return strings.Join(texts, "\n")
}
