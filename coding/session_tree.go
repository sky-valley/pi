package coding

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/sky-valley/pi/agent"
	"github.com/sky-valley/pi/ai"
	"github.com/sky-valley/pi/internal/jstext"
)

func timeParseISO(iso string) (time.Time, error) {
	return time.Parse(time.RFC3339, iso)
}

// SessionEntry is one node in a session tree (port of pi's SessionEntry). Entries
// form a tree via ID/ParentID; the active branch is the path from a leaf to the
// root.
type SessionEntry struct {
	ID            string
	ParentID      string
	Type          string // "message" | "model_change" | "thinking_level_change" | "branch_summary" | "compaction" | "custom_message" | "context_edit" | ...
	Timestamp     string
	Message       ai.Message // for Type=="message"
	Provider      string     // for "model_change"
	ModelID       string     // for "model_change"
	ThinkingLevel string     // for "thinking_level_change"
	Summary       string     // for "branch_summary" / "compaction"
	FromID        string     // for "branch_summary"
	// compaction
	FirstKeptEntryID string
	// SystemMessage is the complete prompt and tool state at a compaction
	// boundary (pi CompactionEntry.systemMessage); BuildContext replays it ahead
	// of the summary. Nil when the entry carries none.
	SystemMessage *ai.SystemMessage
	// Details is a compaction's extension data as written (pi
	// CompactionEntry.details): pi's own compactions store their file lists
	// there. Nil when absent.
	Details json.RawMessage
	// FromHook marks a compaction an extension produced (pi fromHook, read by
	// truthiness); pi ignores its details.
	FromHook bool
	// custom_message
	CustomType string
	Content    ai.ContentList
	// context_edit (pi ContextEditEntry): an append-only edit of TargetID's
	// contribution to model context. A nil Replacement omits the target.
	TargetID    string
	Replacement *ContextEditReplacement

	// messageRole is the role the message field carried in the file, before
	// unmarshalSessionMessage converted pi's extended roles to user messages;
	// context edits replace content only for some of those roles.
	messageRole string
}

// ContextEditReplacement is a context_edit's non-null replacement (pi
// ContextEditEntry.replacement): the content that stands in for the target
// message's content, which pi holds as a string or as blocks.
type ContextEditReplacement struct {
	Content ai.ContentList
	// StringContent marks content that was a plain string in the file; Content
	// then holds it as one text block.
	StringContent bool
}

// SessionTree is the parsed entry tree of a session file.
type SessionTree struct {
	Header   SessionInfo
	Entries  []*SessionEntry
	byID     map[string]*SessionEntry
	children map[string][]*SessionEntry
	// LeafID is the active leaf; defaults to the last entry in the file.
	LeafID string
}

