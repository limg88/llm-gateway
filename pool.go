package main

import (
	"sync"
	"time"
)

// warmProc is a process that can wait idle for a request.
type warmProc interface {
	kill()
	exited() <-chan struct{}
}

type warmEntry[P warmProc] struct {
	key   string
	proc  P
	timer *time.Timer
}

// warmPool keeps processes started ahead of time. A process is bound to a
// key (its command line): a request takes one only with the same key. The
// pool holds at most size processes, replacing the oldest, and stops a
// process that stays idle for too long.
type warmPool[P warmProc] struct {
	size int
	idle time.Duration

	mu       sync.Mutex
	procs    []*warmEntry[P]
	starting int
	closed   bool
	hits     int
	misses   int
}

func newWarmPool[P warmProc](size int, idle time.Duration) *warmPool[P] {
	return &warmPool[P]{size: size, idle: idle}
}

// take returns a live process started for key, if there is one.
func (w *warmPool[P]) take(key string) (proc P, ok bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for i, e := range w.procs {
		if e.key != key {
			continue
		}
		w.procs = append(w.procs[:i], w.procs[i+1:]...)
		e.timer.Stop()
		select {
		case <-e.proc.exited():
			w.misses++
			return proc, false
		default:
		}
		w.hits++
		return e.proc, true
	}
	w.misses++
	return proc, false
}

// add starts, in the background, a process for key unless one is waiting
// already.
func (w *warmPool[P]) add(key string, start func() (P, error)) {
	w.mu.Lock()
	if w.closed || w.size <= 0 || w.starting >= w.size {
		w.mu.Unlock()
		return
	}
	for _, e := range w.procs {
		if e.key == key {
			w.mu.Unlock()
			return
		}
	}
	w.starting++
	w.mu.Unlock()

	go func() {
		proc, err := start()
		w.mu.Lock()
		w.starting--
		if err != nil {
			w.mu.Unlock()
			return
		}
		if w.closed {
			w.mu.Unlock()
			proc.kill()
			return
		}
		var evicted []P
		for len(w.procs) >= w.size {
			evicted = append(evicted, w.procs[0].proc)
			w.procs[0].timer.Stop()
			w.procs = w.procs[1:]
		}
		e := &warmEntry[P]{key: key, proc: proc}
		e.timer = time.AfterFunc(w.idle, func() { w.drop(e) })
		w.procs = append(w.procs, e)
		w.mu.Unlock()
		for _, p := range evicted {
			p.kill()
		}
		// A process that dies while idle leaves the pool.
		go func() {
			<-proc.exited()
			w.drop(e)
		}()
	}()
}

func (w *warmPool[P]) drop(e *warmEntry[P]) {
	w.mu.Lock()
	found := false
	for i, x := range w.procs {
		if x == e {
			w.procs = append(w.procs[:i], w.procs[i+1:]...)
			found = true
			break
		}
	}
	w.mu.Unlock()
	if found {
		e.timer.Stop()
		e.proc.kill()
	}
}

func (w *warmPool[P]) stats() (idle, hits, misses int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.procs), w.hits, w.misses
}

func (w *warmPool[P]) close() {
	w.mu.Lock()
	procs := w.procs
	w.procs, w.closed = nil, true
	w.mu.Unlock()
	for _, e := range procs {
		e.timer.Stop()
		e.proc.kill()
	}
}
