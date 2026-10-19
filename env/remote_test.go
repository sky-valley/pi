package env

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sky-valley/pi/coding"
)

// Ported from upstream's packages/env/test/remote.test.ts (d2e7d8b7c).

func TestTransfersOnlyAboutAWindowOfOutputWithExactCounts(t *testing.T) {
	env, _ := environment(t)
	const total = 5_000_000
	delivered, skipped := 0, 0
	tail := ""
	result, err := env.RunProgram(background(), []string{"sh", "-c", "yes 0123456789abcdef | head -c " + strconv.Itoa(total)}, &RunOptions{
		Window: &OutputWindow{MaxBytes: 1000, MaxLines: 20, MinIntervalMS: 20, BytesPerSecond: 1_000_000},
		OnOutput: func(text string, info OutputInfo) error {
			delivered += len(text)
			if info.Skipped != nil {
				skipped += int(info.Skipped.Bytes)
				tail = ""
			}
			tail += text
			return nil
		},
	})
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("exec: %+v, %v", result, err)
	}
	if delivered+skipped != total {
		t.Fatalf("delivered %d + skipped %d != %d", delivered, skipped, total)
	}
	if skipped == 0 || delivered >= total/10 {
		t.Fatalf("delivered %d, skipped %d: the window did not limit the transfer", delivered, skipped)
	}
	// After the last skip comes everything to the end, more than the window.
	full := strings.Repeat("0123456789abcdef\n", (total+16)/17)[:total]
	if !strings.HasSuffix(full, tail) {
		t.Fatal("the tail is not the end of the output")
	}
	if !(len(tail) > 1000 || strings.Count(tail, "\n") > 20) {
		t.Fatalf("the tail (%d bytes) is not more than the window", len(tail))
	}
}

func TestAbortsCommandsCancelledRightAfterTheyAreSent(t *testing.T) {
	env, _ := environment(t)
	started := time.Now()
	for attempt := 0; attempt < 20; attempt++ {
		ctx, cancel := context.WithCancel(background())
		if attempt%2 == 0 {
			cancel()
		} else {
			time.AfterFunc(time.Duration(attempt%5)*time.Millisecond, cancel)
		}
		_, err := env.RunProgram(ctx, []string{"sleep", "5"}, nil)
		cancel()
		if code := errorCode(err); code != "aborted" {
			t.Fatalf("attempt %d: %q, want aborted", attempt, code)
		}
	}
	if elapsed := time.Since(started); elapsed >= 10*time.Second {
		t.Fatalf("20 aborts took %v", elapsed)
	}
}

func TestReportsTheRemoteSystemInNodesNames(t *testing.T) {
	_, connection := environment(t)
	info, err := connection.Info(background())
	mustDo(t, err)
	platform, arch := NodePlatform(info)
	// process.platform and process.arch of a Node process on this machine.
	wantPlatform, wantArch := runtime.GOOS, runtime.GOARCH
	if wantPlatform == "windows" {
		wantPlatform = "win32"
	}
	if wantArch == "amd64" {
		wantArch = "x64"
	}
	if platform != wantPlatform || arch != wantArch {
		t.Fatalf("NodePlatform = %s %s, want %s %s", platform, arch, wantPlatform, wantArch)
	}
	for _, c := range []struct{ os, arch, platform, nodeArch string }{
		{"windows", "aarch64", "win32", "arm64"},
		{"macos", "x86_64", "darwin", "x64"},
	} {
		if p, a := NodePlatform(RemoteInfo{OS: c.os, Arch: c.arch}); p != c.platform || a != c.nodeArch {
			t.Errorf("NodePlatform(%s, %s) = %s %s", c.os, c.arch, p, a)
		}
	}
}

func TestReportsListFilesAsNotSupportedByOlderDaemons(t *testing.T) {
	env, connection := environment(t)
	info, err := connection.Info(background())
	mustDo(t, err)
	if !info.hasFeature("listFiles") {
		t.Fatal("the daemon does not report listFiles")
	}
	old := NewConnection(ConnectionOptions{Command: []string{os.Args[0], oldDaemonArg}})
	t.Cleanup(old.Close)
	oldEnv := NewRemoteExecutionEnv(RemoteExecutionEnvOptions{Connection: old, ID: "pi-env:old", Cwd: env.Cwd()})
	_, err = oldEnv.ListFiles(background(), ".", nil)
	if code := errorCode(err); code != "not_supported" {
		t.Fatalf("ListFiles on an old daemon: %q", code)
	}
}

