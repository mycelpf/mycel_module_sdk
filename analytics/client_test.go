package analytics

import (
	"context"
	"encoding/json"
	"strconv"
	"sync"
	"testing"
	"time"
)

// fakeTransport records every call and answers with a scripted status sequence. It is
// the only transport any test uses — no gRPC, no network, no container.
type fakeTransport struct {
	mu       sync.Mutex
	calls    [][]byte // one entry per delivery attempt, the raw body
	statuses []uint16 // consumed in order; the last one repeats
	block    chan struct{}
	notConf  bool
	err      error
}

func (f *fakeTransport) fn(ctx context.Context, envKey, method, path string, body []byte) (uint16, []byte, bool, error) {
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return 0, nil, true, ctx.Err()
		}
	}
	f.mu.Lock()
	cp := append([]byte(nil), body...)
	f.calls = append(f.calls, cp)
	st := uint16(200)
	if len(f.statuses) > 0 {
		if len(f.calls) <= len(f.statuses) {
			st = f.statuses[len(f.calls)-1]
		} else {
			st = f.statuses[len(f.statuses)-1]
		}
	}
	f.mu.Unlock()
	if f.notConf {
		return 0, nil, false, nil
	}
	return st, []byte(`{"accepted":1}`), true, f.err
}

func (f *fakeTransport) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeTransport) bodies() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]byte(nil), f.calls...)
}

// newTestClient builds an enabled client with tight timings; t.Setenv makes the target
// look configured without any real module.
func newTestClient(t *testing.T, tr *fakeTransport, env map[string]string) *Client {
	t.Helper()
	t.Setenv(envURL, "127.0.0.1:1")
	for k, v := range env {
		t.Setenv(k, v)
	}
	c := New(Config{Service: "test", Version: "v0", Transport: tr.fn,
		Log: func(level, msg string) {}})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = c.Shutdown(ctx)
	})
	return c
}

func TestEmitNeverBlocksWhenQueueFull(t *testing.T) {
	// A transport that never returns, with batch size 1 so the flusher wedges inside its
	// first delivery instead of quietly draining the queue into its buffer.
	tr := &fakeTransport{block: make(chan struct{})}
	c := newTestClient(t, tr, map[string]string{
		envDepth: "10", envBatch: "1", envFlushMS: "3600000",
	})

	// Emit far past capacity. If Emit ever blocks, this test hangs — which is the failure
	// we care about: a blocking Emit means analytics is in the OLTP path.
	done := make(chan struct{})
	go func() {
		for i := 0; i < 500; i++ {
			c.Emit(Event{Type: "T", SubjectType: "s", SubjectID: strconv.Itoa(i)})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Emit blocked — the write invariant is broken")
	}

	s := c.Stats()
	if s.Emitted != 500 {
		t.Errorf("emitted = %d, want 500", s.Emitted)
	}
	if s.Dropped == 0 {
		t.Error("expected drops once the queue filled, got none")
	}
	// Nothing may vanish silently: every emitted event is either dropped-and-counted,
	// still queued, or in the delivery attempt.
	if accounted := s.Dropped + uint64(s.QueueLen) + uint64(c.inFlight.Load()); accounted != s.Emitted {
		t.Errorf("accounted for %d of %d emitted events: %+v", accounted, s.Emitted, s)
	}
	if !s.Dropping {
		t.Error("expected the dropping flag to be set")
	}
	close(tr.block)
}

// Regression: with a permanently failing store, deliver used to retry forever on a
// Background context, never observe close(stop), and never run the drain — so a SIGTERM
// during an outage delivered nothing at all, which is the exact loss the drain exists to
// prevent. Shutdown must preempt the retry and still get one bounded attempt at the queue.
func TestShutdownPreemptsAnInFlightRetry(t *testing.T) {
	tr := &fakeTransport{statuses: []uint16{500}} // never recovers
	c := newTestClient(t, tr, map[string]string{
		envDepth: "100", envBatch: "1", envFlushMS: "3600000", envBackoffMS: "50",
	})
	c.Emit(Event{Type: "T", SubjectType: "s", SubjectID: "a"})
	waitFor(t, func() bool { return c.Stats().Retries > 0 }, "the retry loop to engage")

	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := c.Shutdown(ctx)

	// It must RETURN (the old code hung forever) and it must honour its deadline — the
	// drain is supposed to keep trying right up to it, then give up and report.
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("Shutdown took %s — it never observed the stop", elapsed)
	}
	if err == nil {
		t.Fatal("expected an error reporting the undeliverable events")
	}
	// The count is asserted through the ERROR, not through Stats: Shutdown returns the
	// moment its deadline fires, which can precede the flusher's own bookkeeping. The
	// error is the record that reaches an operator, so that is what has to be right.
	if !contains(err.Error(), "event(s) undelivered") {
		t.Errorf("error %q does not report what was lost", err)
	}
	// And it must have kept trying during the drain rather than bailing out immediately.
	if tr.count() < 2 {
		t.Errorf("transport tried %d times — the drain gave up without re-attempting", tr.count())
	}
}

