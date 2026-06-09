package coding

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sky-valley/pi/agent"
	"github.com/sky-valley/pi/ai"
	"github.com/sky-valley/pi/ai/providers"
)

func TestSessionPersistenceRoundTrip(t *testing.T) {
	// Redirect HOME so the session dir is isolated.
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := t.TempDir()

	reg := providers.RegisterFauxProvider(providers.RegisterFauxProviderOptions{})
	defer reg.Unregister()
	reg.SetResponses([]providers.FauxResponseStep{
		providers.FauxStatic(providers.FauxAssistantMessage(ai.ContentList{ai.TextContent{Text: "remembered"}}, ai.StopStop)),
	})
	model := reg.GetModel()

	sess := NewSession(SessionOptions{Model: model, Cwd: cwd, Tools: CreateAllTools(cwd)})
	rec, err := StartSession(cwd, model)
	if err != nil {
		t.Fatal(err)
	}
	sess.Record(rec)

	if _, err := sess.RunPrint(context.Background(), &strings.Builder{}, "remember this"); err != nil {
		t.Fatal(err)
	}
	rec.Close()

	// The session file must exist under the encoded dir.
	if !strings.HasPrefix(rec.Path(), filepath.Join(AgentDir(), "sessions")) {
		t.Fatalf("session not stored under agent dir: %s", rec.Path())
	}
	data, _ := os.ReadFile(rec.Path())
	if !strings.Contains(string(data), `"type":"session"`) || !strings.Contains(string(data), `"type":"message"`) {
		t.Fatalf("session file malformed: %s", data)
	}

	// List + reload.
	infos := ListSessions(cwd)
	if len(infos) != 1 || infos[0].Messages < 2 {
		t.Fatalf("unexpected session list: %+v", infos)
	}
	messages, err := LoadSessionMessages(rec.Path())
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) < 2 {
		t.Fatalf("expected >=2 messages reloaded, got %d", len(messages))
	}
	// First is the user prompt, last is the assistant reply.
	if messages[0].MessageRole() != ai.RoleUser {
		t.Fatalf("first message should be user, got %s", messages[0].MessageRole())
	}
}

func TestResumeContinuesConversation(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cwd := t.TempDir()
	reg := providers.RegisterFauxProvider(providers.RegisterFauxProviderOptions{})
	defer reg.Unregister()
	model := reg.GetModel()

	// First session writes one exchange.
	reg.SetResponses([]providers.FauxResponseStep{
		providers.FauxStatic(providers.FauxAssistantMessage(ai.ContentList{ai.TextContent{Text: "first reply"}}, ai.StopStop)),
	})
	s1 := NewSession(SessionOptions{Model: model, Cwd: cwd, Tools: nil})
	rec1, _ := StartSession(cwd, model)
	s1.Record(rec1)
	s1.RunPrint(context.Background(), &strings.Builder{}, "first question")
	rec1.Close()

	// Second session resumes and the model sees prior history.
	var capturedCount int
	reg.SetResponses([]providers.FauxResponseStep{
		func(req ai.Context, opts *ai.SimpleStreamOptions, st *providers.FauxState, m *ai.Model) *ai.AssistantMessage {
			capturedCount = len(req.Messages)
			return providers.FauxAssistantMessage(ai.ContentList{ai.TextContent{Text: "second reply"}}, ai.StopStop)
		},
	})
	latest, ok := LatestSession(cwd)
	if !ok {
		t.Fatal("no latest session found")
	}
	history, _ := LoadSessionMessages(latest.Path)
	s2 := NewSession(SessionOptions{Model: model, Cwd: cwd, Tools: nil})
	s2.LoadHistory(history)
	s2.RunPrint(context.Background(), &strings.Builder{}, "second question")

	// The provider should have seen prior user+assistant + new user = >=3 messages.
	if capturedCount < 3 {
		t.Fatalf("resume did not carry history; provider saw %d messages", capturedCount)
	}
}

var _ = agent.ThinkOff
