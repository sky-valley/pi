package env

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"
)

// WatchTarget is a file or directory to watch (Durable's WatchTarget). It may
// be missing; creating it is a change.
type WatchTarget struct {
	Path string
	// Recursive watches everything below a directory, not only its entries.
	// Symbolic links below it are not followed.
	Recursive bool
	// Exclude names entries below Path that are neither watched nor
	// reported.
	Exclude WatchExclude
}

// WatchExclude is what a WatchTarget leaves out.
type WatchExclude struct {
	// Hidden leaves out names starting with ".".
	Hidden bool
	Names  []string
}

// WatchChange is what changed (Durable's WatchChange); exactly one field is
// set:
//   - Paths: something at or below each path may have changed (a directory
//     path covers its whole subtree). Calls may be spurious; a change is never
//     missed while the watcher is healthy.
//   - Overflow: coverage was uncertain for a while (lost events, reconnect);
//     rescan everything that is watched.
//   - Err: the watcher stopped; no calls follow.
type WatchChange struct {
	Paths    []string
	Overflow bool
	Err      *FileError
}

// RemoteWatchOptions say how the daemon watches, like NodeExecutionEnv's
// NodeWatchOptions.
type RemoteWatchOptions struct {
	// Mode forces "native" or "polling"; by default Windows and file systems
	// that do not report remote changes poll.
	Mode string `json:"mode,omitempty"`
	// PollIntervalMS is the interval between snapshots in polling mode;
	// 0 is 2000 ms.
	PollIntervalMS int64 `json:"pollIntervalMs,omitempty"`
	// MaxDirectories is the most directories one watcher covers; 0 is
	// 10 000.
	MaxDirectories int64 `json:"maxDirectories,omitempty"`
}

const (
	reconnectFirst = time.Second
	reconnectMax   = 30 * time.Second
)

// RemoteWatcher is a watcher running in the daemon (Durable's
// NodeFileWatcher next to the files). When the connection is lost, it is
// opened again once the connection can be started again, and reports
// Overflow: changes in between were not seen.
type RemoteWatcher struct {
	connection *Connection
	targets    []WatchTarget
	onChange   func(WatchChange)
	options    RemoteWatchOptions

	mu      sync.Mutex
	mode    string
	closed  bool
	cancel  context.CancelFunc
	request chan struct{}
	wake    chan struct{}
}

// OpenRemoteWatcher watches targets (resolved paths); it fails like
// NodeFileWatcher.open if coverage cannot be established. onChange is called
// from one goroutine at a time.
func OpenRemoteWatcher(connection *Connection, targets []WatchTarget, onChange func(WatchChange), options RemoteWatchOptions) (*RemoteWatcher, error) {
	w := &RemoteWatcher{connection: connection, targets: targets, onChange: onChange, options: options, mode: "native", wake: make(chan struct{})}
	if err := w.start(); err != nil {
		return nil, err
	}
	return w, nil
}

// Mode is "native" (changes are reported within about two seconds) or
// "polling" (the daemon compares snapshots, because the file system does not
// report changes reliably).
func (w *RemoteWatcher) Mode() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.mode
}

// Close stops watching; no onChange call starts after it returns. It is
// idempotent.
func (w *RemoteWatcher) Close() error {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return nil
	}
	w.closed = true
	close(w.wake)
	cancel, request := w.cancel, w.request
	w.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if request != nil {
		<-request
	}
	return nil
}

func (w *RemoteWatcher) isClosed() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.closed
}

func (w *RemoteWatcher) setClosed() {
	w.mu.Lock()
	w.closed = true
	w.mu.Unlock()
}

func (w *RemoteWatcher) deliver(change WatchChange) {
	if w.isClosed() {
		return
	}
	// A panicking callback must not stop watching.
	defer func() { _ = recover() }()
	w.onChange(change)
}

type watchTargetJSON struct {
	Path      string      `json:"path"`
	Recursive bool        `json:"recursive"`
	Exclude   excludeJSON `json:"exclude"`
}

type excludeJSON struct {
	Hidden bool     `json:"hidden"`
	Names  []string `json:"names"`
}

// start opens the daemon's watcher; it returns once the watcher reports
// coverage.
func (w *RemoteWatcher) start() error {
	targets := make([]watchTargetJSON, len(w.targets))
	for i, target := range w.targets {
		names := append([]string{}, target.Exclude.Names...)
		targets[i] = watchTargetJSON{target.Path, target.Recursive, excludeJSON{target.Exclude.Hidden, names}}
	}
	fields := object{}.with("targets", targets)
	if w.options.Mode != "" {
		fields = fields.with("mode", w.options.Mode)
	}
	if w.options.PollIntervalMS != 0 {
		fields = fields.with("pollIntervalMs", w.options.PollIntervalMS)
	}
	if w.options.MaxDirectories != 0 {
		fields = fields.with("maxDirectories", w.options.MaxDirectories)
	}

	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan struct{})
	isReady := false
	finished := make(chan error, 1)
	request := make(chan struct{})
	w.mu.Lock()
	w.cancel, w.request = cancel, request
	w.mu.Unlock()

	go func() {
		defer close(request)
		defer cancel()
		_, err := w.connection.Request(ctx, "watch", fields, &RequestOptions{
			OnEvent: func(event json.RawMessage, _ []byte) {
				var e struct {
					Kind     string   `json:"kind"`
					Mode     string   `json:"mode"`
					Overflow bool     `json:"overflow"`
					Paths    []string `json:"paths"`
					Code     string   `json:"code"`
					Message  any      `json:"message"`
				}
				if json.Unmarshal(event, &e) != nil {
					return
				}
				switch e.Kind {
				case "ready":
					w.mu.Lock()
					w.mode = "native"
					if e.Mode == "polling" {
						w.mode = "polling"
					}
					w.mu.Unlock()
					if !isReady {
						isReady = true
						close(ready)
					}
				case "change":
					if e.Mode == "polling" {
						w.mu.Lock()
						w.mode = "polling"
						w.mu.Unlock()
					}
					if e.Overflow {
						w.deliver(WatchChange{Overflow: true})
					} else {
						w.deliver(WatchChange{Paths: e.Paths})
					}
				case "error":
					code := FileInvalid
					if e.Code == "permission_denied" {
						code = FilePermissionDenied
					}
					w.deliver(WatchChange{Err: &FileError{Code: code, Message: jsValueString(e.Message)}})
					w.setClosed()
				}
			},
		})
		if !isReady {
			if err == nil {
				err = errors.New("pi-env ended the watch before it was ready")
			}
			finished <- err
			return
		}
		if err == nil || w.isClosed() {
			return
		}
		if IsConnectionLost(err) {
			go w.reconnect()
			return
		}
		w.deliver(WatchChange{Err: toFileError(err, "")})
		w.setClosed()
	}()

	select {
	case <-ready:
		return nil
	case err := <-finished:
		return err
	}
}

func (w *RemoteWatcher) reconnect() {
	for delay := reconnectFirst; !w.isClosed(); delay = min(delay*2, reconnectMax) {
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-w.wake:
			timer.Stop()
		}
		if w.isClosed() {
			return
		}
		err := w.start()
		if err == nil {
			// Changes while disconnected were not seen.
			w.deliver(WatchChange{Overflow: true})
			return
		}
		if IsConnectionLost(err) {
			continue
		}
		w.deliver(WatchChange{Err: toFileError(err, "")})
		w.setClosed()
		return
	}
}

// jsValueString is String(value) of a decoded JSON value.
func jsValueString(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	if v == nil {
		return "undefined"
	}
	return jsString(raw)
}
