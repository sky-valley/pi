package daemon

import (
	"hash/fnv"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/sky-valley/pi/env/internal/frame"
)

// watch: Durable's NodeFileWatcher (packages/durable/src/env/node-watch.ts)
// running next to the files, so a scan costs no round trips. Native events
// only trigger a debounced rescan; changes are the difference between
// snapshots plus the event paths. Events go out as change events of the
// request, which lasts until it is cancelled.
//
// The Rust daemon's native events come from the notify crate (inotify on
// Linux, FSEvents on macOS); here they come from fsnotify (inotify on Linux,
// kqueue on macOS). kqueue is not recursive, so macOS watches every directory
// below a recursive target, as Linux does.

const (
	debounce = 50 * time.Millisecond
	// fseventsSettle: on macOS a rescan this long after installing watchers
	// catches changes made in between (FSEvents streams go live late; kept for
	// kqueue so the rescans happen when the Rust daemon's do).
	fseventsSettle      = 500 * time.Millisecond
	defaultPollMs       = 2000
	defaultMaxDirectory = 10_000
	// In polling mode, recently modified small files are also compared by
	// content.
	hashMaxBytes = 256 * 1024
	hashRecent   = 5 * time.Second
	// cancelPoll is how often a waiting watcher looks for a cancel.
	cancelPoll = 100 * time.Millisecond
)

type watchTarget struct {
	path      string
	recursive bool
	hidden    bool
	names     map[string]bool
}

type watchKind int

const (
	watchFile watchKind = iota
	watchDirectory
	watchSymlink
	watchOther
)

// watchEntry is what a snapshot remembers of one path. Directories and
// ancestors are compared by identity only.
type watchEntry struct {
	kind    watchKind
	dev     uint64
	ino     uint64
	size    uint64
	mtime   [2]int64
	hash    uint64
	hasHash bool
}

type snapshot map[string]watchEntry

type watchMode int

const (
	modeNative watchMode = iota
	modePolling
)

func (m watchMode) name() string {
	if m == modeNative {
		return "native"
	}
	return "polling"
}

// scanError is a failed scan: too many directories, or an I/O error.
type scanError struct {
	budget  int
	err     error
	syscall string
	path    string
}

// failure is the error NodeExecutionEnv.watch returns when opening fails.
func (e *scanError) failure() *failure {
	if e.err == nil {
		return newFailure("EINVAL", "Watched paths exceed "+strconv.Itoa(e.budget)+" directories")
	}
	return ioFailure(e.err, e.syscall, e.path)
}

// code is the FileError code NodeFileWatcher reports when a later rescan
// fails.
func (e *scanError) code() string {
	if e.err != nil && denied(e.err) {
		return "permission_denied"
	}
	return "invalid"
}

func denied(err error) bool {
	name := errorName(err)
	return name == "EACCES" || name == "EPERM"
}

func kindOfStat(st fileStat) watchKind {
	switch {
	case st.isFile():
		return watchFile
	case st.isDir():
		return watchDirectory
	case st.isSymlink():
		return watchSymlink
	}
	return watchOther
}

// entryOf is what a snapshot remembers of path, whose metadata came from stat
// (follow) or lstat.
func entryOf(path string, st fileStat, follow bool) watchEntry {
	kind := kindOfStat(st)
	dev, ino := pathIdentity(path, st, follow)
	entry := watchEntry{kind: kind, dev: dev, ino: ino}
	if kind != watchDirectory {
		entry.size = st.size
		entry.mtime = [2]int64{st.mtimeSec, st.mtimeNsec}
	}
	return entry
}

func withSeparator(directory string) string {
	if strings.HasSuffix(directory, pathSeparator) {
		return directory
	}
	return directory + pathSeparator
}

// isWithin reports whether path is ancestor or below it.
func isWithin(path, ancestor string) bool {
	return path == ancestor || strings.HasPrefix(path, withSeparator(ancestor))
}

func (t *watchTarget) excluded(name string) bool {
	return (t.hidden && strings.HasPrefix(name, ".")) || t.names[name]
}

