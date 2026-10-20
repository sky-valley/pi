package coding

import (
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/sky-valley/pi/agent"
)

// bashDescription and powershellDescription are the composed tool descriptions
// pi ships for the two shell tools. Upstream 80e62761f made them one template
// parameterised by config.shellName; bash's rendering is unchanged.
const (
	bashDescription       = "Execute a bash command in the current working directory. Returns stdout and stderr. Output is truncated to last 2000 lines or 50KB (whichever is hit first). If truncated, full output is saved to a temp file. Optionally provide a timeout in seconds."
	powershellDescription = "Execute a PowerShell command in the current working directory. Returns stdout and stderr. Output is truncated to last 2000 lines or 50KB (whichever is hit first). If truncated, full output is saved to a temp file. Optionally provide a timeout in seconds."
)

// TestShellToolCommandParameterDescription locks the wire-visible schema
// property description shared by both shell tools. Upstream 80e62761f renamed
// it from "Bash command to execute" when the bash tool became one instance of a
// shared shell-tool factory, so this string moved for the existing bash tool.
func TestShellToolCommandParameterDescription(t *testing.T) {
	for _, name := range []string{"bash", "powershell"} {
		tool, err := CreateTool(name, t.TempDir())
		if err != nil {
			t.Fatalf("CreateTool(%q): %v", name, err)
		}
		if got, want := tool.Parameters.Properties["command"].Description, "Shell command to execute"; got != want {
			t.Fatalf("%s command description\n got: %q\nwant: %q", name, got, want)
		}
		if got, want := tool.Parameters.Properties["timeout"].Description, "Timeout in seconds (optional, no default timeout)"; got != want {
			t.Fatalf("%s timeout description\n got: %q\nwant: %q", name, got, want)
		}
		if got, want := tool.Parameters.PropertyOrder, []string{"command", "timeout"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("%s property order\n got: %#v\nwant: %#v", name, got, want)
		}
	}
}

// TestShellToolDescriptions pins both composed descriptions. bash's must be
// byte-identical to the pre-refactor literal.
func TestShellToolDescriptions(t *testing.T) {
	for name, want := range map[string]string{"bash": bashDescription, "powershell": powershellDescription} {
		tool, err := CreateTool(name, t.TempDir())
		if err != nil {
			t.Fatalf("CreateTool(%q): %v", name, err)
		}
		if tool.Description != want {
			t.Fatalf("%s description\n got: %q\nwant: %q", name, tool.Description, want)
		}
		if tool.Name != name || tool.Label != name {
			t.Fatalf("%s name/label: got %q/%q", name, tool.Name, tool.Label)
		}
	}
}

// TestPowerShellToolRegistration locks the powershell tool into the all-tools
// list and its prompt snippet, while keeping it OUT of the default active set
// (upstream sdk.ts defaultActiveToolNames is untouched by 80e62761f).
func TestPowerShellToolRegistration(t *testing.T) {
	if got, want := ToolNames, []string{"read", "bash", "powershell", "edit", "write", "grep", "find", "ls"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ToolNames\n got: %#v\nwant: %#v", got, want)
	}
	if got, want := ToolSnippets["powershell"], "Execute PowerShell commands"; got != want {
		t.Fatalf("powershell snippet\n got: %q\nwant: %q", got, want)
	}
	if slices.Contains(defaultActiveToolNames, "powershell") {
		t.Fatalf("powershell must not be default-active, got %#v", defaultActiveToolNames)
	}
	tool, err := CreateTool("powershell", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"You can inspect PI_* environment variables for current model and session details."}
	if !reflect.DeepEqual(tool.PromptGuidelines, want) {
		t.Fatalf("powershell PromptGuidelines\n got: %#v\nwant: %#v", tool.PromptGuidelines, want)
	}
}

// TestPowerShellToolNotInDefaultSession keeps powershell out of a default
// session's resolved tools: it is opt-in via ToolNames only.
func TestPowerShellToolNotInDefaultSession(t *testing.T) {
	names := func(tools []agent.AgentTool) []string {
		out := make([]string, 0, len(tools))
		for _, tool := range tools {
			out = append(out, tool.Name)
		}
		return out
	}
	if got, want := names(resolveTestTools(t, "/proj", SessionOptions{})), []string{"read", "bash", "edit", "write"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("default active tools\n got: %#v\nwant: %#v", got, want)
	}
	opts := SessionOptions{ToolNames: []string{"read", "powershell", "edit", "write"}}
	if got, want := names(resolveTestTools(t, "/proj", opts)), []string{"read", "powershell", "edit", "write"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("powershell-selected tools\n got: %#v\nwant: %#v", got, want)
	}
}

// TestPowerShellArgs locks pi's POWERSHELL_ARGS: the execution-policy bypass is
// process-scoped and profiles/prompts are disabled.
func TestPowerShellArgs(t *testing.T) {
	want := []string{"-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command"}
	if !reflect.DeepEqual(powershellArgs, want) {
		t.Fatalf("powershellArgs\n got: %#v\nwant: %#v", powershellArgs, want)
	}
}