// LoadSessionTree parses a JSONL session file into its entry tree. A v1/v2
// file is migrated in memory first (pi migrateToCurrentVersion via
// readSessionEntries); unlike pi's SessionManager this read-only loader never
// rewrites the file — ResumeSession, the append path, does.
//
// A current-version file is decoded line by line as written, so messages keep
// their key order (a system message's sections render in it). A migrated
// file's entries are re-encoded from their migrated form first, which orders
// nested keys alphabetically; such files predate system messages.
func LoadSessionTree(path string) (*SessionTree, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	lines := readSessionLines(data)
	entries := make([]map[string]any, len(lines))
	for i, line := range lines {
		entries[i] = line.entry
	}
	migrated := migrateSessionEntries(entries)
	t := &SessionTree{byID: map[string]*SessionEntry{}, children: map[string][]*SessionEntry{}}
	for i, entry := range entries {
		line := []byte(lines[i].raw)
		if migrated {
			if line, err = json.Marshal(entry); err != nil {
				continue
			}
		}
		var head struct {
			Type             string          `json:"type"`
			ID               string          `json:"id"`
			ParentID         *string         `json:"parentId"`
			Timestamp        string          `json:"timestamp"`
			Cwd              string          `json:"cwd"`
			Message          json.RawMessage `json:"message"`
			Provider         string          `json:"provider"`
			ModelID          string          `json:"modelId"`
			ThinkingLevel    string          `json:"thinkingLevel"`
			Summary          string          `json:"summary"`
			FromID           string          `json:"fromId"`
			FirstKeptEntryID string          `json:"firstKeptEntryId"`
			SystemMessage    json.RawMessage `json:"systemMessage"`
			Details          json.RawMessage `json:"details"`
			FromHook         json.RawMessage `json:"fromHook"`
			CustomType       string          `json:"customType"`
			Content          json.RawMessage `json:"content"`
			TargetID         string          `json:"targetId"`
			Replacement      json.RawMessage `json:"replacement"`
		}
		if json.Unmarshal(line, &head) != nil {
			continue
		}
		if head.Type == "session" {
			t.Header = SessionInfo{Path: path, ID: head.ID, Cwd: head.Cwd, Timestamp: head.Timestamp}
			continue
		}
		e := &SessionEntry{
			ID: head.ID, Type: head.Type, Timestamp: head.Timestamp,
			Provider: head.Provider, ModelID: head.ModelID,
			ThinkingLevel: head.ThinkingLevel, Summary: head.Summary, FromID: head.FromID,
			FirstKeptEntryID: head.FirstKeptEntryID, CustomType: head.CustomType,
			TargetID: head.TargetID,
		}
		if head.ParentID != nil {
			e.ParentID = *head.ParentID
		}
		if head.Type == "compaction" {
			// pi replays the stored system message only when there is one (a
			// falsy systemMessage is skipped); an undecodable one drops out like
			// an undecodable message.
			if len(head.SystemMessage) > 0 && string(head.SystemMessage) != "null" {
				var system ai.SystemMessage
				if json.Unmarshal(head.SystemMessage, &system) == nil {
					e.SystemMessage = &system
				}
			}
			if len(head.Details) > 0 && string(head.Details) != "null" {
				e.Details = head.Details
			}
			// pi reads fromHook by truthiness, and nothing checks its type when
			// it is written, so a value that is not a boolean must not fail the
			// entry's decode. An absent one is undefined: falsy.
			if fromHook, err := jstext.Parse(head.FromHook); err == nil {
				e.FromHook = jstext.Truthy(fromHook)
			}
		}
		if head.Type == "custom_message" && len(head.Content) > 0 {
			e.Content = parseCustomContent(head.Content)
		}
		if head.Type == "context_edit" {
			// A replacement that is neither null nor {content: string|blocks}
			// still keeps the entry: dropping it would orphan every later entry
			// whose parent chain runs through it, losing the history before it.
			// pi spreads the malformed value over the target, which leaves the
			// target with no usable content, so it is emptied here. (A missing
			// replacement key makes pi throw; the port reads it the same way.)
			replacement, ok := parseContextEditReplacement(head.Replacement)
			if !ok {
				replacement = &ContextEditReplacement{Content: ai.ContentList{}}
			}
			e.Replacement = replacement
		}
		if head.Type == "message" && len(head.Message) > 0 {
			var role struct {
				Role string `json:"role"`
			}
			_ = json.Unmarshal(head.Message, &role)
			e.messageRole = role.Role
			if m, ok := unmarshalSessionMessage(head.Message); ok {
				e.Message = m
				if e.Type == "message" {
					t.Header.Messages++
				}
			}
		}
		t.Entries = append(t.Entries, e)
		t.byID[e.ID] = e
		t.children[e.ParentID] = append(t.children[e.ParentID], e)
	}
	if len(t.Entries) > 0 {
		t.LeafID = t.Entries[len(t.Entries)-1].ID
	}
	return t, nil
}

// resolveLeaf mirrors pi's leaf selection in buildSessionContext: a known id is
// used directly; an empty/unknown id ("undefined") falls back to the last entry
// in the file. (The explicit-null "before first entry" state is BuildContextNull.)
func (t *SessionTree) resolveLeaf(id string) *SessionEntry {
	if id != "" {
		if e := t.byID[id]; e != nil {
			return e
		}
	}
	if len(t.Entries) == 0 {
		return nil
	}
	return t.Entries[len(t.Entries)-1]
}

// Branch returns the entries along the path from the given leaf (default LeafID)
// up to the root, in root→leaf order (port of getBranch). An unknown leaf id
// falls back to the last entry, matching pi.
func (t *SessionTree) Branch(fromID ...string) []*SessionEntry {
	id := t.LeafID
	if len(fromID) > 0 && fromID[0] != "" {
		id = fromID[0]
	}
	leaf := t.resolveLeaf(id)
	var path []*SessionEntry
	// Walk leaf→root appending, then reverse once, for linear traversal (pi
	// a1da88ae: push + reverse instead of an O(n²) prepend per ancestor).
	for cur := leaf; cur != nil; cur = t.byID[cur.ParentID] {
		path = append(path, cur)
		if cur.ParentID == "" {
			break
		}
	}
	for i, j := 0, len(path)-1; i < j; i, j = i+1, j-1 {
		path[i], path[j] = path[j], path[i]
	}
	return path
}

