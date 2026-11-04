//go:build !windows

package daemon

import (
	"io"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// Ported from sys/unix.rs's test (upstream 032656b6c).
func TestEndingAGroupKillsWhatTheProgramLeftRunning(t *testing.T) {
	// A wrapper that starts a background process in its group, prints its pid
	// and exits, as npx can.
	null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer null.Close()
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer read.Close()
	process, err := spawnProcess("/bin/sh", []string{"-c", "sleep 30 </dev/null >/dev/null 2>&1 & echo $!"}, t.TempDir(), object{}, true, []*os.File{null, write, null})
	write.Close()
	if err != nil {
		t.Fatal(err)
	}
	// A test that stops early leaves nothing running or unreaped.
	reaped := false
	t.Cleanup(func() {
		if !reaped {
			killTree(process.pid)
			_, _ = process.wait()
		}
	})
	group, err := newProgramGroup(process)
	if err != nil {
		t.Fatal(err)
	}
	if err := read.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	stdout, err := io.ReadAll(read)
	if err != nil {
		t.Fatal(err)
	}
	sleeper, err := strconv.Atoi(strings.TrimSpace(string(stdout)))
	if err != nil {
		t.Fatalf("pid: %q", stdout)
	}
	if err := group.waitForExit(); err != nil {
		t.Fatal(err)
	}
	group.end()
	code, err := process.wait()
	reaped = true
	if err != nil || code != 0 {
		t.Fatalf("the wrapper: exit %d, %v", code, err)
	}
	// The killed sleeper is an orphan; it is gone once init has reaped it.
	for deadline := time.Now().Add(5 * time.Second); unix.Kill(sleeper, 0) == nil && time.Now().Before(deadline); {
		time.Sleep(20 * time.Millisecond)
	}
	if unix.Kill(sleeper, 0) == nil {
		t.Fatalf("the sleeper %d survived the end of its group", sleeper)
	}
}
