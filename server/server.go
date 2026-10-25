// Package server is the transport glue of the mycel module SDK — the one place the
// gRPC ModuleHandler contract, the listener, the datastore, and the per-request
// transaction are wired. It is COPIED verbatim into each module by `mycel init`, so
// every module boots, migrates, and dispatches identically; a module author only
// fills a modulesdk.Descriptor and calls Serve.
//
// It implements contracts/modulerpc/v1.ModuleHandler:
//   - Describe → the host builds /api/app-config nav/menu-group from the Descriptor.
//   - Handle   → maps the gRPC HandleRequest to a modulesdk.Request, runs the route
//     inside a Postgres transaction scoped to the module's schema, maps the
//     modulesdk.Response back. Commits on status < 500, else rolls back.
//   - Health   → SERVING only when there is no DB, or a recent (debounced) DB ping
//     succeeded; NOT_SERVING otherwise.
//
// Transport is hardened: keepalive + enforcement, 32 MiB message caps, an otelgrpc
// stats handler (exporter is env-driven; none by default), graceful shutdown on
// SIGTERM/SIGINT, 0700 socket dir with UDS path-length validation, and best-effort
// OS-aware peer-uid auth on the unix socket.
package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mycelpf/mycel_module_sdk"
	modulerpc "github.com/mycelpf/mycel_module_sdk/contracts/modulerpc/v1"
	"github.com/mycelpf/mycel_module_sdk/wire"

	_ "github.com/lib/pq"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/status"
)

// defaultSocketDir is the consistent per-federation socket location. `mycel run`
// overrides the full address via MYCEL_LISTEN (the [module].backend); standalone
// this default keeps every module's socket together under one greppable dir.
const defaultSocketDir = "/tmp/mycel_app_core"

// maxMsgBytes caps both inbound and outbound gRPC messages at 32 MiB. The host pins
// the same ceiling, so the two ends agree on the largest Handle body either way.
const maxMsgBytes = 32 << 20

// shutdownGrace is how long GracefulStop is given to drain in-flight RPCs before the
// server is hard-stopped. Bounded so a wedged stream cannot block process exit.
const shutdownGrace = 15 * time.Second

// healthPingTTL debounces the readiness DB ping: Health reuses the last ping result
// for this long instead of hammering the datastore on every probe.
const healthPingTTL = 2 * time.Second

// validateUDSPath fails when a unix-socket path would overflow the kernel's fixed
// sockaddr_un.sun_path (which silently truncates), so a too-long MYCEL_LISTEN is a
// clear boot error rather than a phantom "wrong socket" at runtime.
func validateUDSPath(path string) error {
	limit := 104 // darwin sun_path
	if runtime.GOOS == "linux" {
		limit = 108
	}
	if len(path) >= limit {
		return fmt.Errorf("unix socket path %q is %d bytes; the OS limit is %d", path, len(path), limit)
	}
	return nil
}

