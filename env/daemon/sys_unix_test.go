//go:build !windows

package daemon

import (
	"bufio"
	"io"
	"os"
	"os/exec"
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

// Ported from sys/unix.rs's test (upstream 3da8737d6).
func TestKillingASessionKillsItsJobsInGroupsOfTheirOwn(t *testing.T) {
	// A shell with job control, as a terminal's is: its background job gets a
	// process group of its own (bash: a POSIX sh may not do job control
	// without a terminal).
	bash := lookBash(t)
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
	process, err := spawnProcess(bash, []string{"-c", "set -m; sleep 30 </dev/null >/dev/null 2>&1 & echo $!; wait"}, t.TempDir(), object{}, true, []*os.File{null, write, null})
	write.Close()
	if err != nil {
		t.Fatal(err)
	}
	reaped := false
	t.Cleanup(func() {
		if !reaped {
			killTree(process.pid)
			_, _ = process.wait()
		}
	})
	if err := read.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(read).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	job, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil {
		t.Fatalf("pid: %q", line)
	}
	t.Cleanup(func() { _ = unix.Kill(job, unix.SIGKILL) })
	if group, _ := unix.Getpgid(job); group == process.pid {
		t.Fatal("the job has no group of its own")
	}
	killTree(process.pid)
	_, _ = process.wait()
	reaped = true
	assertGone(t, job, "the job ended with its session")
}

// Upstream 094ba3a69: a process found for a session is killed only if it is
// still of that session when the signal goes: through a pidfd on Linux
// (killMember), right before the signal elsewhere (killIfOfSession, also
// killMember's fallback).
func TestKillingASessionsMemberChecksItsSessionFirst(t *testing.T) {
	for name, kill := range map[string]func(member, sid int){"killMember": killMember, "killIfOfSession": killIfOfSession} {
		t.Run(name, func(t *testing.T) {
			null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer null.Close()
			// It leads a session of its own: its session id is its pid.
			process, err := spawnProcess("/bin/sh", []string{"-c", "exec sleep 30"}, t.TempDir(), object{}, true, []*os.File{null, null, null})
			if err != nil {
				t.Fatal(err)
			}
			reaped := false
			t.Cleanup(func() {
				if !reaped {
					process.kill()
					_, _ = process.wait()
				}
			})
			kill(process.pid, process.pid+1)
			time.Sleep(100 * time.Millisecond)
			var status unix.WaitStatus
			if pid, err := unix.Wait4(process.pid, &status, unix.WNOHANG, nil); err != nil || pid != 0 {
				t.Fatalf("a process of another session was killed: %d, %v", pid, err)
			}
			kill(process.pid, process.pid)
			code, err := process.wait()
			reaped = true
			if err != nil || code != 128+int64(unix.SIGKILL) {
				t.Fatalf("the member: exit %d, %v", code, err)
			}
		})
	}
}

// lookBash is bash's path; the test is skipped without it.
func lookBash(t *testing.T) string {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("needs bash")
	}
	return bash
}

// assertGone fails unless pid is gone within 5 s: a killed orphan is gone
// once init has reaped it.
func assertGone(t *testing.T, pid int, what string) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); unix.Kill(pid, 0) == nil && time.Now().Before(deadline); {
		time.Sleep(20 * time.Millisecond)
	}
	if unix.Kill(pid, 0) == nil {
		t.Fatalf("%s: %d still runs", what, pid)
	}
}
