package coding

import (
	"context"
	"strings"
	"testing"

	"github.com/sky-valley/pi/agent"
	"github.com/sky-valley/pi/ai"
	"github.com/sky-valley/pi/ai/providers"
)

// A new transcript starts with no compaction. pi's /new starts a fresh
// SessionManager (agent-session-runtime.ts newSession), whose branch holds no
// compaction entry, so the next request is the new prompt alone. The port's
// checkpoint indexes the old transcript: applied to the new one it replaced the
// new prompt with the old conversation's summary. An empty summary is a
// checkpoint too, so it must be dropped the same way.
func TestNewTranscriptDropsTheCompaction(t *testing.T) {
	for _, tc := range []struct {
		name  string
		reply string
		load  bool // replace the transcript with LoadHistory instead of Reset
	}{
		{"Reset", "## Goal\nold conversation", false},
		{"Reset after an empty summary", "", false},
		{"LoadHistory", "## Goal\nold conversation", true},
		{"LoadHistory after an empty summary", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg := providers.RegisterFauxProvider(providers.RegisterFauxProviderOptions{
				Models: []providers.FauxModelDefinition{{ID: "faux-1", ContextWindow: 1000}},
			})
			t.Cleanup(reg.Unregister)
			sess := newTestSession(t, SessionOptions{Model: reg.GetModel(), Cwd: t.TempDir(), NoTools: NoToolsAll,
				Compaction: &CompactionSettings{Enabled: true, ReserveTokens: 200, KeepRecentTokens: 150}})

			summarizations := 0
			var last []string
			step := func(req ai.TranscriptContext, _ *ai.SimpleStreamOptions, _ *providers.FauxState, _ *ai.Model) *ai.AssistantMessage {
				if lead, ok := ai.GetInitialSystemMessage(req.Messages); ok && ai.GetSystemMessageText(lead) == summarizationSystemPrompt {
					summarizations++
					return providers.FauxAssistantMessage(ai.ContentList{ai.TextContent{Text: tc.reply}}, ai.StopStop)
				}
				last = nil
				for _, m := range req.Messages {
					if m.MessageRole() == ai.RoleSystem {
						last = append(last, "system")
						continue
					}
					var content ai.ContentList
					switch m := m.(type) {
					case ai.UserMessage:
						content = m.Content
					case ai.AssistantMessage:
						content = m.Content
					}
					last = append(last, string(m.MessageRole())+":"+textOf(content))
				}
				return providers.FauxAssistantMessage(ai.ContentList{ai.TextContent{Text: strings.Repeat("m", 1000)}}, ai.StopStop)
			}
			steps := make([]providers.FauxResponseStep, 16)
			for i := range steps {
				steps[i] = step
			}
			reg.SetResponses(steps)

			// Four 250-token turns: the fourth request crosses 800 tokens.
			for i := range 4 {
				if _, err := sess.Run(context.Background(), strings.Repeat("m", 1000)+string(rune('a'+i))); err != nil {
					t.Fatal(err)
				}
			}
			if summarizations == 0 {
				t.Fatal("setup: no compaction happened")
			}

			want := []string{"system", "user:hello after /new"}
			if tc.load {
				sess.LoadHistory([]agent.AgentMessage{
					ai.NewUserText("prior question", 1),
					ai.AssistantMessage{Content: ai.ContentList{ai.TextContent{Text: "prior answer"}}, StopReason: ai.StopStop, Timestamp: 2},
				})
				// The loaded transcript declares no prompt, so the new prompt
				// declares it (TestSessionDeclaresPromptForTranscriptWithoutSystemMessage).
				want = []string{"user:prior question", "assistant:prior answer", "system", "user:hello after /new"}
			} else if err := sess.Reset(); err != nil {
				t.Fatal(err)
			}
			if _, err := sess.Run(context.Background(), "hello after /new"); err != nil {
				t.Fatal(err)
			}
			if strings.Join(last, "|") != strings.Join(want, "|") {
				t.Fatalf("request after the new transcript = %q, want %q (no checkpoint from the old one)", abbreviated(last), want)
			}
		})
	}
}

func abbreviated(texts []string) []string {
	out := make([]string, len(texts))
	for i, s := range texts {
		if len(s) > 60 {
			s = s[:60] + "..."
		}
		out[i] = s
	}
	return out
}