func TestCancelsAListingInTheDaemon(t *testing.T) {
	env, connection := environment(t)
	for dir := 0; dir < 100; dir++ {
		path := filepath.Join(env.Cwd(), "d"+strconv.Itoa(dir))
		mustDo(t, os.Mkdir(path, 0o755))
		for file := 0; file < 100; file++ {
			mustDo(t, os.WriteFile(filepath.Join(path, "f"+strconv.Itoa(file)), nil, 0o644))
		}
	}
	// The cancel follows the request at once, bypassing the client's own
	// abort check, so the daemon stops the walk.
	_, err := connection.Request(abortedContext(), "listFiles", object{}.with("path", env.Cwd()), nil)
	if code := errorCode(err); code != "aborted" {
		t.Fatalf("cancelled listing: %q", code)
	}
	listed, err := env.ListFiles(background(), ".", nil)
	mustDo(t, err)
	if len(listed.Entries) != 10_100 || listed.Truncated {
		t.Fatalf("listing: %d entries, truncated %v", len(listed.Entries), listed.Truncated)
	}
	bounded, err := env.ListFiles(background(), ".", &ListFilesOptions{MaxEntries: new(int64(5000))})
	mustDo(t, err)
	if len(bounded.Entries) != 5000 || !bounded.Truncated {
		t.Fatalf("bounded listing: %d entries, truncated %v", len(bounded.Entries), bounded.Truncated)
	}
}

func TestTimesOutACommandThatProducesNoOutput(t *testing.T) {
	env, _ := environment(t)
	started := time.Now()
	_, err := env.RunProgram(background(), []string{"sleep", "5"}, &RunOptions{Timeout: 0.3})
	if code := errorCode(err); code != "timeout" {
		t.Fatalf("%q, want timeout", code)
	}
	if elapsed := time.Since(started); elapsed >= 3*time.Second {
		t.Fatalf("took %v", elapsed)
	}
}

func TestFailsRequestsOnHandlesOfALostConnection(t *testing.T) {
	env, connection := environment(t)
	mustDo(t, env.WriteFile(background(), "a.txt", []byte("AAA")))
	mustDo(t, env.WriteFile(background(), "b.txt", []byte("BBB")))
	a, err := env.OpenBinaryReader(background(), "a.txt", nil)
	mustDo(t, err)
	info, err := connection.Info(background())
	mustDo(t, err)
	pid := info.PID
	killPID(t, pid)
	time.Sleep(300 * time.Millisecond)
	// The new daemon numbers its handles from 1 again.
	b, err := env.OpenBinaryReader(background(), "b.txt", nil)
	mustDo(t, err)
	if info, err = connection.Info(background()); err != nil || info.PID == pid {
		t.Fatal("the daemon was not started again")
	}
	if _, err := a.Read(background(), 0, 3); errorCode(err) != "unknown" {
		t.Fatalf("read on a stale handle: %v", err)
	}
	if data, err := b.Read(background(), 0, 3); err != nil || string(data) != "BBB" {
		t.Fatalf("read %q, %v", data, err)
	}
	a.Close()
	b.Close()
}

func TestComputesTheCommandAtEachStartAndRetriesAFailedStart(t *testing.T) {
	cwd := tempDir(t, "pi-env-remote-")
	starts := 0
	connection := NewConnection(ConnectionOptions{CommandFunc: func(context.Context) ([]string, error) {
		starts++
		if starts == 1 {
			return nil, errors.New("ssh gpu-box failed: Network is unreachable")
		}
		return []string{testDaemon}, nil
	}})
	t.Cleanup(connection.Close)
	env := NewRemoteExecutionEnv(RemoteExecutionEnvOptions{Connection: connection, ID: "pi-env:test", Cwd: cwd})
	_, err := env.ReadTextFile(background(), "missing.txt")
	var failure *FileError
	if !errors.As(err, &failure) || failure.Code != "unknown" || failure.Message != "ssh gpu-box failed: Network is unreachable" {
		t.Fatalf("offline read: %#v", err)
	}
	result, err := env.RunProgram(background(), []string{"sh", "-c", "exit 0"}, nil)
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("exec after the failed start: %+v, %v", result, err)
	}
	if starts != 2 {
		t.Fatalf("%d starts", starts)
	}
}

