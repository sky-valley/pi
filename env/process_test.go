package env

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Ported from upstream's packages/env/test/process.test.ts (9c9ce8547). Its
// programs are Node scripts, so they run the same on every system; here they
// are this test binary in processProgramArg's modes, for the same reason.

// processProgramArg makes the test binary a program for OpenProcess:
// `<test binary> pi-env-process-program <mode> [args]` (runProcessProgram).
const processProgramArg = "pi-env-process-program"

// program is the argv that runs this test binary in one of
// runProcessProgram's modes.
func program(t *testing.T, mode string, args ...string) []string {
	t.Helper()
	self, err := os.Executable()
	mustDo(t, err)
	return append([]string{self, processProgramArg, mode}, args...)
}

// runProcessProgram is the program of the tests' processes, by mode:
//
//   - echo: echoes each stdin chunk to stdout, and the rest of chunks
//     starting with "err:" to stderr; exits with code 7 at the end of stdin.
//   - hash: reports the length and SHA-256 of everything it read once stdin
//     ends.
//   - where: prints [its working directory, $PI_ENV_PROCESS_TEST] as JSON.
//   - large: writes 12 MiB of "x".
//   - exit <code>: exits with code.
//   - hold: holds stdin open without reading, for 30 s.
//   - wrapper: starts `sleep` with its stdio ignored, as npx and uvx start the
//     real server, prints its pid and waits 30 s.
//   - sleep: waits 30 s.
//   - spawn-descendant <file>: starts `descendant <file>` in a session of
//     its own, inheriting stdin, and exits after 300 ms.
//   - descendant <file>: after 1.5 s reads stdin until its end and writes how
//     many bytes it read to file; exits with 1 after 15 s.
func runProcessProgram(args []string) int {
	self, _ := os.Executable()
	switch args[0] {
	case "echo":
		buf := make([]byte, 64*1024)
		for {
			n, err := os.Stdin.Read(buf)
			if n > 0 {
				if chunk := buf[:n]; bytes.HasPrefix(chunk, []byte("err:")) {
					os.Stderr.Write(chunk[4:])
				} else {
					os.Stdout.Write(chunk)
				}
			}
			if err != nil {
				return 7
			}
		}
	case "hash":
		hash := sha256.New()
		length, _ := io.Copy(hash, os.Stdin)
		fmt.Printf("%d %s", length, hex.EncodeToString(hash.Sum(nil)))
	case "where":
		cwd, _ := os.Getwd()
		data, _ := json.Marshal([]string{cwd, os.Getenv("PI_ENV_PROCESS_TEST")})
		os.Stdout.Write(data)
	case "large":
		os.Stdout.Write(bytes.Repeat([]byte("x"), 12*1024*1024))
	case "exit":
		code, _ := strconv.Atoi(args[1])
		return code
	case "hold", "sleep":
		time.Sleep(30 * time.Second)
	case "wrapper":
		child := exec.Command(self, processProgramArg, "sleep")
		if err := child.Start(); err != nil {
			return 1
		}
		fmt.Printf("%d\n", child.Process.Pid)
		time.Sleep(30 * time.Second)
	case "spawn-descendant":
		descendant := exec.Command(self, processProgramArg, "descendant", args[1])
		descendant.Stdin = os.Stdin
		detach(descendant)
		if err := descendant.Start(); err != nil {
			return 1
		}
		time.Sleep(300 * time.Millisecond)
	case "descendant":
		time.AfterFunc(15*time.Second, func() { os.Exit(1) })
		time.Sleep(1500 * time.Millisecond)
		length, _ := io.Copy(io.Discard, os.Stdin)
		os.WriteFile(args[1], []byte(strconv.FormatInt(length, 10)), 0o644)
	default:
		return 2
	}
	return 0
}

// waitProcess waits for a program's end, as long as upstream's test timeout
// allows.
func waitProcess(t *testing.T, process *RemoteProcess) (int, error) {
	t.Helper()
	select {
	case <-process.Done():
		return process.Wait()
	case <-time.After(30 * time.Second):
		process.Close()
		t.Fatal("the program did not end within 30 s")
		return 0, nil
	}
}

// expectExit fails unless the program exits with code.
func expectExit(t *testing.T, process *RemoteProcess, code int) {
	t.Helper()
	if got, err := waitProcess(t, process); err != nil || got != code {
		t.Fatalf("exit %d, %v; want exit %d", got, err, code)
	}
}

// waitUntilGone reports whether pid is gone within 5 seconds.
func waitUntilGone(pid int) bool {
	for deadline := time.Now().Add(5 * time.Second); processAlive(pid) && time.Now().Before(deadline); {
		time.Sleep(50 * time.Millisecond)
	}
	return !processAlive(pid)
}

