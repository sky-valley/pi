package coding

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/sky-valley/pi/ai"
)

// The system prompt goldens are pi's: testdata/systemprompt/capture.mts feeds
// the Go port's inputs (ToolSnippets, the default tools' PromptGuidelines, and
// fixture paths) to the published build's buildSystemPrompt,
// buildSystemPromptSections, buildSystemPromptState and
// diffSystemPromptSections (pi-coding-agent 1.3.0) under node and records what
// they return.

const systemPromptCaptureFile = "testdata/systemprompt/systemprompt-1.3.0.json"

// sectionPairs is a JS object written as [name, value] pairs, so its key order
// survives JSON.
type sectionPairs []ai.SystemSection

func (p *sectionPairs) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		*p = nil
		return nil
	}
	var raw [][]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	out := sectionPairs{}
	for _, pair := range raw {
		if len(pair) != 2 {
			return fmt.Errorf("section pair %s: want [name, value]", pair)
		}
		var section ai.SystemSection
		if err := json.Unmarshal(pair[0], &section.Name); err != nil {
			return err
		}
		if err := json.Unmarshal(pair[1], &section.Value); err != nil {
			return err
		}
		out = append(out, section)
	}
	*p = out
	return nil
}

type promptCaptureInput struct {
	CustomPrompt       string              `json:"customPrompt"`
	ForceSystemPrompt  *string             `json:"forceSystemPrompt"`
	SelectedTools      []string            `json:"selectedTools"`
	HiddenTools        []string            `json:"hiddenTools"`
	ToolSnippets       map[string]string   `json:"toolSnippets"`
	ToolGuidelines     map[string][]string `json:"toolGuidelines"`
	PromptGuidelines   []string            `json:"promptGuidelines"`
	AppendSystemPrompt string              `json:"appendSystemPrompt"`
	Sections           sectionPairs        `json:"sections"`
	Cwd                string              `json:"cwd"`
	ContextFiles       []struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	} `json:"contextFiles"`
	Skills []struct {
		Name                   string `json:"name"`
		Description            string `json:"description"`
		FilePath               string `json:"filePath"`
		DisableModelInvocation bool   `json:"disableModelInvocation"`
	} `json:"skills"`
}

// options is the Go call the capture made in TS, with pi's documentation paths
// pinned to the capture's PI_PACKAGE_DIR.
func (in promptCaptureInput) options() BuildSystemPromptOptions {
	opts := BuildSystemPromptOptions{
		CustomPrompt:       in.CustomPrompt,
		ForceSystemPrompt:  in.ForceSystemPrompt,
		SelectedTools:      in.SelectedTools,
		HiddenTools:        in.HiddenTools,
		ToolSnippets:       in.ToolSnippets,
		ToolGuidelines:     in.ToolGuidelines,
		PromptGuidelines:   in.PromptGuidelines,
		AppendSystemPrompt: in.AppendSystemPrompt,
		Cwd:                in.Cwd,
		ReadmePath:         "/pkg/README.md",
		DocsPath:           "/pkg/docs",
		ExamplesPath:       "/pkg/examples",
	}
	if in.Sections != nil {
		opts.Sections = ai.SystemSections(in.Sections)
	}
	for _, file := range in.ContextFiles {
		opts.ContextFiles = append(opts.ContextFiles, ContextFile{Path: file.Path, Content: file.Content})
	}
	for _, skill := range in.Skills {
		opts.Skills = append(opts.Skills, Skill{
			Name: skill.Name, Description: skill.Description, FilePath: skill.FilePath,
			DisableModelInvocation: skill.DisableModelInvocation,
		})
	}
	return opts
}

// promptCaptureBuild is one build case: each builder's result, or the message
// it threw.
type promptCaptureBuild struct {
	Name          string              `json:"name"`
	Input         promptCaptureInput  `json:"input"`
	Prompt        string              `json:"prompt"`
	PromptError   string              `json:"promptError"`
	Sections      sectionPairs        `json:"sections"`
	SectionsError string              `json:"sectionsError"`
	State         *promptCaptureState `json:"state"`
	StateError    string              `json:"stateError"`
}

// promptCaptureState is buildSystemPromptState's result; Sections is nil when
// pi leaves the key out.
type promptCaptureState struct {
	Content  string       `json:"content"`
	Sections sectionPairs `json:"sections"`
}

