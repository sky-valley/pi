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
		_, _ = bashTool(dir).Execute(ctx, "id",
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