// scan is one scan: the snapshot, and targets that are symbolic links to
// files, whose files need their own watchers.
type scan struct {
	snapshot    snapshot
	linkedFiles map[string]bool
	// counted are the directories counted against the limit; traversed are the
	// directories traversed per target.
	counted   map[string]bool
	traversed map[string]bool
	// listed are the kinds of listed entries without following links.
	listed map[string]watchKind
}

type installed struct{ dev, ino uint64 }

type fileWatcher struct {
	id             uint32
	out            *output
	ctl            *control
	targets        []watchTarget
	mode           watchMode
	pollInterval   time.Duration
	maxDirectories int
	snapshot       snapshot
	native         *nativeWatcher
	installed      map[string]installed
	events         map[string]bool
	settleAt       time.Time
}

func (w *fileWatcher) send(json object) {
	w.out.sendControl(frame.Frame{Kind: frame.Event, ID: w.id, JSON: marshal(json)})
}

func (w *fileWatcher) cancelled() bool { return w.ctl.aborted.Load() }

func (w *fileWatcher) record(s *scan, path string, st fileStat, follow bool) {
	entry := entryOf(path, st, follow)
	if w.mode == modePolling && st.isFile() && st.size <= hashMaxBytes && recentlyModified(st) {
		if content, err := os.ReadFile(path); err == nil {
			h := fnv.New64a()
			h.Write(content)
			entry.hash, entry.hasHash = h.Sum64(), true
		}
	}
	s.snapshot[path] = entry
}

// recentlyModified reports whether a file changed within hashRecent, or
// claims a modification time in the future.
func recentlyModified(st fileStat) bool {
	modified := time.Unix(st.mtimeSec, st.mtimeNsec)
	return time.Since(modified) < hashRecent
}

func (w *fileWatcher) count(s *scan, path string) *scanError {
	s.counted[path] = true
	if len(s.counted) > w.maxDirectories {
		return &scanError{budget: w.maxDirectories}
	}
	return nil
}

func (w *fileWatcher) scanDirectory(s *scan, index int, directory string) *scanError {
	target := &w.targets[index]
	key := strconv.Itoa(index) + "\x00" + directory
	if s.traversed[key] {
		return nil
	}
	s.traversed[key] = true
	skip := func(err error) *scanError {
		// The watched directory itself must be readable; below it, unreadable
		// directories are skipped.
		if directory == target.path && denied(err) {
			return &scanError{err: err, syscall: "scandir", path: directory}
		}
		switch errorName(err) {
		case "ENOENT", "EACCES", "EPERM", "ENOTDIR":
			return nil
		}
		return &scanError{err: err, syscall: "scandir", path: directory}
	}
	f, err := openDir(directory)
	if err != nil {
		return skip(unwrapPathError(err))
	}
	names, err := f.Readdirnames(-1)
	f.Close()
	if err != nil {
		return skip(unwrapPathError(err))
	}
	prefix := withSeparator(directory)
	for _, raw := range names {
		name := decodeUTF8([]byte(raw))
		if target.excluded(name) {
			continue
		}
		path := prefix + name
		kind, ok := s.listed[path]
		if !ok {
			st, err := lstatPath(path)
			if err != nil {
				continue
			}
			kind = kindOfStat(st)
			s.listed[path] = kind
			// A target's own entry (following links) wins over its listing by
			// another target.
			if _, present := s.snapshot[path]; !present {
				w.record(s, path, st, false)
			}
		}
		if target.recursive && kind == watchDirectory {
			if fail := w.count(s, path); fail != nil {
				return fail
			}
			if fail := w.scanDirectory(s, index, path); fail != nil {
				return fail
			}
		}
	}
	return nil
}

