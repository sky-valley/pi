package coding

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sessionSystemPrompt returns the session's effective system prompt, the one
// its first prompt declares (TestSessionSystemPromptBeforeTheFirstRun).
func sessionSystemPrompt(t *testing.T, s *Session) string {
	t.Helper()
	prompt, err := s.SystemPrompt()
	if err != nil {
		t.Fatal(err)
	}
	return prompt
}

// TestNewSessionCollectsPromptGuidelines locks NewSession → prompt wiring for
// I1: the resolved tools' PromptGuidelines must appear in the rules section of
// the session's system prompt, in tool order.
func TestNewSessionCollectsPromptGuidelines(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s := newTestSession(t, SessionOptions{Cwd: t.TempDir()})
	prompt := sessionSystemPrompt(t, s)

	wantOrder := []string{
		"- Use bash for file operations like ls, rg, find",
		"- Use read to examine files instead of cat or sed.",
		"- Use edit for precise changes (edits[].oldText must match exactly)",
		"- Use write only for new files or complete rewrites.",
		"- Be concise in your responses",
	}
	last := -1
	for _, g := range wantOrder {
		idx := strings.Index(prompt, g)
		if idx == -1 {
			t.Fatalf("guideline missing from system prompt: %q\n%s", g, prompt)
		}
		if idx < last {
			t.Fatalf("guideline out of order: %q\n%s", g, prompt)
		}
		last = idx
	}
}

// A session whose only file-reading tool is bash still gets its skills, with
// the block telling the model to load them with bash (upstream 1d6dbf9e3).
// This is the wiring half: NewSession → resolveTools → BuildSystemPrompt.
func TestNewSessionBashOnlyKeepsSkills(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cwd := t.TempDir()
	skillDir := filepath.Join(cwd, ".pi", "skills", "demo-skill")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("---\nname: demo-skill\ndescription: Demo skill for tests\n---\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := newTestSession(t, SessionOptions{
		Cwd:          cwd,
		ToolNames:    []string{"bash"},
		TrustProject: true,
	})
	prompt := sessionSystemPrompt(t, s)

	if !strings.Contains(prompt, "<name>demo-skill</name>") {
		t.Fatalf("bash-only session dropped its skills:\n%s", prompt)
	}
	if !strings.Contains(prompt, "Use bash to load a skill's file when the task matches its description.") {
		t.Fatalf("bash-only session should name bash as the skill file reader:\n%s", prompt)
	}
	if strings.Contains(prompt, "Use the read tool to load a skill's file") {
		t.Fatalf("bash-only session must not name the absent read tool:\n%s", prompt)
	}
}

// TestNewSessionCustomPromptStillAssembles locks I2: a custom SystemPrompt
// still gets the project context, skills and cwd sections after it, in pi's
// order — and never the default prompt's tools, rules or docs sections.
func TestNewSessionCustomPromptStillAssembles(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, "AGENTS.md"), []byte("follow the rules"), 0o644); err != nil {
		t.Fatal(err)
	}
	skillDir := filepath.Join(cwd, ".pi", "skills", "demo-skill")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("---\nname: demo-skill\ndescription: Demo skill for tests\n---\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := newTestSession(t, SessionOptions{
		Cwd:          cwd,
		SystemPrompt: "You are a custom agent.",
		// The fixture skill lives in <cwd>/.pi/skills, which is gated on
		// project trust. This test is about assembly ORDER, not the gate —
		// see TestSystemPromptOmitsUntrustedProjectSkill for that.
		TrustProject: true,
	})
	prompt := sessionSystemPrompt(t, s)

	if !strings.HasPrefix(prompt, "You are a custom agent.") {
		t.Fatalf("custom prompt should lead the system prompt:\n%s", prompt)
	}
	// pi's order: custom prompt, project context, skills, cwd.
	ordered := []string{
		"You are a custom agent.",
		"<project_context>",
		"<project_instructions path=\"" + filepath.Join(cwd, "AGENTS.md") + "\">\nfollow the rules\n</project_instructions>",
		"<skills>",
		"<available_skills>",
		"<name>demo-skill</name>",
		"<cwd>\n",
	}
	last := -1
	for _, sub := range ordered {
		idx := strings.Index(prompt, sub)
		if idx == -1 {
			t.Fatalf("custom prompt assembly missing %q:\n%s", sub, prompt)
		}
		if idx < last {
			t.Fatalf("custom prompt assembly out of order at %q:\n%s", sub, prompt)
		}
		last = idx
	}
	// Custom prompts get neither the docs, tools nor rules sections.
	for _, banned := range []string{"Pi documentation", "Guidelines:", "<rules>", "<tools>", "<docs>", "expert coding assistant"} {
		if strings.Contains(prompt, banned) {
			t.Fatalf("custom prompt must not include %q:\n%s", banned, prompt)
		}
	}
}