// TestGetPowerShellConfigOffWindows locks pi's Windows-only guard. pi throws;
// the Go shell resolvers report failure as an error, which the tool surfaces
// verbatim as the tool result the model sees.
func TestGetPowerShellConfigOffWindows(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("guard only fires off Windows")
	}
	_, _, _, err := unpackShellConfig(GetPowerShellConfig())
	if err == nil {
		t.Fatal("expected an error off Windows")
	}
	want := "The powershell tool is only available on Windows."
	if got := err.Error(); got != want {
		t.Fatalf("GetPowerShellConfig error\n got: %q\nwant: %q", got, want)
	}
	tool, err := CreateTool("powershell", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_, execErr := run(t, tool, map[string]any{"command": "Write-Output hi"})
	if execErr == nil || execErr.Error() != want {
		t.Fatalf("powershell execute off Windows\n got: %v\nwant: %q", execErr, want)
	}
}

// TestPowerShellUTF8CommandPrefix pins the UTF-8 opt-in prepended to every
// PowerShell command, byte for byte including the trailing newline
// (powershell.ts UTF8_OUTPUT_PREFIX).
func TestPowerShellUTF8CommandPrefix(t *testing.T) {
	want := "try { [Console]::OutputEncoding=[System.Text.Encoding]::UTF8 } catch {}\n"
	if got := powershellToolConfig.commandPrefix; got != want {
		t.Fatalf("powershell command prefix\n got: %q\nwant: %q", got, want)
	}
	if got := bashToolConfig.commandPrefix; got != "" {
		t.Fatalf("bash must carry no command prefix, got %q", got)
	}
}

// TestShellToolCommandPrefixIsExecuted proves the prefix reaches the shell
// ahead of the model's command rather than only being stored on the config.
func TestShellToolCommandPrefixIsExecuted(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses /bin/sh")
	}
	cfg := shellToolConfig{
		name:           "testshell",
		shellName:      "testshell",
		tempFilePrefix: "pi-testshell",
		commandPrefix:  "echo PREFIX\n",
		resolveShell:   func() (ShellConfig, error) { return ShellConfig{Shell: "/bin/sh", Args: []string{"-c"}}, nil },
	}
	r, err := run(t, shellTool(t.TempDir(), cfg, nil), map[string]any{"command": "echo BODY"})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := resultText(r), "PREFIX\nBODY"; !strings.Contains(got, want) {
		t.Fatalf("prefixed command output\n got: %q\nwant it to contain: %q", got, want)
	}
}

// TestShellToolMissingCwdMessage locks the shell-name interpolation in the
// missing-working-directory error (bash.ts createLocalShellOperations).
func TestShellToolMissingCwdMessage(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses /bin/sh")
	}
	cfg := shellToolConfig{
		name:           "testshell",
		shellName:      "testshell",
		tempFilePrefix: "pi-testshell",
		resolveShell:   func() (ShellConfig, error) { return ShellConfig{Shell: "/bin/sh", Args: []string{"-c"}}, nil },
	}
	missing := t.TempDir() + "/does-not-exist"
	_, err := run(t, shellTool(missing, cfg, nil), map[string]any{"command": "true"})
	want := "Working directory does not exist: " + missing + "\nCannot execute testshell commands."
	if err == nil || err.Error() != want {
		t.Fatalf("missing cwd error\n got: %v\nwant: %q", err, want)
	}
	_, err = run(t, bashTool(missing, nil), map[string]any{"command": "true"})
	want = "Working directory does not exist: " + missing + "\nCannot execute bash commands."
	if err == nil || err.Error() != want {
		t.Fatalf("bash missing cwd error\n got: %v\nwant: %q", err, want)
	}
}

// TestSystemPromptShellGuideline locks the three-way file-exploration guideline
// introduced by upstream 80e62761f (system-prompt.ts:102-112).
func TestSystemPromptShellGuideline(t *testing.T) {
	guideline := func(tools ...string) string {
		prompt := mustBuildSystemPrompt(t, BuildSystemPromptOptions{
			SelectedTools: tools,
			ToolSnippets:  ToolSnippets,
			Cwd:           "/proj",
		})
		for _, line := range strings.Split(prompt, "\n") {
			if strings.HasPrefix(line, "- Use bash") || strings.HasPrefix(line, "- Use PowerShell") {
				return strings.TrimPrefix(line, "- ")
			}
		}
		return ""
	}
	cases := []struct {
		tools []string
		want  string
	}{
		{[]string{"bash"}, "Use bash for file operations like ls, rg, find"},
		{[]string{"powershell"}, "Use PowerShell for file operations like listing, searching, and finding files"},
		{[]string{"bash", "powershell"}, "Use bash or PowerShell for file operations like listing, searching, and finding files"},
		// A dedicated exploration tool suppresses the guideline entirely.
		{[]string{"bash", "powershell", "ls"}, ""},
		{[]string{"read"}, ""},
	}
	for _, tc := range cases {
		if got := guideline(tc.tools...); got != tc.want {
			t.Fatalf("guideline for %v\n got: %q\nwant: %q", tc.tools, got, tc.want)
		}
	}
}

// TestCreateAllToolsCoversToolNames pins the invariant that powershell broke:
// CreateAllTools is pi's createAllTools, so it must yield exactly the built-in
// set ToolNames names, in the same order. The two are separate literals in this
// package, and adding a tool to one without the other is silent — a consumer
// asking CreateAllTools for "everything" just gets less than everything.
func TestCreateAllToolsCoversToolNames(t *testing.T) {
	got := make([]string, 0, len(ToolNames))
	for _, tool := range CreateAllTools(t.TempDir()) {
		got = append(got, tool.Name)
	}
	if !reflect.DeepEqual(got, ToolNames) {
		t.Fatalf("CreateAllTools must yield every name in ToolNames, in order\n got: %#v\nwant: %#v", got, ToolNames)
	}
}