type promptCaptureDiff struct {
	Name     string       `json:"name"`
	Previous sectionPairs `json:"previous"`
	Current  sectionPairs `json:"current"`
	Patch    sectionPairs `json:"patch"`
}

type promptCapture struct {
	Version string `json:"version"`
	// ObjectPrototypeNames are Object.getOwnPropertyNames(Object.prototype)
	// under the capturing node.
	ObjectPrototypeNames []string             `json:"objectPrototypeNames"`
	Builds               []promptCaptureBuild `json:"builds"`
	Diffs                []promptCaptureDiff  `json:"diffs"`
}

func loadPromptCapture(t *testing.T) promptCapture {
	t.Helper()
	data, err := os.ReadFile(systemPromptCaptureFile)
	if err != nil {
		t.Fatal(err)
	}
	var capture promptCapture
	if err := json.Unmarshal(data, &capture); err != nil {
		t.Fatalf("%s: %v", systemPromptCaptureFile, err)
	}
	if len(capture.Builds) == 0 || len(capture.Diffs) == 0 {
		t.Fatalf("%s holds no cases", systemPromptCaptureFile)
	}
	return capture
}

func captureBuild(t *testing.T, name string) promptCaptureBuild {
	t.Helper()
	for _, build := range loadPromptCapture(t).Builds {
		if build.Name == name {
			return build
		}
	}
	t.Fatalf("%s has no build case %q", systemPromptCaptureFile, name)
	return promptCaptureBuild{}
}

func sectionEntries(sections ai.SystemSections) []ai.SystemSection {
	if sections == nil {
		return nil
	}
	return sections.Entries()
}

func formatSections(sections []ai.SystemSection) string {
	if sections == nil {
		return "<nil>"
	}
	var b strings.Builder
	for _, section := range sections {
		if section.Value == nil {
			fmt.Fprintf(&b, "%q: null\n", section.Name)
		} else {
			fmt.Fprintf(&b, "%q: %q\n", section.Name, *section.Value)
		}
	}
	return b.String()
}

// checkCaptureError reports whether pi threw for a builder, failing unless the
// Go error agrees: pi's message followed by a resolution hint.
func checkCaptureError(t *testing.T, builder string, err error, piError string) bool {
	t.Helper()
	if piError == "" {
		if err != nil {
			t.Fatalf("%s: unexpected error %v", builder, err)
		}
		return false
	}
	if err == nil || !strings.HasPrefix(err.Error(), piError+" (") {
		t.Fatalf("%s: error = %v, want pi's %q followed by a resolution hint", builder, err, piError)
	}
	return true
}

// Every build case: the rendered prompt, the ordered sections and the prompt
// state are pi's bytes, and an invalid custom section name fails where pi
// throws — in every builder but a forced prompt's state and rendering, which
// never build the sections.
func TestSystemPromptMatchesPiCapture(t *testing.T) {
	for _, build := range loadPromptCapture(t).Builds {
		t.Run(build.Name, func(t *testing.T) {
			prompt, err := BuildSystemPrompt(build.Input.options())
			if !checkCaptureError(t, "BuildSystemPrompt", err, build.PromptError) && prompt != build.Prompt {
				t.Fatalf("prompt drift from pi.\n--- got ---\n%s\n--- pi ---\n%s", prompt, build.Prompt)
			}
			sections, err := BuildSystemPromptSections(build.Input.options())
			if !checkCaptureError(t, "BuildSystemPromptSections", err, build.SectionsError) {
				if got, want := sectionEntries(sections), []ai.SystemSection(build.Sections); !reflect.DeepEqual(got, want) {
					t.Fatalf("sections drift from pi.\n--- got ---\n%s--- pi ---\n%s", formatSections(got), formatSections(want))
				}
			}
			state, err := BuildSystemPromptState(build.Input.options())
			if checkCaptureError(t, "BuildSystemPromptState", err, build.StateError) {
				return
			}
			if build.State == nil {
				t.Fatalf("%s records neither a state nor a state error; rerun capture.mts", systemPromptCaptureFile)
			}
			if state.Content != build.State.Content {
				t.Fatalf("state content = %q, pi %q", state.Content, build.State.Content)
			}
			if got, want := sectionEntries(state.Sections), []ai.SystemSection(build.State.Sections); !reflect.DeepEqual(got, want) {
				t.Fatalf("state sections drift from pi.\n--- got ---\n%s--- pi ---\n%s", formatSections(got), formatSections(want))
			}
		})
	}
}