// applyMigrations creates the module's schema and runs each shipped migration inside
// it, in order — AT MOST ONCE. A per-schema ledger (_mycel_migrations) records every
// applied migration id, so a migration never re-runs across reboots; that makes
// non-idempotent DDL (CREATE TYPE, CREATE INDEX, …) safe. Each migration + its ledger
// row commit in ONE transaction (search_path = the schema), so a failed migration
// leaves no ledger row and is retried next boot.
func applyMigrations(db *sql.DB, d modulesdk.Descriptor) error {
	// d.Token is validated [a-z][a-z0-9_]* at scaffold time, so it is safe to embed.
	if _, err := db.Exec("CREATE SCHEMA IF NOT EXISTS " + d.Token); err != nil {
		return fmt.Errorf("create schema %s: %w", d.Token, err)
	}
	if _, err := db.Exec("CREATE TABLE IF NOT EXISTS " + d.Token +
		"._mycel_migrations (id text PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())"); err != nil {
		return fmt.Errorf("create migrations ledger: %w", err)
	}
	for _, m := range d.Migrations {
		if strings.TrimSpace(m.SQL) == "" {
			continue
		}
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec("SET LOCAL search_path TO " + d.Token); err != nil {
			tx.Rollback()
			return err
		}
		// Claim the id; ON CONFLICT DO NOTHING → no row returned means it already ran.
		var claimed bool
		err = tx.QueryRow("INSERT INTO _mycel_migrations (id) VALUES ($1) ON CONFLICT (id) DO NOTHING RETURNING true", m.ID).Scan(&claimed)
		if err == sql.ErrNoRows {
			tx.Rollback() // already applied — skip
			continue
		}
		if err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %s ledger: %w", m.ID, err)
		}
		if _, err := tx.Exec(m.SQL); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %s: %w", m.ID, err)
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// recoverUnary turns a panic in module code into one failed request instead of a dead
// module. grpc-go does NOT recover handler panics, so without this a single bad input
// unwinds past Serve and kills the process — taking every concurrent in-flight request
// with it, and skipping Descriptor.Shutdown, so an analytics queue is lost rather than
// drained. It is worse for Descriptor.Handler modules: under a real net/http server a
// handler panic costs one connection, but bridgeHTTP routes it through this gRPC
// handler, so the same panic became fatal on being mounted as a module.
//
// sdk.go asserts Route "never panics on bad input". This is what makes that assertion
// survivable when it turns out to be false.
func recoverUnary(token string) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, next grpc.UnaryHandler) (resp any, err error) {
		defer func() {
			r := recover()
			if r == nil {
				return
			}
			buf := make([]byte, 8192)
			buf = buf[:runtime.Stack(buf, false)]
			log.Printf("%s: PANIC in %s: %v\n%s", token, info.FullMethod, r, buf)

			// Handle answers in HTTP terms, so give the caller a real 500 rather than a
			// transport error the host would render as 502 "module unreachable" — the
			// module is up; one route misbehaved, and that distinction is the whole
			// difference between "redeploy" and "look at this handler".
			if info.FullMethod == handleFullMethod {
				resp, err = errResponse(500, modulesdk.ErrorCodeInternal, "internal error", traceIDFromContext(ctx)), nil
				return
			}
			resp, err = nil, status.Errorf(codes.Internal, "panic in %s", info.FullMethod)
		}()
		return next(ctx, req)
	}
}

// handleFullMethod is the one RPC whose reply carries an HTTP status of its own.
const handleFullMethod = "/mycel.modulerpc.v1.ModuleHandler/Handle"

// handler implements ModuleHandler by delegating to the Descriptor's pure Route.
type handler struct {
	modulerpc.UnimplementedModuleHandlerServer
	d      modulesdk.Descriptor
	db     *sql.DB
	health *healthGate
	// describe is built and validated once at boot; nil builds it per call (tests).
	describe *modulerpc.DescribeResponse
}

// Describe answers from the descriptor, including while startup verification is pending
// (sdk.md S3): the supervisor and app_core read metadata without calling a gated route.
func (h *handler) Describe(context.Context, *modulerpc.DescribeRequest) (*modulerpc.DescribeResponse, error) {
	if h.describe != nil {
		return h.describe, nil
	}
	return wire.DescribeResponseFromDiscovery(h.d.Discovery()), nil
}

// Health reports readiness: SERVING when the module has no DB, or a recent debounced
// ping of it succeeded; NOT_SERVING otherwise. The ping is cached (healthPingTTL), so
// frequent probes don't hammer the datastore.
func (h *handler) Health(ctx context.Context, _ *modulerpc.HealthRequest) (*modulerpc.HealthResponse, error) {
	status := modulerpc.HealthResponse_SERVING
	if !h.health.serving(ctx) {
		status = modulerpc.HealthResponse_NOT_SERVING
	}
	return &modulerpc.HealthResponse{Status: status}, nil
}

