package coding

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/sky-valley/pi/agent"
)

// TestBashKillsProcessTree verifies that cancelling a bash command kills
// backgrounded grandchildren too — not just the direct child. The command
// spawns a background subshell that writes a marker after a delay; if only the
// direct child were killed, the marker would still appear.
func TestBashKillsProcessTree(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses POSIX backgrounding")
	}
	dir := t.TempDir()
	marker := filepath.Join(dir, "marker")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		// Background a subshell that writes the marker after 1s, then the parent
		// blocks for 10s. Cancelling must kill the whole group before 1s elapses.
		_, _ = bashTool(dir, nil).Execute(ctx, "id",
			map[string]any{"command": "(sleep 1; echo alive > " + marker + ") & sleep 10"},
			func(agent.AgentToolResult) {})
		close(done)
	}()

	time.Sleep(300 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("bash did not return promptly after cancel")
	}

	// Give the (should-be-killed) background subshell well past its 1s delay.
	time.Sleep(1500 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("backgrounded grandchild survived cancellation (process tree not killed)")
	}
}

// TestTaskkillPath locks pi's absolute resolution of taskkill.exe (upstream
// 7af2d27dc, pi#6596; 61060ddf8, pi#9490): utils/shell.ts builds
// `join(process.env.SystemRoot || "C:\\Windows", "System32", "taskkill.exe")`
// instead of letting the OS search PATH. Expectations are assembled by hand
// rather than with filepath.Join so the test pins the components and their
// order instead of restating the implementation.
func TestTaskkillPath(t *testing.T) {
	sep := string(filepath.Separator)
	suffix := sep + "System32" + sep + "taskkill.exe"

	tests := []struct {
		name       string
		systemRoot string
		unset      bool
		want       string
	}{
		{
			name:       "SystemRoot set",
			systemRoot: "C:" + sep + "CustomWindows",
			want:       "C:" + sep + "CustomWindows" + suffix,
		},
		{
			name:  "unset SystemRoot falls back to C:\\Windows",
			unset: true,
			want:  `C:\Windows` + suffix,
		},
		{
			// pi's `||` takes the fallback for an empty SystemRoot too, so the
			// path stays absolute (upstream 61060ddf8).
			name:       "empty SystemRoot falls back to C:\\Windows",
			systemRoot: "",
			want:       `C:\Windows` + suffix,
		},
		{
			name:       "trailing separator collapses",
			systemRoot: "C:" + sep + "Windows" + sep,
			want:       "C:" + sep + "Windows" + suffix,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// t.Setenv first, so the process's real SystemRoot is restored on
			// cleanup even in the unset case.
			t.Setenv("SystemRoot", tt.systemRoot)
			if tt.unset {
				if err := os.Unsetenv("SystemRoot"); err != nil {
					t.Fatalf("unset SystemRoot: %v", err)
				}
			}
			if got := taskkillPath(); got != tt.want {
				t.Errorf("taskkillPath() = %q, want %q", got, tt.want)
			}
		})
	}
}