// The capture's tool inputs are the Go port's own: NewSession passes
// ToolSnippets and the resolved tools' guidelines keyed by name, exactly as the
// default-tools case does.
func TestSystemPromptCaptureInputsAreTheGoValues(t *testing.T) {
	tools := resolveTools("/proj", SessionOptions{}, nil, nil)
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool.Name)
	}
	build := captureBuild(t, "default-tools")
	if !reflect.DeepEqual(build.Input.SelectedTools, names) {
		t.Fatalf("selectedTools = %v, NewSession resolves %v", build.Input.SelectedTools, names)
	}
	if !reflect.DeepEqual(build.Input.ToolSnippets, ToolSnippets) {
		t.Fatalf("capture toolSnippets = %v, want ToolSnippets %v", build.Input.ToolSnippets, ToolSnippets)
	}
	if got := toolPromptGuidelines(tools); !reflect.DeepEqual(build.Input.ToolGuidelines, got) {
		t.Fatalf("capture toolGuidelines = %v, NewSession collects %v", build.Input.ToolGuidelines, got)
	}
}

// Every diff case, including JS object semantics: a patch follows JS key order
// (integer-like names first), and a name Object.prototype carries is never
// reported removed because the inherited lookup is not undefined.
func TestDiffSystemPromptSectionsMatchesPiCapture(t *testing.T) {
	for _, diff := range loadPromptCapture(t).Diffs {
		t.Run(diff.Name, func(t *testing.T) {
			patch, changed := DiffSystemPromptSections(ai.SystemSections(diff.Previous), ai.SystemSections(diff.Current))
			if changed != (diff.Patch != nil) {
				t.Fatalf("changed = %v, pi patch = %s", changed, formatSections(diff.Patch))
			}
			if got, want := sectionEntries(patch), []ai.SystemSection(diff.Patch); !reflect.DeepEqual(got, want) {
				t.Fatalf("patch drift from pi.\n--- got ---\n%s--- pi ---\n%s", formatSections(got), formatSections(want))
			}
		})
	}
}

// objectPrototypeKeys is exactly the set of names node reports on
// Object.prototype: a missing name would let a removed section of that name be
// patched away where pi keeps it, and an extra one would keep a section pi
// removes.
func TestObjectPrototypeKeysMatchNode(t *testing.T) {
	names := loadPromptCapture(t).ObjectPrototypeNames
	if len(names) == 0 {
		t.Fatalf("%s records no Object.prototype names", systemPromptCaptureFile)
	}
	want := make(map[string]bool, len(names))
	for _, name := range names {
		want[name] = true
	}
	if !reflect.DeepEqual(objectPrototypeKeys, want) {
		t.Fatalf("objectPrototypeKeys = %v, node's Object.prototype names %v", objectPrototypeKeys, names)
	}
}

func mustBuildSystemPrompt(t *testing.T, opts BuildSystemPromptOptions) string {
	t.Helper()
	prompt, err := BuildSystemPrompt(opts)
	if err != nil {
		t.Fatal(err)
	}
	return prompt
}

func mustBuildSystemPromptSections(t *testing.T, opts BuildSystemPromptOptions) ai.SystemSections {
	t.Helper()
	sections, err := BuildSystemPromptSections(opts)
	if err != nil {
		t.Fatal(err)
	}
	return sections
}

func sectionsOf(pairs ...string) ai.SystemSections {
	sections := ai.SystemSections{}
	for i := 0; i+1 < len(pairs); i += 2 {
		value := pairs[i+1]
		sections.Set(pairs[i], &value)
	}
	return sections
}

