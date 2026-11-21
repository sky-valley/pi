package coding

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sky-valley/pi/agent"
	"github.com/sky-valley/pi/ai"
)

func assistantMsg(text string) ai.AssistantMessage {
	return ai.AssistantMessage{Content: ai.ContentList{ai.TextContent{Text: text}}, Provider: "p", Model: "m", StopReason: ai.StopStop, Timestamp: 1}
}

func userTexts(msgs []agent.AgentMessage) []string {
	var out []string
	for _, m := range msgs {
		switch v := m.(type) {
		case ai.UserMessage:
			if t, ok := v.Content[0].(ai.TextContent); ok {
				out = append(out, "U:"+t.Text)
			}
		case ai.AssistantMessage:
			out = append(out, "A:"+textFromAssistant(v.Content))
		case *ai.AssistantMessage:
			out = append(out, "A:"+textFromAssistant(v.Content))
		}
	}
	return out
}

func textFromAssistant(c ai.ContentList) string {
	for _, b := range c {
		if t, ok := b.(ai.TextContent); ok {
			return t.Text
		}
	}
	return ""
}

func TestSessionTreeForkAndBranches(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cwd := t.TempDir()

	rec, err := StartSession(cwd, &ai.Model{ID: "m", Provider: "p"})
	if err != nil {
		t.Fatal(err)
	}
	// Shared trunk.
	rec.RecordMessage(ai.NewUserText("question", 1))
	branchPoint := rec.RecordMessage(assistantMsg("trunk answer"))

	// Branch A continues from the trunk.
	rec.RecordMessage(ai.NewUserText("follow-up A", 2))
	rec.RecordMessage(assistantMsg("answer A"))

	// Fork a new branch B from the trunk's assistant answer.
	rec.ForkFrom(branchPoint)
	rec.RecordMessage(ai.NewUserText("follow-up B", 3))
	rec.RecordMessage(assistantMsg("answer B"))
	rec.Close()

	tree, err := LoadSessionTree(rec.Path())
	if err != nil {
		t.Fatal(err)
	}

	// Two divergent tips.
	leaves := tree.Leaves()
	if len(leaves) != 2 {
		t.Fatalf("expected 2 leaves (branches), got %d", len(leaves))
	}

	// Identify each leaf by its assistant text.
	var leafA, leafB string
	for _, l := range leaves {
		if am, ok := messageAsAssistant(l.Message); ok {
			switch textFromAssistant(am.Content) {
			case "answer A":
				leafA = l.ID
			case "answer B":
				leafB = l.ID
			}
		}
	}
	if leafA == "" || leafB == "" {
		t.Fatalf("could not find both branch leaves: A=%q B=%q", leafA, leafB)
	}

	ctxA := tree.BuildContext(leafA)
	if got := userTexts(ctxA.Messages); !eq(got, []string{"U:question", "A:trunk answer", "U:follow-up A", "A:answer A"}) {
		t.Fatalf("branch A context wrong: %v", got)
	}
	ctxB := tree.BuildContext(leafB)
	if got := userTexts(ctxB.Messages); !eq(got, []string{"U:question", "A:trunk answer", "U:follow-up B", "A:answer B"}) {
		t.Fatalf("branch B context wrong: %v", got)
	}
	// Branches share the trunk but diverge after it.
	if ctxA.Provider != "p" || ctxA.ModelID != "m" {
		t.Fatalf("model not recovered from branch: %+v", ctxA)
	}
}

