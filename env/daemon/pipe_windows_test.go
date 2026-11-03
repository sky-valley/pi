package daemon

import (
	"testing"
	"time"
)

// Upstream 9c9ce8547: on Windows a reader or writer of a child pipe stops once
// the command settles, also while a descendant holds the child's end without
// writing or reading. Here: closing the daemon's end ends a read or a write
// that waits, though the child's end stays open.

func TestClosingTheDaemonsEndStopsAWaitingRead(t *testing.T) {
	ours, theirs, err := childPipe(true)
	if err != nil {
		t.Fatal(err)
	}
	defer theirs.Close()
	read := make(chan error, 1)
	go func() {
		_, err := ours.Read(make([]byte, 16))
		read <- err
	}()
	time.Sleep(100 * time.Millisecond)
	ours.Close()
	select {
	case err := <-read:
		if err == nil {
			t.Fatal("the read succeeded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the read still waits after its pipe was closed")
	}
}

func TestClosingTheDaemonsEndStopsAWaitingWrite(t *testing.T) {
	ours, theirs, err := childPipe(false)
	if err != nil {
		t.Fatal(err)
	}
	defer theirs.Close()
	written := make(chan error, 1)
	go func() {
		// Far more than the pipe holds, and nobody reads.
		_, err := ours.Write(make([]byte, 1<<20))
		written <- err
	}()
	time.Sleep(100 * time.Millisecond)
	ours.Close()
	select {
	case err := <-written:
		if err == nil {
			t.Fatal("the write succeeded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the write still waits after its pipe was closed")
	}
}

func TestTheChildsEndClosingEndsReads(t *testing.T) {
	ours, theirs, err := childPipe(true)
	if err != nil {
		t.Fatal(err)
	}
	defer ours.Close()
	if _, err := theirs.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	theirs.Close()
	buf := make([]byte, 16)
	if n, err := ours.Read(buf); n != 1 || err != nil {
		t.Fatalf("read %d, %v", n, err)
	}
	if n, err := ours.Read(buf); n != 0 || err == nil {
		t.Fatalf("read after the end: %d, %v", n, err)
	}
}