// system-prompt.test.ts at 9e05370b2, cases the capture does not already cover
// byte for byte.
func TestBuildSystemPromptUpstreamCases(t *testing.T) {
	cwd, _ := os.Getwd()

	t.Run("shows (none) for empty tools list", func(t *testing.T) {
		prompt := mustBuildSystemPrompt(t, BuildSystemPromptOptions{SelectedTools: []string{}, Cwd: cwd})
		if !strings.Contains(prompt, "<tools>\n(none)\n") {
			t.Fatalf("missing empty tools section:\n%s", prompt)
		}
	})
	t.Run("shows file paths guideline even with no tools", func(t *testing.T) {
		prompt := mustBuildSystemPrompt(t, BuildSystemPromptOptions{SelectedTools: []string{}, Cwd: cwd})
		if !strings.Contains(prompt, "Show file paths clearly") {
			t.Fatalf("missing file paths guideline:\n%s", prompt)
		}
	})
	t.Run("keeps the default and custom prompt prefixes exact", func(t *testing.T) {
		defaultPrompt := mustBuildSystemPrompt(t, BuildSystemPromptOptions{Cwd: "/tmp", SelectedTools: []string{}})
		customPrompt := mustBuildSystemPrompt(t, BuildSystemPromptOptions{CustomPrompt: "You are Exact.", Cwd: "/tmp", SelectedTools: []string{}})
		if !strings.HasPrefix(defaultPrompt, "You are an expert coding assistant operating inside pi") {
			t.Fatalf("default prefix:\n%s", defaultPrompt)
		}
		if !strings.HasPrefix(customPrompt, "You are Exact.\n\n<cwd>") {
			t.Fatalf("custom prefix:\n%s", customPrompt)
		}
	})
	t.Run("preserves an exact forced prompt without sections", func(t *testing.T) {
		force := "exact"
		if prompt := mustBuildSystemPrompt(t, BuildSystemPromptOptions{ForceSystemPrompt: &force, Cwd: "/tmp"}); prompt != "exact" {
			t.Fatalf("forced prompt = %q", prompt)
		}
	})
	t.Run("maps appended instructions and project context to stable sections", func(t *testing.T) {
		prompt := mustBuildSystemPrompt(t, BuildSystemPromptOptions{
			CustomPrompt:       "You are Exact.",
			AppendSystemPrompt: "Additional instructions.",
			ContextFiles:       []ContextFile{{Path: "/tmp/AGENTS.md", Content: "Project instructions."}},
			SelectedTools:      []string{},
			Cwd:                "/tmp",
		})
		for _, want := range []string{
			"<addendum>\nAdditional instructions.\n</addendum>",
			"<project_context>\nProject-specific instructions and guidelines:\n\n<project_instructions path=\"/tmp/AGENTS.md\">",
			"<cwd>\n/tmp\n</cwd>",
		} {
			if !strings.Contains(prompt, want) {
				t.Fatalf("missing %q:\n%s", want, prompt)
			}
		}
	})
	t.Run("includes all default tools when snippets are provided", func(t *testing.T) {
		prompt := mustBuildSystemPrompt(t, BuildSystemPromptOptions{
			ToolSnippets: map[string]string{
				"read": "Read file contents", "bash": "Execute bash commands",
				"edit": "Make surgical edits", "write": "Create or overwrite files",
			},
			Cwd: cwd,
		})
		for _, want := range []string{"- read:", "- bash:", "- edit:", "- write:"} {
			if !strings.Contains(prompt, want) {
				t.Fatalf("missing %q:\n%s", want, prompt)
			}
		}
	})
	for _, tc := range []struct {
		tools []string
		want  string
	}{
		{[]string{"powershell"}, "Use PowerShell for file operations"},
		{[]string{"bash", "powershell"}, "Use bash or PowerShell for file operations"},
	} {
		t.Run(fmt.Sprintf("uses shell-specific guidance for %v", tc.tools), func(t *testing.T) {
			if prompt := mustBuildSystemPrompt(t, BuildSystemPromptOptions{SelectedTools: tc.tools, Cwd: cwd}); !strings.Contains(prompt, tc.want) {
				t.Fatalf("missing %q:\n%s", tc.want, prompt)
			}
		})
	}
	t.Run("instructs models to resolve pi docs and examples under absolute base paths", func(t *testing.T) {
		prompt := mustBuildSystemPrompt(t, BuildSystemPromptOptions{Cwd: cwd})
		for _, want := range []string{
			"- When reading pi docs or examples, resolve docs/... under Additional docs and examples/... under Examples, not the current working directory",
			"environment variables (docs/environment-variables.md), MCP servers (docs/mcp.md)",
		} {
			if !strings.Contains(prompt, want) {
				t.Fatalf("missing %q:\n%s", want, prompt)
			}
		}
	})
	t.Run("includes custom tools in available tools section when promptSnippet is provided", func(t *testing.T) {
		prompt := mustBuildSystemPrompt(t, BuildSystemPromptOptions{
			SelectedTools: []string{"read", "dynamic_tool"},
			ToolSnippets:  map[string]string{"dynamic_tool": "Run dynamic test behavior"},
			Cwd:           cwd,
		})
		if !strings.Contains(prompt, "- dynamic_tool: Run dynamic test behavior") {
			t.Fatalf("missing dynamic tool:\n%s", prompt)
		}
	})
	t.Run("omits custom tools from available tools section when promptSnippet is not provided", func(t *testing.T) {
		prompt := mustBuildSystemPrompt(t, BuildSystemPromptOptions{SelectedTools: []string{"read", "dynamic_tool"}, Cwd: cwd})
		if strings.Contains(prompt, "dynamic_tool") {
			t.Fatalf("dynamic_tool must be omitted:\n%s", prompt)
		}
	})
	t.Run("appends promptGuidelines to default guidelines", func(t *testing.T) {
		prompt := mustBuildSystemPrompt(t, BuildSystemPromptOptions{
			SelectedTools:    []string{"read", "dynamic_tool"},
			PromptGuidelines: []string{"Use dynamic_tool for project summaries."},
			Cwd:              cwd,
		})
		if !strings.Contains(prompt, "- Use dynamic_tool for project summaries.") {
			t.Fatalf("missing guideline:\n%s", prompt)
		}
	})
	t.Run("deduplicates and trims promptGuidelines", func(t *testing.T) {
		prompt := mustBuildSystemPrompt(t, BuildSystemPromptOptions{
			SelectedTools:    []string{"read", "dynamic_tool"},
			PromptGuidelines: []string{"Use dynamic_tool for summaries.", "  Use dynamic_tool for summaries.  ", "   "},
			Cwd:              cwd,
		})
		if n := strings.Count(prompt, "- Use dynamic_tool for summaries."); n != 1 {
			t.Fatalf("guideline appears %d times:\n%s", n, prompt)
		}
	})
	skill := Skill{Name: "test-skill", Description: "A test skill.", FilePath: "/skills/test-skill/SKILL.md", BaseDir: "/skills/test-skill"}
	for _, tc := range []struct{ name, customPrompt string }{
		{"default prompt", ""},
		{"custom prompt", "Custom system prompt"},
	} {
		t.Run("includes skills with only bash in the "+tc.name, func(t *testing.T) {
			prompt := mustBuildSystemPrompt(t, BuildSystemPromptOptions{
				CustomPrompt:  tc.customPrompt,
				SelectedTools: []string{"bash"},
				Skills:        []Skill{skill},
				Cwd:           cwd,
			})
			for _, want := range []string{"<skills>", "<available_skills>", "<name>test-skill</name>", "Use bash to load a skill's file"} {
				if !strings.Contains(prompt, want) {
					t.Fatalf("missing %q:\n%s", want, prompt)
				}
			}
		})
	}
	t.Run("omits skills without read or bash", func(t *testing.T) {
		prompt := mustBuildSystemPrompt(t, BuildSystemPromptOptions{SelectedTools: []string{"write"}, Skills: []Skill{skill}, Cwd: cwd})
		if strings.Contains(prompt, "<available_skills>") {
			t.Fatalf("skills must be omitted:\n%s", prompt)
		}
	})
}

