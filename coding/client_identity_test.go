package coding

import (
	"context"
	"maps"
	"testing"

	"github.com/sky-valley/pi/agent"
	"github.com/sky-valley/pi/ai"
	"github.com/sky-valley/pi/ai/providers"
)

// upstream sdk-client-identity.test.ts (adc5fa9f8, 04a93bad8, 484da3a86): a
// session's requests identify pi with a detailed User-Agent and attribution
// as a CLI agent, replacing the agent loop's default. Attribution is always
// sent: pi removed install telemetry and the PI_TELEMETRY switch with it, so
// the variable set to 0 changes nothing. Summaries take the same identity,
// since they go through the session's stream function.
func TestSessionClientIdentity(t *testing.T) {
	t.Setenv("PI_TELEMETRY", "0") // no longer read; attribution stays on
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
		if got == nil || got.UserAgent != ai.UserAgentDetailed || got.Version != ai.PiAIVersion || got.Attribution != &ai.PiAttribution {
			t.Errorf("request %d identity = %+v, want detailed, %s, pi attribution", i, got, ai.PiAIVersion)
		}
	}

	// "always sends provider attribution headers"
	for _, tc := range []struct {
		target ai.ClientIdentityTarget
		want   map[string]string
	}{
		{ai.ClientIdentityTarget{Provider: "nvidia", BaseURL: "https://integrate.api.nvidia.com/v1"}, map[string]string{"X-BILLING-INVOKE-ORIGIN": "Pi"}},
		{ai.ClientIdentityTarget{Provider: "opencode", BaseURL: "https://opencode.ai/zen/v1"}, map[string]string{"x-opencode-client": "pi"}},
	} {
		if got := ai.GetAttributionHeaders(tc.target, identities[0]); !maps.Equal(got, tc.want) {
			t.Errorf("%s attribution headers = %v, want %v", tc.target.Provider, got, tc.want)
		}
	}
}
