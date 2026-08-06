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
	"fmt"
	"log"
	"math/rand"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/mycelpf/mycel_module_sdk"
	modulerpc "github.com/mycelpf/mycel_module_sdk/contracts/modulerpc/v1"

	_ "github.com/lib/pq"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
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

// Serve launches the module's gRPC server from a Descriptor and blocks until it
// exits. It reads MYCEL_LISTEN (the backend the host dials; default a per-token unix
// socket) and DATABASE_URL (the module's own datastore; absent → DB routes error).
//
// On SIGTERM/SIGINT it stops accepting, drains in-flight RPCs (GracefulStop, with a
// hard Stop after shutdownGrace), closes the DB, and removes the unix socket file.
func Serve(d modulesdk.Descriptor) {
	addr := envOr("MYCEL_LISTEN", "unix://"+filepath.Join(defaultSocketDir, d.Token+".sock"))
	network, address := "tcp", addr
	if s, ok := strings.CutPrefix(addr, "unix://"); ok {
		network, address = "unix", s
		// Validate the UDS path length BEFORE listening: the kernel's sockaddr_un.sun_path
		// is fixed-size (104 on darwin, 108 on linux), and an over-long path is silently
		// truncated, so fail loudly with the actual limit instead of dialing a wrong socket.
		if err := validateUDSPath(address); err != nil {
			log.Fatalf("%s: %v", d.Token, err)
		}
		// 0700: only this uid may enter the dir (defence in depth alongside peer-uid auth).
		_ = os.MkdirAll(filepath.Dir(address), 0o700)
		_ = os.Remove(address) // clear a stale socket from a prior run
	}
	lis, err := net.Listen(network, address)
	if err != nil {
		log.Fatalf("%s: listen %s: %v", d.Token, addr, err)
	}
	// On a unix socket, gate accepted connections on the peer's uid (best effort).
	if network == "unix" {
		lis = &peerAuthListener{Listener: lis, token: d.Token, wantUID: os.Getuid()}
	}

	var db *sql.DB
	if dsn := os.Getenv("DATABASE_URL"); dsn != "" {
		if db, err = sql.Open("postgres", dsn); err != nil {
			log.Fatalf("%s: open db: %v", d.Token, err)
		}
		// Bound the pool: sql.Open defaults to UNLIMITED open conns, so a request
		// spike from one module can exhaust Postgres max_connections for the whole
		// federation. Idle conns stay warm; a max lifetime recycles conns through
		// poolers/failovers.
		db.SetMaxOpenConns(intEnv("MYCEL_DB_MAX_OPEN", 16))
		db.SetMaxIdleConns(intEnv("MYCEL_DB_MAX_IDLE", 4))
		db.SetConnMaxLifetime(30 * time.Minute)
		if err := applyMigrations(db, d); err != nil {
			log.Fatalf("%s: migrate: %v", d.Token, err)
		}
	} else {
		log.Printf("%s: DATABASE_URL unset — DB-backed routes will error", d.Token)
	}

	// OpenTelemetry OTLP trace export — no-op unless OTEL_EXPORTER_OTLP_ENDPOINT is set.
	// Installs the TracerProvider + W3C propagator the otelgrpc stats handler below needs.
	telCtx := context.Background()
	shutdownTel, err := initTelemetry(telCtx, envOr("OTEL_SERVICE_NAME", d.Token))
	if err != nil {
		log.Printf("%s: telemetry init: %v (continuing without traces)", d.Token, err)
	}
	// Flushed in cleanup(), not deferred: the serve-error branch exits via os.Exit, which
	// skips defers — and dropping the last batch of spans is exactly what you need when
	// diagnosing why serve failed.

	srv := grpc.NewServer(
		grpc.MaxRecvMsgSize(maxMsgBytes),
		grpc.MaxSendMsgSize(maxMsgBytes),
		grpc.ChainUnaryInterceptor(recoverUnary(d.Token)),
		grpc.StatsHandler(otelgrpc.NewServerHandler()),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             5 * time.Second, // reject clients pinging faster than this
			PermitWithoutStream: true,            // the host keepalives idle conns (no active RPC)
		}),
		grpc.KeepaliveParams(keepalive.ServerParameters{
			MaxConnectionIdle: 0,                // host conns are long-lived; don't recycle on idle
			Time:              30 * time.Second, // ping an idle conn to detect a dead host
			Timeout:           10 * time.Second, // ...and drop it if the ping isn't answered
		}),
	)
	hg := newHealthGate(db, d.Token)
	// Deploy-time conformance gate: hold the module NOT_SERVING until Descriptor.Verify
	// passes, so a configured-but-non-conformant collaborator (e.g. a client extension
	// that doesn't honor the contract) makes the whole app unready — fail-closed and
	// loud — instead of silently degrading. Runs in the background so the gRPC server
	// (incl. Health) is already up for k8s readiness probes to observe NOT_SERVING.
	if d.Verify != nil {
		hg.forceUnready("startup verification pending")
		go runVerifyGate(d, hg)
	}
	modulerpc.RegisterModuleHandlerServer(srv, &handler{d: d, db: db, health: hg})
	log.Printf("%s module serving gRPC on %s://%s", d.Token, network, address)

	// Graceful shutdown: trap signals, drain, then clean up the DB + socket.
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(lis) }()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	select {
	case err := <-errCh:
		if err != nil {
			// NOT log.Fatalf: os.Exit would skip the module's Shutdown hook (losing
			// whatever it had queued) and the telemetry flush. Clean up, then exit 1.
			log.Printf("%s: serve: %v", d.Token, err)
			cleanup(d, db, network, address, shutdownTel)
			os.Exit(1)
		}
	case sig := <-sigCh:
		log.Printf("%s: %v received, draining…", d.Token, sig)
		done := make(chan struct{})
		go func() { srv.GracefulStop(); close(done) }()
		select {
		case <-done:
		case <-time.After(shutdownGrace):
			log.Printf("%s: graceful shutdown timed out after %s, forcing stop", d.Token, shutdownGrace)
			srv.Stop()
		}
	}

	cleanup(d, db, network, address, shutdownTel)
}

