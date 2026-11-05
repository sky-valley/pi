//go:build unix

package coding

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// An abort ends a read blocked on a pipe that never sends EOF: the tool
// closes the stream, as pi destroys it on its signal.
func TestReadAbortEndsABlockedStream(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skip("mkfifo:", err)
	}
	// Hold a writer open so the reader never sees EOF.
	writer := make(chan *os.File, 1)
	go func() {
		f, _ := os.OpenFile(fifo, os.O_WRONLY, 0)
		writer <- f
	}()
	defer func() {
		if f := <-writer; f != nil {
			f.Close()
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		// The image sniff would block on the FIFO first; this test is about
		// the stream (the wider gap is recorded as known debt).
		ops := DefaultReadOperations()
		ops.DetectImageMimeType = nil
		_, err := readToolOps("/", &ops, nil).Execute(ctx, "call", map[string]any{"path": fifo}, nil)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || err.Error() != "Operation aborted" {
			t.Fatalf("err = %v, want Operation aborted", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the read stayed blocked after its context ended")
	}
}
