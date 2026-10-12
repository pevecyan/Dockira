package notify

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"time"
)

// Source provides the current channels and rules (from SQLite, secrets decrypted). It is asked
// on every Dispatch, so edits in the UI apply to the next event without a reload step.
type Source interface {
	Channels(ctx context.Context) ([]Channel, error)
	Rules(ctx context.Context) ([]Rule, error)
}

// Delivery is one send attempt, for notify_log and the channel's "last result" in the UI.
type Delivery struct {
	Time      time.Time
	ChannelID int64
	Event     Event
	DedupeKey string
	Attempt   int // 1-based
	OK        bool
	Err       string
}

type DispatcherOptions struct {
	HTTPClient *http.Client                    // nil: a client with DefaultTimeout
	Retries    int                             // attempts per delivery; 0 = 3
	Backoff    func(attempt int) time.Duration // wait after failed attempt n; nil = 2s, then 8s
	// OnDelivery is called after every attempt, failed or not, from the delivering goroutine
	// (so it must be safe for concurrent use). Events suppressed by quiet hours or dedupe are
	// not attempts and are not reported.
	OnDelivery func(Delivery)
	Now        func() time.Time // nil = time.Now; quiet hours use this time's location
	Logger     *slog.Logger     // nil = slog.Default(); for errors with no channel to report on
}

// Dispatcher evaluates rules, suppresses repeats, and delivers with retries.
type Dispatcher struct {
	src  Source
	o    DispatcherOptions
	log  *slog.Logger
	sem  chan struct{} // bounds concurrent sends
	wg   sync.WaitGroup
	mu   sync.Mutex
	sent map[string]time.Time // dedupe key → repeats suppressed until
}

const (
	maxConcurrentSends = 8
	sourceTimeout      = 10 * time.Second
	attemptTimeout     = 30 * time.Second // on top of the client's own timeout
)