func (h *handler) Handle(ctx context.Context, req *modulerpc.HandleRequest) (*modulerpc.HandleResponse, error) {
	// Fail-closed: while the startup gate is held (e.g. a wired extension that doesn't
	// honor the contract), the module refuses ALL traffic — not just signalling
	// NOT_SERVING on Health, but actually not serving its routes — so a non-conformant
	// collaborator makes the module unusable until it's fixed. Cleared gate ⇒ no cost.
	if h.health.gate.Load() != nil {
		// A stable code and a fixed, safe message (sdk.md S8). The verification report can
		// name collaborators, addresses or credentials, so it stays in this process's log
		// (redacted) and never reaches a public caller; app_core decides what to show.
		return errResponse(503, modulesdk.ErrorCodeVerificationPending,
			"module not ready: startup verification has not passed", traceIDFromContext(ctx)), nil
	}

	// Budgets and header validity are checked before the module runs (sdk.md S4, S5).
	if err := wire.CheckHandleRequestBudget(req); err != nil {
		log.Printf("%s: request refused: %v", h.d.Token, err)
		return errResponse(413, modulesdk.ErrorCodeRequestTooLarge, "the request is too large", traceIDFromContext(ctx)), nil
	}
	fields, err := wire.HeaderFieldsFromHandleRequest(req)
	if err != nil {
		log.Printf("%s: request refused: %v", h.d.Token, err)
		return errResponse(400, modulesdk.ErrorCodeInvalidHeader, "a request header is invalid", traceIDFromContext(ctx)), nil
	}

	// SDK-owned diagnostics precede both handler styles and never open a transaction.
	if req.GetPath() == "/_mycel/vitals" {
		return vitalsResponse(ctx, h), nil
	}
	// HTTP handlers own their datastore; do not apply SDK transactions here.
	if h.d.Handler != nil {
		return h.checkResponse(ctx, req, bridgeHTTP(modulesdk.ContextWithUser(ctx, wire.UserFromUserContext(req.GetUser())), h.d.Handler, req), nil), nil
	}

	// The legacy map is what the host sent, or the compatibility view of the header
	// fields when the host sent those (sdk.md S4).
	legacy := req.GetHeaders()
	if len(req.GetHeaderFields()) > 0 {
		legacy = modulesdk.LegacyHeaderMap(fields)
	}
	lr := modulesdk.Request{
		Method:       req.GetMethod(),
		Path:         req.GetPath(),
		Query:        req.GetQuery(),
		Headers:      legacy,
		HeaderFields: fields,
		Body:         req.GetBody(),
		// Only from the wire field app_core sets after verifying a credential; never from
		// a header (sdk.md S11.4).
		User: wire.UserFromUserContext(req.GetUser()),
	}.WithContext(ctx)

	// Propagate the active trace context into the request headers (traceparent/
	// tracestate) so module logic can forward it on outbound HTTP calls (e.g. a Temporal
	// worker service) and keep the distributed trace stitched across that hop. No-op
	// when tracing is off (no propagator).
	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)
	if lr.Headers == nil || len(carrier) > 0 {
		copied := make(map[string]string, len(lr.Headers)+len(carrier))
		for name, value := range lr.Headers {
			copied[name] = value
		}
		lr.Headers = copied
	}
	for name, value := range carrier {
		lr.Headers[name] = value
		lr.HeaderFields = setHeaderField(lr.HeaderFields, name, value)
	}

	// No DB wired → a stub host whose data calls error (static routes still serve).
	var host modulesdk.Host = noDBHost{ctx: ctx}
	var lazy *lazyTxHost
	if h.db != nil {
		lazy = &lazyTxHost{txHost: txHost{ctx: ctx}, db: h.db, token: h.d.Token}
		// Backstop for the panic path: recoverUnary turns a panic in Route into a 500,
		// but a transaction the route opened is still open at that point and nothing else
		// would ever release its pool connection. Rollback after a Commit is ErrTxDone — a
		// no-op — so this is safe on every path, not just the panicking one.
		defer lazy.rollback()
		host = lazy
	}

	resp := h.d.Route(lr, host)
	encoded, encodeErr := encodeRouteResponse(resp)
	if encodeErr == nil {
		encodeErr = wire.CheckHandleResponseBudget(encoded)
	}

	if tx := lazy.opened(); tx != nil {
		if encodeErr != nil {
			// The response cannot be delivered, so the write is not applied: the caller
			// sees a failure that means "nothing happened" (sdk.md S5). Never retried.
			_ = tx.Rollback()
		} else if resp.Status < 500 {
			// The commit error is the answer, not a detail to drop. A connection that
			// dies during COMMIT — precisely what a Postgres failover produces — rolls
			// the transaction back; returning the route's 200 there tells the caller a
			// write succeeded that does not exist.
			if err := tx.Commit(); err != nil {
				log.Printf("%s: commit failed after %d on %s %s: %v", h.d.Token, resp.Status, lr.Method, lr.Path, err)
				return errResponse(500, modulesdk.ErrorCodeCommitFailed, "commit failed — the request was not applied", traceIDFromContext(ctx)), nil
			}
		} else {
			_ = tx.Rollback() // already failing; the rollback error adds nothing
		}
	}

	return h.checkResponse(ctx, req, encoded, encodeErr), nil
}

// checkResponse turns a module response that cannot be sent into a bounded error: an
// invalid header becomes 500 invalid_header, a response above a budget becomes 502
// module_response_too_large (sdk.md S4, S5). The detail is logged, never returned.
func (h *handler) checkResponse(ctx context.Context, req *modulerpc.HandleRequest, response *modulerpc.HandleResponse, err error) *modulerpc.HandleResponse {
	if err == nil {
		_, err = wire.HeaderFieldsFromHandleResponse(response)
	}
	if err == nil {
		err = wire.CheckHandleResponseBudget(response)
	}
	if err == nil {
		return response
	}
	log.Printf("%s: response to %s %s refused: %v", h.d.Token, req.GetMethod(), req.GetPath(), err)
	if errors.Is(err, modulesdk.ErrInvalidHeader) {
		return errResponse(500, modulesdk.ErrorCodeInvalidHeader, "the module sent an invalid response header", traceIDFromContext(ctx))
	}
	return errResponse(502, modulesdk.ErrorCodeModuleResponseTooLarge, "the module response is too large", traceIDFromContext(ctx))
}

