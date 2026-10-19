//go:build !darwin

package daemon

import "github.com/fsnotify/fsnotify"

// nativeWatcher is an fsnotify watcher (inotify on Linux) whose events are
// queued without bound.
type nativeWatcher struct {
	eventQueue
	watcher *fsnotify.Watcher
	done    chan struct{}
}

func newNativeWatcher() (*nativeWatcher, error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	n := &nativeWatcher{eventQueue: newEventQueue(), watcher: watcher, done: make(chan struct{})}
	go n.pump()
	return n, nil
}

func (n *nativeWatcher) pump() {
	defer close(n.done)
	for {
		select {
		case e, ok := <-n.watcher.Events:
			if !ok {
				return
			}
			// Lossily, as the notify crate's paths are decoded.
			n.push(nativeEvent{path: decodeUTF8([]byte(e.Name))})
		case err, ok := <-n.watcher.Errors:
			if !ok {
				return
			}
			n.push(nativeEvent{err: err})
		}
	}
}

func (n *nativeWatcher) add(path string) error { return n.watcher.Add(path) }

// remove stops watching path; a watch the kernel already dropped (its
// directory is gone) is not an error, as notify's unwatch result is ignored.
func (n *nativeWatcher) remove(path string) { _ = n.watcher.Remove(path) }

func (n *nativeWatcher) close() {
	_ = n.watcher.Close()
	<-n.done
}
