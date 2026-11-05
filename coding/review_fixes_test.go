package coding

import (
	"context"
	"runtime"
	"strings"
	"testing"
	"time"
)

// An abort after the shell exited still kills the background processes
// holding the output pipe, with or without a timeout (pi's onAbort).
func TestBashAbortAfterShellExitKillsBackgroundWriters(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the group kill cannot reach background processes once the shell is gone")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := bashTool(t.TempDir(), nil).Execute(ctx, "call", map[string]any{
		"command": "(i=0; while [ $i -lt 150 ]; do echo tick; sleep 0.02; i=$((i+1)); done) & exit 0",
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "Command aborted") {
		t.Fatalf("err = %v, want Command aborted", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("the background writer outlived the abort: %v", elapsed)
	}
}

// A limit past the int range selects to the end, as pi's
// Math.min(startLine + limit, length) does, on both read paths.
func TestReadHugeLimitSelectsToTheEnd(t *testing.T) {
	data := []byte("one\ntwo\nthree")
	for _, ops := range []ReadOperations{bufferOps(data), streamOps(data, 4096)} {
		got := readWith(t, ops, readParams(2.0, 1e19))
		if got.Err != "" || !strings.Contains(got.Result, `"text":"two\nthree"`) {
			t.Errorf("limit 1e19: %+v", got)
		}
	}
}
