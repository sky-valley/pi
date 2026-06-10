package coding

import (
	"errors"
	"io/fs"
	"path/filepath"
	"sync"
	"syscall"
)

// fileMutationQueue serializes mutating operations that target the same file,
// keyed by resolved (symlink-followed) path, while allowing operations on
// different files to run in parallel. Port of pi's withFileMutationQueue —
// implemented with refcounted per-path mutexes rather than a promise chain.
// Entries are deleted once drained (file-mutation-queue.ts:57-59) so the map
// does not grow without bound.
type mutationLock struct {
	mu   sync.Mutex
	refs int
}

var (
	mutationMu    sync.Mutex
	mutationLocks = map[string]*mutationLock{}
)

// isMissingPathError mirrors pi's isMissingPathError (ENOENT or ENOTDIR).
func isMissingPathError(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR)
}

func mutationQueueKey(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	// Follow symlinks so two paths to the same file share a lock. Missing paths
	// (file not created yet) fall back to the absolute path; any other realpath
	// error propagates like pi's getMutationQueueKey.
	real, err := filepath.EvalSymlinks(abs)
	if err == nil {
		return real, nil
	}
	if isMissingPathError(err) {
		return abs, nil
	}
	return "", err
}

// withFileMutationQueue runs fn while holding the per-file lock for path.
func withFileMutationQueue[T any](path string, fn func() (T, error)) (T, error) {
	key, err := mutationQueueKey(path)
	if err != nil {
		var zero T
		return zero, err
	}

	mutationMu.Lock()
	lock := mutationLocks[key]
	if lock == nil {
		lock = &mutationLock{}
		mutationLocks[key] = lock
	}
	lock.refs++
	mutationMu.Unlock()

	lock.mu.Lock()
	defer func() {
		lock.mu.Unlock()
		mutationMu.Lock()
		lock.refs--
		if lock.refs == 0 {
			delete(mutationLocks, key)
		}
		mutationMu.Unlock()
	}()
	return fn()
}
