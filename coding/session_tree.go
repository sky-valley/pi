package coding

import (
	"encoding/json"
	"os"
	"strings"
	"time"

	"github.com/sky-valley/pi/agent"
	"github.com/sky-valley/pi/ai"
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
	Type          string // "message" | "model_change" | "thinking_level_change" | "branch_summary" | "compaction" | "custom_message" | ...
	Timestamp     string
	Message       ai.Message // for Type=="message"
	Provider      string     // for "model_change"
	ModelID       string     // for "model_change"
	ThinkingLevel string     // for "thinking_level_change"
	Summary       string     // for "branch_summary" / "compaction"
	FromID        string     // for "branch_summary"
	// compaction
	FirstKeptEntryID string
	// custom_message
	CustomType string
	Content    ai.ContentList
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

// LoadSessionTree parses a JSONL session file into its entry tree.
func LoadSessionTree(path string) (*SessionTree, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	t := &SessionTree{byID: map[string]*SessionEntry{}, children: map[string][]*SessionEntry{}}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
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
			CustomType       string          `json:"customType"`
			Content          json.RawMessage `json:"content"`
		}
		if json.Unmarshal([]byte(line), &head) != nil {
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
		}
		if head.ParentID != nil {
			e.ParentID = *head.ParentID
		}
		if head.Type == "custom_message" && len(head.Content) > 0 {
			e.Content = parseCustomContent(head.Content)
		}
		if head.Type == "message" && len(head.Message) > 0 {
			if m, err := ai.UnmarshalMessage(head.Message); err == nil {
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
	for cur := leaf; cur != nil; cur = t.byID[cur.ParentID] {
		path = append([]*SessionEntry{cur}, path...)
		if cur.ParentID == "" {
			break
		}
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

// BranchContext is the reconstructed LLM context for a branch.
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
// the active branch. It mirrors pi's buildSessionContext followed by convertToLlm:
// it handles the compaction checkpoint (emit summary, then kept entries from
// firstKeptEntryId, then post-compaction entries) and converts custom_message /
// branch_summary entries to their user-message form with pi's exact wrapper text.
func (t *SessionTree) BuildContext(leafID ...string) BranchContext {
	leaf := t.LeafID
	if len(leafID) > 0 {
		leaf = leafID[0]
	}
	path := t.Branch(leaf)

	ctx := BranchContext{ThinkingLevel: "off"}
	var compaction *SessionEntry
	compactionIdx := -1
	for i, e := range path {
		switch e.Type {
		case "thinking_level_change":
			ctx.ThinkingLevel = e.ThinkingLevel
		case "model_change":
			ctx.Provider, ctx.ModelID = e.Provider, e.ModelID
		case "message":
			if am, ok := messageAsAssistant(e.Message); ok {
				ctx.Provider, ctx.ModelID = am.Provider, am.Model
			}
		case "compaction":
			compaction = e
			compactionIdx = i
		}
	}

	appendMessage := func(e *SessionEntry) {
		switch e.Type {
		case "message":
			if e.Message != nil {
				ctx.Messages = append(ctx.Messages, e.Message)
			}
		case "custom_message":
			ctx.Messages = append(ctx.Messages, ai.UserMessage{Content: e.Content, Timestamp: entryMillis(e.Timestamp)})
		case "branch_summary":
			if e.Summary != "" {
				ctx.Messages = append(ctx.Messages, branchSummaryMessage(e.Summary, entryMillis(e.Timestamp)))
			}
		}
	}

	if compaction != nil {
		// 1. Emit the compaction summary first.
		ctx.Messages = append(ctx.Messages, compactionSummaryMessage(compaction.Summary, entryMillis(compaction.Timestamp)))
		// 2. Emit kept messages before the compaction, starting at firstKeptEntryId.
		foundFirstKept := false
		for i := 0; i < compactionIdx; i++ {
			if path[i].ID == compaction.FirstKeptEntryID {
				foundFirstKept = true
			}
			if foundFirstKept {
				appendMessage(path[i])
			}
		}
		// 3. Emit everything after the compaction.
		for i := compactionIdx + 1; i < len(path); i++ {
			appendMessage(path[i])
		}
	} else {
		for _, e := range path {
			appendMessage(e)
		}
	}
	return ctx
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