func TestEmitIsRaceFreeUnderConcurrency(t *testing.T) {
	tr := &fakeTransport{}
	c := newTestClient(t, tr, map[string]string{envDepth: "64", envFlushMS: "10"})
	var wg sync.WaitGroup
	for g := 0; g < 50; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				c.Emit(Event{Type: "T", SubjectType: "s", SubjectID: strconv.Itoa(i)})
			}
		}(g)
	}
	wg.Wait()
	if got := c.Stats().Emitted; got != 5000 {
		t.Errorf("emitted = %d, want 5000", got)
	}
}

func TestFlushOnBatchSize(t *testing.T) {
	tr := &fakeTransport{}
	// A one-hour tick, so only the size trigger can fire.
	c := newTestClient(t, tr, map[string]string{envBatch: "3", envFlushMS: "3600000"})
	for i := 0; i < 3; i++ {
		c.Emit(Event{Type: "T", SubjectType: "s", SubjectID: strconv.Itoa(i)})
	}
	waitFor(t, func() bool { return tr.count() == 1 }, "one delivery on reaching batch size")

	var b batchBody
	if err := json.Unmarshal(tr.bodies()[0], &b); err != nil {
		t.Fatal(err)
	}
	if len(b.Events) != 3 {
		t.Errorf("batch carried %d events, want 3", len(b.Events))
	}
}

func TestFlushOnInterval(t *testing.T) {
	tr := &fakeTransport{}
	c := newTestClient(t, tr, map[string]string{envBatch: "1000", envFlushMS: "20"})
	c.Emit(Event{Type: "T", SubjectType: "s", SubjectID: "1"})
	c.Emit(Event{Type: "T", SubjectType: "s", SubjectID: "2"})
	waitFor(t, func() bool { return tr.count() >= 1 }, "a delivery on the flush interval")

	var b batchBody
	if err := json.Unmarshal(tr.bodies()[0], &b); err != nil {
		t.Fatal(err)
	}
	if len(b.Events) != 2 {
		t.Errorf("batch carried %d events, want 2", len(b.Events))
	}
}

// The retry must resend the IDENTICAL event ids. That is precisely what makes the store's
// ReplacingMergeTree dedup work: different ids on a retry would double-count instead.
func TestRetryResendsTheSameEventIDs(t *testing.T) {
	tr := &fakeTransport{statuses: []uint16{500, 503, 200}}
	c := newTestClient(t, tr, map[string]string{
		envBatch: "2", envFlushMS: "3600000", envBackoffMS: "10",
	})
	c.Emit(Event{Type: "T", SubjectType: "s", SubjectID: "a"})
	c.Emit(Event{Type: "T", SubjectType: "s", SubjectID: "b"})
	waitFor(t, func() bool { return tr.count() == 3 }, "two retries then success")

	var first, last batchBody
	bodies := tr.bodies()
	if err := json.Unmarshal(bodies[0], &first); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(bodies[2], &last); err != nil {
		t.Fatal(err)
	}
	if len(first.Events) != 2 || len(last.Events) != 2 {
		t.Fatalf("batch sizes changed across retries: %d then %d", len(first.Events), len(last.Events))
	}
	for i := range first.Events {
		if first.Events[i].EventID != last.Events[i].EventID {
			t.Errorf("event %d id changed across retry: %q → %q — dedup would double-count",
				i, first.Events[i].EventID, last.Events[i].EventID)
		}
	}
	if c.Stats().Sent != 2 {
		t.Errorf("sent = %d, want 2", c.Stats().Sent)
	}
}