// Leaves returns the entries that have no children (the tips of each branch).
func (t *SessionTree) Leaves() []*SessionEntry {
	var leaves []*SessionEntry
	for _, e := range t.Entries {
		if len(t.children[e.ID]) == 0 {
			leaves = append(leaves, e)
		}
	}
	return leaves
}

// BranchContext is the reconstructed LLM context for a branch (pi
// SessionContext).
type BranchContext struct {
	Messages      []agent.AgentMessage
	ThinkingLevel string
	Provider      string
	ModelID       string
}

// BuildContextNull returns the empty context pi produces for an explicit-null
// leaf (leafId === null) — the "navigated to before the first entry" state.
func (t *SessionTree) BuildContextNull() BranchContext {
	return BranchContext{ThinkingLevel: "off"}
}

// BuildContext reconstructs the LLM message list, thinking level, and model for
// the active branch. It mirrors pi's buildSessionContext followed by
// convertToLlm; see BuildProjection for how the messages are selected.
func (t *SessionTree) BuildContext(leafID ...string) BranchContext {
	return t.BuildProjection(leafID...).BranchContext
}

// ProjectedSessionEntry is one selected entry's contribution to model context
// (pi ProjectedSessionEntry).
type ProjectedSessionEntry struct {
	// SourceEntry is the raw entry that owns the contribution.
	SourceEntry *SessionEntry
	// Messages are the entry's model-visible messages after context edits:
	// empty for state-only entries and omitted targets.
	Messages []agent.AgentMessage
}

// BranchProjection is a branch's model context together with the entry each
// message came from (pi SessionProjection). Messages is the concatenation of
// the entries' messages.
type BranchProjection struct {
	BranchContext
	Entries []ProjectedSessionEntry
}

// BuildProjection reconstructs the active branch's model context with
// per-entry provenance (pi buildSessionProjection followed by convertToLlm).
//
// The selected entries are pi's buildContextEntries: without a compaction the
// whole path; with one, the newest compaction, then the entries before it from
// its firstKeptEntryId on (minus system messages, which the compaction's stored
// system message replays), then everything after it. A retain-none compaction
// names itself as firstKeptEntryId, so nothing before it is kept.
//
// Among the selected entries the latest context_edit per target wins: a nil
// replacement omits the target's messages, a non-nil one replaces only the
// content of a user, assistant, toolResult or custom message. The raw entries
// are not modified. Only the newest compaction contributes its system message
// and summary; an older one inside the kept range contributes nothing.
func (t *SessionTree) BuildProjection(leafID ...string) BranchProjection {
	leaf := t.LeafID
	if len(leafID) > 0 {
		leaf = leafID[0]
	}
	path := t.Branch(leaf)

	var p BranchProjection
	p.ThinkingLevel = "off"
	for _, e := range path {
		switch e.Type {
		case "thinking_level_change":
			p.ThinkingLevel = e.ThinkingLevel
		case "model_change":
			p.Provider, p.ModelID = e.Provider, e.ModelID
		case "message":
			if am, ok := messageAsAssistant(e.Message); ok {
				p.Provider, p.ModelID = am.Provider, am.Model
			}
		}
	}

	selected := contextEntries(path)
	edits := map[string]*SessionEntry{}
	for _, e := range selected {
		if e.Type == "context_edit" {
			edits[e.TargetID] = e
		}
	}
	p.Entries = make([]ProjectedSessionEntry, len(selected))
	for i, e := range selected {
		p.Entries[i].SourceEntry = e
		// contextEntries may keep an older compaction whose id lies inside the
		// newest one's kept range; only the newest, at index 0, contributes a
		// checkpoint and summary.
		if e.Type == "compaction" && i > 0 {
			continue
		}
		p.Entries[i].Messages = projectContextEntry(e, edits[e.ID])
		p.Messages = append(p.Messages, p.Entries[i].Messages...)
	}
	return p
}

