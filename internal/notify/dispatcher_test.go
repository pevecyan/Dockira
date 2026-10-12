package notify

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeSource struct {
	channels []Channel
	rules    []Rule
	err      error
}

func (f *fakeSource) Channels(context.Context) ([]Channel, error) { return f.channels, f.err }
func (f *fakeSource) Rules(context.Context) ([]Rule, error)       { return f.rules, f.err }

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type deliveries struct {
	mu   sync.Mutex
	list []Delivery
}

func (d *deliveries) add(x Delivery) { d.mu.Lock(); d.list = append(d.list, x); d.mu.Unlock() }
func (d *deliveries) all() []Delivery {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]Delivery(nil), d.list...)
}

// hook returns a webhook channel posting to a fresh recorder.
func hook(t *testing.T, id int64) (Channel, *recorder) {
	rec, srv := newServer(t)
	return Channel{ID: id, Type: TypeWebhook, Name: "hook", Enabled: true, Config: cfg(WebhookConfig{URL: srv.URL})}, rec
}

type env struct {
	d     *Dispatcher
	src   *fakeSource
	clk   *clock
	dels  *deliveries
	recs  map[int64]*recorder
	chans map[int64]Channel
}

func newEnv(t *testing.T, ids ...int64) *env {
	e := &env{src: &fakeSource{}, clk: &clock{t: time.Date(2026, 10, 11, 12, 0, 0, 0, time.Local)},
		dels: &deliveries{}, recs: map[int64]*recorder{}, chans: map[int64]Channel{}}
	for _, id := range ids {
		ch, rec := hook(t, id)
		e.src.channels = append(e.src.channels, ch)
		e.recs[id], e.chans[id] = rec, ch
	}
	e.d = NewDispatcher(e.src, DispatcherOptions{
		Backoff:    func(int) time.Duration { return 0 },
		OnDelivery: e.dels.add,
		Now:        e.clk.Now,
	})
	return e
}

func (e *env) dispatch(ev Event)  { e.d.Dispatch(ev); e.d.Wait() }
func (e *env) count(id int64) int { return len(e.recs[id].all()) }

func TestDispatchRouting(t *testing.T) {
	e := newEnv(t, 1, 2, 3)
	e.src.channels[2].Enabled = false // channel 3
	e.src.rules = []Rule{
		{ChannelID: 1, MinSeverity: Warning},
		{ChannelID: 1, Events: []string{"container.*"}}, // also matches: still one delivery
		{ChannelID: 2, Events: []string{"update.*"}},
		{ChannelID: 3},  // disabled channel
		{ChannelID: 99}, // unknown channel
	}
	e.dispatch(Event{Type: "container.died", Severity: Error, Stack: "web"})
	if e.count(1) != 1 || e.count(2) != 0 || e.count(3) != 0 {
		t.Errorf("counts: %d %d %d", e.count(1), e.count(2), e.count(3))
	}
	e.dispatch(Event{Type: "update.applied", Severity: Info})
	if e.count(1) != 1 || e.count(2) != 1 {
		t.Errorf("counts: %d %d", e.count(1), e.count(2))
	}
	ds := e.dels.all()
	if len(ds) != 2 || !ds[0].OK || ds[0].Attempt != 1 || ds[0].DedupeKey != "container.died|web|1" || ds[0].Event.Time.IsZero() {
		t.Errorf("deliveries: %+v", ds)
	}
}