func NewDispatcher(src Source, o DispatcherOptions) *Dispatcher {
	if o.HTTPClient == nil {
		o.HTTPClient = &http.Client{Timeout: DefaultTimeout}
	}
	if o.Retries <= 0 {
		o.Retries = 3
	}
	if o.Backoff == nil {
		o.Backoff = func(attempt int) time.Duration {
			if attempt <= 1 {
				return 2 * time.Second
			}
			return 8 * time.Second
		}
	}
	if o.OnDelivery == nil {
		o.OnDelivery = func(Delivery) {}
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	log := o.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Dispatcher{src: src, o: o, log: log, sem: make(chan struct{}, maxConcurrentSends), sent: map[string]time.Time{}}
}

// Dispatch delivers e to every enabled channel with a matching rule, asynchronously: it returns
// at once, the work happens in background goroutines (see Wait). A channel gets the event at
// most once even if several of its rules match. A rule in quiet hours doesn't deliver, unless
// the event is a security event. Repeats (same type, stack, channel and Fields["dedupe"]) are
// suppressed for the smallest dedupe window among the delivering rules; a delivery that fails
// on every attempt doesn't count, so the next occurrence is tried again. Safe for concurrent use.
func (d *Dispatcher) Dispatch(e Event) {
	if e.Time.IsZero() {
		e.Time = d.o.Now()
	}
	d.wg.Go(func() { d.dispatch(e) })
}

type target struct {
	ch     Channel
	window time.Duration
}

func (d *Dispatcher) dispatch(e Event) {
	ctx, cancel := context.WithTimeout(context.Background(), sourceTimeout)
	defer cancel()
	channels, err := d.src.Channels(ctx)
	if err != nil {
		d.log.Error("notify: loading channels", "event", e.Type, "err", err)
		return
	}
	rules, err := d.src.Rules(ctx)
	if err != nil {
		d.log.Error("notify: loading rules", "event", e.Type, "err", err)
		return
	}

	byID := make(map[int64]Channel, len(channels))
	for _, ch := range channels {
		byID[ch.ID] = ch
	}
	now := d.o.Now()
	targets := map[int64]*target{}
	for _, r := range rules {
		ch, ok := byID[r.ChannelID]
		if !ok || !ch.Enabled || !r.Matches(e) {
			continue
		}
		if !e.Security() && r.QuietHours.Active(now) {
			continue
		}
		w := r.dedupeWindow()
		if t := targets[ch.ID]; t == nil {
			targets[ch.ID] = &target{ch: ch, window: w}
		} else if w < t.window {
			t.window = w
		}
	}

	ids := make([]int64, 0, len(targets))
	for id := range targets {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		t := targets[id]
		key := dedupeKey(e, id)
		until, ok := d.claim(key, now, t.window)
		if !ok {
			continue
		}
		d.wg.Go(func() {
			if !d.deliver(t.ch, e, key) {
				d.release(key, until)
			}
		})
	}
}

func dedupeKey(e Event, channelID int64) string {
	k := e.Type + "|" + e.Stack + "|" + strconv.FormatInt(channelID, 10)
	if v := e.Fields["dedupe"]; v != "" {
		k += "|" + v
	}
	return k
}

// claim records a send for key unless one happened within the window. It is decided before
// sending, so two concurrent Dispatches of the same event can't both get through.
func (d *Dispatcher) claim(key string, now time.Time, window time.Duration) (time.Time, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for k, until := range d.sent {
		if !now.Before(until) {
			delete(d.sent, k)
		}
	}
	if until, ok := d.sent[key]; ok && now.Before(until) {
		return time.Time{}, false
	}
	if window <= 0 {
		return time.Time{}, true
	}
	until := now.Add(window)
	d.sent[key] = until
	return until, true
}

// release forgets a failed send, unless a later one has claimed the key since.
func (d *Dispatcher) release(key string, until time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !until.IsZero() && d.sent[key].Equal(until) {
		delete(d.sent, key)
	}
}

// deliver tries up to Retries times and reports each attempt. It returns whether one succeeded.
func (d *Dispatcher) deliver(ch Channel, e Event, key string) bool {
	s, err := NewSender(ch, d.o.HTTPClient)
	if err != nil { // config is broken; retrying won't fix it
		d.report(ch, e, key, 1, fmt.Errorf("invalid channel config: %w", err))
		return false
	}
	for attempt := 1; ; attempt++ {
		d.sem <- struct{}{}
		ctx, cancel := context.WithTimeout(context.Background(), attemptTimeout)
		err := s.Send(ctx, e)
		cancel()
		<-d.sem
		d.report(ch, e, key, attempt, err)
		if err == nil {
			return true
		}
		if attempt >= d.o.Retries {
			return false
		}
		time.Sleep(d.o.Backoff(attempt))
	}
}

func (d *Dispatcher) report(ch Channel, e Event, key string, attempt int, err error) {
	dl := Delivery{Time: d.o.Now(), ChannelID: ch.ID, Event: e, DedupeKey: key, Attempt: attempt, OK: err == nil}
	if err != nil {
		dl.Err = err.Error()
	}
	d.o.OnDelivery(dl)
}

// SendTest sends a "test" event to ch once, synchronously, ignoring rules, dedupe, quiet hours
// and the Enabled flag (so a channel can be tried before it is switched on). The attempt is
// reported to OnDelivery like any other; the error is returned for the UI.
func (d *Dispatcher) SendTest(ctx context.Context, ch Channel) error {
	s, err := NewSender(ch, d.o.HTTPClient)
	if err != nil {
		return err
	}
	e := Event{
		Type:     TypeTest,
		Severity: Info,
		Title:    "Test notification",
		Message:  fmt.Sprintf("Channel %q works: Dockira can reach it.", ch.Name),
		Time:     d.o.Now(),
	}
	err = s.Send(ctx, e)
	d.report(ch, e, "", 1, err)
	return err
}

// Wait blocks until every delivery started so far has finished, retries included. For
// shutdown and tests; Dispatch must not be called concurrently with the end of Wait.
func (d *Dispatcher) Wait() { d.wg.Wait() }
