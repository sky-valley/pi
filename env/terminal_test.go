package env

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// Ported from upstream's packages/env/test/terminal.test.ts (d2e7d8b7c).

// collector collects terminal output and waits for text in it.
type collector struct {
	mu   sync.Mutex
	data bytes.Buffer
}

func (c *collector) onData(data []byte) {
	c.mu.Lock()
	c.data.Write(data)
	c.mu.Unlock()
}

func (c *collector) bytes() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]byte(nil), c.data.Bytes()...)
}

func (c *collector) waitFor(t *testing.T, text string) {
	t.Helper()
	waitFor(t, 10*time.Second, fmt.Sprintf("%q in the terminal (got %q)", text, c.bytes()), func() bool {
		return bytes.Contains(c.bytes(), []byte(text))
	})
}

// waitTerminal waits for a terminal's end, as long as upstream's test
// timeout allows.
func waitTerminal(t *testing.T, terminal *RemoteTerminal) (int, error) {
	t.Helper()
	select {
	case <-terminal.Done():
		return terminal.Wait()
	case <-time.After(20 * time.Second):
		terminal.Close()
		t.Fatal("the terminal did not end within 20 s")
		return 0, nil
	}
}

func skipTerminalsOnWindows(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("terminals are not supported on Windows yet")
	}
}

func TestTerminalRunsAProgramWithInputResizingAndItsExitCode(t *testing.T) {
	skipTerminalsOnWindows(t)
	connection := newConnection(t)
	output := &collector{}
	terminal, err := OpenTerminal(background(), connection, TerminalOptions{
		Cwd: tempDir(t, "pi-env-terminal-"), Argv: []string{"/bin/sh"},
		Env:  map[string]string{"PS1": "$ ", "TERM": "xterm-256color"},
		Cols: 80, Rows: 24, OnData: output.onData,
	})
	mustDo(t, err)
	if terminal.PID <= 0 {
		t.Fatalf("pid %d", terminal.PID)
	}
	terminal.Write([]byte("test -t 0 && echo IS-A-TTY; stty size; echo $TERM\n"))
	output.waitFor(t, "IS-A-TTY")
	output.waitFor(t, "24 80")
	output.waitFor(t, "xterm-256color")
	terminal.Resize(100, 30)
	terminal.Write([]byte("stty size\n"))
	output.waitFor(t, "30 100")
	terminal.Write([]byte("exit 3\n"))
	if code, err := waitTerminal(t, terminal); err != nil || code != 3 {
		t.Fatalf("exit %d, %v", code, err)
	}
}

func TestTerminalReportsTheForegroundJobNotTheShellAtItsPrompt(t *testing.T) {
	skipTerminalsOnWindows(t)
	connection := newConnection(t)
	output := &collector{}
	terminal, err := OpenTerminal(background(), connection, TerminalOptions{
		Cwd: tempDir(t, "pi-env-terminal-"),
		// Interactive, for job control: the shell puts each job in front of
		// the terminal.
		Argv: []string{"/bin/sh", "-i"},
		Env:  map[string]string{"PS1": "$ ", "TERM": "xterm-256color"},
		Cols: 80, Rows: 24, OnData: output.onData,
	})
	mustDo(t, err)
	terminal.Write([]byte("echo READY\n"))
	output.waitFor(t, "READY")
	if foreground, err := terminal.Foreground(background()); err != nil || foreground != nil {
		t.Fatalf("foreground at the prompt: %+v, %v", foreground, err)
	}
	terminal.Write([]byte("sleep 100\n"))
	// The shell forks before it execs: wait until the foreground job is the
	// command itself, not the forked shell.
	named := runtime.GOOS == "darwin" || runtime.GOOS == "linux"
	foreground, _ := terminal.Foreground(background())
	for tries := 0; (foreground == nil || (named && foreground.Name != "sleep")) && tries < 100; tries++ {
		time.Sleep(20 * time.Millisecond)
		foreground, _ = terminal.Foreground(background())
	}
	if foreground == nil || foreground.PID <= 0 || foreground.PID == terminal.PID {
		t.Fatalf("foreground %+v (terminal %d)", foreground, terminal.PID)
	}
	if named && foreground.Name != "sleep" {
		t.Fatalf("foreground name %q", foreground.Name)
	}
	terminal.Close()
	if _, err := waitTerminal(t, terminal); errorCode(err) != "aborted" {
		t.Fatalf("closed terminal: %v", err)
	}
	if _, err := terminal.Foreground(background()); errorCode(err) != "EBADF" {
		t.Fatalf("foreground of an ended terminal: %v", err)
	}
}