// A 400 is permanent. Retrying it forever wedges the flusher and turns one bad event into
// total data loss, so it must be dropped after exactly one attempt.
func TestDropsOn4xxWithoutRetrying(t *testing.T) {
	tr := &fakeTransport{statuses: []uint16{400}}
	c := newTestClient(t, tr, map[string]string{envBatch: "1", envFlushMS: "3600000"})
	c.Emit(Event{Type: "T", SubjectType: "s", SubjectID: "a"})
	waitFor(t, func() bool { return c.Stats().Rejected == 1 }, "the batch to be rejected")

	time.Sleep(50 * time.Millisecond) // give a stray retry a chance to appear
	if n := tr.count(); n != 1 {
		t.Errorf("transport called %d times, want exactly 1 — a 4xx must not be retried", n)
	}
	if c.Stats().Sent != 0 {
		t.Error("a rejected batch must not count as sent")
	}
}

func TestBackoffIsCapped(t *testing.T) {
	tr := &fakeTransport{statuses: []uint16{500}}
	c := newTestClient(t, tr, map[string]string{
		envBatch: "1", envFlushMS: "3600000", envBackoffMS: "30",
	})
	c.Emit(Event{Type: "T", SubjectType: "s", SubjectID: "a"})
	// With a 30ms cap, ~8 attempts must land well inside a second. An uncapped
	// exponential would still be on attempt 4 or 5.
	waitFor(t, func() bool { return tr.count() >= 8 }, "8 capped retries inside the deadline")
}

func TestDisabledWhenNoTargetConfigured(t *testing.T) {
	t.Setenv(envURL, "")
	tr := &fakeTransport{}
	c := New(Config{Service: "test", Transport: tr.fn, Log: func(string, string) {}})
	c.Emit(Event{Type: "T", SubjectType: "s", SubjectID: "a"})
	if got := c.Stats().Emitted; got != 0 {
		t.Errorf("a disabled client must not count emits, got %d", got)
	}
	if err := c.Shutdown(context.Background()); err != nil {
		t.Errorf("disabled Shutdown returned %v", err)
	}
	if tr.count() != 0 {
		t.Error("a disabled client must not call the transport")
	}
}