func processDir(t *testing.T) string { return tempDir(t, "pi-env-process-") }

func TestProcessPipesStdinToTheProgramAndItsStdoutAndStderrBackSeparately(t *testing.T) {
	stdout, stderr := &collector{}, &collector{}
	process, err := OpenProcess(background(), newConnection(t), ProcessOptions{
		Cwd: processDir(t), Argv: program(t, "echo"), OnStdout: stdout.onData, OnStderr: stderr.onData,
	})
	mustDo(t, err)
	if process.PID <= 0 {
		t.Fatalf("pid %d", process.PID)
	}
	process.Write([]byte(`{"jsonrpc":"2.0","id":1}` + "\n"))
	stdout.waitFor(t, `{"jsonrpc":"2.0","id":1}`+"\n")
	process.Write([]byte("err:diagnostic\n"))
	stderr.waitFor(t, "diagnostic\n")
	if got := string(stdout.bytes()); got != `{"jsonrpc":"2.0","id":1}`+"\n" {
		t.Fatalf("stdout %q", got)
	}
	process.CloseStdin()
	expectExit(t, process, 7)
}

func TestProcessPassesBytesThroughUnchanged(t *testing.T) {
	stdout := &collector{}
	process, err := OpenProcess(background(), newConnection(t), ProcessOptions{
		Cwd: processDir(t), Argv: program(t, "echo"), OnStdout: stdout.onData,
	})
	mustDo(t, err)
	input := []byte{0x61, 0x0a, 0xff, 0x0d, 0x0a, 0x62}
	process.Write(input)
	process.CloseStdin()
	expectExit(t, process, 7)
	if got := stdout.bytes(); !bytes.Equal(got, input) {
		t.Fatalf("stdout %x, want %x: no echo, no newline translation, no decoding", got, input)
	}
}

func TestProcessPassesTheWorkingDirectoryAndEnvironment(t *testing.T) {
	stdout := &collector{}
	process, err := OpenProcess(background(), newConnection(t), ProcessOptions{
		Cwd: processDir(t), Argv: program(t, "where"),
		Env: map[string]string{"PI_ENV_PROCESS_TEST": "set"}, OnStdout: stdout.onData,
	})
	mustDo(t, err)
	expectExit(t, process, 0)
	var got [2]string
	mustDo(t, json.Unmarshal(stdout.bytes(), &got))
	if !strings.Contains(strings.ToLower(got[0]), "pi-env-process-") || got[1] != "set" {
		t.Fatalf("working directory and variable %q", got)
	}
}

func TestProcessKeepsInputInOrderAcrossManyWritesAndWritesLargerThanOneChunk(t *testing.T) {
	stdout := &collector{}
	process, err := OpenProcess(background(), newConnection(t), ProcessOptions{
		Cwd: processDir(t), Argv: program(t, "hash"), OnStdout: stdout.onData,
	})
	mustDo(t, err)
	expected := sha256.New()
	length := 0
	for index := range 4000 {
		piece := []byte(fmt.Sprintf("%09d\n", index))
		expected.Write(piece)
		length += len(piece)
		process.Write(piece)
	}
	big := make([]byte, 2_500_003)
	for index := range big {
		big[index] = byte(index * 7919 % 251)
	}
	expected.Write(big)
	length += len(big)
	process.Write(big)
	process.CloseStdin()
	expectExit(t, process, 0)
	if got, want := string(stdout.bytes()), fmt.Sprintf("%d %x", length, expected.Sum(nil)); got != want {
		t.Fatalf("the program read %q, want %q", got, want)
	}
}

func TestProcessDeliversLargeOutputCompletely(t *testing.T) {
	stdout := &collector{}
	process, err := OpenProcess(background(), newConnection(t), ProcessOptions{
		Cwd: processDir(t), Argv: program(t, "large"), OnStdout: stdout.onData,
	})
	mustDo(t, err)
	expectExit(t, process, 0)
	output := stdout.bytes()
	if len(output) != 12*1024*1024 || bytes.Count(output, []byte("x")) != len(output) {
		t.Fatalf("%d bytes of output, want 12 MiB of x", len(output))
	}
}

