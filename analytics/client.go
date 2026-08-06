package analytics

import (
	"context"
	"fmt"
	"log"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Transport is the module→module dial the client delivers over. It matches
// server.CallExt exactly, and is taken as a func value so this package never imports
// ./server — which keeps it gRPC-free and lets every test drive a fake.
//
// Its semantics are inherited and shape the flusher:
//   - status is the TARGET's HTTP-ish status, so a non-2xx arrives with err == nil
//   - configured=false means the env var is unset — not a failure, just no target
//   - only method, path and body cross the wire: no headers, no query string
type Transport func(ctx context.Context, envKey, method, path string, body []byte) (status uint16, out []byte, configured bool, err error)

// Config is everything a module supplies. Only Service and Transport are required; every
// tuning value has a default and an env override, because a queue depth you cannot raise
// without cutting a release is a queue depth you cannot raise during an incident.
type Config struct {
	Service   string    // the emitting module's token. Required.
	Version   string    // build stamp, recorded on every event. Optional.
	Transport Transport // server.CallExt. Required.

	EnvKey string                  // env var holding the kpi module's backend. Default MYCEL_KPI_URL.
	Log    func(level, msg string) // default: stdlib log

	// test seams
	now func() time.Time
}

// Env overrides, all read once in New.
const (
	envURL        = "MYCEL_KPI_URL"
	envDepth      = "MYCEL_ANALYTICS_QUEUE_DEPTH"
	envBatch      = "MYCEL_ANALYTICS_BATCH_SIZE"
	envFlushMS    = "MYCEL_ANALYTICS_FLUSH_MS"
	envBackoffMS  = "MYCEL_ANALYTICS_MAX_BACKOFF_MS"
	envReadTOMS   = "MYCEL_KPI_READ_TIMEOUT_MS"
	envCacheTTLMS = "MYCEL_KPI_CACHE_TTL_MS"
)

// Defaults. Depth 2000 at the design's 0.06–0.23 events/s is hours of buffer for ~2MB —
// overflow is not the realistic loss mode, ungraceful pod death is (see the package doc).
const (
	defaultDepth     = 2000
	defaultBatch     = 200
	defaultFlush     = 5 * time.Second
	defaultMaxBackff = 30 * time.Second
	defaultReadTO    = 3 * time.Second // strictly under the host's 5s MYCEL_HANDLE_TIMEOUT
	defaultCacheTTL  = 45 * time.Second

	// defaultDrainCap bounds Shutdown when the caller passes a context with no deadline.
	// Matches the SDK's own MYCEL_SHUTDOWN_TIMEOUT_MS default.
	defaultDrainCap = 8 * time.Second
)

// Client is one module's analytics seam. The zero value is unusable; use New. A nil
// *Client is safe to call — every method no-ops — so a module can hold one
// unconditionally.
type Client struct {
	cfg      Config
	envKey   string
	disabled bool // MYCEL_KPI_URL unset: no target, so no queue and no goroutine

	q         chan Event
	batchSize int
	flushIvl  time.Duration
	maxBackff time.Duration

	stop    chan struct{} // closed by Shutdown; wakes the flusher into drain mode
	stopped chan struct{} // closed by the flusher when it has finished draining
	closed  atomic.Bool   // Emit is a no-op once set
	drainCx atomic.Value  // context.Context, published before close(stop)

	dropping atomic.Bool
	emitted  atomic.Uint64
	dropped  atomic.Uint64
	rejected atomic.Uint64
	sent     atomic.Uint64
	retries  atomic.Uint64
	inFlight atomic.Int64

	// read side
	readTO   time.Duration
	cacheTTL time.Duration
	cacheMu  sync.RWMutex
	cache    map[string]cacheEntry
}

// New builds the client and starts its flusher. When MYCEL_KPI_URL (or Config.EnvKey) is
// unset the client is DISABLED: Emit counts and returns, no goroutine runs, Shutdown is a
// no-op. That keeps a dev app with no kpi module from filling a queue and retrying into
// the void, and it is logged once so the silence is explained.
func New(cfg Config) *Client {
	if cfg.EnvKey == "" {
		cfg.EnvKey = envURL
	}
	if cfg.Log == nil {
		cfg.Log = func(level, msg string) { log.Printf("[%s] %s", level, msg) }
	}
	if cfg.now == nil {
		cfg.now = time.Now
	}
	c := &Client{
		cfg:       cfg,
		envKey:    cfg.EnvKey,
		batchSize: intEnv(envBatch, defaultBatch),
		flushIvl:  msEnv(envFlushMS, defaultFlush),
		maxBackff: msEnv(envBackoffMS, defaultMaxBackff),
		readTO:    msEnv(envReadTOMS, defaultReadTO),
		cacheTTL:  msEnv(envCacheTTLMS, defaultCacheTTL),
		stop:      make(chan struct{}),
		stopped:   make(chan struct{}),
		cache:     map[string]cacheEntry{},
	}
	if os.Getenv(cfg.EnvKey) == "" || cfg.Transport == nil {
		c.disabled = true
		close(c.stopped)
		cfg.Log("info", "analytics: "+cfg.EnvKey+" unset — events are discarded (no kpi module wired)")
		return c
	}
	c.q = make(chan Event, intEnv(envDepth, defaultDepth))
	go c.run()
	return c
}

// Emit records one business fact. It never blocks, never returns an error, and never
// touches the network on the caller's goroutine — it is a bounded channel send and
// nothing more.
//
// On a full queue the NEWEST event is dropped and counted. Drop-newest is a one-line
// non-blocking send; drop-oldest needs a ring buffer, and building one for a case that
// should almost never fire is misplaced effort — if the queue is filling, the fix is
// downstream, not a more elegant choice of which events to lose.
func (c *Client) Emit(e Event) {
	if c == nil || c.disabled || c.closed.Load() {
		return
	}
	// Stamped here, not in the flusher: see Event.OccurredAt and Event.ID.
	if e.OccurredAt.IsZero() {
		e.OccurredAt = c.cfg.now()
	}
	if e.ID == "" {
		e.ID = uuidV4()
	}
	c.emitted.Add(1)
	select {
	case c.q <- e:
		if c.dropping.CompareAndSwap(true, false) {
			c.cfg.Log("warn", "analytics: queue drained below capacity, no longer dropping")
		}
	default:
		c.dropped.Add(1)
		// Logged on the TRANSITION only. Per-event logging during an incident produces
		// thousands of identical lines and buries the one that matters.
		if c.dropping.CompareAndSwap(false, true) {
			c.cfg.Log("warn", "analytics: queue full — dropping newest events until it drains")
		}
	}
}

// Shutdown drains the queue and blocks until it is empty or ctx expires. Wire it to
// Descriptor.Shutdown: without it every rolling deploy silently loses whatever was in
// flight — Emit already returned successfully and no drop counter fires, so the loss is
// invisible and permanent.
//
// It deliberately does NOT close the queue channel: that races a concurrent Emit and
// panics on send. Instead Emit checks a flag, and a racing Emit that passes the check
// still sends into a buffered channel — the event is lost, counted, and nothing panics.
func (c *Client) Shutdown(ctx context.Context) error {
	if c == nil || c.disabled {
		return nil
	}
	if !c.closed.CompareAndSwap(false, true) {
		return nil // idempotent: a second call must not close(stop) twice
	}
	// A context with no deadline would let the drain retry a down store forever and hang
	// the process on exit. The SDK always passes a bounded one, but a module author may
	// not, and "my pod won't terminate" is a miserable way to discover that.
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, defaultDrainCap)
		defer cancel()
	}
	c.drainCx.Store(ctx)
	close(c.stop)
	select {
	case <-c.stopped:
		return nil
	case <-ctx.Done():
		n := len(c.q) + int(c.inFlight.Load())
		// The count IS the deliverable here: it is the only record that these events
		// existed, since the process is about to exit.
		return fmt.Errorf("analytics: drain deadline expired with %d event(s) undelivered", n)
	}
}

// Stats reads the counters. Also the body of the per-batch counter payload.
func (c *Client) Stats() Stats {
	if c == nil {
		return Stats{}
	}
	return Stats{
		Emitted:  c.emitted.Load(),
		Dropped:  c.dropped.Load(),
		Rejected: c.rejected.Load(),
		Sent:     c.sent.Load(),
		Retries:  c.retries.Load(),
		QueueLen: len(c.q),
		Dropping: c.dropping.Load(),
	}
}

func intEnv(key string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(key)); err == nil && v > 0 {
		return v
	}
	return def
}

func msEnv(key string, def time.Duration) time.Duration {
	if v, err := strconv.Atoi(os.Getenv(key)); err == nil && v > 0 {
		return time.Duration(v) * time.Millisecond
	}
	return def
}