// encodeRouteResponse maps a Route response to the wire (sdk.md S4). HeaderFields is
// authoritative when set; otherwise the legacy map is read through the compatibility
// adapter, and sent unchanged as the legacy map for hosts that predate header fields.
// content-type defaults to application/json, except on 204 and 304 and when the module
// set one. The body is sent byte for byte.
func encodeRouteResponse(resp modulesdk.Response) (*modulerpc.HandleResponse, error) {
	source := resp.HeaderFields
	if len(source) == 0 {
		source = modulesdk.HeaderFieldsFromLegacyMap(resp.Headers)
	}
	fields, err := modulesdk.NormalizeHeaderFields(source)
	if err != nil {
		return nil, err
	}
	var legacy map[string]string
	if len(resp.HeaderFields) == 0 {
		legacy = make(map[string]string, len(resp.Headers)+1)
		for name, value := range resp.Headers {
			legacy[name] = value
		}
	} else {
		legacy = modulesdk.LegacyHeaderMap(fields)
	}
	hasContentType := false
	for _, field := range fields {
		if field.Name == "content-type" && len(field.Values) > 0 && field.Values[0] != "" {
			hasContentType = true
		}
	}
	if !hasContentType && resp.Status != 204 && resp.Status != 304 {
		for name := range legacy {
			if strings.EqualFold(name, "content-type") {
				delete(legacy, name)
			}
		}
		fields = setHeaderField(fields, "content-type", "application/json")
		if legacy == nil {
			legacy = map[string]string{}
		}
		legacy["content-type"] = "application/json"
	}
	return &modulerpc.HandleResponse{
		Status:       uint32(resp.Status),
		Headers:      legacy,
		HeaderFields: wire.HeaderFieldsToWire(fields),
		Body:         []byte(resp.Body),
	}, nil
}

// setHeaderField returns fields with name set to the single value, replacing any values
// it had. fields is not modified.
func setHeaderField(fields []modulesdk.HeaderField, name, value string) []modulesdk.HeaderField {
	updated := make([]modulesdk.HeaderField, 0, len(fields)+1)
	for _, field := range fields {
		if field.Name != name {
			updated = append(updated, field)
		}
	}
	return append(updated, modulesdk.HeaderField{Name: name, Values: []string{value}})
}

// vitalsResponse answers the reserved /_mycel/vitals route: process runtime stats
// (this process's own goroutines/heap/GC — only it can know these) and, when a
// datastore is configured, the schema-scoped ping result (reusing the SAME debounced
// check healthGate.serving() uses — never a second live ping) plus connection-pool
// saturation (DESIGN_console_performance §0.3b, §2 — the "silent killer" signal the
// recent pool-bounds work made meaningful for the first time).
func vitalsResponse(ctx context.Context, h *handler) *modulerpc.HandleResponse {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	lastPauseMs := 0.0
	if m.NumGC > 0 {
		lastPauseMs = float64(m.PauseNs[(m.NumGC+255)%256]) / float64(time.Millisecond)
	}
	body := map[string]any{
		"runtime": map[string]any{
			"goroutines":    runtime.NumGoroutine(),
			"heapAllocMB":   float64(m.HeapAlloc) / (1 << 20),
			"numGC":         m.NumGC,
			"lastGCPauseMs": lastPauseMs,
		},
	}
	if h.db == nil {
		body["db"] = map[string]any{"configured": false}
	} else {
		ok, pingMs := h.health.check(ctx)
		body["db"] = map[string]any{"configured": true, "ok": ok, "pingMs": pingMs}
		ps := h.db.Stats()
		body["pool"] = map[string]any{
			"open":           ps.OpenConnections,
			"inUse":          ps.InUse,
			"idle":           ps.Idle,
			"waitCount":      ps.WaitCount,
			"waitDurationMs": float64(ps.WaitDuration) / float64(time.Millisecond),
		}
	}
	b, err := json.Marshal(body)
	if err != nil {
		return errResponse(500, modulesdk.ErrorCodeInternal, "vitals marshal", traceIDFromContext(ctx))
	}
	return &modulerpc.HandleResponse{Status: 200, Headers: map[string]string{"content-type": "application/json"}, Body: b}
}