func TestProcessRefusesInputAfterStdinWasClosedWhileTheProgramKeepsRunning(t *testing.T) {
	connection := newConnection(t)
	started := make(chan struct{})
	var target uint32
	fields := object{}.with("cwd", processDir(t)).with("argv", program(t, "hold"))
	go func() {
		_, _ = connection.Request(background(), "process", fields, &RequestOptions{
			OnStart: func(id uint32, _ int) { target = id },
			OnEvent: func(event json.RawMessage, _ []byte) {
				if strings.Contains(string(event), `"started"`) {
					close(started)
				}
			},
		})
	}()
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("the program did not start")
	}
	_, err := connection.Request(background(), "processWrite", object{}.with("target", target), &RequestOptions{Payload: []byte{1}})
	mustDo(t, err)
	_, err = connection.Request(background(), "processCloseStdin", object{}.with("target", target), nil)
	mustDo(t, err)
	// The writer closes stdin after the input queued before it; from then on
	// input is refused.
	var refused error
	for tries := 0; refused == nil && tries < 100; tries++ {
		if _, refused = connection.Request(background(), "processWrite", object{}.with("target", target), &RequestOptions{Payload: []byte{2}}); refused == nil {
			time.Sleep(10 * time.Millisecond)
		}
	}
	if errorCode(refused) != "EBADF" {
		t.Fatalf("input after processCloseStdin: %v", refused)
	}
	connection.Close()
}

func TestProcessRefusesInputForProgramsThatDoNotRun(t *testing.T) {
	connection := newConnection(t)
	for _, op := range []string{"processWrite", "processCloseStdin"} {
		if _, err := connection.Request(background(), op, object{}.with("target", 999), nil); errorCode(err) != "EBADF" {
			t.Fatalf("%s to no program: %v", op, err)
		}
	}
}

func TestProcessReportsTheExitCodeOfAProgramThatExitsOnItsOwn(t *testing.T) {
	process, err := OpenProcess(background(), newConnection(t), ProcessOptions{
		Cwd: processDir(t), Argv: program(t, "exit", "3"), OnStdout: func([]byte) {},
	})
	mustDo(t, err)
	expectExit(t, process, 3)
}

// A nil OnStdout drops stdout instead of panicking on the connection's
// goroutine (review of 08c8ef5; pi types onStdout as required, so this is a
// Go-only guard, like OnStderr's and RemoteTerminal's OnData).
func TestProcessWithoutOnStdoutDropsStdout(t *testing.T) {
	process, err := OpenProcess(background(), newConnection(t), ProcessOptions{
		Cwd: processDir(t), Argv: program(t, "large"),
	})
	mustDo(t, err)
	expectExit(t, process, 0)
}

func TestProcessKillsTheProgramAndWhatItStartedOnClose(t *testing.T) {
	stdout := &collector{}
	process, err := OpenProcess(background(), newConnection(t), ProcessOptions{
		Cwd: processDir(t), Argv: program(t, "wrapper"), OnStdout: stdout.onData,
	})
	mustDo(t, err)
	stdout.waitFor(t, "\n")
	child, err := strconv.Atoi(strings.TrimSpace(string(stdout.bytes())))
	mustDo(t, err)
	if !processAlive(child) {
		t.Fatalf("the wrapper's child %d is not running", child)
	}
	process.Close()
	if _, err := waitProcess(t, process); errorCode(err) != "aborted" {
		t.Fatalf("closed program: %v", err)
	}
	if !waitUntilGone(process.PID) {
		t.Fatalf("the program %d survived", process.PID)
	}
	if !waitUntilGone(child) {
		t.Fatalf("the program's child %d survived", child)
	}
}

// On Windows the Rust daemon's pipe ends are overlapped; a writer blocked in
// the standard library's write could not be cancelled, so it kept stdin open,
// still writing, for as long as a descendant held it.
func TestProcessStopsWritingToStdinADescendantStillHoldsOnceTheProgramHasExited(t *testing.T) {
	cwd := processDir(t)
	count := filepath.Join(cwd, "count.txt")
	process, err := OpenProcess(background(), newConnection(t), ProcessOptions{
		Cwd: cwd, Argv: program(t, "spawn-descendant", count), OnStdout: func([]byte) {},
	})
	mustDo(t, err)
	// Far more than the pipe holds: the writer blocks until it is stopped.
	process.Write(make([]byte, 4*1024*1024))
	expectExit(t, process, 0)
	var data []byte
	for deadline := time.Now().Add(8 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if data, err = os.ReadFile(count); err == nil {
			break
		}
	}
	if read, err := strconv.Atoi(string(data)); err != nil || read >= 4*1024*1024 {
		t.Fatalf("the descendant read %q bytes of stdin (%v); the writer should have stopped when the program exited", data, err)
	}
}