func TestDispatchDedupe(t *testing.T) {
	e := newEnv(t, 1, 2)
	e.src.rules = []Rule{
		{ChannelID: 1}, // default 60 min
		{ChannelID: 1, Events: []string{"job.*"}, DedupeMinutes: 10}, // smallest matching window wins
		{ChannelID: 2, DedupeMinutes: -1},                            // never suppress
	}
	unhealthy := Event{Type: "container.unhealthy", Severity: Error, Stack: "web"}
	e.dispatch(unhealthy)
	e.clk.Add(59 * time.Minute)
	e.dispatch(unhealthy)
	if e.count(1) != 1 || e.count(2) != 2 {
		t.Fatalf("within window: %d %d", e.count(1), e.count(2))
	}
	other := unhealthy
	other.Stack = "db" // different stack: different key
	e.dispatch(other)
	withField := unhealthy
	withField.Fields = map[string]string{"dedupe": "container-b"}
	e.dispatch(withField)
	if e.count(1) != 3 {
		t.Fatalf("different stack/dedupe field should get through: %d", e.count(1))
	}
	e.clk.Add(time.Minute) // 60 min after the first
	e.dispatch(unhealthy)
	if e.count(1) != 4 {
		t.Fatalf("after window: %d", e.count(1))
	}

	job := Event{Type: "job.failed", Severity: Warning}
	e.dispatch(job)
	e.clk.Add(9 * time.Minute)
	e.dispatch(job)
	e.clk.Add(time.Minute)
	e.dispatch(job)
	if e.count(1) != 6 {
		t.Fatalf("10-minute window: %d", e.count(1))
	}
	// Suppressed events are not reported as deliveries.
	if n := len(e.dels.all()); n != e.count(1)+e.count(2) {
		t.Errorf("deliveries %d, sends %d", n, e.count(1)+e.count(2))
	}
}

func TestDispatchFailureIsNotDeduped(t *testing.T) {
	e := newEnv(t, 1)
	e.recs[1].status = http.StatusInternalServerError
	e.src.rules = []Rule{{ChannelID: 1}}
	ev := Event{Type: "job.failed", Severity: Warning}
	e.dispatch(ev)
	e.recs[1].mu.Lock()
	e.recs[1].status = 0
	e.recs[1].mu.Unlock()
	e.dispatch(ev)
	if e.count(1) != 4 { // 3 failed attempts + 1 success
		t.Fatalf("sends: %d", e.count(1))
	}
	e.dispatch(ev)
	if e.count(1) != 4 {
		t.Fatalf("successful send should now be deduped: %d", e.count(1))
	}
}

func TestDispatchQuietHours(t *testing.T) {
	e := newEnv(t, 1, 2)
	e.clk.t = time.Date(2026, 10, 11, 23, 30, 0, 0, time.Local)
	q := &QuietHours{Start: "22:00", End: "07:00"}
	e.src.rules = []Rule{
		{ChannelID: 1, QuietHours: q},
		{ChannelID: 2, QuietHours: q},
		{ChannelID: 2, Events: []string{"update.failed"}}, // another rule for 2 without quiet hours
	}
	e.dispatch(Event{Type: "container.died", Severity: Error})
	if e.count(1) != 0 || e.count(2) != 0 {
		t.Fatalf("quiet: %d %d", e.count(1), e.count(2))
	}
	e.dispatch(Event{Type: "auth.lockout", Severity: Warning})
	if e.count(1) != 1 || e.count(2) != 1 {
		t.Fatalf("security events must get through: %d %d", e.count(1), e.count(2))
	}
	e.dispatch(Event{Type: "update.failed", Severity: Error})
	if e.count(1) != 1 || e.count(2) != 2 {
		t.Fatalf("non-quiet rule should deliver: %d %d", e.count(1), e.count(2))
	}
	e.clk.t = time.Date(2026, 10, 12, 7, 0, 0, 0, time.Local)
	e.dispatch(Event{Type: "container.died", Severity: Error})
	if e.count(1) != 2 {
		t.Fatalf("after quiet hours: %d", e.count(1))
	}
}

func TestDispatchRetries(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) <= 2 {
			http.Error(w, "busy", http.StatusServiceUnavailable)
		}
	}))
	defer srv.Close()
	src := &fakeSource{
		channels: []Channel{{ID: 1, Type: TypeWebhook, Name: "h", Enabled: true, Config: cfg(WebhookConfig{URL: srv.URL})}},
		rules:    []Rule{{ChannelID: 1}},
	}
	dels := &deliveries{}
	var backoffs []int
	var mu sync.Mutex
	d := NewDispatcher(src, DispatcherOptions{
		OnDelivery: dels.add,
		Backoff: func(n int) time.Duration {
			mu.Lock()
			backoffs = append(backoffs, n)
			mu.Unlock()
			return time.Millisecond
		},
	})
	d.Dispatch(Event{Type: "job.failed", Severity: Warning})
	d.Wait()
	if calls.Load() != 3 {
		t.Fatalf("calls = %d", calls.Load())
	}
	ds := dels.all()
	if len(ds) != 3 || ds[0].OK || ds[1].OK || !ds[2].OK || ds[2].Attempt != 3 || !strings.Contains(ds[0].Err, "HTTP 503: busy") {
		t.Fatalf("deliveries: %+v", ds)
	}
	if len(backoffs) != 2 || backoffs[0] != 1 || backoffs[1] != 2 {
		t.Errorf("backoffs: %v", backoffs)
	}
}