// system-prompt-updates.test.ts at e4c75a732, its system-prompt.ts cases.
func TestSystemPromptSectionUpdatesUpstreamCases(t *testing.T) {
	t.Run("diffs sections into a patch", func(t *testing.T) {
		previous := mustBuildSystemPromptSections(t, BuildSystemPromptOptions{Cwd: "/tmp", Sections: sectionsOf("plan_mode", "Plan only.")})
		current := mustBuildSystemPromptSections(t, BuildSystemPromptOptions{Cwd: "/tmp", Sections: sectionsOf("plan_mode", "Implementation allowed.")})

		patch, changed := DiffSystemPromptSections(previous, current)
		if want := sectionsOf("plan_mode", "<plan_mode>\nImplementation allowed.\n</plan_mode>"); !changed || !reflect.DeepEqual(patch.Entries(), want.Entries()) {
			t.Fatalf("patch = %s changed = %v", formatSections(sectionEntries(patch)), changed)
		}
		if patch, changed := DiffSystemPromptSections(previous, previous); changed || patch != nil {
			t.Fatalf("an unchanged prompt must diff to nothing, got %s", formatSections(sectionEntries(patch)))
		}
		patch, changed = DiffSystemPromptSections(previous, mustBuildSystemPromptSections(t, BuildSystemPromptOptions{Cwd: "/tmp"}))
		if want := (ai.SystemSections{{Name: "plan_mode"}}); !changed || !reflect.DeepEqual(patch.Entries(), want.Entries()) {
			t.Fatalf("removal patch = %s changed = %v", formatSections(sectionEntries(patch)), changed)
		}
	})
	t.Run("keeps the preamble untagged and replaces it like any section", func(t *testing.T) {
		previous := mustBuildSystemPromptSections(t, BuildSystemPromptOptions{CustomPrompt: "You are A.", Cwd: "/tmp"})
		current := mustBuildSystemPromptSections(t, BuildSystemPromptOptions{CustomPrompt: "You are B.", Cwd: "/tmp"})
		if value, ok := previous.Get("preamble"); !ok || value == nil || *value != "You are A." {
			t.Fatalf("preamble = %v", formatSections(sectionEntries(previous)))
		}
		patch, changed := DiffSystemPromptSections(previous, current)
		if want := sectionsOf("preamble", "You are B."); !changed || !reflect.DeepEqual(patch.Entries(), want.Entries()) {
			t.Fatalf("patch = %s", formatSections(sectionEntries(patch)))
		}

		force := "Exact prompt."
		forced, err := BuildSystemPromptState(BuildSystemPromptOptions{ForceSystemPrompt: &force, Cwd: "/tmp"})
		if err != nil || !reflect.DeepEqual(forced, SystemPromptState{Content: "Exact prompt."}) {
			t.Fatalf("forced state = %+v, %v; want {Content: %q} with no sections", forced, err, force)
		}
		structured, err := BuildSystemPromptState(BuildSystemPromptOptions{Cwd: "/tmp"})
		want := SystemPromptState{Sections: mustBuildSystemPromptSections(t, BuildSystemPromptOptions{Cwd: "/tmp"})}
		if err != nil || !reflect.DeepEqual(structured, want) {
			t.Fatalf("structured state = %+v, %v; want %+v", structured, err, want)
		}
		if _, err := BuildSystemPromptSections(BuildSystemPromptOptions{Cwd: "/tmp", Sections: sectionsOf("preamble", "x")}); err == nil ||
			!strings.Contains(err.Error(), "Invalid system prompt section name") {
			t.Fatalf("a custom preamble section must be rejected, got %v", err)
		}
	})
}

