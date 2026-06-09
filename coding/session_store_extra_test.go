package coding

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/sky-valley/pi/agent"
	"github.com/sky-valley/pi/ai"
)

// TestStartedUnusedSessionAbsentFromDisk verifies a session that records no
// assistant message never touches disk: no file, and it is invisible to
// ListSessions (pi _persist withholds writes until the first assistant message).
func TestStartedUnusedSessionAbsentFromDisk(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cwd := t.TempDir()

	rec, err := StartSession(cwd, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Record a user message and a thinking-level change — but no assistant message.
	rec.RecordMessage(ai.NewUserText("hello", 1))
	rec.RecordThinkingLevel("medium")
	rec.Close()

	if _, err := os.Stat(rec.Path()); !os.IsNotExist(err) {
		t.Fatalf("session file should not exist yet, stat err=%v", err)
	}
	if infos := ListSessions(cwd); len(infos) != 0 {
		t.Fatalf("ListSessions should be empty, got %+v", infos)
	}

	// Once an assistant message arrives, the whole buffer flushes atomically.
	rec.RecordMessage(&ai.AssistantMessage{Content: ai.ContentList{ai.TextContent{Text: "hi"}}, StopReason: ai.StopStop, Timestamp: 2})
	if _, err := os.Stat(rec.Path()); err != nil {
		t.Fatalf("session file should exist after assistant message: %v", err)
	}
	data, _ := os.ReadFile(rec.Path())
	// All buffered entries (header + user + thinking + assistant) are present.
	for _, want := range []string{`"type":"session"`, `"hello"`, `"thinking_level_change"`, `"role":"assistant"`} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("flushed file missing %q:\n%s", want, data)
		}
	}
	if infos := ListSessions(cwd); len(infos) != 1 {
		t.Fatalf("ListSessions should now show 1 session, got %+v", infos)
	}
}

// TestSessionIDAndFilenameShape verifies the header id is a uuidv7 (version 7,
// RFC-4122 variant) and the filename is <iso-with-dashes>_<uuidv7>.jsonl.
func TestSessionIDAndFilenameShape(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cwd := t.TempDir()
	rec, err := StartSession(cwd, nil)
	if err != nil {
		t.Fatal(err)
	}
	uuidRe := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	if !uuidRe.MatchString(rec.ID()) {
		t.Fatalf("header id is not a uuidv7: %q", rec.ID())
	}
	base := filepath.Base(rec.Path())
	if !strings.HasSuffix(base, "_"+rec.ID()+".jsonl") {
		t.Fatalf("filename does not embed the uuidv7 id: %q", base)
	}
	// The timestamp portion contains no ':' or '.' (replaced with '-').
	tsPart := strings.TrimSuffix(base, "_"+rec.ID()+".jsonl")
	if strings.ContainsAny(tsPart, ":.") {
		t.Fatalf("filename timestamp not sanitized: %q", tsPart)
	}
}

// TestEntryIDsCollisionChecked verifies entry ids are 8 hex chars and unique
// across many recorded entries.
func TestEntryIDsCollisionChecked(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cwd := t.TempDir()
	rec, _ := StartSession(cwd, nil)
	// Force a flush so entries are observable, then read ids back from disk.
	rec.RecordMessage(&ai.AssistantMessage{Content: ai.ContentList{ai.TextContent{Text: "a"}}, StopReason: ai.StopStop, Timestamp: 1})
	for i := 0; i < 50; i++ {
		rec.RecordThinkingLevel("medium")
	}
	rec.Close()

	data, _ := os.ReadFile(rec.Path())
	ids := map[string]bool{}
	idRe := regexp.MustCompile(`^[0-9a-f]{8}$`)
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var e struct {
			Type string `json:"type"`
			ID   string `json:"id"`
		}
		if json.Unmarshal([]byte(line), &e) != nil {
			continue
		}
		if e.Type == "session" {
			continue // header id is a uuidv7, not an 8-hex entry id
		}
		if !idRe.MatchString(e.ID) {
			t.Fatalf("entry id not 8 hex chars: %q (type %s)", e.ID, e.Type)
		}
		if ids[e.ID] {
			t.Fatalf("duplicate entry id: %q", e.ID)
		}
		ids[e.ID] = true
	}
}

