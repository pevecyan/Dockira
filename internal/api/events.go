package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

const pingInterval = 20 * time.Second

// sse starts an event stream and returns its send function. The ResponseController sees
// through middleware wrappers (Unwrap) to the connection's Flusher.
func sse(w http.ResponseWriter) (func(event string, data any) error, bool) {
	rc := http.NewResponseController(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no") // nginx: do not buffer the stream
	w.WriteHeader(http.StatusOK)
	if err := rc.Flush(); err != nil {
		return nil, false
	}
	return func(event string, data any) error {
		b, err := json.Marshal(data)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b); err != nil {
			return err
		}
		return rc.Flush()
	}, true
}

// events is the UI's live channel: a snapshot on connect, then whatever the hub publishes
// (stacks, host and container stats, feed lines, job progress), with a ping to keep proxies
// from closing an idle stream.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	sub := s.Hub.Subscribe(256)
	defer s.Hub.Unsubscribe(sub)
	send, ok := sse(w)
	if !ok {
		return
	}
	if send("hello", map[string]string{"version": s.Version}) != nil || send("stacks", s.stackList()) != nil {
		return
	}
	if s.Updates != nil && send("updates", s.Updates.Summary(r.Context())) != nil {
		return
	}
	t := time.NewTicker(pingInterval)
	defer t.Stop()
	for {
		var err error
		select {
		case <-r.Context().Done():
			return
		case ev := <-sub.C:
			err = send(ev.Name, ev.Data)
			if n := sub.Dropped(); n > 0 && err == nil {
				err = send("dropped", map[string]int{"count": n}) // the UI reloads its state
			}
		case now := <-t.C:
			err = send("ping", map[string]time.Time{"time": now})
		}
		if err != nil {
			return
		}
	}
}
