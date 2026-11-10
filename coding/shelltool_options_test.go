package coding

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/sky-valley/pi/agent"
)

// recordedExec is one call a recordingShellOps saw.
type recordedExec struct {
	command, cwd string
	env          []string
}

// recordingShellOps is a BashOperations that records what reaches it and
// exits 0.
func recordingShellOps(calls *[]recordedExec) *BashOperations {
	return &BashOperations{
		Exec: func(_ context.Context, command, cwd string, o BashExecOptions) (*int, error) {
			*calls = append(*calls, recordedExec{command, cwd, o.Env})
			zero := 0
			return &zero, nil
		},
	}
}

// customShellScript writes an executable that announces itself and then runs
// the command through /bin/sh, so a test can tell it was the shell used.
func customShellScript(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("uses a /bin/sh script as the custom shell")
	}
	path := filepath.Join(t.TempDir(), "mysh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho CUSTOM\nexec /bin/sh \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestBashToolShellPathOption ports BashToolOptions.shellPath (bash.ts): the
// tool's local operations resolve the shell with getShellConfig(shellPath).
func TestBashToolShellPathOption(t *testing.T) {
	custom := customShellScript(t)
	r, err := run(t, CreateBashTool(t.TempDir(), &BashToolOptions{ShellPath: custom}), map[string]any{"command": "echo hi"})
	if err != nil {
		t.Fatal(err)
	}
	if got := resultText(r); got != "CUSTOM\nhi\n" {
		t.Fatalf("output %q, want the custom shell's CUSTOM then hi", got)
	}

	missing := filepath.Join(t.TempDir(), "nope", "bash")
	_, err = run(t, CreateBashTool(t.TempDir(), &BashToolOptions{ShellPath: missing}), map[string]any{"command": "true"})
	if want := "Custom shell path not found: " + missing; err == nil || err.Error() != want {
		t.Fatalf("missing shellPath\n got: %v\nwant: %q", err, want)
	}
}

// TestLocalBashOperationsShellPath ports createLocalBashOperations({shellPath}).
func TestLocalBashOperationsShellPath(t *testing.T) {
	custom := customShellScript(t)
	var out strings.Builder
	code, err := LocalBashOperations(LocalBashOptions{ShellPath: custom}).Exec(context.Background(), "echo hi", t.TempDir(),
		BashExecOptions{OnData: func(p []byte) { out.Write(p) }, Env: os.Environ()})
	if err != nil {
		t.Fatal(err)
	}
	if code == nil || *code != 0 || out.String() != "CUSTOM\nhi\n" {
		t.Fatalf("exit %v output %q, want 0 and CUSTOM then hi", code, out.String())
	}
}

// TestBashToolCommandPrefixOption ports BashToolOptions.commandPrefix: it is
// joined to the command with a newline, and absent it the command is as given.
func TestBashToolCommandPrefixOption(t *testing.T) {
	var calls []recordedExec
	tool := CreateBashTool("/work", &BashToolOptions{Operations: recordingShellOps(&calls), CommandPrefix: "shopt -s expand_aliases"})
	if _, err := run(t, tool, map[string]any{"command": "ll"}); err != nil {
		t.Fatal(err)
	}
	tool = CreateBashTool("/work", &BashToolOptions{Operations: recordingShellOps(&calls)})
	if _, err := run(t, tool, map[string]any{"command": "ll"}); err != nil {
		t.Fatal(err)
	}
	if got := []string{calls[0].command, calls[1].command}; !reflect.DeepEqual(got, []string{"shopt -s expand_aliases\nll", "ll"}) {
		t.Fatalf("commands %q", got)
	}
}

func hasEnv(env []string, kv string) bool { return slices.Contains(env, kv) }

// TestBashToolExposeSessionEnvironment ports exposeSessionEnvironment: by
// default the tool sets the PI_* session variables and advertises them; with
// false it sets none (the inherited ones are still stripped) and drops the
// guideline.
func TestBashToolExposeSessionEnvironment(t *testing.T) {
	t.Setenv("PI_MODEL", "stale")
	session := func() map[string]string { return map[string]string{"PI_MODEL": "m1", "PI_SESSION_ID": "s1"} }
	no := false

	var calls []recordedExec
	exposed := createShellTool("/work", bashToolConfig, &BashToolOptions{Operations: recordingShellOps(&calls)}, session)
	hidden := createShellTool("/work", bashToolConfig, &BashToolOptions{Operations: recordingShellOps(&calls), ExposeSessionEnvironment: &no}, session)
	if _, err := run(t, exposed, map[string]any{"command": "x"}); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, hidden, map[string]any{"command": "x"}); err != nil {
		t.Fatal(err)
	}
	if !hasEnv(calls[0].env, "PI_MODEL=m1") || !hasEnv(calls[0].env, "PI_SESSION_ID=s1") {
		t.Errorf("exposed env lacks the session variables")
	}
	if !reflect.DeepEqual(exposed.PromptGuidelines, []string{piSessionEnvGuideline}) {
		t.Errorf("exposed guidelines = %q", exposed.PromptGuidelines)
	}
	for _, kv := range calls[1].env {
		if strings.HasPrefix(kv, "PI_MODEL=") || strings.HasPrefix(kv, "PI_SESSION_ID=") {
			t.Errorf("hidden env carries %q", kv)
		}
	}
	if hidden.PromptGuidelines != nil {
		t.Errorf("hidden guidelines = %q, want none", hidden.PromptGuidelines)
	}
}