// contextEntries selects the path entries that feed model context (pi
// buildContextEntries): with a compaction, the compaction, then the entries it
// kept, then the entries recorded after it.
func contextEntries(path []*SessionEntry) []*SessionEntry {
	compactionIdx := -1
	for i, e := range path {
		if e.Type == "compaction" {
			compactionIdx = i
		}
	}
	if compactionIdx < 0 {
		return path
	}
	compaction := path[compactionIdx]
	selected := []*SessionEntry{compaction}
	foundFirstKept := false
	for _, e := range path[:compactionIdx] {
		if e.ID == compaction.FirstKeptEntryID {
			foundFirstKept = true
		}
		if foundFirstKept && !(e.Type == "message" && e.Message != nil && e.Message.MessageRole() == ai.RoleSystem) {
			selected = append(selected, e)
		}
	}
	return append(selected, path[compactionIdx+1:]...)
}

// sessionEntryMessages is an entry's own contribution to model context, already
// in convertToLlm form (pi sessionEntryToContextMessages + convertToLlm):
// custom_message and branch_summary entries become user messages with pi's
// exact wrapper text. A system message whose content is null or missing reads
// as "" (the codec's own rule, which is pi's sessionEntryToContextMessages rule).
func sessionEntryMessages(e *SessionEntry) []agent.AgentMessage {
	switch e.Type {
	case "message":
		if e.Message != nil {
			return []agent.AgentMessage{e.Message}
		}
	case "custom_message":
		return []agent.AgentMessage{ai.UserMessage{Content: e.Content, Timestamp: entryMillis(e.Timestamp)}}
	case "branch_summary":
		if e.Summary != "" {
			return []agent.AgentMessage{branchSummaryMessage(e.Summary, entryMillis(e.Timestamp))}
		}
	case "compaction":
		// The prompt and tool state it stored, when it stored one, then its
		// summary.
		var msgs []agent.AgentMessage
		if e.SystemMessage != nil {
			msgs = append(msgs, *e.SystemMessage)
		}
		return append(msgs, compactionSummaryMessage(e.Summary, entryMillis(e.Timestamp)))
	}
	return nil
}

// projectContextEntry applies an entry's latest context edit, if any, to its
// messages (pi projectContextEntry). A replacement swaps the content of the
// roles pi edits and keeps every other field; assistant and toolResult content
// is always blocks, so a string replacement becomes one text block. A custom
// message reaches the model as a user message whose content is always blocks
// (convertToLlm).
func projectContextEntry(e *SessionEntry, edit *SessionEntry) []agent.AgentMessage {
	msgs := sessionEntryMessages(e)
	if edit == nil {
		return msgs
	}
	r := edit.Replacement
	if r == nil {
		return nil
	}
	role := e.messageRole
	if e.Type == "custom_message" {
		role = "custom"
	}
	out := make([]agent.AgentMessage, len(msgs))
	for i, m := range msgs {
		out[i] = m
		switch v := m.(type) {
		case ai.UserMessage:
			switch role {
			case "user":
				if r.StringContent {
					out[i] = ai.NewUserText(r.Content[0].(ai.TextContent).Text, v.Timestamp)
				} else {
					out[i] = ai.UserMessage{Content: r.Content, Timestamp: v.Timestamp}
				}
			case "custom":
				out[i] = ai.UserMessage{Content: r.Content, Timestamp: v.Timestamp}
			}
		case ai.AssistantMessage:
			v.Content = r.Content
			out[i] = v
		case ai.ToolResultMessage:
			v.Content = r.Content
			out[i] = v
		}
	}
	return out
}

