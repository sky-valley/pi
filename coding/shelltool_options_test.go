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

// TestLocalPowerShellOperationsPrefix ports createLocalPowerShellOperations:
// it prepends UTF8_OUTPUT_PREFIX to every command it runs.
func TestLocalPowerShellOperationsPrefix(t *testing.T) {
	var calls []recordedExec
	ops := withUTF8OutputPrefix(*recordingShellOps(&calls))
	if _, err := ops.Exec(context.Background(), "Write-Output hi", "/work", BashExecOptions{}); err != nil {
		t.Fatal(err)
	}
	if want := utf8OutputPrefix + "Write-Output hi"; calls[0].command != want {
		t.Fatalf("ran %q, want %q", calls[0].command, want)
	}
}
