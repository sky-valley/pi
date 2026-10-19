package env

import "sync"

// serial calls fn with the values pushed to it, in order and one call at a
// time, on a goroutine of its own that runs while values wait. Whoever
// pushes never waits for fn, so fn may call back into it, as upstream's
// callbacks may, which run on its one thread and get promises back.
type serial[T any] struct {
	fn      func(T)
	mu      sync.Mutex
	queue   []T
	running bool
}

func (s *serial[T]) push(value T) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.queue = append(s.queue, value)
	if !s.running {
		s.running = true
		go s.run()
	}
}

// clear drops the values fn has yet to receive: no call starts for them.
func (s *serial[T]) clear() {
	s.mu.Lock()
	s.queue = nil
	s.mu.Unlock()
}

func (s *serial[T]) run() {
	for {
		s.mu.Lock()
		if len(s.queue) == 0 {
			s.queue = nil
			s.running = false
			s.mu.Unlock()
			return
		}
		value := s.queue[0]
		var zero T
		s.queue[0] = zero // the queue does not keep what it passed on
		s.queue = s.queue[1:]
		s.mu.Unlock()
		s.fn(value)
	}
}
