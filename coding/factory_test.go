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

// TestSessionNoToolsAll: pi's noTools "all" sets an EMPTY allowlist
// (sdk.ts:245), and custom tools also pass through isAllowedTool
// (agent-session.ts:2285-2298) — so "all" disables custom tools too.
// (This test previously pinned the old always-append bug, expecting the
// custom tool to survive noTools=all.)
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
	if got := toolNamesOf(s); len(got) != 0 {
		t.Fatalf("noTools=all must disable custom tools too, got: %v", got)
	}
}

// TestSessionNoToolsBuiltinKeepsCustom: "builtin" empties the initial active
// set but leaves the allowlist nil, so custom tools stay enabled.
func TestSessionNoToolsBuiltinKeepsCustom(t *testing.T) {
	reg := providers.RegisterFauxProvider(providers.RegisterFauxProviderOptions{})
	defer reg.Unregister()
	custom := agent.AgentTool{Name: "custom", Parameters: ai.Object()}
	s := NewSession(SessionOptions{
		Model:       reg.GetModel(),
		Cwd:         t.TempDir(),
		NoTools:     NoToolsBuiltin,
		CustomTools: []agent.AgentTool{custom},
	})
	got := toolNamesOf(s)
	if len(got) != 1 || got[0] != "custom" {
		t.Fatalf("noTools=builtin should keep custom tools: %v", got)
	}
}

// TestSessionAllowlistConstrainsCustom: a ToolNames allowlist applies to
// custom tools too — a custom tool not in the allowlist is disabled, one in
// the allowlist is enabled.
func TestSessionAllowlistConstrainsCustom(t *testing.T) {
	reg := providers.RegisterFauxProvider(providers.RegisterFauxProviderOptions{})
	defer reg.Unregister()
	inList := agent.AgentTool{Name: "allowed_custom", Parameters: ai.Object()}
	outOfList := agent.AgentTool{Name: "blocked_custom", Parameters: ai.Object()}
	s := NewSession(SessionOptions{
		Model:       reg.GetModel(),
		Cwd:         t.TempDir(),
		ToolNames:   []string{"read", "allowed_custom"},
		CustomTools: []agent.AgentTool{inList, outOfList},
	})
	got := toolNamesOf(s)
	if len(got) != 2 || got[0] != "read" || got[1] != "allowed_custom" {
		t.Fatalf("allowlist should constrain custom tools: %v", got)
	}
}

// TestSessionExcludeAppliesToCustom: ExcludeTools applies to custom tools.
func TestSessionExcludeAppliesToCustom(t *testing.T) {
	reg := providers.RegisterFauxProvider(providers.RegisterFauxProviderOptions{})
	defer reg.Unregister()
	custom := agent.AgentTool{Name: "custom", Parameters: ai.Object()}
	keep := agent.AgentTool{Name: "keep", Parameters: ai.Object()}
	s := NewSession(SessionOptions{
		Model:        reg.GetModel(),
		Cwd:          t.TempDir(),
		ExcludeTools: []string{"custom", "bash"},
		CustomTools:  []agent.AgentTool{custom, keep},
	})
	got := toolNamesOf(s)
	want := []string{"read", "edit", "write", "keep"}
	if len(got) != len(want) {
		t.Fatalf("exclude should drop custom + builtin: got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("exclude should drop custom + builtin: got %v, want %v", got, want)
		}
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

// TestSessionAllowlistOptInWebFetch: web_fetch (a Go-port extra beyond pi's
// core set) stays opt-in via the ToolNames allowlist.
func TestSessionAllowlistOptInWebFetch(t *testing.T) {
	reg := providers.RegisterFauxProvider(providers.RegisterFauxProviderOptions{})
	defer reg.Unregister()
	s := NewSession(SessionOptions{
		Model:     reg.GetModel(),
		Cwd:       t.TempDir(),
		ToolNames: []string{"read", "web_fetch"},
	})
	got := toolNamesOf(s)
	if len(got) != 2 || got[0] != "read" || got[1] != "web_fetch" {
		t.Fatalf("web_fetch opt-in via ToolNames broken: %v", got)
	}
}
