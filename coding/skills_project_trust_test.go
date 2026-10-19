package coding

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// pi discovers <cwd>/.pi/skills only inside `if (projectTrusted)`
// (package-manager.ts:2417 at ccfe79ed2), and resolveProjectTrusted answers
// false when the host has no UI to prompt with (project-trust.ts:
// `if (!options.projectTrustContext.hasUI) return false`). This port is
// headless, so untrusted is the faithful default. These tests pin BOTH arms:
// the gate closes on the project dir, and it never touches the user dirs.

func TestLoadSkillsUntrustedSkipsProjectDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := t.TempDir()

	writeSkill(t, filepath.Join(home, ".pi", "agent", "skills", "user-skill"),
		"---\nname: user-skill\ndescription: from the user directory\n---\n")
	writeSkill(t, filepath.Join(cwd, ".pi", "skills", "repo-skill"),
		"---\nname: repo-skill\ndescription: authored by whoever owns this repo\n---\n")

	untrusted, _ := LoadSkillsWithTrust(cwd, false)
	if len(untrusted) != 1 || untrusted[0].Name != "user-skill" {
		t.Fatalf("untrusted load must see the user skill and only the user skill, got %+v", untrusted)
	}

	// LoadSkills and LoadSkillsWithDiagnostics are the same default.
	if got := LoadSkills(cwd); len(got) != 1 || got[0].Name != "user-skill" {
		t.Fatalf("LoadSkills must default to untrusted, got %+v", got)
	}
	if got, _ := LoadSkillsWithDiagnostics(cwd); len(got) != 1 || got[0].Name != "user-skill" {
		t.Fatalf("LoadSkillsWithDiagnostics must default to untrusted, got %+v", got)
	}

	trusted, _ := LoadSkillsWithTrust(cwd, true)
	if len(trusted) != 2 {
		t.Fatalf("trusted load must add the project skill, got %+v", trusted)
	}
	// Order is pi's: user dir first, then project.
	if trusted[0].Name != "user-skill" || trusted[1].Name != "repo-skill" {
		t.Fatalf("trusted load must keep user-then-project order, got %s, %s", trusted[0].Name, trusted[1].Name)
	}
}

// With no HOME, AgentDir() and AgentsSkillsDir() fall back to RELATIVE paths
// (".pi/agent", ".agents/skills") which resolve against the process cwd — the
// untrusted repository. That walked straight around the gate: the repo's own
// .pi/agent/skills and .agents/skills were read as though they were the user's.
// pi cannot reach this state (getHomeDir is `process.env.HOME || homedir()`,
// and Node's homedir() consults the passwd database rather than returning a
// relative path), so failing closed is both the safe and the faithful answer.
func TestLoadSkillsWithoutHomeIgnoresRepoLocalUserDirs(t *testing.T) {
	cwd := t.TempDir()
	t.Setenv("HOME", "")

	const marker = "ZZ-REPO-PLANTED-AS-USER-SKILL"
	writeSkill(t, filepath.Join(cwd, ".pi", "agent", "skills", "planted-agent"),
		"---\nname: planted-agent\ndescription: "+marker+"\n---\n")
	writeSkill(t, filepath.Join(cwd, ".agents", "skills", "planted-agents"),
		"---\nname: planted-agents\ndescription: "+marker+"\n---\n")
	writeSkill(t, filepath.Join(cwd, ".pi", "skills", "planted-project"),
		"---\nname: planted-project\ndescription: "+marker+"\n---\n")

	// The relative-path fallback only bites when the process cwd IS the repo.
	restore, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(cwd); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(restore) })

	if got, _ := LoadSkillsWithTrust(cwd, false); len(got) != 0 {
		t.Fatalf("HOME-less untrusted load must find no skills at all, got %+v", got)
	}
	// The same root cause reached the GLOBAL context file: with a relative
	// AgentDir(), <cwd>/.pi/agent/AGENTS.md was read as the user's own.
	if err := os.MkdirAll(filepath.Join(cwd, ".pi", "agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cwd, ".pi", "agent", "AGENTS.md"), []byte(marker), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, cf := range LoadProjectContextFiles(cwd) {
		if strings.Contains(cf.Content, marker) {
			t.Fatalf("repo-local .pi/agent/AGENTS.md was loaded as the global context file (%s)", cf.Path)
		}
	}
	if AgentDir() != "" || AgentsSkillsDir() != "" {
		t.Fatalf("with no home both dirs must be empty, got %q and %q", AgentDir(), AgentsSkillsDir())
	}
	if got := DefaultSessionDir(cwd); got != "" {
		t.Fatalf("with no home the session dir must be empty, got %q", got)
	}
	// Trust opens the project dir and nothing else: the two repo-local
	// directories that merely LOOK like user dirs stay shut.
	trusted, _ := LoadSkillsWithTrust(cwd, true)
	if len(trusted) != 1 || trusted[0].Name != "planted-project" {
		t.Fatalf("HOME-less trusted load must see only the project dir, got %+v", trusted)
	}
}

// The gate's only production consumer is NewSession. Exercising the loader
// directly would leave the wiring — SessionOptions.TrustProject reaching
// sessionSkills — unlocked, and the ZERO VALUE is the security-relevant case.
func TestNewSessionDefaultsToUntrustedProject(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cwd := t.TempDir()

	const marker = "ZZ-UNTRUSTED-VIA-NEWSESSION"
	writeSkill(t, filepath.Join(cwd, ".pi", "skills", "repo-skill"),
		"---\nname: repo-skill\ndescription: "+marker+"\n---\n")

	// Zero value for TrustProject — the default every embedder gets.
	untrusted := newTestSession(t, SessionOptions{Cwd: cwd})
	if strings.Contains(sessionSystemPrompt(t, untrusted), marker) {
		t.Fatalf("NewSession must default to an untrusted project")
	}

	trusted := newTestSession(t, SessionOptions{Cwd: cwd, TrustProject: true})
	if !strings.Contains(sessionSystemPrompt(t, trusted), marker) {
		t.Fatalf("NewSession with TrustProject must load the project skill")
	}
}

// The exposure this gate closes is the SYSTEM PROMPT: a skill's name and
// description are rendered into it by FormatSkillsForPrompt together with an
// instruction to read the file when the task matches. An untrusted repo must
// not be able to put its own text there.
func TestSystemPromptOmitsUntrustedProjectSkill(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := t.TempDir()

	const marker = "ZZ-ATTACKER-CONTROLLED-DESCRIPTION"
	writeSkill(t, filepath.Join(cwd, ".pi", "skills", "repo-skill"),
		"---\nname: repo-skill\ndescription: "+marker+"\n---\n")

	build := func(trust bool) string {
		return mustBuildSystemPrompt(t, BuildSystemPromptOptions{
			SelectedTools: defaultActiveToolNames,
			ToolSnippets:  ToolSnippets,
			Cwd:           cwd,
			Skills:        sessionSkills(cwd, trust),
		})
	}

	if got := build(false); strings.Contains(got, marker) {
		t.Fatalf("untrusted project skill reached the system prompt")
	}
	if got := build(true); !strings.Contains(got, marker) {
		t.Fatalf("trusted project skill must still reach the system prompt")
	}
}