func (w *fileWatcher) scan() (*scan, *scanError) {
	s := &scan{
		snapshot:    snapshot{},
		linkedFiles: map[string]bool{},
		counted:     map[string]bool{},
		traversed:   map[string]bool{},
		listed:      map[string]watchKind{},
	}
	for index := range w.targets {
		target := &w.targets[index]
		for _, ancestor := range ancestors(target.path) {
			if _, present := s.snapshot[ancestor]; present {
				continue
			}
			// Identity only: an ancestor's own timestamps change with every
			// unrelated sibling.
			if st, err := lstatPath(ancestor); err == nil {
				entry := entryOf(ancestor, st, false)
				entry.size, entry.mtime = 0, [2]int64{}
				s.snapshot[ancestor] = entry
			}
		}
		// The target may be a symbolic link to what is watched; follow it. A
		// missing target is watched for its creation; one that cannot be
		// reached for lack of permission fails.
		st, err := statPath(target.path)
		if err != nil {
			if denied(err) {
				return nil, &scanError{err: unwrapPathError(err), syscall: "stat", path: target.path}
			}
			continue
		}
		w.record(s, target.path, st, true)
		if st.isFile() {
			if link, err := lstatPath(target.path); err == nil && link.isSymlink() {
				s.linkedFiles[target.path] = true
			}
		}
		if st.isDir() {
			if fail := w.count(s, target.path); fail != nil {
				return nil, fail
			}
			if fail := w.scanDirectory(s, index, target.path); fail != nil {
				return nil, fail
			}
		}
	}
	return s, nil
}

// reported is the path a change is reported under: an ancestor that changed
// identity moved every target below it, so it reports those targets.
func (w *fileWatcher) reported(path string) string {
	for _, target := range w.targets {
		if isWithin(path, target.path) {
			return path
		}
	}
	for _, target := range w.targets {
		if isWithin(target.path, path) {
			return target.path
		}
	}
	return path
}

func (w *fileWatcher) diff(previous, next snapshot) []string {
	var changed []string
	for path, entry := range next {
		if old, ok := previous[path]; !ok || old != entry {
			changed = append(changed, w.reported(path))
		}
	}
	for path := range previous {
		if _, ok := next[path]; !ok {
			changed = append(changed, w.reported(path))
		}
	}
	return changed
}

// sync rescans, reports differences, and installs watchers for new
// directories, rescanning until none are new.
func (w *fileWatcher) sync(report bool) (map[string]bool, *scanError) {
	changed := map[string]bool{}
	for range 10 {
		if w.cancelled() {
			break
		}
		s, fail := w.scan()
		if fail != nil {
			return nil, fail
		}
		if report {
			for _, path := range w.diff(w.snapshot, s.snapshot) {
				changed[path] = true
			}
		}
		w.snapshot = s.snapshot
		if w.mode == modePolling || !w.reconcile(s.linkedFiles) {
			break
		}
		if runtime.GOOS == "darwin" {
			w.settleAt = time.Now().Add(fseventsSettle)
		}
		// Something written into a new directory before its watcher existed
		// shows up in the next round.
		report = true
	}
	return changed, nil
}

// reconcile watches every existing ancestor of each target, each target
// directory, each target that links to a file, and each directory below a
// recursive target. It returns whether a watcher was added.
func (w *fileWatcher) reconcile(linkedFiles map[string]bool) bool {
	wanted := map[string]bool{}
	for path := range linkedFiles {
		wanted[path] = true
	}
	for _, target := range w.targets {
		for _, ancestor := range ancestors(target.path) {
			if entry, ok := w.snapshot[ancestor]; ok && entry.kind == watchDirectory {
				wanted[ancestor] = true
			}
		}
		if entry, ok := w.snapshot[target.path]; !ok || entry.kind != watchDirectory {
			continue
		}
		wanted[target.path] = true
		if target.recursive && watchPerDirectory {
			for path, entry := range w.snapshot {
				if entry.kind == watchDirectory && path != target.path && isWithin(path, target.path) {
					wanted[path] = true
				}
			}
		}
	}
	// Gone, or replaced: a watcher follows the directory it was installed on,
	// not the path.
	for path, watch := range w.installed {
		entry, ok := w.snapshot[path]
		if !wanted[path] || !ok || entry.dev != watch.dev || entry.ino != watch.ino {
			delete(w.installed, path)
			if w.native != nil {
				w.native.remove(path)
			}
		}
	}
	added := false
	for path := range wanted {
		entry, ok := w.snapshot[path]
		if !ok {
			continue
		}
		if _, present := w.installed[path]; present {
			continue
		}
		if w.native == nil {
			native, err := newNativeWatcher()
			if err != nil {
				w.switchToPolling()
				return false
			}
			w.native = native
		}
		if err := w.native.add(path); err != nil {
			// Out of watches or unsupported: compare snapshots from now on, and
			// say coverage was uncertain.
			switch errorName(unwrapPathError(err)) {
			case "ENOENT", "EACCES", "EPERM":
				continue
			}
			w.switchToPolling()
			return false
		}
		w.installed[path] = installed{entry.dev, entry.ino}
		added = true
	}
	return added
}