func TestTerminalDeliversRawBytesNotDecodedText(t *testing.T) {
	skipTerminalsOnWindows(t)
	connection := newConnection(t)
	output := &collector{}
	terminal, err := OpenTerminal(background(), connection, TerminalOptions{
		Cwd: tempDir(t, "pi-env-terminal-"), Argv: []string{"sh", "-c", `printf 'a\377b'`},
		Cols: 80, Rows: 24, OnData: output.onData,
	})
	mustDo(t, err)
	if code, err := waitTerminal(t, terminal); err != nil || code != 0 {
		t.Fatalf("exit %d, %v", code, err)
	}
	if got := output.bytes(); !bytes.Equal(got, []byte{0x61, 0xff, 0x62}) {
		t.Fatalf("output %x", got)
	}
}

func TestTerminalKeepsInputInOrder(t *testing.T) {
	skipTerminalsOnWindows(t)
	connection := newConnection(t)
	cwd := tempDir(t, "pi-env-terminal-")
	terminal, err := OpenTerminal(background(), connection, TerminalOptions{
		Cwd: cwd, Argv: []string{"sh", "-c", "head -c 40000 > input.txt"},
		Cols: 80, Rows: 24, OnData: func([]byte) {},
	})
	mustDo(t, err)
	var text strings.Builder
	for index := 0; index < 4000; index++ {
		piece := fmt.Sprintf("%09d\n", index)
		text.WriteString(piece)
		terminal.Write([]byte(piece))
	}
	if code, err := waitTerminal(t, terminal); err != nil || code != 0 {
		t.Fatalf("exit %d, %v", code, err)
	}
	got, err := os.ReadFile(filepath.Join(cwd, "input.txt"))
	mustDo(t, err)
	if string(got) != text.String() {
		t.Fatal("input arrived out of order")
	}
}

func TestTerminalKillsTheProgramOnClose(t *testing.T) {
	skipTerminalsOnWindows(t)
	connection := newConnection(t)
	terminal, err := OpenTerminal(background(), connection, TerminalOptions{
		Cwd: tempDir(t, "pi-env-terminal-"), Argv: []string{"sleep", "100"},
		Cols: 80, Rows: 24, OnData: func([]byte) {},
	})
	mustDo(t, err)
	terminal.Close()
	if _, err := waitTerminal(t, terminal); errorCode(err) != "aborted" {
		t.Fatalf("closed terminal: %v", err)
	}
}

func TestTerminalFailsToOpenInAMissingDirectory(t *testing.T) {
	skipTerminalsOnWindows(t)
	connection := newConnection(t)
	_, err := OpenTerminal(background(), connection, TerminalOptions{
		Cwd: filepath.Join(tempDir(t, "pi-env-terminal-"), "missing"), Cols: 80, Rows: 24, OnData: func([]byte) {},
	})
	var remote *RemoteError
	if !errors.As(err, &remote) {
		t.Fatalf("open in a missing directory: %#v", err)
	}
}

func TestTerminalRejectsWritesToATerminalThatDoesNotExist(t *testing.T) {
	skipTerminalsOnWindows(t)
	connection := newConnection(t)
	_, err := connection.Request(background(), "ptyWrite", object{}.with("target", 999), nil)
	if errorCode(err) != "EBADF" {
		t.Fatalf("write to no terminal: %v", err)
	}
}

// TestTerminalSendsLargeInputInOrder writes more than one 1 MiB chunk in one
// call: the chunks must reach the program in order.
func TestTerminalSendsLargeInputInOrder(t *testing.T) {
	skipTerminalsOnWindows(t)
	connection := newConnection(t)
	cwd := tempDir(t, "pi-env-terminal-")
	var text strings.Builder
	for index := 0; text.Len() < 2*terminalWriteChunk+terminalWriteChunk/4; index++ {
		// Short printable lines: the terminal's line discipline handles
		// input line by line.
		fmt.Fprintf(&text, "%098d\n", index)
	}
	terminal, err := OpenTerminal(background(), connection, TerminalOptions{
		Cwd: cwd, Argv: []string{"sh", "-c", fmt.Sprintf("head -c %d > input.txt", text.Len())},
		Cols: 80, Rows: 24, OnData: func([]byte) {},
	})
	mustDo(t, err)
	terminal.Write([]byte(text.String()))
	if code, err := waitTerminal(t, terminal); err != nil || code != 0 {
		t.Fatalf("exit %d, %v", code, err)
	}
	got, err := os.ReadFile(filepath.Join(cwd, "input.txt"))
	mustDo(t, err)
	if string(got) != text.String() {
		t.Fatal("large input arrived out of order")
	}
}