// errResponse answers with the shared error envelope (sdk.md S8). json.Marshal (not
// string concatenation) keeps the body valid JSON whatever the message holds.
func errResponse(status uint32, code, message, traceID string) *modulerpc.HandleResponse {
	body, _ := json.Marshal(modulesdk.ErrorEnvelope{Code: code, Message: message, TraceID: traceID})
	return &modulerpc.HandleResponse{
		Status:  status,
		Headers: map[string]string{"content-type": "application/json"},
		Body:    body,
	}
}

// traceIDFromContext is the active trace ID, or "" when tracing is off.
func traceIDFromContext(ctx context.Context) string {
	spanContext := trace.SpanContextFromContext(ctx)
	if !spanContext.HasTraceID() {
		return ""
	}
	return spanContext.TraceID().String()
}

// healthGate debounces the readiness DB ping behind a short TTL cache so Health
// probes don't synchronously ping Postgres on every call. The ping is SCHEMA-SCOPED
// (DESIGN_console_performance §0.1): a bare db.Ping() only proves Postgres itself is
// reachable, not that THIS module's own schema is — Postgres can be perfectly healthy
// while a module's migrations never ran or its search_path is broken. Querying its own
// migrations ledger table catches that; a bare connectivity ping cannot.
type healthGate struct {
	db    *sql.DB
	token string // the module's schema — validated at scaffold time, safe to embed in SQL

	// gate, when non-nil, forces NOT_SERVING regardless of the DB ping — it holds the
	// reason (e.g. a failed startup Verify / extension-conformance report). This is how a
	// fail-closed deploy gate keeps the module out of rotation until it's fixed.
	gate atomic.Pointer[string]

	mu         sync.Mutex
	last       time.Time
	serveOK    bool
	lastPingMs float64
}

// forceUnready pins the gate to NOT_SERVING with a human-readable reason; ready() clears it.
func (g *healthGate) forceUnready(reason string) { g.gate.Store(&reason) }
func (g *healthGate) ready()                     { g.gate.Store(nil) }

func newHealthGate(db *sql.DB, token string) *healthGate { return &healthGate{db: db, token: token} }

func (g *healthGate) serving(ctx context.Context) bool {
	if g.gate.Load() != nil {
		return false // pinned NOT_SERVING by a failed startup gate (fail-closed)
	}
	ok, _ := g.check(ctx)
	return ok
}

