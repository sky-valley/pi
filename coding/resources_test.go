package coding

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestLoadProjectContextFilesAncestorOrder(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "a", "b")
	os.MkdirAll(sub, 0o755)
	os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("root rules"), 0o644)
	os.WriteFile(filepath.Join(sub, "CLAUDE.md"), []byte("leaf rules"), 0o644)

	files := LoadProjectContextFiles(sub)
	// Ancestors are ordered root -> cwd.
	var contents []string
	for _, f := range files {
		contents = append(contents, f.Content)
	}
	joined := strings.Join(contents, "|")
	if !strings.Contains(joined, "root rules") || !strings.Contains(joined, "leaf rules") {
		t.Fatalf("missing context files: %q", joined)
	}
	if strings.Index(joined, "root rules") > strings.Index(joined, "leaf rules") {
		t.Fatalf("expected root before leaf: %q", joined)
	}
}

// Upstream 8ecf8a988: AGENTS.override.md wins within each directory — global
// agent dir included — while directories without one keep layering as before.
func TestContextFileOverridePreferredPerDirectory(t *testing.T) {
	home := isolatedHome(t)
	agentDir := filepath.Join(home, ConfigDirName, "agent")
	cwd := t.TempDir()
	nested := filepath.Join(cwd, "service")
	writeFile(t, filepath.Join(agentDir, "AGENTS.md"), "global instructions")
	writeFile(t, filepath.Join(agentDir, "AGENTS.override.md"), "global override")
	writeFile(t, filepath.Join(cwd, "AGENTS.md"), "project instructions")
	writeFile(t, filepath.Join(nested, "AGENTS.md"), "service instructions")
	writeFile(t, filepath.Join(nested, "AGENTS.override.md"), "service override")

	got := LoadProjectContextFiles(nested)
	want := []ContextFile{
		{Path: filepath.Join(agentDir, "AGENTS.override.md"), Content: "global override"},
		{Path: filepath.Join(cwd, "AGENTS.md"), Content: "project instructions"},
		{Path: filepath.Join(nested, "AGENTS.override.md"), Content: "service override"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestContextFileOverrideBeatsClaude(t *testing.T) {
	isolatedHome(t)
	cwd := t.TempDir()
	writeFile(t, filepath.Join(cwd, "CLAUDE.md"), "claude instructions")
	writeFile(t, filepath.Join(cwd, "AGENTS.override.md"), "override instructions")

	assertContents(t, contextContents(LoadProjectContextFiles(cwd)), "override instructions")
}

// A candidate that is a directory is skipped in favour of the next loadable
// one, AGENTS.override.md included.
func TestContextFileCandidateDirectoriesIgnored(t *testing.T) {
	isolatedHome(t)
	cwd := t.TempDir()
	for _, name := range []string{"AGENTS.override.md", "AGENTS.md"} {
		if err := os.MkdirAll(filepath.Join(cwd, name), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", name, err)
		}
	}
	writeFile(t, filepath.Join(cwd, "CLAUDE.md"), "fallback instructions")

	assertContents(t, contextContents(LoadProjectContextFiles(cwd)), "fallback instructions")
}

func TestLoadSkillsAndFormat(t *testing.T) {
	cwd := t.TempDir()
	skillDir := filepath.Join(cwd, ".pi", "skills", "my-skill")
	os.MkdirAll(skillDir, 0o755)
	os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(`---
name: my-skill
description: Does a specialized thing for tests
---
# body
`), 0o644)
	// A hidden skill should be excluded from the prompt.
	hiddenDir := filepath.Join(cwd, ".pi", "skills", "hidden")
	os.MkdirAll(hiddenDir, 0o755)
	os.WriteFile(filepath.Join(hiddenDir, "SKILL.md"), []byte(`---
name: hidden
description: Should not appear
disable-model-invocation: true
---
`), 0o644)

	skills, _ := LoadSkillsWithTrust(cwd, true)
	if len(skills) != 2 {
		t.Fatalf("expected 2 skills loaded, got %d", len(skills))
	}
	prompt := FormatSkillsForPrompt(skills)
	if !strings.Contains(prompt, "<name>my-skill</name>") {
		t.Fatalf("skill missing from prompt: %q", prompt)
	}
	if strings.Contains(prompt, "hidden") {
		t.Fatalf("disabled skill should be excluded: %q", prompt)
	}
	if !strings.Contains(prompt, "<available_skills>") {
		t.Fatalf("missing skills block: %q", prompt)
	}
}

func TestSystemPromptIncludesContextAndSkills(t *testing.T) {
	p := mustBuildSystemPrompt(t, BuildSystemPromptOptions{
		SelectedTools: []string{"read", "bash"},
		ToolSnippets:  ToolSnippets,
		Cwd:           "/proj",
		ContextFiles:  []ContextFile{{Path: "/proj/AGENTS.md", Content: "follow the rules"}},
		Skills:        []Skill{{Name: "demo", Description: "d", FilePath: "/proj/.pi/skills/demo/SKILL.md"}},
	})
	if !strings.Contains(p, "<project_instructions path=\"/proj/AGENTS.md\">") {
		t.Fatalf("missing project context: %q", p)
	}
	if !strings.Contains(p, "<name>demo</name>") {
		t.Fatalf("missing skills block: %q", p)
	}
}

// The exported skills formatter is SDK surface (pi index.ts exports
// formatSkillsForPrompt), so both arms of its fileReadTool argument are pinned
// here byte-for-byte, and the one-argument form must stay on pi's default.
func TestFormatSkillsForPromptWithToolBothArms(t *testing.T) {
	skills := []Skill{
		{Name: "demo", Description: "d", FilePath: "/proj/.pi/skills/demo/SKILL.md"},
		{Name: "hidden", Description: "no", FilePath: "x", DisableModelInvocation: true},
	}
	block := func(instruction string) string {
		return "\n\nThe following skills provide specialized instructions for specific tasks.\n" +
			instruction + "\n" +
			"Resolve relative paths against the skill's directory (parent of SKILL.md). Use absolute paths in tool calls.\n" +
			"\n<available_skills>\n  <skill>\n    <name>demo</name>\n    <description>d</description>\n" +
			"    <location>/proj/.pi/skills/demo/SKILL.md</location>\n  </skill>\n</available_skills>"
	}
	wantRead := block("Use the read tool to load a skill's file when the task matches its description.")
	wantBash := block("Use bash to load a skill's file when the task matches its description.")

	if got := FormatSkillsForPromptWithTool(skills, SkillFileReadToolRead); got != wantRead {
		t.Fatalf("read arm:\n got %q\nwant %q", got, wantRead)
	}
	if got := FormatSkillsForPromptWithTool(skills, SkillFileReadToolBash); got != wantBash {
		t.Fatalf("bash arm:\n got %q\nwant %q", got, wantBash)
	}
	// pi's default argument: formatSkillsForPrompt(skills) is the read arm, and
	// so is the Go zero value of the tool type.
	if got := FormatSkillsForPrompt(skills); got != wantRead {
		t.Fatalf("default arm:\n got %q\nwant %q", got, wantRead)
	}
	if got := FormatSkillsForPromptWithTool(skills, SkillFileReadTool("")); got != wantRead {
		t.Fatalf("zero-value arm:\n got %q\nwant %q", got, wantRead)
	}
	// No visible skills stays empty on either arm.
	hiddenOnly := []Skill{{Name: "hidden", Description: "no", FilePath: "x", DisableModelInvocation: true}}
	if got := FormatSkillsForPromptWithTool(hiddenOnly, SkillFileReadToolBash); got != "" {
		t.Fatalf("hidden-only skills should render nothing, got %q", got)
	}
}

// Upstream 1d6dbf9e3 inverted this test's original premise: a bash-only tool
// set no longer drops skills, it switches the block's instruction to bash.
// pi picks the first of ["read", "bash"] present in the tool set — read wins
// when both are — and omits the block only when neither is active.
func TestSkillsPromptFollowsFileReadTool(t *testing.T) {
	const (
		readLine = "Use the read tool to load a skill's file when the task matches its description."
		bashLine = "Use bash to load a skill's file when the task matches its description."
	)
	build := func(tools ...string) string {
		return mustBuildSystemPrompt(t, BuildSystemPromptOptions{
			SelectedTools: tools,
			ToolSnippets:  ToolSnippets,
			Cwd:           "/proj",
			Skills:        []Skill{{Name: "demo", Description: "d", FilePath: "x"}},
		})
	}

	for _, tc := range []struct {
		name  string
		tools []string
		// want is the instruction line the skills block must carry; empty
		// means the block must not appear at all.
		want string
	}{
		{name: "bash only", tools: []string{"bash"}, want: bashLine},
		{name: "read only", tools: []string{"read"}, want: readLine},
		{name: "read wins over bash", tools: []string{"bash", "read"}, want: readLine},
		{name: "neither", tools: []string{"edit", "write"}},
		{name: "no tools at all", tools: []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := build(tc.tools...)
			if tc.want == "" {
				if strings.Contains(p, "available_skills") {
					t.Fatalf("skills must be omitted without a file-reading tool: %q", p)
				}
				return
			}
			if !strings.Contains(p, "<name>demo</name>") {
				t.Fatalf("skills block missing: %q", p)
			}
			if !strings.Contains(p, tc.want) {
				t.Fatalf("skills block missing %q: %q", tc.want, p)
			}
			other := readLine
			if tc.want == readLine {
				other = bashLine
			}
			if strings.Contains(p, other) {
				t.Fatalf("skills block carries the wrong instruction %q: %q", other, p)
			}
		})
	}
}

// pi 1355cd36e: a context file's leading UTF-8 BOM is stripped at load, so it
// never reaches the <project_instructions> block — the system prompt emits the
// content verbatim, and a stray U+FEFF would land in the model's first bytes.
func TestContextFileStripsLeadingBOM(t *testing.T) {
	isolatedHome(t)
	cwd := t.TempDir()
	writeFile(t, filepath.Join(cwd, "AGENTS.md"), "\ufefffollow the rules")

	files := LoadProjectContextFiles(cwd)
	assertContents(t, contextContents(files), "follow the rules")

	prompt := mustBuildSystemPrompt(t, BuildSystemPromptOptions{
		SelectedTools: []string{"read", "bash"},
		ToolSnippets:  ToolSnippets,
		Cwd:           cwd,
		ContextFiles:  files,
	})
	if strings.Contains(prompt, "\ufeff") {
		t.Fatalf("system prompt carries a BOM: %q", prompt)
	}
	// Byte-identical to the same file without a BOM.
	plainCwd := t.TempDir()
	writeFile(t, filepath.Join(plainCwd, "AGENTS.md"), "follow the rules")
	plain := mustBuildSystemPrompt(t, BuildSystemPromptOptions{
		SelectedTools: []string{"read", "bash"},
		ToolSnippets:  ToolSnippets,
		Cwd:           plainCwd,
		ContextFiles:  LoadProjectContextFiles(plainCwd),
	})
	if strings.ReplaceAll(prompt, cwd, "CWD") != strings.ReplaceAll(plain, plainCwd, "CWD") {
		t.Fatalf("BOM changed the assembled prompt:\n got: %q\nwant: %q", prompt, plain)
	}
}

// findNodePackageDir mirrors pi's helper of the same name (config.ts). The
// dist case is upstream 7d4c0e05d: build:binary copies binary metadata —
// package.json included — into dist/, and asset paths must still resolve
// against the package root or they become dist/dist/.
//
// The "no package.json" case necessarily walks out of t.TempDir() to the
// filesystem root, so a stray package.json above TMPDIR would fail it. Making
// it hermetic would mean injecting the fileExists probe, which is not worth an
// interface for one helper.
func TestFindNodePackageDir(t *testing.T) {
	for _, tc := range []struct {
		name     string
		packages []string // dirs (relative to root) that get a package.json
		dirs     []string // dirs (relative to root) created empty
		start    string
		want     string
	}{
		{"skips binary metadata copied into dist", []string{".", "dist"}, []string{"dist/bundle"}, "dist/bundle", "."},
		{"keeps dist when it is the only package", []string{"dist"}, nil, "dist", "dist"},
		{"keeps a non-dist directory even when the parent is a package", []string{".", "build"}, nil, "build", "build"},
		{"walks up to the nearest package root", []string{"."}, []string{"a/b/c"}, "a/b/c", "."},
		{"returns the start directory when no package.json is found", nil, []string{"a/b"}, "a/b", "a/b"},
		{"a start directory that is itself a package is its own answer", []string{"."}, nil, ".", "."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			for _, d := range tc.dirs {
				if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
					t.Fatalf("mkdir %s: %v", d, err)
				}
			}
			for _, d := range tc.packages {
				writeFile(t, filepath.Join(root, d, "package.json"), "{}")
			}
			start, want := filepath.Join(root, tc.start), filepath.Join(root, tc.want)
			if got := findNodePackageDir(start); got != want {
				t.Fatalf("findNodePackageDir(%q) = %q, want %q", start, got, want)
			}
		})
	}
}
