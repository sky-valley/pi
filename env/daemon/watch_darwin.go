package daemon

import (
	"os"
	"sync"

	"golang.org/x/sys/unix"
)

// Native watching on macOS is kqueue: FSEvents, which the Rust daemon uses,
// needs a C callback (cgo). kqueue reports changes per open file. A watched
// directory says that its entries changed without naming them, so it is
// listed again and the entries that came or went are reported; the regular
// files in it are opened too, so that their writes are seen, as inotify and
// FSEvents report them. Entries are opened without following links and only
// regular files are kept: what changes behind a symbolic link, possibly far
// outside the tree, is not reported (FSEvents and inotify report nothing
// either), while the link's own coming and going shows in its directory.
// (fsnotify's kqueue backend opens every entry following links, reporting
// changes behind them under the link's name and missing a link's removal.)

// vnodeEvents are the changes a watched file or directory reports.
const vnodeEvents = unix.NOTE_DELETE | unix.NOTE_WRITE | unix.NOTE_EXTEND | unix.NOTE_ATTRIB | unix.NOTE_RENAME

// vnodeWatch is one open descriptor kqueue watches: a path add installed, or
// a regular file in a watched directory.
type vnodeWatch struct {
	fd   int
	path string
	// parent and name place a regular file in its watched directory; parent
	// is nil for a path add installed.
	parent *vnodeWatch
	name   string
	// names are a watched directory's entries at the last listing, files the
	// regular files among them that are open; nil for a file.
	names map[string]bool
	files map[string]*vnodeWatch
}

type nativeWatcher struct {
	eventQueue
	kq int
	// wake is a pipe whose write end closing ends the reader.
	wake [2]int
	mu   sync.Mutex
	// added are the paths add installed; byFD every open watch.
	added map[string]*vnodeWatch
	byFD  map[int]*vnodeWatch
	done  chan struct{}
}

func newNativeWatcher() (*nativeWatcher, error) {
	kq, err := unix.Kqueue()
	if err != nil {
		return nil, err
	}
	unix.CloseOnExec(kq)
	var wake [2]int
	if err := unix.Pipe(wake[:]); err != nil {
		unix.Close(kq)
		return nil, err
	}
	unix.CloseOnExec(wake[0])
	unix.CloseOnExec(wake[1])
	var change unix.Kevent_t
	unix.SetKevent(&change, wake[0], unix.EVFILT_READ, unix.EV_ADD|unix.EV_ENABLE)
	if _, err := unix.Kevent(kq, []unix.Kevent_t{change}, nil, nil); err != nil {
		unix.Close(kq)
		unix.Close(wake[0])
		unix.Close(wake[1])
		return nil, err
	}
	n := &nativeWatcher{
		eventQueue: newEventQueue(),
		kq:         kq,
		wake:       wake,
		added:      map[string]*vnodeWatch{},
		byFD:       map[int]*vnodeWatch{},
		done:       make(chan struct{}),
	}
	go n.read()
	return n, nil
}

// open opens path for event notifications only and registers it.
func (n *nativeWatcher) open(path string, flags int) (int, error) {
	var fd int
	err := retry(func() (err error) {
		fd, err = unix.Open(path, unix.O_EVTONLY|unix.O_NONBLOCK|unix.O_CLOEXEC|flags, 0)
		return err
	})
	if err != nil {
		return -1, err
	}
	var change unix.Kevent_t
	unix.SetKevent(&change, fd, unix.EVFILT_VNODE, unix.EV_ADD|unix.EV_ENABLE|unix.EV_CLEAR)
	change.Fflags = vnodeEvents
	if _, err := unix.Kevent(n.kq, []unix.Kevent_t{change}, nil, nil); err != nil {
		unix.Close(fd)
		return -1, err
	}
	return fd, nil
}

// add watches path, following a final symbolic link (a target may be a link
// to what is watched), and, when it is a directory, the regular files in it.
func (n *nativeWatcher) add(path string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.added[path] != nil {
		return nil
	}
	fd, err := n.open(path, 0)
	if err != nil {
		return &os.PathError{Op: "open", Path: path, Err: err}
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		unix.Close(fd)
		return &os.PathError{Op: "fstat", Path: path, Err: err}
	}
	w := &vnodeWatch{fd: fd, path: path}
	n.added[path] = w
	n.byFD[fd] = w
	if st.Mode&unix.S_IFMT == unix.S_IFDIR {
		w.names, w.files = map[string]bool{}, map[string]*vnodeWatch{}
		if err := n.list(w, false); err != nil {
			n.drop(w)
			return err
		}
	}
	return nil
}