// check is serving()'s superset: it also returns the ping's own latency, for the
// /_mycel/vitals route (DESIGN_console_performance §2). Same debounce, same cache —
// vitals reads the SAME cached result Health does, never a second live ping.
func (g *healthGate) check(ctx context.Context) (ok bool, pingMs float64) {
	if g.db == nil {
		return true, 0 // no datastore to gate on
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if time.Since(g.last) < healthPingTTL && !g.last.IsZero() {
		return g.serveOK, g.lastPingMs
	}
	// Detached from the caller's context on purpose. This verdict is CACHED for
	// healthPingTTL and shared by every subsequent probe, so inheriting the caller meant
	// one client hanging up mid-probe cancelled the query, recorded the module as
	// NOT_SERVING, and served that answer to everyone for the next 2 seconds — a browser
	// refresh could make a perfectly healthy module report itself down. The module's own
	// readiness is not the caller's to cancel; only the 1s timeout bounds it.
	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
	defer cancel()
	start := time.Now()
	// SELECT 1 FROM <token>._mycel_migrations — reaches the module's OWN schema, not
	// just the database. An empty ledger (a module with zero shipped migrations, e.g.
	// a data-less module) is a NO-ROWS result, not a failure — the table itself being
	// queryable IS the health signal; ErrNoRows means "reachable and empty", not "down".
	var discard int
	err := g.db.QueryRowContext(pctx, "SELECT 1 FROM "+g.token+"._mycel_migrations LIMIT 1").Scan(&discard)
	g.lastPingMs = float64(time.Since(start)) / float64(time.Millisecond)
	g.serveOK = err == nil || err == sql.ErrNoRows
	g.last = time.Now()
	return g.serveOK, g.lastPingMs
}

// txHost implements modulesdk.Host over a request-scoped *sql.Tx. It carries the
// inbound request context so Query/Exec honour the host's deadline/cancel — threaded
// internally to keep the public modulesdk.Host signatures (no ctx arg) unchanged.
type txHost struct {
	ctx context.Context
	tx  *sql.Tx
}

// lazyTxHost is the per-request Host: it opens the request's transaction on the route's
// FIRST Query or Exec, not before Route runs. A route that never touches the datastore —
// /openapi.json, /info, /health, one that only calls other services — then costs no
// BEGIN/SET LOCAL/COMMIT round trips and holds no pooled connection.
//
// BeginTx carries the inbound request context, so a host deadline/cancel aborts the
// transaction (and its statements) instead of running detached. A failure to open is
// returned to the route as the error of the call that needed it, and is sticky for the
// request, so a route never runs half its statements outside the transaction.
type lazyTxHost struct {
	txHost
	db    *sql.DB
	token string
	mu    sync.Mutex
	err   error
}

func (h *lazyTxHost) open() (*txHost, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.tx != nil || h.err != nil {
		return &h.txHost, h.err
	}
	t, err := h.db.BeginTx(h.ctx, nil)
	if err != nil {
		h.err = fmt.Errorf("db begin: %w", err)
		return nil, h.err
	}
	if _, err := t.ExecContext(h.ctx, "SET LOCAL search_path TO "+h.token); err != nil { // token validated
		_ = t.Rollback()
		h.err = fmt.Errorf("search_path: %w", err)
		return nil, h.err
	}
	h.tx = t
	return &h.txHost, nil
}

func (h *lazyTxHost) Query(q string, params []string) ([]modulesdk.Row, error) {
	t, err := h.open()
	if err != nil {
		return nil, err
	}
	return t.Query(q, params)
}

func (h *lazyTxHost) Exec(q string, params []string) (uint64, error) {
	t, err := h.open()
	if err != nil {
		return 0, err
	}
	return t.Exec(q, params)
}

// opened is the transaction the route caused, or nil if it never touched the datastore.
// Nil-safe so the no-DB path needs no branch.
func (h *lazyTxHost) opened() *sql.Tx {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.tx
}

func (h *lazyTxHost) rollback() {
	if tx := h.opened(); tx != nil {
		_ = tx.Rollback()
	}
}

func (h *txHost) Query(q string, params []string) ([]modulesdk.Row, error) {
	rows, err := h.tx.QueryContext(h.ctx, q, toArgs(params)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	var out []modulesdk.Row
	for rows.Next() {
		cells := make([]sql.NullString, len(cols))
		ptrs := make([]any, len(cols))
		for i := range cells {
			ptrs[i] = &cells[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		strs := make([]string, len(cols))
		for i, c := range cells {
			if c.Valid {
				strs[i] = c.String
			}
		}
		out = append(out, modulesdk.Row{Cells: strs})
	}
	return out, rows.Err()
}

func (h *txHost) Exec(q string, params []string) (uint64, error) {
	res, err := h.tx.ExecContext(h.ctx, q, toArgs(params)...)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return uint64(n), nil
}

func (h *txHost) Log(level, msg string) { log.Printf("[%s] %s", level, msg) }
func (h *txHost) NowMillis() uint64     { return uint64(time.Now().UnixMilli()) }
func (h *txHost) CallExt(envKey, method, path string, body []byte) (uint16, []byte, bool, error) {
	return callExt(h.ctx, envKey, method, path, body)
}

// --- extension hooks: a base module delegates client-specific decisions to an
// extension service over gRPC (the same primitive the host uses to dial modules). ---

// CallExt is Host.CallExt for callers that have no Host — a BACKGROUND goroutine.
// Host is per-request; a queue flusher or a rollup ticker outlives every request and
// still needs the same module→module dial, with the same connection pool and the same
// semantics (status is the target's HTTP-ish status, so a non-2xx returns err == nil;
// configured=false means the env var is unset, not a failure).
//
// It exists so a package like ./analytics can take a transport as a plain func value
// and never import this package — keeping it free of gRPC and testable with a fake.
func CallExt(ctx context.Context, envKey, method, path string, body []byte) (uint16, []byte, bool, error) {
	return callExt(ctx, envKey, method, path, body)
}

// extConns pools one multiplexed connection per extension address. A sync.Map, not a
// mutex-guarded map: this is on the request path of every module that wires an
// extension, and a single process-wide lock taken per call serialized all of them —
// including calls to different targets. The host removed exactly this contention from
// its own dialer; the SDK had kept it.
var extConns sync.Map // addr → *grpc.ClientConn

// extCallTimeout bounds one extension hop. Overridable because it has to fit INSIDE the
// caller's own budget: the host allows a module 5s total (MYCEL_HANDLE_TIMEOUT), so an
// extension hop that also takes 5s guarantees the host times out first and the module
// never gets to answer gracefully.
var extCallTimeout = envDuration("MYCEL_EXT_TIMEOUT_MS", 3*time.Second)

// extDialOptions mirrors the host's hardened dial set. Two of these were missing here
// and their absence is a recovery bug, not a tuning preference:
//
//   - Keepalive: without it a half-open connection (a NAT/conntrack eviction, a pod that
//     vanished without a FIN) is only discovered when a call burns its whole timeout.
//   - ConnectParams: grpc-go's default reconnect backoff climbs to 120 SECONDS and
//     nothing resets it, so an extension that came back stayed unreachable for minutes.
//
// UNAVAILABLE can follow an applied write. Select retries by HTTP method in
// callExt rather than applying a blanket policy to the Handle RPC.
func extDialOptions() []grpc.DialOption {
	return []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithStatsHandler(otelgrpc.NewClientHandler()),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                10 * time.Second,
			Timeout:             3 * time.Second,
			PermitWithoutStream: true,
		}),
		grpc.WithConnectParams(grpc.ConnectParams{
			Backoff: backoff.Config{
				BaseDelay: 1 * time.Second, Multiplier: 1.6, Jitter: 0.2,
				MaxDelay: 15 * time.Second,
			},
			MinConnectTimeout: 5 * time.Second,
		}),
		grpc.WithDisableRetry(),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(maxMsgBytes),
			grpc.MaxCallSendMsgSize(maxMsgBytes),
		),
	}
}

