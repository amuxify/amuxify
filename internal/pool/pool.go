// Package pool runs the per-file work of a walk on a bounded number of
// goroutines. It is the one place the --jobs flag turns into concurrency:
// scan, remux, clean and ingest hand it the walked paths and a function that
// processes one of them, and it decides which items may run at the same time.
//
// Two rules keep a parallel run as safe as a sequential one. Items that share
// a key never run at the same time and run in walk order, so two names of one
// inode or two sources that map to one output are handled one after the
// other, exactly as they would be with --jobs 1. And a claim set lets the
// stages reserve a destination before they create anything under it, so two
// workers cannot both write the same temp name or place the same file.
package pool

import (
	"context"
	"sync"
)

// Run calls fn(i) for every i in [0, n) using at most jobs goroutines and
// returns, per item, whether fn ran for it. keys[i] lists the serialisation
// keys of item i; it may be nil, and keys itself may be nil when nothing
// needs serialising. Items that share a key run one at a time in index order.
//
// With jobs of one or less every item runs inline on the calling goroutine,
// in order, exactly as a plain loop would, and the context is checked before
// each item. With more jobs the items are started as workers become free and
// their keys allow; an item is never started after ctx is done, and Run
// returns only after every item it started has finished, so nothing runs
// past the return. The items that did not run are those the caller reports
// as interrupted.
func Run(ctx context.Context, jobs, n int, keys [][]string, fn func(i int)) []bool {
	ran := make([]bool, n)
	if n == 0 {
		return ran
	}
	if jobs > n {
		jobs = n
	}
	if jobs <= 1 {
		for i := 0; i < n; i++ {
			if ctx.Err() != nil {
				return ran
			}
			fn(i)
			ran[i] = true
		}
		return ran
	}

	s := newScheduler(n, keys)
	// One goroutine turns the context's cancellation into a broadcast so
	// idle workers wake up and leave; it is released when Run returns.
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		select {
		case <-ctx.Done():
			s.cancel()
		case <-stop:
		}
	}()
	for w := 0; w < jobs; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i, ok := s.next(ctx)
				if !ok {
					return
				}
				fn(i)
				s.finish(i)
			}
		}()
	}
	// The workers exit once every item is finished or the run is cancelled
	// and the started items have completed; the stop channel then releases
	// the cancellation goroutine when the context was never cancelled.
	s.wait()
	close(stop)
	wg.Wait()
	for i := range ran {
		ran[i] = s.done[i]
	}
	return ran
}

// scheduler hands out items in index order subject to the key rule. For
// every key it keeps the ordered list of items that carry it and the position
// of the first one not yet finished; an item may start only when it is at
// that position for each of its keys. Since the lowest unstarted index is
// always eligible once everything before it has finished, a worker can
// always make progress and the rule cannot deadlock.
type scheduler struct {
	mu        sync.Mutex
	cond      *sync.Cond
	keys      [][]string
	order     map[string][]int
	pos       map[string]int
	started   []bool
	done      []bool
	low       int // lowest index not yet started
	remaining int // items not yet finished
	cancelled bool
}

func newScheduler(n int, keys [][]string) *scheduler {
	s := &scheduler{
		keys:      keys,
		order:     map[string][]int{},
		pos:       map[string]int{},
		started:   make([]bool, n),
		done:      make([]bool, n),
		remaining: n,
	}
	s.cond = sync.NewCond(&s.mu)
	for i := 0; i < n && i < len(keys); i++ {
		for _, k := range keys[i] {
			s.order[k] = append(s.order[k], i)
		}
	}
	return s
}

// eligible reports whether item i may start now: not started, and at the
// head of every key it carries.
func (s *scheduler) eligible(i int) bool {
	if s.started[i] {
		return false
	}
	if i < len(s.keys) {
		for _, k := range s.keys[i] {
			if s.order[k][s.pos[k]] != i {
				return false
			}
		}
	}
	return true
}

// next blocks until an item may start and returns its index, or false when
// there is nothing left to start: every item is finished, or the run was
// cancelled. A cancelled run starts nothing more.
func (s *scheduler) next(ctx context.Context) (int, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for {
		if s.cancelled || ctx.Err() != nil || s.low >= len(s.started) {
			return 0, false
		}
		for i := s.low; i < len(s.started); i++ {
			if s.eligible(i) {
				s.started[i] = true
				for s.low < len(s.started) && s.started[s.low] {
					s.low++
				}
				return i, true
			}
		}
		s.cond.Wait()
	}
}

// finish records that item i completed and advances the head of each of its
// keys, which may make later items eligible.
func (s *scheduler) finish(i int) {
	s.mu.Lock()
	s.done[i] = true
	s.remaining--
	if i < len(s.keys) {
		for _, k := range s.keys[i] {
			s.pos[k]++
		}
	}
	s.cond.Broadcast()
	s.mu.Unlock()
}

// cancel stops further items from starting and wakes the waiting workers.
func (s *scheduler) cancel() {
	s.mu.Lock()
	s.cancelled = true
	s.cond.Broadcast()
	s.mu.Unlock()
}

// wait returns once every item has finished, or once the run was cancelled
// and every started item has finished.
func (s *scheduler) wait() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for {
		if s.remaining == 0 {
			return
		}
		if s.cancelled {
			running := false
			for i := range s.started {
				if s.started[i] && !s.done[i] {
					running = true
					break
				}
			}
			if !running {
				return
			}
		}
		s.cond.Wait()
	}
}

// Claims is a run-wide set of destinations that a worker has reserved. A
// stage claims the path it is about to create before it makes a temp file or
// places anything, and releases it only when it wrote nothing, so a second
// item of the same run that maps to the same path meets the claim rather
// than the other worker's half-written file. The zero value is ready to use.
type Claims struct {
	mu   sync.Mutex
	held map[string]struct{}
}

// Claim reserves key and reports whether it was free.
func (c *Claims) Claim(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.held == nil {
		c.held = map[string]struct{}{}
	}
	if _, taken := c.held[key]; taken {
		return false
	}
	c.held[key] = struct{}{}
	return true
}

// Release gives key back. Releasing a key that is not held is harmless.
func (c *Claims) Release(key string) {
	c.mu.Lock()
	delete(c.held, key)
	c.mu.Unlock()
}

// Held reports whether key is currently claimed.
func (c *Claims) Held(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, taken := c.held[key]
	return taken
}
