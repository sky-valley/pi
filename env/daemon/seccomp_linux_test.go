package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/sky-valley/pi/env/internal/frame"
	"golang.org/x/sys/unix"
)

// seccompExecVariable names a daemon the test binary, started with it set,
// runs under seccompPidfd (see TestMain).
const seccompExecVariable = "PI_ENV_TEST_SECCOMP_EXEC"

// execUnderSeccomp runs the daemon at path the way Android before 12 runs an
// app: the pidfd system calls, missing from its seccomp allowlist, raise
// SIGSYS (SECCOMP_RET_TRAP) instead of running.
func execUnderSeccomp(path string) {
	runtime.LockOSThread()
	trap := []uint32{unix.SYS_PIDFD_OPEN, unix.SYS_PIDFD_SEND_SIGNAL, unix.SYS_PIDFD_GETFD, unix.SYS_CLONE3}
	// BPF: load the system call number; trap the listed ones; allow the rest.
	filter := []unix.SockFilter{{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 0}}
	for i, nr := range trap {
		filter = append(filter, unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, K: nr, Jt: uint8(len(trap) - i)})
	}
	filter = append(filter,
		unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ALLOW},
		unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_TRAP})
	program := unix.SockFprog{Len: uint16(len(filter)), Filter: &filter[0]}
	fail := func(step string, err error) {
		os.Stderr.WriteString("seccomp " + step + ": " + err.Error() + "\n")
		os.Exit(3)
	}
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		fail("no_new_privs", err)
	}
	if _, _, errno := unix.Syscall(unix.SYS_SECCOMP, unix.SECCOMP_SET_MODE_FILTER, unix.SECCOMP_FILTER_FLAG_TSYNC, uintptr(unsafe.Pointer(&program))); errno != 0 {
		fail("filter", errno)
	}
	fail("exec", syscall.Exec(path, []string{path, "serve", "--token", "0123abcd"}, os.Environ()))
}

// The android/amd64 daemon is a linux/amd64 build with the pienv_android tag,
// so Go's GOOS=android guard (ignoring SIGSYS while it probes pidfd support,
// golang/go#69065) is not in it. Android before 12 kills a process that calls
// pidfd_open; the daemon must start commands without the probe. This builds
// the tagged daemon for this machine and runs commands under such a filter.
func TestAndroidBuildRunsCommandsWithoutPidfd(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "pi-env-android")
	build := exec.Command("go", append(append([]string{"build", "-tags", "pienv_android"}, buildFlags...), "-o", binary, "github.com/sky-valley/pi/env/cmd/pi-env")...)
	build.Stdout, build.Stderr = os.Stderr, os.Stderr
	if err := build.Run(); err != nil {
		t.Fatal("building the daemon with -tags pienv_android:", err)
	}
	wrapper := exec.Command(os.Args[0])
	wrapper.Env = append(os.Environ(), seccompExecVariable+"="+binary)
	s := startDaemonWith(t, wrapper)
	cwd := t.TempDir()
	for _, request := range []string{
		`{"op":"exec","argv":["echo","hi"],"cwd":"` + cwd + `","env":{}}`,
		`{"op":"exec","command":"echo hi","cwd":"` + cwd + `","env":{}}`,
		`{"op":"pty","argv":["echo","hi"],"cwd":"` + cwd + `","env":{},"cols":80,"rows":24}`,
	} {
		id := s.start(request, nil)
		deadline := time.After(20 * time.Second)
		for {
			s.mu.Lock()
			frames := s.frames[id]
			s.mu.Unlock()
			if finished(frames) {
				if last := frames[len(frames)-1]; last.Kind != frame.Result || string(last.JSON) != `{"exitCode":0}` {
					t.Fatalf("%s: %d %s", request, last.Kind, last.JSON)
				}
				break
			}
			select {
			case <-s.ended:
				s.cmd.Wait()
				t.Fatalf("%s: the daemon ended (%v) without a reply", request, s.cmd.ProcessState)
			case <-deadline:
				t.Fatalf("%s: no reply", request)
			case <-time.After(10 * time.Millisecond):
			}
		}
	}
}