// parseContextEditReplacement decodes a context_edit's replacement: JSON null
// is an omission (nil), an object with string or block content a replacement.
// Anything else is not an edit pi could have written (appendContextEdit rejects
// it), so ok is false; LoadSessionTree keeps the entry and empties the target.
func parseContextEditReplacement(raw json.RawMessage) (replacement *ContextEditReplacement, ok bool) {
	if string(raw) == "null" {
		return nil, true
	}
	var body struct {
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(raw, &body) != nil || len(body.Content) == 0 {
		return nil, false
	}
	if body.Content[0] == '"' {
		var text string
		if json.Unmarshal(body.Content, &text) != nil {
			return nil, false
		}
		return &ContextEditReplacement{Content: ai.ContentList{ai.TextContent{Text: text}}, StringContent: true}, true
	}
	var content ai.ContentList
	if json.Unmarshal(body.Content, &content) != nil {
		return nil, false
	}
	return &ContextEditReplacement{Content: content}, true
}

// unmarshalSessionMessage decodes a message entry's message field. Standard LLM
// roles go through ai.UnmarshalMessage; pi's extended AgentMessage roles
// (messages.ts: bashExecution, custom, branchSummary, compactionSummary) are
// converted to the user message pi's convertToLlm would send, so sessions
// written by pi reconstruct identically instead of dropping those entries.
// Returns ok=false for undecodable messages and for bashExecution messages
// excluded from context (the "!!" prefix), which convertToLlm skips.
func unmarshalSessionMessage(raw json.RawMessage) (ai.Message, bool) {
	var head struct {
		Role string `json:"role"`
	}
	if json.Unmarshal(raw, &head) != nil {
		return nil, false
	}
	switch head.Role {
	case "bashExecution":
		var m struct {
			Command            string `json:"command"`
			Output             string `json:"output"`
			ExitCode           *int   `json:"exitCode"`
			Cancelled          bool   `json:"cancelled"`
			Truncated          bool   `json:"truncated"`
			FullOutputPath     string `json:"fullOutputPath"`
			Timestamp          int64  `json:"timestamp"`
			ExcludeFromContext bool   `json:"excludeFromContext"`
		}
		if json.Unmarshal(raw, &m) != nil {
			return nil, false
		}
		if m.ExcludeFromContext {
			return nil, false
		}
		text := bashExecutionToText(m.Command, m.Output, m.ExitCode, m.Cancelled, m.Truncated, m.FullOutputPath)
		return ai.UserMessage{Content: ai.ContentList{ai.TextContent{Text: text}}, Timestamp: m.Timestamp}, true
	case "custom":
		var m struct {
			Content   json.RawMessage `json:"content"`
			Timestamp int64           `json:"timestamp"`
		}
		if json.Unmarshal(raw, &m) != nil {
			return nil, false
		}
		return ai.UserMessage{Content: parseCustomContent(m.Content), Timestamp: m.Timestamp}, true
	case "branchSummary":
		var m struct {
			Summary   string `json:"summary"`
			Timestamp int64  `json:"timestamp"`
		}
		if json.Unmarshal(raw, &m) != nil {
			return nil, false
		}
		return branchSummaryMessage(m.Summary, m.Timestamp), true
	case "compactionSummary":
		var m struct {
			Summary   string `json:"summary"`
			Timestamp int64  `json:"timestamp"`
		}
		if json.Unmarshal(raw, &m) != nil {
			return nil, false
		}
		return compactionSummaryMessage(m.Summary, m.Timestamp), true
	default:
		m, err := ai.UnmarshalMessage(raw)
		if err != nil {
			return nil, false
		}
		return m, true
	}
}

// bashExecutionToText ports pi's bashExecutionToText (messages.ts:148-167):
// the user-facing text a bashExecution message renders to in LLM context.
func bashExecutionToText(command, output string, exitCode *int, cancelled, truncated bool, fullOutputPath string) string {
	text := "Ran `" + command + "`\n"
	if output != "" {
		text += "```\n" + output + "\n```"
	} else {
		text += "(no output)"
	}
	if cancelled {
		text += "\n\n(command cancelled)"
	} else if exitCode != nil && *exitCode != 0 {
		text += fmt.Sprintf("\n\nCommand exited with code %d", *exitCode)
	}
	if truncated && fullOutputPath != "" {
		text += "\n\n[Output truncated. Full output: " + fullOutputPath + "]"
	}
	return text
}

// parseCustomContent decodes a custom_message content field (string or block array).
func parseCustomContent(raw json.RawMessage) ai.ContentList {
	if len(raw) > 0 && raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return ai.ContentList{ai.TextContent{Text: s}}
		}
		return nil
	}
	var cl ai.ContentList
	_ = json.Unmarshal(raw, &cl)
	return cl
}

// entryMillis converts an ISO timestamp to Unix millis (pi: new Date(ts).getTime()).
func entryMillis(iso string) int64 {
	if iso == "" {
		return 0
	}
	if tm, err := timeParseISO(iso); err == nil {
		return tm.UnixMilli()
	}
	return 0
}