func (w *fileWatcher) switchToPolling() {
	if w.mode == modePolling {
		return
	}
	w.mode = modePolling
	if w.native != nil {
		w.native.close()
		w.native = nil
	}
	clear(w.installed)
	w.send(object{"kind": "change", "overflow": true, "mode": "polling"})
}

func (w *fileWatcher) inScope(path string) bool {
	for _, target := range w.targets {
		if isWithin(target.path, path) {
			return true
		}
		if !isWithin(path, target.path) || path == target.path {
			continue
		}
		components := strings.Split(path[len(withSeparator(target.path)):], pathSeparator)
		if !target.recursive && len(components) > 1 {
			continue
		}
		if slices.ContainsFunc(components, target.excluded) {
			continue
		}
		return true
	}
	return false
}

// onEvent notes a native event; it returns whether it calls for a rescan.
func (w *fileWatcher) onEvent(event nativeEvent) bool {
	if event.err != nil || event.path == "" {
		// A failed watcher, or events lost: rescan, which also replaces
		// watchers.
		return true
	}
	// Events about unrelated siblings of an ancestor, or about excluded
	// entries, are ignored.
	if !w.inScope(event.path) {
		return false
	}
	w.events[w.reported(event.path)] = true
	return true
}

// flush rescans and reports what changed; an error ends the watcher.
func (w *fileWatcher) flush() bool {
	events := w.events
	w.events = map[string]bool{}
	changed, fail := w.sync(true)
	if fail != nil {
		w.send(object{"kind": "error", "code": fail.code(), "message": fail.failure().message})
		return false
	}
	for path := range events {
		changed[path] = true
	}
	if len(changed) > 0 && !w.cancelled() {
		paths := make([]string, 0, len(changed))
		for path := range changed {
			paths = append(paths, path)
		}
		slices.SortFunc(paths, compareUTF16)
		w.send(object{"kind": "change", "paths": paths})
	}
	return true
}

func parseTargets(json any) ([]watchTarget, *failure) {
	invalid := newFailure("EINVAL", "watch needs targets")
	items, ok := asArray(get(json, "targets"))
	if !ok {
		return nil, invalid
	}
	targets := make([]watchTarget, 0, len(items))
	for _, item := range items {
		path, ok := asString(get(item, "path"))
		if !ok {
			return nil, invalid
		}
		target := watchTarget{path: path, names: map[string]bool{}}
		target.recursive, _ = asBool(get(item, "recursive"))
		exclude := get(item, "exclude")
		target.hidden, _ = asBool(get(exclude, "hidden"))
		names, _ := asArray(get(exclude, "names"))
		for _, name := range names {
			if s, ok := asString(name); ok {
				target.names[s] = true
			}
		}
		targets = append(targets, target)
	}
	return targets, nil
}