// TestCompactionBoundaryOnAnUnknownEntryTypeKeepsTheTail characterizes
// BuildContext at a compaction boundary that lands on an entry type the port
// does not model — here a pi `label` entry, a real tree node carrying no
// message. Upstream's buildContextEntries (session-manager.ts:440-448 @
// 64eeb82a4) flips `foundFirstKept` on whatever node matches firstKeptEntryId
// and keeps everything from there on, and sessionEntryToContextMessages returns
// [] for an unmodelled type; the port must do the same, which means
// LoadSessionTree has to retain unknown entry types as tree nodes rather than
// dropping them.
//
// It is NOT a regression test for upstream #8989 (2631b25c3): that fix lives
// entirely in SessionManager.createBranchedSession, which strips labels and
// re-chains the retained path, leaving firstKeptEntryId dangling. On this
// fixture — an unforked file where the label is still on the path — upstream
// produces [summary, kept, after] both before and after the fix. The port has
// no label entries and no fork writer, so #8989 has no Go home at all; see
// docs/UPSTREAM.md before "fixing" that here.
func TestCompactionBoundaryOnAnUnknownEntryTypeKeepsTheTail(t *testing.T) {
	path := writeSessionLines(t, []string{
		`{"type":"session","version":3,"id":"0190aaaa-bbbb-7ccc-8ddd-eeeeffff0000","timestamp":"2026-01-01T00:00:00.000Z","cwd":"/p"}`,
		`{"type":"message","id":"e1","parentId":null,"timestamp":"2026-01-01T00:00:01.000Z","message":{"role":"user","content":[{"type":"text","text":"old"}],"timestamp":1}}`,
		`{"type":"label","id":"lbl","parentId":"e1","timestamp":"2026-01-01T00:00:02.000Z","targetId":"e1","label":"checkpoint"}`,
		`{"type":"message","id":"e2","parentId":"lbl","timestamp":"2026-01-01T00:00:03.000Z","message":{"role":"user","content":[{"type":"text","text":"kept"}],"timestamp":2}}`,
		`{"type":"compaction","id":"cmp","parentId":"e2","timestamp":"2026-01-01T00:00:04.000Z","summary":"SUMMARY","firstKeptEntryId":"lbl","tokensBefore":100}`,
		`{"type":"message","id":"e3","parentId":"cmp","timestamp":"2026-01-01T00:00:05.000Z","message":{"role":"user","content":[{"type":"text","text":"after"}],"timestamp":3}}`,
	})

	tree, err := LoadSessionTree(path)
	if err != nil {
		t.Fatal(err)
	}
	got := userTexts(tree.BuildContext().Messages)
	if len(got) != 3 || !strings.Contains(got[0], "SUMMARY") {
		t.Fatalf("expected summary + kept + after, got %v", got)
	}
	if !eq(got[1:], []string{"U:kept", "U:after"}) {
		t.Fatalf("compaction boundary on a label dropped the kept tail: %v", got)
	}
}