// cleanup is the ONE shutdown path, run from both the signal branch and the serve-error
// branch. The order is load-bearing:
//
//	GracefulStop (already done by the caller) → Descriptor.Shutdown → db.Close → socket
//
// Shutdown runs only once the gRPC server has stopped accepting, so a handler cannot
// enqueue into a queue that is already draining; and before db.Close, so the hook may
// still write (e.g. flushing to its own datastore).
func cleanup(d modulesdk.Descriptor, db *sql.DB, network, address string, flushTel telemetryShutdown) {
	if d.Shutdown != nil {
		to := time.Duration(intEnv("MYCEL_SHUTDOWN_TIMEOUT_MS", 8000)) * time.Millisecond
		// context.Background(), not any inherited ctx: the signal has already fired, so
		// every request-scoped context is cancelled and the hook would get no time at all.
		sctx, cancel := context.WithTimeout(context.Background(), to)
		if err := d.Shutdown(sctx); err != nil {
			// Not fatal — the process is exiting regardless. The error exists to leave a
			// trace of what could NOT be delivered, which is the whole point of the hook.
			log.Printf("%s: shutdown hook: %v", d.Token, err)
		}
		cancel()
	}
	// After the hook, which may itself have used CallExt to drain (the analytics client
	// does exactly that), and before the process goes: nothing had ever closed these, so
	// they were left to process exit.
	closeExtConns()
	if db != nil {
		_ = db.Close()
	}
	if network == "unix" {
		_ = os.Remove(address)
	}
	if flushTel != nil {
		tctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = flushTel(tctx)
		cancel()
	}
	log.Printf("%s: stopped", d.Token)
}

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
				resp, err = errResponse(500, "internal error"), nil
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
}

