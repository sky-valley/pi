//go:build !windows

package daemon

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// runTerminalWithJob runs a bash with job control in a terminal, which puts a
// background sleep in a process group of its own and then runs then; once the
// job has started, end (if any) is called with the request's control. It
// returns the job's pid once the request has settled.
func runTerminalWithJob(t *testing.T, then string, end func(*control)) int {
	t.Helper()
	bash := lookBash(t)
	dir := t.TempDir()
	jobFile := filepath.Join(dir, "job")
	script := "set -m; sleep 30 </dev/null >/dev/null 2>&1 & echo $! >" + jobFile + ".tmp && mv " + jobFile + ".tmp " + jobFile + "; " + then
	argv, _ := json.Marshal([]string{bash, "-c", script})
	cwd, _ := json.Marshal(dir)
	request, fail := ptyRequestFromJSON(parseJSON([]byte(`{"argv":` + string(argv) + `,"cwd":` + string(cwd) + `,"cols":80,"rows":24}`)))
	if fail != nil {
		t.Fatal(fail)
	}
	out := newOutput()
	go out.run(bufio.NewWriter(io.Discard))
	defer out.close()
	ctl := newControl()
	settled := make(chan *failure, 1)
	go func() {
		_, fail := runPty(1, request, out, ctl, newGroups(), newTerminals())
		settled <- fail
	}()
	job := 0
	for deadline := time.Now().Add(10 * time.Second); job == 0; time.Sleep(10 * time.Millisecond) {
		if text, err := os.ReadFile(jobFile); err == nil {
			if job, err = strconv.Atoi(strings.TrimSpace(string(text))); err != nil {
				t.Fatalf("pid: %q", text)
			}
		} else if time.Now().After(deadline) {
			ctl.cancel(true)
			t.Fatal("the job did not start")
		}
	}
	t.Cleanup(func() { _ = unix.Kill(job, unix.SIGKILL) })
	if end != nil {
		end(ctl)
	}
	select {
	case <-settled:
	case <-time.After(10 * time.Second):
		t.Fatal("the terminal did not settle")
	}
	return job
}

// Upstream 3da8737d6: closing a terminal kills its background jobs, which a
// shell with job control puts in groups of their own.
func TestClosingATerminalKillsItsBackgroundJobs(t *testing.T) {
	job := runTerminalWithJob(t, "wait", func(ctl *control) { ctl.cancel(true) })
	assertGone(t, job, "the job ended with its terminal")
}