func TestWritesAndReadsFilesLargerThanOneTransferChunkInOrder(t *testing.T) {
	env, _ := environment(t)
	content := make([]byte, 3_500_017)
	for i := range content {
		content[i] = byte((i * 7919) % 251)
	}
	mustDo(t, env.WriteFile(background(), "big.bin", content))
	mustDo(t, env.AppendFile(background(), "big.bin", content))
	read, err := env.ReadBinaryFile(background(), "big.bin")
	mustDo(t, err)
	if len(read) != 2*len(content) || !bytes.Equal(read[:len(content)], content) || !bytes.Equal(read[len(content):], content) {
		t.Fatalf("read %d bytes, not the content twice", len(read))
	}
	reader, err := env.OpenBinaryReader(background(), "big.bin", nil)
	mustDo(t, err)
	part, err := reader.Read(background(), 1_000_003, 2_000_000)
	mustDo(t, err)
	reader.Close()
	if !bytes.Equal(part, read[1_000_003:3_000_003]) {
		t.Fatal("the range differs")
	}
}

// TestListsDirectoriesInNodesOrder: Node's readdir fails on an entry it
// cannot lstat, and libuv sorts names by their bytes on POSIX (a name that is
// not UTF-8 by its raw bytes) but keeps the file system's order on Windows; a
// directory reader skips entries removed meanwhile and fails on others. The
// entries come from a daemon that lists them unsorted (runListingDaemon).
func TestListsDirectoriesInNodesOrder(t *testing.T) {
	names := func(infos []coding.FileInfo) []string {
		var out []string
		for _, info := range infos {
			out = append(out, info.Name)
		}
		return out
	}
	for _, c := range []struct {
		system, cwd, first string
		want               []string
	}{
		{"linux", "/", "/sorted/B", []string{"B", "a", "b", "\ufffd"}},
		{"windows", `C:\`, `C:\sorted\b`, []string{"b", "a", "\ufffd", "B"}},
	} {
		connection := NewConnection(ConnectionOptions{Command: []string{os.Args[0], listingDaemonArg, c.system}})
		t.Cleanup(connection.Close)
		env := NewRemoteExecutionEnv(RemoteExecutionEnvOptions{Connection: connection, ID: "pi-env:listing", Cwd: c.cwd})
		infos, err := env.ListDir(background(), "sorted")
		mustDo(t, err)
		if got := names(infos); !reflect.DeepEqual(got, c.want) || infos[0].Path != c.first {
			t.Errorf("%s: ListDir %v (%s), want %v", c.system, got, infos[0].Path, c.want)
		}
		if _, err := env.ListDir(background(), "vanishing"); errorCode(err) != "not_found" {
			t.Errorf("%s: ListDir with a vanished entry: %v", c.system, err)
		}
		reader, err := env.OpenDirReader(background(), "vanishing")
		mustDo(t, err)
		entries, done, err := reader.Next(background(), 10)
		if err != nil || !done || !reflect.DeepEqual(names(entries), []string{"x", "y"}) {
			t.Errorf("%s: reading a directory with a vanished entry: %v %v %v", c.system, names(entries), done, err)
		}
		reader.Close()
		reader, err = env.OpenDirReader(background(), "denied")
		mustDo(t, err)
		_, _, err = reader.Next(background(), 10)
		var failure *FileError
		if !errors.As(err, &failure) || failure.Code != "permission_denied" || !strings.HasSuffix(failure.Path, "secret") {
			t.Errorf("%s: reading a directory with an unreadable entry: %#v", c.system, err)
		}
		reader.Close()
	}
}

// expiringContext is a context whose Err reports cancellation after a number
// of checks, to end an operation between two of its abort checkpoints.
type expiringContext struct {
	context.Context
	mu     sync.Mutex
	checks int
}

func (c *expiringContext) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.checks--
	if c.checks < 0 {
		return context.Canceled
	}
	return nil
}

// endingContext ends, Err and Done, right after a number of Err checks that
// reported none, to end an operation just past one of its abort checkpoints.
type endingContext struct {
	context.Context
	cancel context.CancelFunc
	mu     sync.Mutex
	checks int
}

func newEndingContext(checks int) *endingContext {
	ctx, cancel := context.WithCancel(context.Background())
	return &endingContext{Context: ctx, cancel: cancel, checks: checks}
}

func (c *endingContext) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.checks > 0 {
		c.checks--
		if c.checks == 0 {
			c.cancel()
		}
		return nil
	}
	return c.Context.Err()
}

// TestEndedContextsAbortOnlyWhereUpstreamChecks: upstream's info() never
// looks at an abort signal, nor do its operations after their last abort
// checkpoint (Stat after lstat, BinaryReader.Info after fstat, ListDir of an
// empty directory after readdir, CreateTempFile after mkdtemp): a context
// that ends there does not fail them, also when the connection is lost
// meanwhile and must start again.
func TestEndedContextsAbortOnlyWhereUpstreamChecks(t *testing.T) {
	env, connection := environment(t)
	mustDo(t, env.WriteFile(background(), "a.txt", []byte("a")))
	mustDo(t, env.CreateDir(background(), "empty", nil))
	failures := map[string]int{}
	for range 30 {
		if _, err := connection.Info(abortedContext()); err != nil {
			failures["Info"]++
		}
		if _, err := env.Stat(newEndingContext(1), "a.txt"); err != nil {
			failures["Stat"]++
		}
		reader, err := env.OpenBinaryReader(background(), "a.txt", nil)
		mustDo(t, err)
		if _, err := reader.Info(newEndingContext(1)); err != nil {
			failures["BinaryReader.Info"]++
		}
		reader.Close()
		if infos, err := env.ListDir(newEndingContext(1), "empty"); err != nil || len(infos) != 0 {
			failures["ListDir"]++
		}
		if path, err := env.CreateTempFile(newEndingContext(1), nil); err != nil {
			failures["CreateTempFile"]++
		} else {
			mustDo(t, os.RemoveAll(filepath.Dir(path)))
		}
	}
	if len(failures) != 0 {
		t.Errorf("failed with an ended context (of 30 each): %v", failures)
	}
	// Upstream's createTempDir checks before it waits for the daemon to start,
	// not after.
	fresh := NewRemoteExecutionEnv(RemoteExecutionEnvOptions{Connection: newConnection(t), ID: "pi-env:test", Cwd: env.Cwd()})
	if dir, err := fresh.CreateTempDir(newEndingContext(1), ""); err != nil {
		t.Errorf("CreateTempDir with a context that ended while the daemon started: %v", err)
	} else {
		mustDo(t, os.Remove(dir))
	}
	// The listing daemon exits when the handle of "exiting" is closed, after
	// readdir: ListDir starts it again for the remote's path rules.
	listing := NewConnection(ConnectionOptions{Command: []string{os.Args[0], listingDaemonArg, "linux"}})
	t.Cleanup(listing.Close)
	listingEnv := NewRemoteExecutionEnv(RemoteExecutionEnvOptions{Connection: listing, ID: "pi-env:listing", Cwd: "/"})
	if infos, err := listingEnv.ListDir(newEndingContext(1), "exiting"); err != nil || len(infos) != 0 {
		t.Errorf("ListDir with the connection lost after readdir: %v, %v", infos, err)
	}
}

// TestWriteChecksForAbortsBetweenChunksAndAppendDoesNot: Node's writeFile
// checks its signal before each 512 KiB write, appendFile only before and
// after, so an append aborted midway still writes everything.
func TestWriteChecksForAbortsBetweenChunksAndAppendDoesNot(t *testing.T) {
	env, _ := environment(t)
	content := bytes.Repeat([]byte("0123456789abcdef"), 3*1024*1024/16)
	err := env.WriteFile(&expiringContext{Context: background(), checks: 1}, "w.bin", content)
	if errorCode(err) != "aborted" {
		t.Fatalf("write aborted midway: %v", err)
	}
	if info, err := os.Stat(filepath.Join(env.Cwd(), "w.bin")); err != nil || info.Size() != writeChunk {
		t.Fatalf("an aborted write wrote %d bytes, want %d (%v)", info.Size(), writeChunk, err)
	}
	err = env.AppendFile(&expiringContext{Context: background(), checks: 1}, "w.bin", content)
	if errorCode(err) != "aborted" {
		t.Fatalf("append aborted midway: %v", err)
	}
	if info, err := os.Stat(filepath.Join(env.Cwd(), "w.bin")); err != nil || info.Size() != int64(writeChunk+len(content)) {
		t.Fatalf("an aborted append left %d bytes, want %d (%v)", info.Size(), writeChunk+len(content), err)
	}
}
