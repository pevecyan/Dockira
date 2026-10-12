package events

import (
	"sync/atomic"
	"testing"
	"time"
)

func TestPublishAndDrop(t *testing.T) {
	h := NewHub()
	fast := h.Subscribe(10)
	slow := h.Subscribe(1)
	for i := range 3 {
		h.Publish("tick", i)
	}
	if len(fast.C) != 3 || len(slow.C) != 1 {
		t.Fatalf("fast %d slow %d", len(fast.C), len(slow.C))
	}
	if n := slow.Dropped(); n != 2 {
		t.Errorf("dropped %d", n)
	}
	if n := slow.Dropped(); n != 0 {
		t.Errorf("dropped not reset: %d", n)
	}
	h.Unsubscribe(fast)
	h.Publish("x", nil)
	if len(fast.C) != 3 || h.Subscribers() != 1 {
		t.Error("unsubscribed client still receives")
	}
}

func TestDebouncer(t *testing.T) {
	var n atomic.Int32
	d := NewDebouncer(30*time.Millisecond, func() { n.Add(1) })
	for range 5 {
		d.Trigger()
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(80 * time.Millisecond)
	if n.Load() != 1 {
		t.Errorf("ran %d times", n.Load())
	}
}
