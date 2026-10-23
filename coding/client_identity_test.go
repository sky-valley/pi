package coding

import (
	"context"
	"os"
	"testing"

	"github.com/sky-valley/pi/agent"
	"github.com/sky-valley/pi/ai"
	"github.com/sky-valley/pi/ai/providers"
)

// upstream sdk-client-identity.test.ts (adc5fa9f8, 04a93bad8): a session's
// requests identify pi with a detailed User-Agent and attribution as a CLI
// agent, replacing the agent loop's default, and turn attribution off when
// install telemetry is off. Summaries take the same identity, since they go
// through the session's stream function.
func TestSessionClientIdentity(t *testing.T) {
	for _, tc := range []struct {
		name      string
		telemetry *string
		want      *ai.ClientAttribution
	}{
		{"telemetry on by default", nil, &ai.PiAttribution},
		{"PI_TELEMETRY=1", strPtrCoding("1"), &ai.PiAttribution},
		{"PI_TELEMETRY=true", strPtrCoding("TRUE"), &ai.PiAttribution},
		{"PI_TELEMETRY=0", strPtrCoding("0"), ai.NoAttribution},
		{"PI_TELEMETRY empty", strPtrCoding(""), ai.NoAttribution},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.telemetry != nil {
				t.Setenv("PI_TELEMETRY", *tc.telemetry)
			} else {
				t.Setenv("PI_TELEMETRY", "") // restores the variable afterwards
				os.Unsetenv("PI_TELEMETRY")
			}
			reg := providers.RegisterFauxProvider(providers.RegisterFauxProviderOptions{
				Models: []providers.FauxModelDefinition{{ID: "faux-1", ContextWindow: 200000}},
			})
			defer reg.Unregister()
			var identities []*ai.ClientIdentity
			capture := func(req ai.TranscriptContext, opts *ai.SimpleStreamOptions, st *providers.FauxState, m *ai.Model) *ai.AssistantMessage {
				identities = append(identities, opts.Identity)
				return providers.FauxAssistantMessage(ai.ContentList{ai.TextContent{Text: "ok"}}, ai.StopStop)
			}
			reg.SetResponses([]providers.FauxResponseStep{capture, capture})
			sess := newTestSession(t, SessionOptions{Model: reg.GetModel(), Cwd: t.TempDir(), NoTools: NoToolsAll})
			if _, err := sess.Run(context.Background(), "hello"); err != nil {
				t.Fatal(err)
			}
			sess.summarize(context.Background(), []agent.AgentMessage{ai.NewUserText("hi", 1)})
			if len(identities) != 2 {
				t.Fatalf("captured %d requests, want a turn and a summary", len(identities))
			}
			for i, got := range identities {
				if got == nil || got.UserAgent != ai.UserAgentDetailed || got.Version != ai.PiAIVersion || got.Attribution != tc.want {
					t.Errorf("request %d identity = %+v, want detailed, %s, attribution %p", i, got, ai.PiAIVersion, tc.want)
				}
			}
		})
	}
}

func strPtrCoding(s string) *string { return &s }
