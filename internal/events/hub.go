// Package events is the in-process broadcast behind /api/events: producers publish named
// events, every connected UI gets them over SSE. A slow client loses events instead of slowing
// the server down (the UI resyncs from REST when it reconnects or sees a "dropped" marker).
package events

import (
	"sync"
	"time"
)

type Event struct {
	Name string
	Data any
}

type Hub struct {
	mu   sync.Mutex
	subs map[*Sub]struct{}
}

type Sub struct {
	C       chan Event
	mu      sync.Mutex
	dropped int
}

func NewHub() *Hub { return &Hub{subs: map[*Sub]struct{}{}} }

// Subscribe returns a subscription with a buffer of size events; call Unsubscribe when done.
func (h *Hub) Subscribe(size int) *Sub {
	s := &Sub{C: make(chan Event, size)}
	h.mu.Lock()
	h.subs[s] = struct{}{}
	h.mu.Unlock()
	return s
}

func (h *Hub) Unsubscribe(s *Sub) {
	h.mu.Lock()
	delete(h.subs, s)
	h.mu.Unlock()
}

// Publish never blocks.
func (h *Hub) Publish(name string, data any) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for s := range h.subs {
		select {
		case s.C <- Event{name, data}:
		default:
			s.mu.Lock()
			s.dropped++
			s.mu.Unlock()
		}
	}
}

// Dropped returns and resets the number of events lost since the last call.
func (s *Sub) Dropped() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.dropped
	s.dropped = 0
	return n
}

func (h *Hub) Subscribers() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}

// Debouncer runs fn once after calls stop for d (bursts of Docker events → one refresh).
type Debouncer struct {
	mu sync.Mutex
	t  *time.Timer
	d  time.Duration
	fn func()
}

func NewDebouncer(d time.Duration, fn func()) *Debouncer { return &Debouncer{d: d, fn: fn} }

func (b *Debouncer) Trigger() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.t != nil {
		b.t.Stop()
	}
	b.t = time.AfterFunc(b.d, b.fn)
}