// TestEncodeCwdStripsOneLeadingSeparator verifies the cwd encoding strips
// exactly ONE leading separator (pi replace(/^[/\\]/, "")) rather than all of
// them (the old TrimLeft bug). filepath.Abs collapses consecutive POSIX slashes,
// so this exercises encodeCwdSafePath directly on inputs that retain multiple
// leading separators (e.g. a Windows-style UNC/backslash path).
func TestEncodeCwdStripsOneLeadingSeparator(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/a/b", "a-b"},                         // single leading slash stripped
		{"//double/leading", "-double-leading"}, // only the FIRST of two stripped
		{`\\unc\share`, `-unc-share`},           // backslash UNC: one backslash stripped
		{"/x", "x"},
	}
	for _, c := range cases {
		if got := encodeCwdSafePath(c.in); got != c.want {
			t.Fatalf("encodeCwdSafePath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestLoadSessionMessagesCompactedResume verifies a JSONL session containing a
// compaction entry resumes with the summary (pi wrapper text) in place of the
// pre-compaction turns, rather than naively replaying every message.
func TestLoadSessionMessagesCompactedResume(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sess.jsonl")

	userMsg := func(text string) json.RawMessage {
		b, _ := json.Marshal(ai.NewUserText(text, 1))
		return b
	}
	asstMsg := func(text string) json.RawMessage {
		b, _ := json.Marshal(&ai.AssistantMessage{Content: ai.ContentList{ai.TextContent{Text: text}}, StopReason: ai.StopStop, Timestamp: 1})
		return b
	}

	lines := []map[string]any{
		{"type": "session", "version": CurrentSessionVersion, "id": "0190aaaa-bbbb-7ccc-8ddd-eeeeffff0000", "timestamp": "2026-06-08T00:00:00.000Z", "cwd": dir, "id_": ""},
		{"type": "message", "id": "aaaaaaaa", "parentId": nil, "message": userMsg("old question")},
		{"type": "message", "id": "bbbbbbbb", "parentId": "aaaaaaaa", "message": asstMsg("old answer")},
		{"type": "compaction", "id": "cccccccc", "parentId": "bbbbbbbb", "summary": "SUMMARY OF OLD WORK", "firstKeptEntryId": "dddddddd", "tokensBefore": 1234, "timestamp": "2026-06-08T00:01:00.000Z"},
		{"type": "message", "id": "dddddddd", "parentId": "cccccccc", "message": userMsg("recent question")},
		{"type": "message", "id": "eeeeeeee", "parentId": "dddddddd", "message": asstMsg("recent answer")},
	}
	var sb strings.Builder
	for _, l := range lines {
		b, _ := json.Marshal(l)
		sb.Write(b)
		sb.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	msgs, err := LoadSessionMessages(path)
	if err != nil {
		t.Fatal(err)
	}

	// First reconstructed message must be the compaction summary (pi wrapper).
	if len(msgs) == 0 {
		t.Fatal("no messages reconstructed")
	}
	first, ok := msgs[0].(ai.UserMessage)
	if !ok {
		t.Fatalf("first message should be the summary user message, got %T", msgs[0])
	}
	if !strings.Contains(textOf(first.Content), "SUMMARY OF OLD WORK") {
		t.Fatalf("compaction summary missing from resumed context: %q", textOf(first.Content))
	}

	all := ""
	for _, m := range msgs {
		switch v := m.(type) {
		case ai.UserMessage:
			all += textOf(v.Content) + "\n"
		case *ai.AssistantMessage:
			for _, c := range v.Content {
				if tc, ok := c.(ai.TextContent); ok {
					all += tc.Text + "\n"
				}
			}
		case ai.AssistantMessage:
			for _, c := range v.Content {
				if tc, ok := c.(ai.TextContent); ok {
					all += tc.Text + "\n"
				}
			}
		}
	}
	// Pre-compaction turns must NOT be replayed.
	if strings.Contains(all, "old question") || strings.Contains(all, "old answer") {
		t.Fatalf("pre-compaction turns leaked into resumed context:\n%s", all)
	}
	// Recent turns must be present.
	if !strings.Contains(all, "recent question") || !strings.Contains(all, "recent answer") {
		t.Fatalf("recent turns missing from resumed context:\n%s", all)
	}
}

var _ = agent.ThinkMedium