// Normalizing copies every collection, so a caller mutating its inputs
// afterwards cannot reach the normalized options (pi returns fresh arrays and
// objects), and an absent tool selection becomes pi's default.
func TestNormalizeBuildSystemPromptOptionsCopies(t *testing.T) {
	in := BuildSystemPromptOptions{
		HiddenTools:      []string{"bash"},
		ToolSnippets:     map[string]string{"read": "r"},
		ToolGuidelines:   map[string][]string{"read": {"g"}},
		PromptGuidelines: []string{"p"},
		Sections:         sectionsOf("a", "1"),
		ContextFiles:     []ContextFile{{Path: "/a", Content: "c"}},
		Skills:           []Skill{{Name: "s"}},
		Cwd:              "/proj",
	}
	out := NormalizeBuildSystemPromptOptions(in)
	if !reflect.DeepEqual(out.SelectedTools, []string{"read", "bash", "edit", "write"}) {
		t.Fatalf("default selectedTools = %v", out.SelectedTools)
	}
	in.HiddenTools[0] = "changed"
	in.ToolSnippets["read"] = "changed"
	in.ToolGuidelines["read"][0] = "changed"
	in.PromptGuidelines[0] = "changed"
	*in.Sections[0].Value = "changed"
	in.ContextFiles[0].Content = "changed"
	in.Skills[0].Name = "changed"
	if out.HiddenTools[0] != "bash" || out.ToolSnippets["read"] != "r" || out.ToolGuidelines["read"][0] != "g" || out.PromptGuidelines[0] != "p" ||
		*out.Sections[0].Value != "1" || out.ContextFiles[0].Content != "c" || out.Skills[0].Name != "s" {
		t.Fatalf("normalized options alias the input: %+v", out)
	}

	empty := NormalizeBuildSystemPromptOptions(BuildSystemPromptOptions{SelectedTools: []string{}})
	if empty.SelectedTools == nil || len(empty.SelectedTools) != 0 {
		t.Fatalf("an explicit empty selection must stay empty, got %v", empty.SelectedTools)
	}
	if empty.HiddenTools == nil || empty.ToolSnippets == nil || empty.ToolGuidelines == nil || empty.Sections == nil {
		t.Fatalf("normalized collections must be non-nil: %+v", empty)
	}
}