func TestProcessKillsTheProgramWhenTheConnectionCloses(t *testing.T) {
	connection := newConnection(t)
	process, err := OpenProcess(background(), connection, ProcessOptions{
		Cwd: processDir(t), Argv: program(t, "sleep"), OnStdout: func([]byte) {},
	})
	mustDo(t, err)
	connection.Close()
	if _, err := waitProcess(t, process); err == nil {
		t.Fatal("the program's end after the connection closed is not an error")
	} else if _, ok := err.(*RemoteError); !ok {
		t.Fatalf("the program's end after the connection closed: %#v", err)
	}
	if !waitUntilGone(process.PID) {
		t.Fatalf("the program %d survived", process.PID)
	}
}

func TestProcessFailuresToStart(t *testing.T) {
	start := func(t *testing.T, options ProcessOptions) error {
		if options.Cwd == "" {
			options.Cwd = processDir(t)
		} else {
			options.Cwd = filepath.Join(processDir(t), options.Cwd)
		}
		if options.Argv == nil {
			options.Argv = program(t, "exit", "0")
		}
		options.OnStdout = func([]byte) {}
		_, err := OpenProcess(background(), newConnection(t), options)
		return err
	}
	for _, c := range []struct {
		name    string
		options ProcessOptions
		code    string
	}{
		{"in a missing directory", ProcessOptions{Cwd: "missing"}, "spawn_error"},
		{"for a program that does not exist", ProcessOptions{Argv: []string{"pi-env-no-such-program"}}, "spawn_error"},
		{"for an empty argv", ProcessOptions{Argv: []string{}}, "EINVAL"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if code := errorCode(start(t, c.options)); code != c.code {
				t.Fatalf("code %q, want %q", code, c.code)
			}
		})
	}
}

func TestProcessReportsDaemonsReleasedBeforeTheOperationAsNotSupported(t *testing.T) {
	old := NewConnection(ConnectionOptions{Command: []string{os.Args[0], oldDaemonArg}})
	t.Cleanup(old.Close)
	_, err := OpenProcess(background(), old, ProcessOptions{Cwd: processDir(t), Argv: program(t, "exit", "0"), OnStdout: func([]byte) {}})
	if code := errorCode(err); code != "not_supported" {
		t.Fatalf("OpenProcess on an old daemon: %q", code)
	}
}

// TestProcessSendsUpstreamsFrames runs testdata/wire/process/capture.mts's
// scenario: the frames must be those upstream's openProcess sends, byte for
// byte (see wire_test.go).
func TestProcessSendsUpstreamsFrames(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("records the daemon's input with sh and tee")
	}
	data, err := os.ReadFile("testdata/wire/process/frames.json")
	mustDo(t, err)
	var golden struct {
		Frames []wireFrame `json:"frames"`
	}
	mustDo(t, json.Unmarshal(data, &golden))
	root, err := filepath.EvalSymlinks(tempDir(t, "pi-env-wire-"))
	mustDo(t, err)
	record := filepath.Join(tempDir(t, "pi-env-wire-record-"), "record.bin")
	connection := NewConnection(ConnectionOptions{Command: []string{"sh", "-c", `r=$0 d=$1; shift; tee "$r" | exec "$d" "$@"`, record, testDaemon}})
	defer connection.Close()
	ctx := background()
	info, err := connection.Info(ctx)
	mustDo(t, err)
	piped, err := OpenProcess(ctx, connection, ProcessOptions{
		Cwd: root, Argv: []string{"sh", "-c", "cat > /dev/null; exit 4"}, Env: map[string]string{"A": "1"},
		InheritEnv: new(false), OnStdout: func([]byte) {},
	})
	mustDo(t, err)
	piped.Write([]byte("hi"))
	big := make([]byte, 1536*1024)
	for index := range big {
		big[index] = byte(index % 251)
	}
	piped.Write(big)
	piped.Write([]byte{})
	piped.CloseStdin()
	expectExit(t, piped, 4)
	closed, err := OpenProcess(ctx, connection, ProcessOptions{Cwd: root, Argv: []string{"sleep", "5"}, OnStdout: func([]byte) {}})
	mustDo(t, err)
	closed.Close()
	_, _ = waitProcess(t, closed)
	connection.Close()

	stream, err := os.ReadFile(record)
	mustDo(t, err)
	frames := readWireFrames(stream, root, info.Tmpdir)
	for i := 0; i < max(len(frames), len(golden.Frames)); i++ {
		switch {
		case i >= len(frames):
			t.Fatalf("frame %d missing: upstream sends %+v", i, golden.Frames[i])
		case i >= len(golden.Frames):
			t.Fatalf("frame %d is extra: %+v", i, frames[i])
		case frames[i] != golden.Frames[i]:
			t.Fatalf("frame %d:\n got %+v\nwant %+v", i, frames[i], golden.Frames[i])
		}
	}
}
