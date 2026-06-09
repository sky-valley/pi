package coding

import (
	"testing"

	"github.com/sky-valley/pi/agent"
	"github.com/sky-valley/pi/ai"
	"github.com/sky-valley/pi/ai/providers"
)

func toolNamesOf(s *Session) []string {
	var out []string
	for _, t := range s.Agent.State().Tools {
		out = append(out, t.Name)
	}
	return out
}

func TestSessionDefaultToolSet(t *testing.T) {
	reg := providers.RegisterFauxProvider(providers.RegisterFauxProviderOptions{})
	defer reg.Unregister()
	s := NewSession(SessionOptions{Model: reg.GetModel(), Cwd: t.TempDir()})
	got := toolNamesOf(s)
	want := []string{"read", "bash", "edit", "write"}
	if len(got) != len(want) {
		t.Fatalf("default tools = %v, want %v", got, want)
	}
}

func TestSessionToolAllowlistAndExclude(t *testing.T) {
	reg := providers.RegisterFauxProvider(providers.RegisterFauxProviderOptions{})
	defer reg.Unregister()
	s := NewSession(SessionOptions{
		Model:        reg.GetModel(),
		Cwd:          t.TempDir(),
		ToolNames:    []string{"read", "grep", "bash"},
		ExcludeTools: []string{"bash"},
	})
	got := toolNamesOf(s)
	if len(got) != 2 || got[0] != "read" || got[1] != "grep" {
		t.Fatalf("allowlist/exclude wrong: %v", got)
	}
}

func TestSessionNoToolsAll(t *testing.T) {
	reg := providers.RegisterFauxProvider(providers.RegisterFauxProviderOptions{})
	defer reg.Unregister()
	custom := agent.AgentTool{Name: "custom", Parameters: ai.Object()}
	s := NewSession(SessionOptions{
		Model:       reg.GetModel(),
		Cwd:         t.TempDir(),
		NoTools:     NoToolsAll,
		CustomTools: []agent.AgentTool{custom},
	})
	got := toolNamesOf(s)
	if len(got) != 1 || got[0] != "custom" {
		t.Fatalf("noTools=all + custom wrong: %v", got)
	}
}

func TestSessionClampsThinkingLevel(t *testing.T) {
	// A non-reasoning model clamps any requested level to "off".
	reg := providers.RegisterFauxProvider(providers.RegisterFauxProviderOptions{
		Models: []providers.FauxModelDefinition{{ID: "faux-1", Reasoning: false}},
	})
	defer reg.Unregister()
	s := NewSession(SessionOptions{Model: reg.GetModel(), Cwd: t.TempDir(), ThinkingLevel: agent.ThinkHigh})
	if lvl := s.Agent.State().ThinkingLevel; lvl != agent.ThinkOff {
		t.Fatalf("expected thinking clamped to off for non-reasoning model, got %s", lvl)
	}
}
