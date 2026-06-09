package coding

import (
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