// remove stops watching a path add installed.
func (n *nativeWatcher) remove(path string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if w := n.added[path]; w != nil {
		n.drop(w)
	}
}

// drop closes a watch, and the watches of the files in it; closing the
// descriptor removes its registration.
func (n *nativeWatcher) drop(w *vnodeWatch) {
	for _, file := range w.files {
		n.drop(file)
	}
	delete(n.byFD, w.fd)
	unix.Close(w.fd)
	if w.parent != nil {
		delete(w.parent.files, w.name)
	} else {
		delete(n.added, w.path)
	}
}

// list lists a watched directory again, reports (when report) the entries
// that came or went, and opens the regular files among the new ones. An entry
// that cannot be opened is not watched; running out of descriptors fails.
func (n *nativeWatcher) list(dir *vnodeWatch, report bool) error {
	f, err := openDir(dir.path)
	if err != nil {
		if isNotExist(err) {
			// Gone: its own deletion says so.
			return nil
		}
		return err
	}
	names, err := f.Readdirnames(-1)
	f.Close()
	if err != nil {
		return err
	}
	prefix := withSeparator(dir.path)
	listed := make(map[string]bool, len(names))
	var failed error
	for _, name := range names {
		listed[name] = true
		if dir.names[name] {
			continue
		}
		if report {
			n.push(nativeEvent{path: decodeUTF8([]byte(prefix + name))})
		}
		if err := n.watchFile(dir, name, prefix+name); err != nil && failed == nil {
			failed = err
		}
	}
	for name := range dir.names {
		if listed[name] {
			continue
		}
		if report {
			n.push(nativeEvent{path: decodeUTF8([]byte(prefix + name))})
		}
		if file := dir.files[name]; file != nil {
			n.drop(file)
		}
	}
	dir.names = listed
	return failed
}

// watchFile watches an entry of a watched directory if it is a regular file,
// opening the entry itself rather than what a link leads to.
func (n *nativeWatcher) watchFile(dir *vnodeWatch, name, path string) error {
	var st unix.Stat_t
	if unix.Lstat(path, &st) != nil || st.Mode&unix.S_IFMT != unix.S_IFREG {
		return nil
	}
	fd, err := n.open(path, unix.O_SYMLINK)
	if err != nil {
		switch err {
		case unix.ENOENT, unix.EACCES, unix.EPERM:
			return nil
		}
		return &os.PathError{Op: "open", Path: path, Err: err}
	}
	file := &vnodeWatch{fd: fd, path: path, parent: dir, name: name}
	dir.files[name] = file
	n.byFD[fd] = file
	return nil
}

func (n *nativeWatcher) read() {
	defer close(n.done)
	events := make([]unix.Kevent_t, 64)
	for {
		count, err := unix.Kevent(n.kq, nil, events, nil)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			n.push(nativeEvent{err: err})
			return
		}
		for _, event := range events[:count] {
			if int(event.Ident) == n.wake[0] {
				return
			}
			n.handle(int(event.Ident), event.Fflags)
		}
	}
}

// handle turns one vnode event into the paths that changed.
func (n *nativeWatcher) handle(fd int, flags uint32) {
	n.mu.Lock()
	defer n.mu.Unlock()
	w := n.byFD[fd]
	if w == nil {
		// Dropped meanwhile.
		return
	}
	if w.names == nil {
		// A file: written, or its attributes, name or existence changed.
		n.push(nativeEvent{path: decodeUTF8([]byte(w.path))})
		if w.parent != nil && flags&(unix.NOTE_DELETE|unix.NOTE_RENAME) != 0 {
			// Its name may now hold another file: list the directory again.
			parent := w.parent
			n.drop(w)
			delete(parent.names, w.name)
			if err := n.list(parent, true); err != nil {
				n.push(nativeEvent{err: err})
			}
		}
		return
	}
	if flags&unix.NOTE_WRITE != 0 {
		if err := n.list(w, true); err != nil {
			n.push(nativeEvent{err: err})
		}
	}
	if flags&(unix.NOTE_DELETE|unix.NOTE_RENAME|unix.NOTE_ATTRIB) != 0 {
		n.push(nativeEvent{path: decodeUTF8([]byte(w.path))})
	}
}

func (n *nativeWatcher) close() {
	unix.Close(n.wake[1])
	<-n.done
	n.mu.Lock()
	for fd := range n.byFD {
		unix.Close(fd)
	}
	clear(n.byFD)
	clear(n.added)
	n.mu.Unlock()
	unix.Close(n.wake[0])
	unix.Close(n.kq)
}
