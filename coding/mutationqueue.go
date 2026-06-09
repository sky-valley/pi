package coding

import (
	"path/filepath"
	"sync"
)

// fileMutationQueue serializes mutating operations that target the same file,
// keyed by resolved (symlink-followed) path, while allowing operations on
// different files to run in parallel. Port of pi's withFileMutationQueue —
// implemented with a per-path mutex rather than a promise chain.
var (
	mutationMu    sync.Mutex
	mutationLocks = map[string]*sync.Mutex{}
)

func mutationQueueKey(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	// Follow symlinks so two paths to the same file share a lock. Missing paths
	// (file not created yet) fall back to the absolute path.
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return real
	}
	return abs
}

// withFileMutationQueue runs fn while holding the per-file lock for path.
func withFileMutationQueue[T any](path string, fn func() (T, error)) (T, error) {
	key := mutationQueueKey(path)

	mutationMu.Lock()
	lock := mutationLocks[key]
	if lock == nil {
		lock = &sync.Mutex{}
		mutationLocks[key] = lock
	}
	mutationMu.Unlock()

	lock.Lock()
	defer lock.Unlock()
	return fn()
}
