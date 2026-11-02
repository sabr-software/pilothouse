package daemon

import (
	"sync"

	"github.com/sabr-software/pilothouse/internal/api"
)

// waiters connects permission hooks that are holding for a decision with
// whoever closes their request (Decide, presence release, shutdown, the
// hook's own timeout). Each closer resolves the waiter after its
// compare-and-set commits, so exactly one result is ever delivered.
type waiters struct {
	mu     sync.Mutex
	m      map[string]chan api.Request
	closed bool
}

func newWaiters() *waiters { return &waiters{m: map[string]chan api.Request{}} }

// add registers a waiter for request id. It returns false once the daemon
// is shutting down, so no hook starts holding after the final release.
func (w *waiters) add(id string) (<-chan api.Request, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil, false
	}
	ch := make(chan api.Request, 1)
	w.m[id] = ch
	return ch, true
}

// resolve delivers the closed request to its waiter, if any.
func (w *waiters) resolve(id string, r api.Request) {
	w.mu.Lock()
	ch, ok := w.m[id]
	delete(w.m, id)
	w.mu.Unlock()
	if ok {
		ch <- r // buffered, and only ever sent once
	}
}

func (w *waiters) remove(id string) {
	w.mu.Lock()
	delete(w.m, id)
	w.mu.Unlock()
}

// ids lists the requests currently held.
func (w *waiters) ids() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]string, 0, len(w.m))
	for id := range w.m {
		out = append(out, id)
	}
	return out
}

// close stops new registrations.
func (w *waiters) close() {
	w.mu.Lock()
	w.closed = true
	w.mu.Unlock()
}