func (h *handler) Describe(context.Context, *modulerpc.DescribeRequest) (*modulerpc.DescribeResponse, error) {
	nav := make([]*modulerpc.NavEntry, len(h.d.Nav))
	for i, n := range h.d.Nav {
		nav[i] = &modulerpc.NavEntry{Label: n.Label, Path: n.Path}
	}
	return &modulerpc.DescribeResponse{
		Token:           h.d.Token,
		ContractVersion: h.d.ContractVersion,
		MenuGroup:       h.d.MenuGroup,
		Nav:             nav,
		UiEntry:         h.d.UIEntry,
		Openapi:         h.d.OpenAPI,
	}, nil
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
	if r := h.health.gate.Load(); r != nil {
		// Carry the reason rather than pointing at the logs: the caller is often another
		// module or the console, neither of which can read this process's stderr, and the
		// gate is still retrying — so this is a "not yet", not a verdict.
		return errResponse(503, "module not serving: startup verification failing — "+*r), nil
	}

	// http.Handler modules: bridge the gRPC request to ServeHTTP. The handler owns its
	// own datastore, so the SDK applies no tx/search_path here (it still ran the
	// module's migrations at boot).
	if h.d.Handler != nil {
		return bridgeHTTP(ctx, h.d.Handler, req), nil
	}

	lr := modulesdk.Request{
		Method:  req.GetMethod(),
		Path:    req.GetPath(),
		Query:   req.GetQuery(),
		Headers: req.GetHeaders(),
		Body:    req.GetBody(),
	}

	// Reserved SDK route, namespaced so it can never collide with a module author's
	// own paths: process + DB vitals for the host's /api/system probe
	// (DESIGN_console_performance §2). Intercepted here, before any tx/host setup —
	// module authors never see this route, and it never opens a transaction.
	if lr.Path == "/_mycel/vitals" {
		return vitalsResponse(ctx, h), nil
	}

	// Propagate the active trace context into the request headers (traceparent/
	// tracestate) so module logic — deliberately ctx-free — can forward it on
	// outbound HTTP calls (e.g. a Temporal worker service) and keep the distributed
	// trace stitched across that hop. No-op when tracing is off (no propagator).
	if lr.Headers == nil {
		lr.Headers = map[string]string{}
	}
	otel.GetTextMapPropagator().Inject(ctx, propagation.MapCarrier(lr.Headers))

	// No DB wired → a stub host whose data calls error (static routes still serve).
	var host modulesdk.Host = noDBHost{}
	var tx *sql.Tx
	if h.db != nil {
		// BeginTx carries the inbound request context, so a host deadline/cancel aborts
		// the transaction (and the statements below) instead of running detached.
		t, err := h.db.BeginTx(ctx, nil)
		if err != nil {
			return errResponse(500, "db begin"), nil
		}
		// Backstop for the panic path: recoverUnary turns a panic in Route into a 500,
		// but the transaction below is already open at that point and nothing else would
		// ever release its pool connection. Rollback after a Commit is ErrTxDone — a
		// no-op — so this is safe on every path, not just the panicking one.
		defer t.Rollback()
		if _, err := t.ExecContext(ctx, "SET LOCAL search_path TO "+h.d.Token); err != nil { // token validated
			return errResponse(500, "search_path"), nil
		}
		tx, host = t, &txHost{ctx: ctx, tx: t}
	}

	resp := h.d.Route(lr, host)

	if tx != nil {
		if resp.Status < 500 {
			// The commit error is the answer, not a detail to drop. A connection that
			// dies during COMMIT — precisely what a Postgres failover produces — rolls
			// the transaction back; returning the route's 200 there tells the caller a
			// write succeeded that does not exist.
			if err := tx.Commit(); err != nil {
				log.Printf("%s: commit failed after %d on %s %s: %v", h.d.Token, resp.Status, lr.Method, lr.Path, err)
				return errResponse(500, "commit failed — the request was not applied"), nil
			}
		} else {
			_ = tx.Rollback() // already failing; the rollback error adds nothing
		}
	}

	headers := resp.Headers
	if headers == nil {
		headers = map[string]string{}
	}
	if headers["content-type"] == "" {
		headers["content-type"] = "application/json"
	}
	return &modulerpc.HandleResponse{Status: uint32(resp.Status), Headers: headers, Body: []byte(resp.Body)}, nil
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
		return errResponse(500, "vitals marshal")
	}
	return &modulerpc.HandleResponse{Status: 200, Headers: map[string]string{"content-type": "application/json"}, Body: b}
}