func TestDispatchGivesUpAfterRetries(t *testing.T) {
	e := newEnv(t, 1)
	e.recs[1].status = http.StatusBadRequest
	e.src.rules = []Rule{{ChannelID: 1}}
	e.d.o.Retries = 2
	e.dispatch(Event{Type: "job.failed", Severity: Warning})
	if ds := e.dels.all(); len(ds) != 2 || ds[1].OK || ds[1].Attempt != 2 {
		t.Fatalf("%+v", ds)
	}
}

func TestDispatchInvalidChannelReported(t *testing.T) {
	e := newEnv(t)
	e.src.channels = []Channel{{ID: 5, Type: TypeGotify, Name: "g", Enabled: true, Config: cfg(GotifyConfig{})}}
	e.src.rules = []Rule{{ChannelID: 5}}
	e.dispatch(Event{Type: "job.failed", Severity: Warning})
	ds := e.dels.all()
	if len(ds) != 1 || ds[0].OK || !strings.Contains(ds[0].Err, "invalid channel config") {
		t.Fatalf("%+v", ds)
	}
}

func TestDispatchSourceError(t *testing.T) {
	e := newEnv(t, 1)
	e.src.rules = []Rule{{ChannelID: 1}}
	e.src.err = errors.New("db locked")
	e.dispatch(Event{Type: "job.failed", Severity: Warning})
	if e.count(1) != 0 || len(e.dels.all()) != 0 {
		t.Fatal("sent despite source error")
	}
}

func TestDispatchConcurrent(t *testing.T) {
	e := newEnv(t, 1, 2)
	e.src.rules = []Rule{{ChannelID: 1}, {ChannelID: 2, DedupeMinutes: -1}}
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() { e.d.Dispatch(Event{Type: "container.died", Severity: Error, Stack: "web"}) })
	}
	wg.Wait()
	e.d.Wait()
	if e.count(1) != 1 || e.count(2) != 20 {
		t.Fatalf("counts: %d %d", e.count(1), e.count(2))
	}
}

func TestSendTest(t *testing.T) {
	e := newEnv(t, 1)
	ch := e.chans[1]
	ch.Enabled = false // tests work on disabled channels, without any rule
	e.clk.t = time.Date(2026, 10, 11, 23, 0, 0, 0, time.Local)
	if err := e.d.SendTest(context.Background(), ch); err != nil {
		t.Fatal(err)
	}
	if err := e.d.SendTest(context.Background(), ch); err != nil { // no dedupe
		t.Fatal(err)
	}
	reqs := e.recs[1].all()
	if len(reqs) != 2 || !strings.Contains(reqs[0].Body, `"type":"test"`) {
		t.Fatalf("%+v", reqs)
	}
	ds := e.dels.all()
	if len(ds) != 2 || !ds[0].OK || ds[0].Event.Type != TypeTest {
		t.Fatalf("%+v", ds)
	}

	e.recs[1].mu.Lock()
	e.recs[1].status = http.StatusForbidden
	e.recs[1].reply = "nope"
	e.recs[1].mu.Unlock()
	err := e.d.SendTest(context.Background(), ch)
	if err == nil || err.Error() != "webhook: HTTP 403: nope" {
		t.Fatalf("err = %v", err)
	}
	if len(e.recs[1].all()) != 3 {
		t.Fatal("SendTest should not retry")
	}

	if err := e.d.SendTest(context.Background(), Channel{Type: TypeGotify, Name: "g", Config: cfg(GotifyConfig{})}); err == nil {
		t.Fatal("invalid channel accepted")
	}
}