// TestBashToolSpawnHook ports spawnHook: it sees the prefixed command, the
// tool's cwd and the assembled env, and what it returns is what runs.
func TestBashToolSpawnHook(t *testing.T) {
	var calls []recordedExec
	var seen BashSpawnContext
	hook := func(c BashSpawnContext) BashSpawnContext {
		seen = c
		return BashSpawnContext{Command: "hooked " + c.Command, Cwd: "/elsewhere", Env: append(slices.Clone(c.Env), "HOOKED=1")}
	}
	tool := CreateBashTool("/work", &BashToolOptions{Operations: recordingShellOps(&calls), CommandPrefix: "p", SpawnHook: hook})
	if _, err := run(t, tool, map[string]any{"command": "c"}); err != nil {
		t.Fatal(err)
	}
	if seen.Command != "p\nc" || seen.Cwd != "/work" || len(seen.Env) == 0 {
		t.Fatalf("hook saw %q in %q with %d env entries", seen.Command, seen.Cwd, len(seen.Env))
	}
	if got := calls[0]; got.command != "hooked p\nc" || got.cwd != "/elsewhere" || !hasEnv(got.env, "HOOKED=1") {
		t.Fatalf("ran %q in %q, HOOKED=1 present: %v", got.command, got.cwd, hasEnv(got.env, "HOOKED=1"))
	}
}

// TestPowerShellToolCustomOperations ports createPowerShellToolDefinition:
// the UTF-8 output prefix belongs to the LOCAL PowerShell operations, so
// custom operations (and the spawn hook) get the model's command as is.
func TestPowerShellToolCustomOperations(t *testing.T) {
	var calls []recordedExec
	var hooked string
	tool := CreatePowerShellTool("/work", &PowerShellToolOptions{
		Operations: recordingShellOps(&calls),
		SpawnHook:  func(c PowerShellSpawnContext) PowerShellSpawnContext { hooked = c.Command; return c },
	})
	if _, err := run(t, tool, map[string]any{"command": "Write-Output hi"}); err != nil {
		t.Fatal(err)
	}
	if hooked != "Write-Output hi" || calls[0].command != "Write-Output hi" {
		t.Fatalf("hook saw %q, operations ran %q; want the bare command", hooked, calls[0].command)
	}
}

// TestShellTimeoutValidationBelongsToLocalOperations: pi validates the
// timeout in createLocalShellOperations (resolveTimeoutMs), not in the tool,
// so custom operations receive the model's value as is, and the tool's
// initial empty update has already gone out when the local operations reject
// it.
func TestShellTimeoutValidationBelongsToLocalOperations(t *testing.T) {
	t.Run("custom operations get the raw timeout", func(t *testing.T) {
		var got *float64
		ops := &BashOperations{Exec: func(_ context.Context, _, _ string, o BashExecOptions) (*int, error) {
			got = o.TimeoutSeconds
			zero := 0
			return &zero, nil
		}}
		if _, err := run(t, CreateBashTool("/work", &BashToolOptions{Operations: ops}), map[string]any{"command": "x", "timeout": -1}); err != nil {
			t.Fatalf("custom operations: %v", err)
		}
		if got == nil || *got != -1 {
			t.Fatalf("custom operations got timeout %v, want -1", got)
		}

	})
	t.Run("the initial update precedes the rejection", func(t *testing.T) {
		updates := 0
		tool := CreateBashTool(t.TempDir(), nil)
		_, err := tool.Execute(context.Background(), "1", map[string]any{"command": "x", "timeout": 0}, func(agent.AgentToolResult) { updates++ })
		if err == nil || err.Error() != "Invalid timeout: must be a finite number of seconds" {
			t.Fatalf("local operations: err = %v", err)
		}
		if updates != 1 {
			t.Fatalf("%d updates before the rejection, want pi's 1 initial empty update", updates)
		}

	})
	t.Run("local operations validate", func(t *testing.T) {
		_, err := LocalBashOperations(LocalBashOptions{}).Exec(context.Background(), "true", t.TempDir(), BashExecOptions{TimeoutSeconds: ptr(2147484.0)})
		if err == nil || err.Error() != "Invalid timeout: maximum is 2147483.647 seconds" {
			t.Fatalf("LocalBashOperations oversized timeout: err = %v", err)
		}
	})
}

