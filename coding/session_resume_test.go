package coding

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/sky-valley/pi/ai"
)

func sessionEntryTypes(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var types []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var head struct {
			Type string `json:"type"`
		}
		if json.Unmarshal([]byte(line), &head) != nil {
			continue
		}
		types = append(types, head.Type)
	}
	return types
}

// I10: StartSession with a thinking level records model_change then
// thinking_level_change, in that order (pi sdk.ts:362-373 for new sessions).
func TestStartSessionRecordsThinkingAfterModel(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cwd := t.TempDir()
	rec, err := StartSession(cwd, &ai.Model{ID: "m", Provider: "p"}, "medium")
	if err != nil {
		t.Fatal(err)
	}
	// Flush by recording an assistant message.
	rec.RecordMessage(&ai.AssistantMessage{Content: ai.ContentList{ai.TextContent{Text: "hi"}}, StopReason: ai.StopStop, Timestamp: 1})
	rec.Close()

	types := sessionEntryTypes(t, rec.Path())
	want := []string{"session", "model_change", "thinking_level_change", "message"}
	if len(types) != len(want) {
		t.Fatalf("entry types = %v, want %v", types, want)
	}
	for i := range want {
		if types[i] != want[i] {
			t.Fatalf("entry types = %v, want %v", types, want)
		}
	}
	data, _ := os.ReadFile(rec.Path())
	if !strings.Contains(string(data), `"thinkingLevel":"medium"`) {
		t.Fatalf("thinking level value missing: %s", data)
	}
}

// I13(d): resuming APPENDS to the existing session file (pi setSessionFile:
// entries load, the leaf is the file's last entry, flushed=true so appends are
// immediate) — it never forks a new file.
func TestResumeSessionAppendsToSameFile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cwd := t.TempDir()

	rec, err := StartSession(cwd, &ai.Model{ID: "m", Provider: "p"}, "medium")
	if err != nil {
		t.Fatal(err)
	}
	rec.RecordMessage(ai.NewUserText("first question", 1))
	rec.RecordMessage(&ai.AssistantMessage{Content: ai.ContentList{ai.TextContent{Text: "first reply"}}, StopReason: ai.StopStop, Timestamp: 2})
	firstLeaf := rec.LastEntryID()
	rec.Close()
	path := rec.Path()

	// Resume: same id, appends branch from the prior leaf.
	rec2, err := ResumeSession(path)
	if err != nil {
		t.Fatal(err)
	}
	if rec2.Path() != path {
		t.Fatalf("resume must keep the same file: %s vs %s", rec2.Path(), path)
	}
	if rec2.ID() != rec.ID() {
		t.Fatalf("resume must keep the session id: %s vs %s", rec2.ID(), rec.ID())
	}
	if rec2.LastEntryID() != firstLeaf {
		t.Fatalf("resume leaf should be the file's last entry: %s vs %s", rec2.LastEntryID(), firstLeaf)
	}
	// Appends are immediate in resume mode (pi flushed=true), even before any
	// new assistant message arrives.
	userID := rec2.RecordMessage(ai.NewUserText("second question", 3))
	if data, _ := os.ReadFile(path); !strings.Contains(string(data), "second question") {
		t.Fatalf("resume append not immediate:\n%s", data)
	}
	rec2.RecordMessage(&ai.AssistantMessage{Content: ai.ContentList{ai.TextContent{Text: "second reply"}}, StopReason: ai.StopStop, Timestamp: 4})
	rec2.Close()

	// Single header; tree links the new entries under the old leaf.
	types := sessionEntryTypes(t, path)
	headers := 0
	for _, ty := range types {
		if ty == "session" {
			headers++
		}
	}
	if headers != 1 {
		t.Fatalf("resume duplicated the session header: %v", types)
	}
	tree, err := LoadSessionTree(path)
	if err != nil {
		t.Fatal(err)
	}
	var newUser *SessionEntry
	for _, e := range tree.Entries {
		if e.ID == userID {
			newUser = e
		}
	}
	if newUser == nil || newUser.ParentID != firstLeaf {
		t.Fatalf("appended entry not parented to prior leaf: %+v", newUser)
	}
	ctx := tree.BuildContext()
	if len(ctx.Messages) != 4 {
		t.Fatalf("expected 4 messages on the branch, got %d", len(ctx.Messages))
	}
}

// I13(a): the first flush opens with O_EXCL ("wx" in pi) — an existing file at
// the session path is never clobbered.
func TestFirstFlushExclusiveCreate(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cwd := t.TempDir()
	rec, err := StartSession(cwd, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rec.Path(), []byte("pre-existing\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec.RecordMessage(&ai.AssistantMessage{Content: ai.ContentList{ai.TextContent{Text: "hi"}}, StopReason: ai.StopStop, Timestamp: 1})
	rec.Close()

	data, err := os.ReadFile(rec.Path())
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "pre-existing\n" {
		t.Fatalf("existing file was clobbered:\n%s", data)
	}
}