// TestLoadSessionTreeMigratesAV1Session ports pi's migrateToCurrentVersion
// (session-manager.ts:231-289 @ 64eeb82a4) on the read side. A v1 file has no
// header version and no id/parentId on its entries; pi's migrateV1ToV2 chains
// them in file order under fresh 8-hex ids and turns a compaction's
// firstKeptEntryIndex (an index into the file INCLUDING the header) into a
// firstKeptEntryId, then migrateV2ToV3 renames the hookMessage role to custom.
// Without that walk every entry parses with id "", the tree collapses onto one
// node, and the transcript reconstructs as its last message alone.
func TestLoadSessionTreeMigratesAV1Session(t *testing.T) {
	path := writeSessionLines(t, []string{
		`{"type":"session","id":"0190aaaa-bbbb-7ccc-8ddd-eeeeffff0000","timestamp":"2026-01-01T00:00:00.000Z","cwd":"/p"}`,
		`{"type":"message","timestamp":"2026-01-01T00:00:01.000Z","message":{"role":"user","content":[{"type":"text","text":"q1"}],"timestamp":1}}`,
		`{"type":"message","timestamp":"2026-01-01T00:00:02.000Z","message":{"role":"assistant","content":[{"type":"text","text":"a1"}],"provider":"p","model":"m","stopReason":"stop","timestamp":2}}`,
		`{"type":"message","timestamp":"2026-01-01T00:00:03.000Z","message":{"role":"hookMessage","customType":"x","content":[{"type":"text","text":"hook"}],"timestamp":3}}`,
		`{"type":"compaction","timestamp":"2026-01-01T00:00:04.000Z","summary":"SUMMARY","firstKeptEntryIndex":3,"tokensBefore":100}`,
		`{"type":"message","timestamp":"2026-01-01T00:00:05.000Z","message":{"role":"user","content":[{"type":"text","text":"q2"}],"timestamp":5}}`,
		`{"type":"message","timestamp":"2026-01-01T00:00:06.000Z","message":{"role":"assistant","content":[{"type":"text","text":"a2"}],"provider":"p","model":"m","stopReason":"stop","timestamp":6}}`,
	})

	tree, err := LoadSessionTree(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(tree.Entries) != 6 {
		t.Fatalf("expected 6 migrated entries, got %d", len(tree.Entries))
	}
	// migrateV1ToV2: fresh 8-hex ids, each entry's parent is the previous one.
	prev := ""
	for i, e := range tree.Entries {
		if !entryIDRe.MatchString(e.ID) {
			t.Fatalf("entry %d id %q is not a generated 8-hex entry id", i, e.ID)
		}
		if e.ParentID != prev {
			t.Fatalf("entry %d parent = %q, want the previous entry %q", i, e.ParentID, prev)
		}
		prev = e.ID
	}
	// firstKeptEntryIndex 3 counts the header: it is the hookMessage entry.
	if got, want := tree.Entries[3].FirstKeptEntryID, tree.Entries[2].ID; got != want {
		t.Fatalf("compaction firstKeptEntryId = %q, want the entry at file index 3, %q", got, want)
	}
	got := userTexts(tree.BuildContext().Messages)
	if len(got) != 4 || !strings.Contains(got[0], "SUMMARY") {
		t.Fatalf("expected summary + kept hook + q2 + a2, got %v", got)
	}
	// migrateV2ToV3: the hookMessage entry is the kept tail, resumed as a custom
	// (user-role) message rather than dropped as an unknown role.
	if !eq(got[1:], []string{"U:hook", "U:q2", "A:a2"}) {
		t.Fatalf("v1 transcript reconstructed wrong: %v", got)
	}
}

// TestLoadSessionTreeMigratesAV2HookMessage pins the v2->v3 half on its own: a
// version-2 file already has ids, so only the hookMessage->custom rename runs.
func TestLoadSessionTreeMigratesAV2HookMessage(t *testing.T) {
	path := writeSessionLines(t, []string{
		`{"type":"session","version":2,"id":"0190aaaa-bbbb-7ccc-8ddd-eeeeffff0000","timestamp":"2026-01-01T00:00:00.000Z","cwd":"/p"}`,
		`{"type":"message","id":"e1","parentId":null,"timestamp":"2026-01-01T00:00:01.000Z","message":{"role":"hookMessage","customType":"x","content":[{"type":"text","text":"hook"}],"timestamp":1}}`,
		`{"type":"message","id":"e2","parentId":"e1","timestamp":"2026-01-01T00:00:02.000Z","message":{"role":"user","content":[{"type":"text","text":"q"}],"timestamp":2}}`,
	})

	tree, err := LoadSessionTree(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := userTexts(tree.BuildContext().Messages); !eq(got, []string{"U:hook", "U:q"}) {
		t.Fatalf("v2 hookMessage not migrated to custom: %v", got)
	}
	// Ids were already present and must be kept, not regenerated.
	if tree.Entries[0].ID != "e1" || tree.Entries[1].ParentID != "e1" {
		t.Fatalf("v2 ids must survive migration: %+v", tree.Entries)
	}
}

// TestLoadSessionTreeSkipsAFusedLineAsPiDoes pins pi's parseSessionEntryLine
// (session-manager.ts:503-511 @ 64eeb82a4) on a line carrying two entries fused
// together — the corruption an unterminated tail plus a later append produces
// (issue #8345, D17). JSON.parse throws on trailing bytes, so pi drops the whole
// line: neither entry survives. A decoder that stops after the first value
// would resurrect the first entry as an orphan and diverge from pi's tree.
func TestLoadSessionTreeSkipsAFusedLineAsPiDoes(t *testing.T) {
	path := writeSessionLines(t, []string{
		`{"type":"session","version":3,"id":"0190aaaa-bbbb-7ccc-8ddd-eeeeffff0000","timestamp":"2026-01-01T00:00:00.000Z","cwd":"/p"}`,
		`{"type":"message","id":"e1","parentId":null,"timestamp":"2026-01-01T00:00:01.000Z","message":{"role":"user","content":[{"type":"text","text":"a"}],"timestamp":1}}` +
			`{"type":"message","id":"e2","parentId":"e1","timestamp":"2026-01-01T00:00:02.000Z","message":{"role":"user","content":[{"type":"text","text":"b"}],"timestamp":2}}`,
		`{"type":"message","id":"e3","parentId":"e2","timestamp":"2026-01-01T00:00:03.000Z","message":{"role":"user","content":[{"type":"text","text":"c"}],"timestamp":3}}`,
	})

	tree, err := LoadSessionTree(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(tree.Entries) != 1 || tree.Entries[0].ID != "e3" {
		var ids []string
		for _, e := range tree.Entries {
			ids = append(ids, e.ID)
		}
		t.Fatalf("a fused line must be dropped whole (pi JSON.parse throws): want entries [e3], got %v", ids)
	}
	if got := userTexts(tree.BuildContext().Messages); !eq(got, []string{"U:c"}) {
		t.Fatalf("context after a dropped fused line: want [U:c], got %v", got)
	}
}

// pi cb1205e46 (#12360) 'handles more entries after compaction than fit in the
// call stack'. A characterization test: pi's buildContextEntries spread the
// post-compaction tail into one push call and overflowed JS's argument limit;
// Go's append has no such limit, so the port never failed here and the test
// was never red. It locks that a very long tail stays whole.
func TestBuildContextWithVeryManyEntriesAfterCompaction(t *testing.T) {
	const bulk = 200_000
	tree := &SessionTree{byID: map[string]*SessionEntry{}, children: map[string][]*SessionEntry{}}
	add := func(e *SessionEntry) {
		tree.Entries = append(tree.Entries, e)
		tree.byID[e.ID] = e
		tree.children[e.ParentID] = append(tree.children[e.ParentID], e)
		tree.LeafID = e.ID
	}
	userEntry := func(id, parentID, text string) *SessionEntry {
		return &SessionEntry{ID: id, ParentID: parentID, Type: "message", Timestamp: "2026-01-01T00:00:01.000Z",
			Message: ai.UserMessage{Content: ai.ContentList{ai.TextContent{Text: text}}, Timestamp: 1}}
	}
	add(userEntry("1", "", "first"))
	add(&SessionEntry{ID: "2", ParentID: "1", Type: "compaction", Timestamp: "2026-01-01T00:00:02.000Z",
		Summary: "Summary", FirstKeptEntryID: "1"})
	parentID := "2"
	for i := range bulk {
		id := fmt.Sprintf("c%d", i)
		add(&SessionEntry{ID: id, ParentID: parentID, Type: "custom", Timestamp: "2026-01-01T00:00:03.000Z", CustomType: "bulk"})
		parentID = id
	}
	add(userEntry("last", parentID, "last"))

	if got := len(contextEntries(tree.Branch())); got != bulk+3 {
		t.Fatalf("context entries = %d, want %d", got, bulk+3)
	}
	got := userTexts(tree.BuildContext().Messages)
	if len(got) != 3 || !strings.Contains(got[0], "Summary") || !eq(got[1:], []string{"U:first", "U:last"}) {
		t.Fatalf("messages = %v, want [compaction summary, U:first, U:last]", got)
	}
}

func writeSessionLines(t *testing.T, lines []string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sess.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func eq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