// TestLocalShellOperationsAbortedBeforeResolving: pi's
// createLocalShellOperations throws "aborted" for an already aborted signal
// before it resolves the shell.
func TestLocalShellOperationsAbortedBeforeResolving(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	missing := filepath.Join(t.TempDir(), "nope", "bash")
	_, err := LocalBashOperations(LocalBashOptions{ShellPath: missing}).Exec(ctx, "true", t.TempDir(), BashExecOptions{})
	if !errors.Is(err, ErrShellAborted) {
		t.Fatalf("err = %v, want ErrShellAborted", err)
	}
}

// recordingPowerShellOperations is createLocalPowerShellOperations with the
// shell pinned to shellPath and the process replaced by a recorder.
func recordingPowerShellOperations(shellPath string, calls *[]recordedExec) BashOperations {
	return powerShellOperations(
		func() (ShellConfig, error) {
			return ShellConfig{Shell: shellPath, Args: slices.Clone(powershellArgs)}, nil
		},
		func(ShellConfig) BashOperations { return *recordingShellOps(calls) },
	)
}

// TestLocalPowerShellOperationsPrefix ports createLocalPowerShellOperations:
// it prepends UTF8_OUTPUT_PREFIX to every command it runs.
func TestLocalPowerShellOperationsPrefix(t *testing.T) {
	var calls []recordedExec
	ops := recordingPowerShellOperations(`C:\Program Files\PowerShell\7\pwsh.exe`, &calls)
	if _, err := ops.Exec(context.Background(), "Write-Output hi", "/work", BashExecOptions{}); err != nil {
		t.Fatal(err)
	}
	if want := utf8OutputPrefix + "Write-Output hi"; calls[0].command != want {
		t.Fatalf("ran %q, want %q", calls[0].command, want)
	}
}

// TestLocalPowerShellOperationsWindowsPowerShellExitSuffix ports
// createLocalPowerShellOperations' Windows PowerShell 5.1 rule (upstream
// c9dd2c530, #8842): powershell.exe, matched by its win32 basename in any
// case, gets WINDOWS_POWERSHELL_EXIT_SUFFIX after the command; pwsh does not,
// and neither does a command ending in a line-continuation backtick. The
// exit codes this buys are pinned on Windows by
// TestLocalPowerShellOperationsExitCodes.
func TestLocalPowerShellOperationsWindowsPowerShellExitSuffix(t *testing.T) {
	const command = "git push 2>&1 | Select-Object -Last 3"
	cases := []struct {
		shell, command, want string
	}{
		{`C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`, command, utf8OutputPrefix + command + windowsPowerShellExitSuffix},
		{`C:\Windows\System32\WindowsPowerShell\v1.0\PowerShell.EXE`, command, utf8OutputPrefix + command + windowsPowerShellExitSuffix},
		{"powershell.exe", command, utf8OutputPrefix + command + windowsPowerShellExitSuffix},
		{`C:/tools/powershell.exe`, command, utf8OutputPrefix + command + windowsPowerShellExitSuffix},
		{`C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`, "Write-Output done `", utf8OutputPrefix + "Write-Output done `"},
		{`C:\Program Files\PowerShell\7\pwsh.exe`, command, utf8OutputPrefix + command},
		{`C:\shells\powershell.exe.bak`, command, utf8OutputPrefix + command},
	}
	for _, c := range cases {
		var calls []recordedExec
		if _, err := recordingPowerShellOperations(c.shell, &calls).Exec(context.Background(), c.command, "/work", BashExecOptions{}); err != nil {
			t.Fatal(err)
		}
		if calls[0].command != c.want {
			t.Errorf("%s: ran %q, want %q", c.shell, calls[0].command, c.want)
		}
	}
	// Byte-exact from powershell.ts at c9dd2c530.
	const want = "\n;\nif ($?) { exit 0 }\nif (($LASTEXITCODE -eq 0) -and (\"$($Error[0].FullyQualifiedErrorId)\" -like \"NativeCommandError*\")) { exit 0 }\nexit 1"
	if windowsPowerShellExitSuffix != want {
		t.Fatalf("suffix = %q, want pi's %q", windowsPowerShellExitSuffix, want)
	}
}

// TestWin32Basename pins node's path.win32.basename for the shells pi reads.
func TestWin32Basename(t *testing.T) {
	for in, want := range map[string]string{
		`C:\Windows\powershell.exe`: "powershell.exe",
		`C:/x/pwsh.exe`:             "pwsh.exe",
		`C:\x\powershell.exe\`:      "powershell.exe",
		`C:powershell.exe`:          "powershell.exe",
		"powershell.exe":            "powershell.exe",
		"":                          "",
	} {
		if got := win32Basename(in); got != want {
			t.Errorf("win32Basename(%q) = %q, want %q", in, got, want)
		}
	}
}