// The whole reason Descriptor.Shutdown exists: a rolling deploy must lose nothing.
func TestShutdownDrainsTheQueue(t *testing.T) {
	tr := &fakeTransport{}
	c := newTestClient(t, tr, map[string]string{
		envDepth: "500", envBatch: "1000", envFlushMS: "3600000",
	})
	for i := 0; i < 50; i++ {
		c.Emit(Event{Type: "T", SubjectType: "s", SubjectID: strconv.Itoa(i)})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := c.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	total := 0
	for _, raw := range tr.bodies() {
		var b batchBody
		if err := json.Unmarshal(raw, &b); err != nil {
			t.Fatal(err)
		}
		total += len(b.Events)
	}
	if total != 50 {
		t.Errorf("drain delivered %d of 50 events — the rest would be silently lost on deploy", total)
	}
}

func TestShutdownDeadlineReportsUndeliveredCount(t *testing.T) {
	tr := &fakeTransport{block: make(chan struct{})}
	c := newTestClient(t, tr, map[string]string{
		envDepth: "500", envBatch: "1000", envFlushMS: "3600000",
	})
	for i := 0; i < 7; i++ {
		c.Emit(Event{Type: "T", SubjectType: "s", SubjectID: strconv.Itoa(i)})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	err := c.Shutdown(ctx)
	if err == nil {
		t.Fatal("expected an error naming the undelivered events")
	}
	// The count is the only remaining record that these events existed.
	if want := "7 event(s) undelivered"; !contains(err.Error(), want) {
		t.Errorf("error %q does not report the count (%q)", err, want)
	}
	close(tr.block)
}

func TestEmitDuringShutdownDoesNotPanic(t *testing.T) {
	tr := &fakeTransport{}
	c := newTestClient(t, tr, map[string]string{envDepth: "100", envFlushMS: "5"})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 2000; i++ {
			c.Emit(Event{Type: "T", SubjectType: "s", SubjectID: strconv.Itoa(i)})
		}
	}()
	time.Sleep(2 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = c.Shutdown(ctx) // must not panic on a send to a closed channel
	wg.Wait()
}

func TestShutdownIsIdempotent(t *testing.T) {
	tr := &fakeTransport{}
	c := newTestClient(t, tr, nil)
	ctx := context.Background()
	if err := c.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.Shutdown(ctx); err != nil { // a second close(stop) would panic
		t.Fatalf("second Shutdown: %v", err)
	}
}

// Counters ride on every batch: that is how a drop reaches the dashboard without any
// metrics pipeline in the fabric.
func TestCountersRideOnEveryBatch(t *testing.T) {
	// batch 1 so the flusher wedges in the transport and the queue genuinely fills.
	tr := &fakeTransport{block: make(chan struct{})}
	c := newTestClient(t, tr, map[string]string{
		envDepth: "2", envBatch: "1", envFlushMS: "3600000",
	})
	for i := 0; i < 20; i++ { // overflow depth 2 → drops
		c.Emit(Event{Type: "T", SubjectType: "s", SubjectID: strconv.Itoa(i)})
	}
	if c.Stats().Dropped == 0 {
		t.Fatal("setup did not produce drops")
	}
	close(tr.block)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = c.Shutdown(ctx)

	bodies := tr.bodies()
	if len(bodies) == 0 {
		t.Fatal("no batch delivered")
	}
	var b batchBody
	if err := json.Unmarshal(bodies[len(bodies)-1], &b); err != nil {
		t.Fatal(err)
	}
	if b.Counters.Dropped == 0 {
		t.Error("the batch did not carry the drop count — drops would be invisible")
	}
	if b.Service != "test" || b.Version != "v0" {
		t.Errorf("batch metadata missing: service=%q version=%q", b.Service, b.Version)
	}
}

func TestDroppingLogsOnTransitionOnly(t *testing.T) {
	// The realistic incident: the store is down, so the flusher is stuck and the queue
	// stays full. One transition in, and no way out until it recovers.
	tr := &fakeTransport{block: make(chan struct{})}
	t.Setenv(envURL, "127.0.0.1:1")
	t.Setenv(envDepth, "2")
	t.Setenv(envBatch, "1")
	t.Setenv(envFlushMS, "3600000")

	var mu sync.Mutex
	var warns int
	c := New(Config{Service: "test", Transport: tr.fn, Log: func(level, msg string) {
		if level == "warn" {
			mu.Lock()
			warns++
			mu.Unlock()
		}
	}})
	for i := 0; i < 500; i++ { // hundreds of drops
		c.Emit(Event{Type: "T", SubjectType: "s", SubjectID: strconv.Itoa(i)})
	}
	mu.Lock()
	got := warns
	mu.Unlock()
	// The property is that logging is O(transitions), not O(events). The exact count is
	// not 1: the flusher pulls an event as it works, which briefly frees a slot and can
	// genuinely flap the queue between full and not-full. Each of those IS a transition
	// and deserves its line — what must never happen is a line per dropped event.
	if got == 0 {
		t.Error("dropping was never announced at all")
	}
	if got > 10 {
		t.Errorf("logged %d warnings for ~498 drops — this is per-event logging, which buries the signal", got)
	}
	close(tr.block)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = c.Shutdown(ctx)
}

func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func contains(hay, needle string) bool {
	return len(hay) >= len(needle) && (hay == needle || len(needle) == 0 ||
		indexOf(hay, needle) >= 0)
}

func indexOf(hay, needle string) int {
	for i := 0; i+len(needle) <= len(hay); i++ {
		if hay[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