// callExt dials the extension service at the URL in os.Getenv(envKey) (a ModuleHandler
// backend) and calls Handle{method, path, body}. configured=false when the env var is
// unset (no extension wired). Connections are pooled per address. The parent context
// bounds the call so a host cancel/deadline propagates to the extension hop.
//
// Note for callers: a non-2xx from the extension comes back with err == nil, because the
// status is the TARGET's HTTP-ish status, not a transport failure. Check it. Silently
// treating a 503 as "no answer" is how a transient blip turns into an identity minted
// without its enriched roles — which is why a failure here is logged rather than dropped.
func callExt(parent context.Context, envKey, method, path string, body []byte) (uint16, []byte, bool, error) {
	addr := os.Getenv(envKey)
	if addr == "" {
		return 0, nil, false, nil
	}
	cl, err := extClient(addr)
	if err != nil {
		log.Printf("ext %s: dial %s: %v", envKey, addr, err)
		return 0, nil, true, err
	}
	ctx, cancel := context.WithTimeout(parent, extCallTimeout)
	defer cancel()
	req := &modulerpc.HandleRequest{Method: method, Path: path, Body: body}
	var resp *modulerpc.HandleResponse
	for attempt := 0; ; attempt++ {
		resp, err = cl.Handle(ctx, req)
		if err == nil || status.Code(err) != codes.Unavailable || attempt >= 2 ||
			(method != http.MethodGet && method != http.MethodHead && method != http.MethodOptions) {
			break
		}
		timer := time.NewTimer(time.Duration(100*(1<<attempt)) * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
		if ctx.Err() != nil {
			err = ctx.Err()
			break
		}
	}
	if err != nil {
		// Log it: the callers of this are best-effort by design (an unreachable
		// extension leaves the base identity untouched), so without a line here a
		// degraded answer is indistinguishable from a correct one.
		log.Printf("ext %s: %s %s failed: %v", envKey, method, path, err)
		return 0, nil, true, err
	}
	return uint16(resp.GetStatus()), resp.GetBody(), true, nil
}

// extClient returns the pooled client for addr, dialing on first use. The hot path is a
// lock-free map read; a construction race is cheap because grpc.NewClient does not
// connect, so the loser just closes its unused conn.
func extClient(addr string) (modulerpc.ModuleHandlerClient, error) {
	if v, ok := extConns.Load(addr); ok {
		return modulerpc.NewModuleHandlerClient(v.(*grpc.ClientConn)), nil
	}
	conn, err := grpc.NewClient(strings.TrimPrefix(addr, "grpc://"), extDialOptions()...)
	if err != nil {
		return nil, err
	}
	if actual, loaded := extConns.LoadOrStore(addr, conn); loaded {
		_ = conn.Close()
		conn = actual.(*grpc.ClientConn)
	}
	return modulerpc.NewModuleHandlerClient(conn), nil
}

// closeExtConns closes every pooled extension connection. Called from cleanup so a
// draining module releases them instead of leaving sockets to the process exit.
func closeExtConns() {
	extConns.Range(func(k, v any) bool {
		if c, ok := v.(*grpc.ClientConn); ok {
			_ = c.Close()
		}
		extConns.Delete(k)
		return true
	})
}

// noDBHost is the host when DATABASE_URL is unset: static routes work; data routes
// get a clear error rather than a nil-pointer panic. CallExt carries the request context.
type noDBHost struct{ ctx context.Context }

func (noDBHost) Query(string, []string) ([]modulesdk.Row, error) { return nil, errNoDB }
func (noDBHost) Exec(string, []string) (uint64, error)           { return 0, errNoDB }
func (noDBHost) Log(level, msg string)                           { log.Printf("[%s] %s", level, msg) }
func (noDBHost) NowMillis() uint64                               { return uint64(time.Now().UnixMilli()) }
func (h noDBHost) CallExt(envKey, method, path string, body []byte) (uint16, []byte, bool, error) {
	ctx := h.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	return callExt(ctx, envKey, method, path, body)
}

var errNoDB = errString("DATABASE_URL not set — this module's data routes are unavailable")

type errString string

func (e errString) Error() string { return string(e) }

func toArgs(params []string) []any {
	a := make([]any, len(params))
	for i, p := range params {
		a[i] = p
	}
	return a
}

// bridgeHTTP serves a gRPC HandleRequest through an http.Handler and maps the
// recorded response back — the seam for Descriptor.Handler modules. The path arrives
// with the /m/<token> prefix already stripped by the host.
func bridgeHTTP(ctx context.Context, h http.Handler, req *modulerpc.HandleRequest) *modulerpc.HandleResponse {
	path := req.GetPath()
	if path == "" {
		path = "/"
	} else if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	target := path
	if q := req.GetQuery(); q != "" {
		target += "?" + q
	}
	method := req.GetMethod()
	if method == "" {
		method = http.MethodGet
	}

	hr := httptest.NewRequestWithContext(ctx, method, target, bytes.NewReader(req.GetBody()))
	if fields := req.GetHeaderFields(); len(fields) > 0 {
		for _, field := range fields {
			for _, value := range field.GetValues() {
				hr.Header.Add(field.GetName(), value)
			}
		}
	} else {
		for k, v := range req.GetHeaders() {
			hr.Header.Set(k, v)
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, hr)

	result := rec.Result()
	defer result.Body.Close()
	names := make([]string, 0, len(result.Header))
	for name := range result.Header {
		names = append(names, name)
	}
	sort.Strings(names)
	headers := make(map[string]string, len(result.Header))
	fields := make([]*modulerpc.HeaderField, 0, len(names))
	for _, k := range names {
		values := result.Header[k]
		separator := ", "
		if strings.EqualFold(k, "set-cookie") {
			separator = "\n"
		}
		headers[k] = strings.Join(values, separator)
		fields = append(fields, &modulerpc.HeaderField{Name: strings.ToLower(k), Values: append([]string(nil), values...)})
	}
	return &modulerpc.HandleResponse{Status: uint32(rec.Code), Headers: headers, HeaderFields: fields, Body: rec.Body.Bytes()}
}

// peerAuthListener wraps a unix-socket listener and rejects connections whose peer
// uid is not wantUID. This is best-effort defence in depth: if the peer credentials
// cannot be read (unsupported OS, race), the connection is allowed and the failure is
// logged — never fatal. The socket dir is already 0700, so this is a second gate.
type peerAuthListener struct {
	net.Listener
	token   string
	wantUID int
}

func (l *peerAuthListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		uc, ok := c.(*net.UnixConn)
		if !ok {
			return c, nil // not a unix conn (shouldn't happen on a unix listener)
		}
		uid, ok, perr := peerUID(uc)
		if perr != nil {
			// Couldn't read creds — log once and allow (the 0700 dir is the hard gate).
			log.Printf("%s: peer-uid check unavailable: %v", l.token, perr)
			return c, nil
		}
		if ok && uid != l.wantUID {
			log.Printf("%s: rejecting unix peer uid=%d (want %d)", l.token, uid, l.wantUID)
			_ = c.Close()
			continue // wait for the next connection
		}
		return c, nil
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// intEnv reads a positive integer env var, falling back on unset/invalid.
func intEnv(key string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(key)); err == nil && v > 0 {
		return v
	}
	return def
}

// envDuration reads a positive millisecond env var as a Duration, falling back on
// unset/invalid — a timeout you cannot change without cutting a release is a timeout you
// cannot change during an incident.
func envDuration(key string, def time.Duration) time.Duration {
	if ms := intEnv(key, 0); ms > 0 {
		return time.Duration(ms) * time.Millisecond
	}
	return def
}