// runWatch watches until cancelled: a ready event once coverage is
// established, then change events, or an error event after which nothing
// follows.
func runWatch(id uint32, json any, out *output, ctl *control) (object, *failure) {
	targets, fail := parseTargets(json)
	if fail != nil {
		return nil, fail
	}
	// Windows refuses to rename a directory while a directory below it is
	// open, and native watchers keep watched directories open, so Windows
	// polls, as do file systems that do not report remote changes.
	mode := modeNative
	switch requested, _ := asString(get(json, "mode")); {
	case requested == "native":
	case requested == "polling":
		mode = modePolling
	case runtime.GOOS == "windows" || anyUnreliable(targets):
		mode = modePolling
	}
	pollMs, ok := asU64(get(json, "pollIntervalMs"))
	if !ok {
		pollMs = defaultPollMs
	}
	maxDirectories := defaultMaxDirectory
	if limit, ok := asU64(get(json, "maxDirectories")); ok {
		maxDirectories = int(min(limit, uint64(1<<62)))
	}
	w := &fileWatcher{
		id:             id,
		out:            out,
		ctl:            ctl,
		targets:        targets,
		mode:           mode,
		pollInterval:   millis(pollMs),
		maxDirectories: maxDirectories,
		snapshot:       snapshot{},
		installed:      map[string]installed{},
		events:         map[string]bool{},
	}
	defer func() {
		if w.native != nil {
			w.native.close()
		}
	}()
	if _, fail := w.sync(false); fail != nil {
		return nil, fail.failure()
	}
	w.send(object{"kind": "ready", "mode": w.mode.name()})
	var flushAt time.Time
	pollAt := time.Now().Add(w.pollInterval)
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	for {
		if w.cancelled() {
			return object{}, nil
		}
		now := time.Now()
		wake := now.Add(cancelPoll)
		for _, at := range []time.Time{flushAt, w.settleAt} {
			if !at.IsZero() && at.Before(wake) {
				wake = at
			}
		}
		if w.mode == modePolling && pollAt.Before(wake) {
			wake = pollAt
		}
		timer.Reset(max(wake.Sub(now), 0))
		var ready <-chan struct{}
		if w.native != nil {
			ready = w.native.ready
		}
		select {
		case <-ready:
			for _, event := range w.native.take() {
				if w.onEvent(event) && flushAt.IsZero() && w.mode == modeNative {
					flushAt = time.Now().Add(debounce)
				}
			}
		case <-ctl.wake:
		case <-timer.C:
		}
		now = time.Now()
		due := func(at time.Time) bool { return !at.IsZero() && !at.After(now) }
		pollingDue := w.mode == modePolling && !pollAt.After(now)
		if due(flushAt) || due(w.settleAt) || pollingDue {
			if due(flushAt) {
				flushAt = time.Time{}
			}
			if due(w.settleAt) {
				w.settleAt = time.Time{}
			}
			if !w.flush() {
				return object{}, nil
			}
			if pollingDue {
				pollAt = time.Now().Add(w.pollInterval)
			}
		}
	}
}

// millis is Duration::from_millis, saturating.
func millis(ms uint64) time.Duration {
	if ms > uint64(1<<63-1)/uint64(time.Millisecond) {
		return 1<<63 - 1
	}
	return time.Duration(ms) * time.Millisecond
}

// nativeEvent is a path that changed, or an error (events were lost).
type nativeEvent struct {
	path string
	err  error
}

// nativeWatcher is an fsnotify watcher whose events are queued without
// bound, as the notify crate's callback queues them, so a long rescan never
// makes the kernel drop events.
type nativeWatcher struct {
	watcher *fsnotify.Watcher
	mu      sync.Mutex
	queue   []nativeEvent
	ready   chan struct{}
	done    chan struct{}
}

func newNativeWatcher() (*nativeWatcher, error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	n := &nativeWatcher{watcher: watcher, ready: make(chan struct{}, 1), done: make(chan struct{})}
	go n.pump()
	return n, nil
}

func (n *nativeWatcher) pump() {
	defer close(n.done)
	for {
		var event nativeEvent
		select {
		case e, ok := <-n.watcher.Events:
			if !ok {
				return
			}
			// Lossily, as the notify crate's paths are decoded.
			event.path = decodeUTF8([]byte(e.Name))
		case err, ok := <-n.watcher.Errors:
			if !ok {
				return
			}
			event.err = err
		}
		n.mu.Lock()
		n.queue = append(n.queue, event)
		n.mu.Unlock()
		select {
		case n.ready <- struct{}{}:
		default:
		}
	}
}

func (n *nativeWatcher) take() []nativeEvent {
	n.mu.Lock()
	defer n.mu.Unlock()
	events := n.queue
	n.queue = nil
	return events
}

func (n *nativeWatcher) add(path string) error { return n.watcher.Add(path) }

// remove stops watching path; a watch the kernel already dropped (its
// directory is gone) is not an error, as notify's unwatch result is ignored.
func (n *nativeWatcher) remove(path string) { _ = n.watcher.Remove(path) }

func (n *nativeWatcher) close() {
	_ = n.watcher.Close()
	<-n.done
}
