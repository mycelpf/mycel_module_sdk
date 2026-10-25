// Package analytics is the module-side client for business events and KPIs.
//
// A module calls Emit at a business state transition. Emit is a non-blocking send onto
// a bounded in-process channel and returns in microseconds — it takes no context and
// returns no error, because both would drag analytics back into the OLTP path, and the
// write invariant is that emitting never blocks, slows or fails a business transaction.
//
// A background flusher batches (200 rows or 5s) and delivers one POST /events to the
// mycel_kpi module over a direct module→module gRPC dial. THE CALLER'S QUEUE IS THE ONLY
// BUFFER in the system: mycel_kpi inserts synchronously and acks only after ClickHouse
// has, so a failed delivery leaves the batch here and a retry means something. A second
// buffer downstream would double the loss surface and ack the caller from memory.
//
// What is lost, and when:
//
//   - queue full           → newest dropped, counted, WARN logged on the transition
//   - 5xx / transport      → batch retained, retried with capped backoff
//   - 4xx                  → batch dropped, counted, ERROR logged (see flusher.go)
//   - ungraceful pod death → whatever was queued (nothing can help; an in-memory queue)
//   - rolling deploy       → NOTHING, provided Descriptor.Shutdown is wired to Shutdown
//
// Counters ride on every batch so mycel_kpi can record them as KPIs — drops therefore
// show up on the dashboard itself, with no metrics pipeline in the fabric.
//
// This package imports only the standard library and the pure SDK root (for the request
// type): the transport arrives as a plain func value (see Transport), so it never pulls in
// gRPC and every test drives a fake.
package analytics

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	modulesdk "github.com/mycelpf/mycel_module_sdk"
)

// Event is one business fact: true at a moment, immutable forever, past tense, in domain
// language. Facts only — never judgments (annual_premium, not is_underpriced; thresholds
// apply at read time) and never PII, IDs only.
type Event struct {
	Type        string // "OrderSubmitted" — past tense. Required.
	SubjectType string // "order" — what the event is ABOUT. Required.
	SubjectID   string // Required: an event with no subject cannot join a paired metric.
	ActorID     string // Who caused it. Set via WithRequest.
	TraceID     string // W3C trace id, for click-through to Tempo. Set via WithRequest or WithHeaders.

	// OccurredAt is when the fact became true. Zero → filled by Emit, NOT by the flusher:
	// a flusher-set timestamp is up to one flush interval late, which would make the
	// event's own claim about when it happened wrong by seconds.
	OccurredAt time.Time

	// ID is the dedup key. Zero → filled by Emit. The store is a ReplacingMergeTree keyed
	// on it, which is what makes an at-least-once retry idempotent rather than a
	// double-count.
	ID string

	Attrs map[string]string  // dimensions you would GROUP BY
	Nums  map[string]float64 // measures you would SUM
}

// WithRequest fills ActorID and TraceID from one inbound request, which is the only place
// they exist: the flusher runs on a background goroutine with no request and no span.
// ActorID comes from the verified user app_core attached to the request (Request.User,
// sdk.md S11.4): its sub, or its email when it has no sub. A request without a verified
// user has no actor; the x-mycel-user header is never read. TraceID comes from the W3C
// traceparent the SDK injects. Values already set are kept.
func (e Event) WithRequest(request modulesdk.Request) Event {
	if e.ActorID == "" {
		e.ActorID = request.User.Sub
		if e.ActorID == "" {
			e.ActorID = request.User.Email
		}
	}
	return e.WithHeaders(request.Headers)
}

// WithHeaders fills TraceID from one inbound request's headers. It no longer sets
// ActorID: the actor comes only from the verified user (WithRequest), never from a
// caller-supplied header such as x-mycel-user (sdk.md S11.4).
//
// Header names are lower-cased by the SDK. A malformed or all-zero traceparent yields an
// empty TraceID rather than a bogus one — a wrong trace id is worse than none, because it
// links the event to somebody else's trace.
func (e Event) WithHeaders(h map[string]string) Event {
	if h == nil {
		return e
	}
	if e.TraceID == "" {
		e.TraceID = traceIDFrom(h["traceparent"])
	}
	return e
}

// traceIDFrom parses the trace-id field of a W3C traceparent
// (version "-" trace-id "-" parent-id "-" flags). An all-zero trace-id is invalid per the
// spec and is treated as absent.
func traceIDFrom(v string) string {
	parts := strings.Split(v, "-")
	if len(parts) < 4 || len(parts[1]) != 32 {
		return ""
	}
	id := parts[1]
	if _, err := hex.DecodeString(id); err != nil {
		return ""
	}
	if strings.Trim(id, "0") == "" {
		return ""
	}
	return id
}

// Stats is a point-in-time read of the client's counters. Emitted/Dropped/Rejected are
// cumulative SINCE PROCESS START and ride on every batch, so mycel_kpi can record them as
// KPIs. They reset on restart — which means the hourly value can go DOWN across a rolling
// deploy. Adequate for "did we lose anything"; do not alert on the delta.
type Stats struct {
	Emitted  uint64 // accepted by Emit
	Dropped  uint64 // refused because the queue was full
	Rejected uint64 // permanently refused by the store (4xx)
	Sent     uint64 // delivered and acked
	Retries  uint64 // delivery attempts that failed and were retried
	QueueLen int    // events waiting right now
	Dropping bool   // the queue is currently full
}

// uuidV4 is a RFC-4122 v4 id. Local rather than a dependency: this package's
// stdlib-only rule is what lets a module vendor it without dragging in a module graph.
func uuidV4() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing is unrecoverable and not this package's business to
		// paper over; a time-based fallback keeps Emit total, and the dedup key is
		// only ever compared for equality.
		n := time.Now().UnixNano()
		for i := range b {
			b[i] = byte(n >> (uint(i%8) * 8))
		}
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
