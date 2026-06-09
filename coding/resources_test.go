package coding

import (
	"os"
	"path/filepath"
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

	skills := LoadSkills(cwd)
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
	p := BuildSystemPrompt(BuildSystemPromptOptions{
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

func TestSkillsExcludedWithoutReadTool(t *testing.T) {
	p := BuildSystemPrompt(BuildSystemPromptOptions{
		SelectedTools: []string{"bash"}, // no read tool
		ToolSnippets:  ToolSnippets,
		Cwd:           "/proj",
		Skills:        []Skill{{Name: "demo", Description: "d", FilePath: "x"}},
	})
	if strings.Contains(p, "available_skills") {
		t.Fatalf("skills should be excluded without read tool: %q", p)
	}
}