func errResponse(status uint32, msg string) *modulerpc.HandleResponse {
	// json.Marshal (not string concatenation) so a message containing a quote or
	// backslash — e.g. a pq error naming a "quoted" identifier — still yields VALID
	// JSON instead of a body the client can't parse. Marshal of a map[string]string
	// cannot fail, so the error is safe to drop.
	body, _ := json.Marshal(map[string]string{"error": msg})
	return &modulerpc.HandleResponse{
		Status:  status,
		Headers: map[string]string{"content-type": "application/json"},
		Body:    body,
	}
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

// Verify-gate retry shape. The gate is fail-closed but NOT one-shot: it keeps probing
// until it passes, so a collaborator that is fixed or finishes booting brings this module
// back on its own.
// vars, not consts, so tests can shrink the sleeps and exercise the control flow without
// the clock (same reason the CLI's load simulator keeps its backoff knobs assignable).
var (
	verifyBackoffBase = 2 * time.Second
	verifyBackoffMax  = 30 * time.Second
	verifyLoudAfter   = 12              // ~22s of quiet grace before the banner
	verifyRemindEvery = 5 * time.Minute // then a reminder at this cadence, not per attempt
)

// runVerifyGate runs the module's Verify until it passes, holding the module NOT_SERVING
// meanwhile. It never gives up.
//
// It used to stop after 12 attempts (~22s) and pin the gate forever. Nothing cleared that
// pin — there was no re-verify and no path back to ready() — so the module answered 503 on
// EVERY route until a human redeployed it, and module pods carry no livenessProbe, so
// nothing restarted them either. A collaborator that took 23 seconds to boot bricked its
// base module. Since the failure modes that gate legitimately catches — a dependency still
// booting, a dependency down, a dependency deployed non-conformant and then fixed — are
// all repairable WITHOUT touching this module, giving up was never the right answer to any
// of them; retrying is correct for all three and needs no way to tell them apart.
//
// Fail-closed is preserved: the module stays out of rotation the entire time it is failing.
func runVerifyGate(d modulesdk.Descriptor, hg *healthGate) {
	var host modulesdk.Host = noDBHost{}
	delay := verifyBackoffBase
	failed := false
	lastLoud := time.Time{}

	for attempt := 1; ; attempt++ {
		err := d.Verify(host)
		if err == nil {
			hg.ready()
			if failed {
				log.Printf("%s: startup verification passed after %d attempts — module READY (recovered without a redeploy)", d.Token, attempt)
			} else {
				log.Printf("%s: startup verification passed — module READY", d.Token)
			}
			return
		}

		hg.forceUnready(err.Error())

		// Loud once when it stops looking like ordinary boot lag, then only at a slow
		// cadence: this loop runs forever, and a banner per attempt would bury the logs
		// an operator needs to diagnose the collaborator.
		if attempt == verifyLoudAfter || (failed && time.Since(lastLoud) >= verifyRemindEvery) {
			bar := strings.Repeat("=", 72)
			log.Printf("\n%s\n%s: STARTUP VERIFICATION FAILING — holding NOT_SERVING (fail-closed), still retrying\n%s\n%s",
				bar, d.Token, err.Error(), bar)
			lastLoud = time.Now()
			failed = true
		}

		// Full jitter on a capped exponential, matching the analytics flusher: without it
		// every module in the fleet re-probes a struggling collaborator in lockstep.
		time.Sleep(time.Duration(rand.Int63n(int64(delay) + 1)))
		if delay < verifyBackoffMax {
			if delay *= 2; delay > verifyBackoffMax {
				delay = verifyBackoffMax
			}
		}
	}
}

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
// The retry policy covers UNAVAILABLE only. That code means the RPC did not reach a
// server — no ready subchannel, connection refused, or the transport went away before
// the request was sent — so replaying it does not risk double-applying anything. Codes
// that mean the server DID see it are deliberately excluded.
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
		grpc.WithDefaultServiceConfig(`{
  "methodConfig": [{
    "name": [{"service": "mycel.modulerpc.v1.ModuleHandler", "method": "Handle"}],
    "retryPolicy": {
      "maxAttempts": 3,
      "initialBackoff": "0.1s",
      "maxBackoff": "1s",
      "backoffMultiplier": 2.0,
      "retryableStatusCodes": ["UNAVAILABLE"]
    }
  }]
}`),
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
	resp, err := cl.Handle(ctx, &modulerpc.HandleRequest{Method: method, Path: path, Body: body})
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
// get a clear error rather than a nil-pointer panic.
type noDBHost struct{}

func (noDBHost) Query(string, []string) ([]modulesdk.Row, error) { return nil, errNoDB }
func (noDBHost) Exec(string, []string) (uint64, error)           { return 0, errNoDB }
func (noDBHost) Log(level, msg string)                           { log.Printf("[%s] %s", level, msg) }
func (noDBHost) NowMillis() uint64                               { return uint64(time.Now().UnixMilli()) }
func (noDBHost) CallExt(envKey, method, path string, body []byte) (uint16, []byte, bool, error) {
	return callExt(context.Background(), envKey, method, path, body)
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
	for k, v := range req.GetHeaders() {
		hr.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, hr)

	headers := make(map[string]string, len(rec.Header()))
	for k := range rec.Header() {
		headers[k] = rec.Header().Get(k)
	}
	return &modulerpc.HandleResponse{Status: uint32(rec.Code), Headers: headers, Body: rec.Body.Bytes()}
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
