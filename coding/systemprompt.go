package coding

import (
	"fmt"
	"strings"
	"time"
)

// ContextFile is a project context file injected into the system prompt.
type ContextFile struct {
	Path    string
	Content string
}

// BuildSystemPromptOptions configures buildSystemPrompt.
type BuildSystemPromptOptions struct {
	CustomPrompt       string
	SelectedTools      []string
	ToolSnippets       map[string]string
	PromptGuidelines   []string
	AppendSystemPrompt string
	Cwd                string
	ContextFiles       []ContextFile
	Skills             []Skill
	// ReadmePath/DocsPath/ExamplesPath are the absolute pi documentation paths
	// referenced by the "Pi documentation" prompt section. Empty values fall back
	// to ReadmePath()/DocsPath()/ExamplesPath().
	ReadmePath   string
	DocsPath     string
	ExamplesPath string
	// Now allows deterministic dates in tests; zero value uses time.Now().
	Now time.Time
}

// BuildSystemPrompt constructs the coding agent system prompt (port of
// buildSystemPrompt), including tools, guidelines, project context, and footer.
func BuildSystemPrompt(opts BuildSystemPromptOptions) string {
	promptCwd := strings.ReplaceAll(opts.Cwd, "\\", "/")
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	date := now.Format("2006-01-02")

	appendSection := ""
	if opts.AppendSystemPrompt != "" {
		appendSection = "\n\n" + opts.AppendSystemPrompt
	}

	contextSection := func() string {
		if len(opts.ContextFiles) == 0 {
			return ""
		}
		var b strings.Builder
		b.WriteString("\n\n<project_context>\n\n")
		b.WriteString("Project-specific instructions and guidelines:\n\n")
		for _, cf := range opts.ContextFiles {
			b.WriteString(fmt.Sprintf("<project_instructions path=%q>\n%s\n</project_instructions>\n\n", cf.Path, cf.Content))
		}
		b.WriteString("</project_context>\n")
		return b.String()
	}

	tools := opts.SelectedTools
	if tools == nil {
		tools = []string{"read", "bash", "edit", "write"}
	}
	hasRead := false
	for _, t := range tools {
		if t == "read" {
			hasRead = true
		}
	}
	skillsSection := ""
	if hasRead {
		skillsSection = FormatSkillsForPrompt(opts.Skills)
	}

	if opts.CustomPrompt != "" {
		prompt := opts.CustomPrompt + appendSection + contextSection() + skillsSection
		prompt += "\nCurrent date: " + date
		prompt += "\nCurrent working directory: " + promptCwd
		return prompt
	}
	var visible []string
	for _, name := range tools {
		if snippet, ok := opts.ToolSnippets[name]; ok && snippet != "" {
			visible = append(visible, fmt.Sprintf("- %s: %s", name, snippet))
		}
	}
	toolsList := "(none)"
	if len(visible) > 0 {
		toolsList = strings.Join(visible, "\n")
	}

	var guidelines []string
	seen := map[string]bool{}
	add := func(g string) {
		if g == "" || seen[g] {
			return
		}
		seen[g] = true
		guidelines = append(guidelines, g)
	}

	has := func(name string) bool {
		for _, t := range tools {
			if t == name {
				return true
			}
		}
		return false
	}
	if has("bash") && !has("grep") && !has("find") && !has("ls") {
		add("Use bash for file operations like ls, rg, find")
	}
	for _, g := range opts.PromptGuidelines {
		add(strings.TrimSpace(g))
	}
	add("Be concise in your responses")
	add("Show file paths clearly when working with files")

	var gb strings.Builder
	for i, g := range guidelines {
		if i > 0 {
			gb.WriteString("\n")
		}
		gb.WriteString("- " + g)
	}

	readmePath := opts.ReadmePath
	if readmePath == "" {
		readmePath = ReadmePath()
	}
	docsPath := opts.DocsPath
	if docsPath == "" {
		docsPath = DocsPath()
	}
	examplesPath := opts.ExamplesPath
	if examplesPath == "" {
		examplesPath = ExamplesPath()
	}

	prompt := fmt.Sprintf(`You are an expert coding assistant operating inside pi, a coding agent harness. You help users by reading files, executing commands, editing code, and writing new files.

Available tools:
%s

In addition to the tools above, you may have access to other custom tools depending on the project.

Guidelines:
%s

Pi documentation (read only when the user asks about pi itself, its SDK, extensions, themes, skills, or TUI):
- Main documentation: %s
- Additional docs: %s
- Examples: %s (extensions, custom tools, SDK)
- When reading pi docs or examples, resolve docs/... under Additional docs and examples/... under Examples, not the current working directory
- When asked about: extensions (docs/extensions.md, examples/extensions/), themes (docs/themes.md), skills (docs/skills.md), prompt templates (docs/prompt-templates.md), TUI components (docs/tui.md), keybindings (docs/keybindings.md), SDK integrations (docs/sdk.md), custom providers (docs/custom-provider.md), adding models (docs/models.md), pi packages (docs/packages.md)
- When working on pi topics, read the docs and examples, and follow .md cross-references before implementing
- Always read pi .md files completely and follow links to related docs (e.g., tui.md for TUI API details)`, toolsList, gb.String(), readmePath, docsPath, examplesPath)

	prompt += appendSection + contextSection() + skillsSection
	prompt += "\nCurrent date: " + date
	prompt += "\nCurrent working directory: " + promptCwd
	return prompt
}
