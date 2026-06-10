package coding

import (
	"testing"
	"time"
)

// TestDefaultSystemPromptGolden pins the full assembled default system prompt
// byte-for-byte, so any drift in the prompt text — including the "Pi
// documentation" routing block and the per-tool promptGuidelines folded into
// the Guidelines section — fails. The tool snippets and guidelines are
// collected from the real resolved tool set, exactly like NewSession (pi
// agent-session.ts _rebuildSystemPrompt). The expected text was verified
// against pi's npm build by invoking the built buildSystemPrompt with the
// build's own tool definitions (createAllToolDefinitions) for the default
// [read, bash, edit, write] set.
func TestDefaultSystemPromptGolden(t *testing.T) {
	tools := resolveTools("/proj", SessionOptions{})
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool.Name)
	}
	got := BuildSystemPrompt(BuildSystemPromptOptions{
		SelectedTools:    names,
		ToolSnippets:     ToolSnippets,
		PromptGuidelines: collectPromptGuidelines(tools),
		Cwd:              "/proj",
		ReadmePath:       "/pkg/README.md",
		DocsPath:         "/pkg/docs",
		ExamplesPath:     "/pkg/examples",
		Now:              time.Date(2026, 6, 8, 0, 0, 0, 0, time.UTC),
	})

	want := `You are an expert coding assistant operating inside pi, a coding agent harness. You help users by reading files, executing commands, editing code, and writing new files.

Available tools:
- read: Read file contents
- bash: Execute bash commands (ls, grep, find, etc.)
- edit: Make precise file edits with exact text replacement, including multiple disjoint edits in one call
- write: Create or overwrite files

In addition to the tools above, you may have access to other custom tools depending on the project.

Guidelines:
- Use bash for file operations like ls, rg, find
- Use read to examine files instead of cat or sed.
- Use edit for precise changes (edits[].oldText must match exactly)
- When changing multiple separate locations in one file, use one edit call with multiple entries in edits[] instead of multiple edit calls
- Each edits[].oldText is matched against the original file, not after earlier edits are applied. Do not emit overlapping or nested edits. Merge nearby changes into one edit.
- Keep edits[].oldText as small as possible while still being unique in the file. Do not pad with large unchanged regions.
- Use write only for new files or complete rewrites.
- Be concise in your responses
- Show file paths clearly when working with files

Pi documentation (read only when the user asks about pi itself, its SDK, extensions, themes, skills, or TUI):
- Main documentation: /pkg/README.md
- Additional docs: /pkg/docs
- Examples: /pkg/examples (extensions, custom tools, SDK)
- When reading pi docs or examples, resolve docs/... under Additional docs and examples/... under Examples, not the current working directory
- When asked about: extensions (docs/extensions.md, examples/extensions/), themes (docs/themes.md), skills (docs/skills.md), prompt templates (docs/prompt-templates.md), TUI components (docs/tui.md), keybindings (docs/keybindings.md), SDK integrations (docs/sdk.md), custom providers (docs/custom-provider.md), adding models (docs/models.md), pi packages (docs/packages.md)
- When working on pi topics, read the docs and examples, and follow .md cross-references before implementing
- Always read pi .md files completely and follow links to related docs (e.g., tui.md for TUI API details)
Current date: 2026-06-08
Current working directory: /proj`

	if got != want {
		t.Fatalf("default system prompt drift.\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestCustomSystemPromptAssemblyGolden pins pi's custom-prompt assembly order
// (system-prompt.ts:53-80): custom prompt, append section, project context,
// skills, then date and cwd — and asserts the docs block and Guidelines
// section are NOT included for custom prompts.
func TestCustomSystemPromptAssemblyGolden(t *testing.T) {
	got := BuildSystemPrompt(BuildSystemPromptOptions{
		CustomPrompt:       "You are a custom agent.",
		AppendSystemPrompt: "Appended instructions.",
		SelectedTools:      []string{"read", "bash"},
		ToolSnippets:       ToolSnippets,
		PromptGuidelines:   []string{"Use read to examine files instead of cat or sed."},
		Cwd:                "/proj",
		ContextFiles:       []ContextFile{{Path: "/proj/AGENTS.md", Content: "follow the rules"}},
		Skills:             []Skill{{Name: "demo", Description: "d", FilePath: "/proj/.pi/skills/demo/SKILL.md"}},
		Now:                time.Date(2026, 6, 8, 0, 0, 0, 0, time.UTC),
	})

	want := `You are a custom agent.

Appended instructions.

<project_context>

Project-specific instructions and guidelines:

<project_instructions path="/proj/AGENTS.md">
follow the rules
</project_instructions>

</project_context>


The following skills provide specialized instructions for specific tasks.
Use the read tool to load a skill's file when the task matches its description.
When a skill file references a relative path, resolve it against the skill directory (parent of SKILL.md / dirname of the path) and use that absolute path in tool commands.

<available_skills>
  <skill>
    <name>demo</name>
    <description>d</description>
    <location>/proj/.pi/skills/demo/SKILL.md</location>
  </skill>
</available_skills>
Current date: 2026-06-08
Current working directory: /proj`

	if got != want {
		t.Fatalf("custom system prompt assembly drift.\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}
