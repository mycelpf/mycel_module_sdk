package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	modulesdk "github.com/mycelpf/mycel_module_sdk"
	modulerpc "github.com/mycelpf/mycel_module_sdk/contracts/modulerpc/v1"
	"github.com/mycelpf/mycel_module_sdk/wire"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/keepalive"
)

// verificationAttemptTimeout is the default per-attempt deadline of VerifyContext
// (sdk.md S3).
const verificationAttemptTimeout = 5 * time.Second

// HostReservedVariables are the base host's variables (core I11, sdk.md S7). The
// supervisor delivers them to app_core only; a typed (schema 1) module process that sees
// one refuses to boot, because its environment was not filtered.
func HostReservedVariables() []string {
	return []string{"MYCEL_HOST_CLIENT_ID", "MYCEL_HOST_CLIENT_PROOF_KEY", "MYCEL_HOST_IAM_URL"}
}

// Serve launches the module's gRPC server from a Descriptor and blocks until it
// exits. It reads MYCEL_LISTEN (the backend the host dials; default a per-token unix
// socket) and DATABASE_URL (the module's own datastore; absent → DB routes error).
// MYCEL_PPROF_ADDR (default unset = off) opts into a loopback-only Go pprof listener.
//
// Boot order: validate the descriptor (a malformed declaration exits non-zero without
// retry), apply the release protections of a typed descriptor, listen, open and migrate
// the datastore, serve Describe and health, run startup verification, run Start, then
// report SERVING. On SIGTERM/SIGINT it stops verification, drains in-flight RPCs, cancels
// the start-hook context and waits for its work, runs Shutdown, closes the datastore and
// removes the unix socket, all inside the 15-second grace. It exits non-zero when boot
// fails, the server fails or Start fails.
func Serve(d modulesdk.Descriptor) {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	code := run(ctx, d, bootDependencies{
		openDatabase:     func(dsn string) (*sql.DB, error) { return sql.Open("postgres", dsn) },
		markNonDumpable:  markNonDumpable,
		developmentBuild: developmentBuild,
	})
	stop()
	if code != 0 {
		os.Exit(code)
	}
}

// bootDependencies are the parts of boot that tests replace.
type bootDependencies struct {
	openDatabase     func(dsn string) (*sql.DB, error)
	markNonDumpable  func() error
	developmentBuild bool
	// attemptTimeout overrides verificationAttemptTimeout when positive.
	attemptTimeout time.Duration
}

// validateBoot checks everything about the descriptor that cannot change by retrying
// (sdk.md S3): the discovery data, the encoded metadata budget and the hook rules.
func validateBoot(d modulesdk.Descriptor) (*modulerpc.DescribeResponse, error) {
	if d.Verify != nil && d.VerifyContext != nil {
		return nil, fmt.Errorf("%w: set Verify or VerifyContext, not both", modulesdk.ErrInvalidDescriptor)
	}
	if d.Start != nil && d.Handler != nil {
		return nil, fmt.Errorf("%w: Start is allowed only with Route; a Handler module owns its own datastore", modulesdk.ErrInvalidDescriptor)
	}
	discovery := d.Discovery()
	if err := modulesdk.ValidateDiscovery(discovery); err != nil {
		return nil, err
	}
	if d.DescriptorSchemaMajor == modulesdk.DescriptorSchemaLegacy && len(d.OpenAPI) > 0 {
		// A legacy module keeps booting; a typed one fails in ValidateDiscovery (sdk.md S6).
		if err := modulesdk.ValidateOpenAPI(d.Token, d.OpenAPI); err != nil {
			log.Printf("%s: warning: %v; app_core federation will refuse this document", d.Token, err)
		}
	}
	response := wire.DescribeResponseFromDiscovery(discovery)
	if err := wire.ValidateDescribeResponse(response); err != nil {
		return nil, err
	}
	return response, nil
}

// checkReleaseBoundary applies the v0.2 process rules to a typed (schema 1) descriptor
// (sdk.md S7, core I7, I11, I12). Legacy descriptors keep their current behaviour until
// they are rebuilt as typed modules.
func checkReleaseBoundary(d modulesdk.Descriptor, network string, dependencies bootDependencies) error {
	if d.DescriptorSchemaMajor < modulesdk.DescriptorSchemaTyped {
		return nil
	}
	if err := dependencies.markNonDumpable(); err != nil {
		return fmt.Errorf("mark the process non-dumpable: %w", err)
	}
	for _, name := range HostReservedVariables() {
		if _, present := os.LookupEnv(name); present {
			return fmt.Errorf("host-reserved variable %s is in this module's environment; the supervisor must deliver it to app_core only", name)
		}
	}
	if network != "unix" && !dependencies.developmentBuild {
		return errors.New("a released module serves only on a unix socket; TCP needs a build with the mycel_module_development tag")
	}
	return nil
}

func run(ctx context.Context, d modulesdk.Descriptor, dependencies bootDependencies) int {
	describe, err := validateBoot(d)
	if err != nil {
		log.Printf("%s: boot refused: %v", d.Token, err)
		return 1
	}

	addr := envOr("MYCEL_LISTEN", "unix://"+filepath.Join(defaultSocketDir, d.Token+".sock"))
	network, address := "tcp", addr
	if s, ok := strings.CutPrefix(addr, "unix://"); ok {
		network, address = "unix", s
	}
	if err := checkReleaseBoundary(d, network, dependencies); err != nil {
		log.Printf("%s: boot refused: %v", d.Token, err)
		return 1
	}
	if network == "unix" {
		// Validate the UDS path length BEFORE listening: the kernel's sockaddr_un.sun_path
		// is fixed-size (104 on darwin, 108 on linux), and an over-long path is silently
		// truncated, so fail loudly with the actual limit instead of dialing a wrong socket.
		if err := validateUDSPath(address); err != nil {
			log.Printf("%s: %v", d.Token, err)
			return 1
		}
		// 0700: only this uid may enter the dir (defence in depth alongside peer-uid auth).
		_ = os.MkdirAll(filepath.Dir(address), 0o700)
		_ = os.Remove(address) // clear a stale socket from a prior run
	}
	lis, err := net.Listen(network, address)
	if err != nil {
		log.Printf("%s: listen %s: %v", d.Token, addr, err)
		return 1
	}
	// On a unix socket, gate accepted connections on the peer's uid (best effort).
	if network == "unix" {
		lis = &peerAuthListener{Listener: lis, token: d.Token, wantUID: os.Getuid()}
	}

	var db *sql.DB
	if dsn := os.Getenv("DATABASE_URL"); dsn != "" {
		if db, err = dependencies.openDatabase(dsn); err != nil {
			log.Printf("%s: open db: %v", d.Token, err)
			_ = lis.Close()
			return 1
		}
		// Bound the pool: sql.Open defaults to UNLIMITED open conns, so a request
		// spike from one module can exhaust Postgres max_connections for the whole
		// federation. Idle conns stay warm; a max lifetime recycles conns through
		// poolers/failovers.
		db.SetMaxOpenConns(intEnv("MYCEL_DB_MAX_OPEN", 16))
		db.SetMaxIdleConns(intEnv("MYCEL_DB_MAX_IDLE", 4))
		db.SetConnMaxLifetime(30 * time.Minute)
		if err := applyMigrations(db, d); err != nil {
			log.Printf("%s: migrate: %v", d.Token, err)
			_ = db.Close()
			_ = lis.Close()
			return 1
		}
	} else {
		log.Printf("%s: DATABASE_URL unset — DB-backed routes will error", d.Token)
	}

	// OpenTelemetry OTLP trace export — no-op unless OTEL_EXPORTER_OTLP_ENDPOINT is set.
	shutdownTelemetry, err := initTelemetry(context.Background(), envOr("OTEL_SERVICE_NAME", d.Token))
	if err != nil {
		log.Printf("%s: telemetry init: %v (continuing without traces)", d.Token, err)
	}

	// Opt-in, loopback-only pprof. A refused/failed address is logged and the module keeps
	// serving: the debug listener must never be able to take the module down.
	stopPprof := func(context.Context) error { return nil }
	if paddr := os.Getenv(pprofEnv); paddr != "" {
		if stop, bound, perr := startPprof(paddr); perr != nil {
			log.Printf("%s: pprof disabled: %v", d.Token, perr)
		} else {
			stopPprof = stop
			log.Printf("WARN %s: pprof ENABLED on http://%s/debug/pprof/ — loopback-only, reach it with kubectl port-forward; never expose it", d.Token, bound)
		}
	}

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

	// serverLifetime is the start hook's context: cancelled only after RPCs drain.
	// Verification instead stops as soon as shutdown begins (ctx).
	serverLifetime, cancelServerLifetime := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelServerLifetime()
	background := &backgroundWork{ctx: serverLifetime}
	startupFailed := make(chan error, 1)
	startupDone := make(chan struct{})

	// Hold NOT_SERVING until verification passes and Start returns. Runs in the
	// background so Describe and health already answer (sdk.md S3).
	if d.Verify != nil || d.VerifyContext != nil || d.Start != nil {
		hg.forceUnready("startup verification pending")
	}
	go func() {
		defer close(startupDone)
		if !runVerifyGate(ctx, d, hg, dependencies.attemptTimeout) {
			return // shutdown began during verification
		}
		if d.Start != nil {
			if err := d.Start(serverLifetime, newDatastore(db, d.Token), background); err != nil {
				startupFailed <- err
				return
			}
		}
		hg.ready()
	}()

	modulerpc.RegisterModuleHandlerServer(srv, &handler{d: d, db: db, health: hg, describe: describe})
	// The standard health service too, so k8s can probe this module natively — see
	// health_grpc.go. Same gate, so the two answers can never disagree.
	healthpb.RegisterHealthServer(srv, &grpcHealth{gate: hg})
	log.Printf("%s module serving gRPC on %s://%s", d.Token, network, address)

	serveFailed := make(chan error, 1)
	go func() { serveFailed <- srv.Serve(lis) }()

	code := 0
	select {
	case err := <-serveFailed:
		if err != nil {
			log.Printf("%s: serve: %v", d.Token, err)
			code = 1
		}
	case err := <-startupFailed:
		log.Printf("%s: start hook failed: %v — exiting", d.Token, redactCredentials(err.Error()))
		code = 1
	case <-ctx.Done():
		log.Printf("%s: shutdown signal received, draining…", d.Token)
	}

	// Shutdown order (sdk.md S7, S11.3): stop new calls and drain accepted RPCs, cancel
	// the start-hook context and wait for its work, run Shutdown, close resources — all
	// inside shutdownGrace.
	deadline := time.Now().Add(shutdownGrace)
	drained := make(chan struct{})
	go func() { srv.GracefulStop(); close(drained) }()
	select {
	case <-drained:
	case <-time.After(time.Until(deadline)):
		log.Printf("%s: graceful shutdown timed out after %s, forcing stop", d.Token, shutdownGrace)
		srv.Stop()
	}
	cancelServerLifetime()
	if !waitUntil(deadline, startupDone, background.done()) {
		log.Printf("%s: start-hook work did not finish inside the shutdown grace", d.Token)
	}

	_ = stopPprof(context.Background())
	cleanupWithin(d, db, network, address, shutdownTelemetry, deadline)
	return code
}

// waitUntil waits for every channel to close, or for the deadline. It reports whether
// all of them closed.
func waitUntil(deadline time.Time, channels ...<-chan struct{}) bool {
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	for _, channel := range channels {
		select {
		case <-channel:
		case <-timer.C:
			return false
		}
	}
	return true
}

// cleanup is cleanupWithin without an overall deadline.
func cleanup(d modulesdk.Descriptor, db *sql.DB, network, address string, flushTel telemetryShutdown) {
	cleanupWithin(d, db, network, address, flushTel, time.Time{})
}

// cleanupWithin is the one shutdown path. The order is load-bearing:
//
//	GracefulStop and start-hook work (already done by the caller) → Descriptor.Shutdown → db.Close → socket
//
// Shutdown runs only once the gRPC server has stopped accepting, so a handler cannot
// enqueue into a queue that is already draining; and before db.Close, so the hook may
// still write. A non-zero deadline caps the hook's time so the whole sequence stays
// inside the shutdown grace.
func cleanupWithin(d modulesdk.Descriptor, db *sql.DB, network, address string, flushTel telemetryShutdown, deadline time.Time) {
	if d.Shutdown != nil {
		timeout := time.Duration(intEnv("MYCEL_SHUTDOWN_TIMEOUT_MS", 8000)) * time.Millisecond
		if !deadline.IsZero() {
			if remaining := time.Until(deadline); remaining < timeout {
				timeout = max(remaining, 0)
			}
		}
		// context.Background(), not any inherited ctx: the signal has already fired, so
		// every request-scoped context is cancelled and the hook would get no time at all.
		hookContext, cancel := context.WithTimeout(context.Background(), timeout)
		if err := d.Shutdown(hookContext); err != nil {
			// Not fatal — the process is exiting regardless. The error exists to leave a
			// trace of what could NOT be delivered, which is the whole point of the hook.
			log.Printf("%s: shutdown hook: %v", d.Token, err)
		}
		cancel()
	}
	// After the hook, which may itself have used CallExt to drain (the analytics client
	// does exactly that), and before the process goes.
	closeExtConns()
	if db != nil {
		_ = db.Close()
	}
	if network == "unix" {
		_ = os.Remove(address)
	}
	if flushTel != nil {
		flushContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = flushTel(flushContext)
		cancel()
	}
	log.Printf("%s: stopped", d.Token)
}

// Verify-gate retry shape. The gate is fail-closed but NOT one-shot: it keeps probing
// until it passes, so a collaborator that is fixed or finishes booting brings this module
// back on its own. vars, not consts, so tests can shrink the waits.
var (
	verifyBackoffBase = 2 * time.Second
	verifyBackoffMax  = 30 * time.Second
	verifyLoudAfter   = 12              // ~22s of quiet grace before the banner
	verifyRemindEvery = 5 * time.Minute // then a reminder at this cadence, not per attempt
)

// runVerifyGate runs the module's verification hook until it passes, holding the module
// NOT_SERVING meanwhile. It retries with capped exponential backoff and full jitter and
// never gives up while the process runs: a dependency still booting, down, or deployed
// non-conformant and then fixed are all repaired without redeploying this module.
//
// It returns false when ctx (shutdown) is cancelled first. The backoff wait is
// cancellable. VerifyContext attempts get a per-attempt deadline derived from ctx, so a
// hook that honours its context stops at once. A legacy Verify cannot be interrupted: it
// gets a Host whose CallExt carries ctx, but code that ignores that runs to completion.
//
// The failure report is logged with credentials redacted and kept off the wire: a gated
// Handle answers with a fixed verification_pending envelope (sdk.md S8).
func runVerifyGate(ctx context.Context, d modulesdk.Descriptor, hg *healthGate, attemptTimeout time.Duration) bool {
	if d.Verify == nil && d.VerifyContext == nil {
		return true
	}
	if attemptTimeout <= 0 {
		attemptTimeout = verificationAttemptTimeout
	}
	delay := verifyBackoffBase
	failed := false
	lastLoud := time.Time{}

	for attempt := 1; ; attempt++ {
		if ctx.Err() != nil {
			return false
		}
		var err error
		if d.VerifyContext != nil {
			attemptContext, cancel := context.WithTimeout(ctx, attemptTimeout)
			err = d.VerifyContext(attemptContext, verificationHost{ctx: attemptContext})
			cancel()
		} else {
			err = d.Verify(verificationHost{ctx: ctx})
		}
		if ctx.Err() != nil {
			return false
		}
		if err == nil {
			if failed {
				log.Printf("%s: startup verification passed after %d attempts (recovered without a redeploy)", d.Token, attempt)
			} else {
				log.Printf("%s: startup verification passed", d.Token)
			}
			return true
		}

		report := redactCredentials(err.Error())
		hg.forceUnready(report)

		// Loud once when it stops looking like ordinary boot lag, then only at a slow
		// cadence: this loop runs forever, and a banner per attempt would bury the logs.
		if attempt == verifyLoudAfter || (failed && time.Since(lastLoud) >= verifyRemindEvery) {
			bar := strings.Repeat("=", 72)
			log.Printf("\n%s\n%s: STARTUP VERIFICATION FAILING — holding NOT_SERVING (fail-closed), still retrying\n%s\n%s",
				bar, d.Token, report, bar)
			lastLoud = time.Now()
			failed = true
		}

		// Full jitter on a capped exponential: without it every module in the fleet
		// re-probes a struggling collaborator in lockstep.
		timer := time.NewTimer(time.Duration(rand.Int63n(int64(delay) + 1)))
		select {
		case <-ctx.Done():
			timer.Stop()
			return false
		case <-timer.C:
		}
		if delay < verifyBackoffMax {
			if delay *= 2; delay > verifyBackoffMax {
				delay = verifyBackoffMax
			}
		}
	}
}

// verificationHost is the Host given to verification hooks: no database, and CallExt
// bound to the verification context so cancellation reaches extension calls.
type verificationHost struct{ ctx context.Context }

func (verificationHost) Query(string, []string) ([]modulesdk.Row, error) { return nil, errNoDB }
func (verificationHost) Exec(string, []string) (uint64, error)           { return 0, errNoDB }
func (verificationHost) Log(level, msg string)                           { log.Printf("[%s] %s", level, msg) }
func (verificationHost) NowMillis() uint64                               { return uint64(time.Now().UnixMilli()) }
func (h verificationHost) CallExt(envKey, method, path string, body []byte) (uint16, []byte, bool, error) {
	return callExt(h.ctx, envKey, method, path, body)
}

// credentialPatterns match credential-looking text in diagnostic logs.
var credentialPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(bearer|basic)\s+[A-Za-z0-9._~+/=-]+`),
	regexp.MustCompile(`(?i)((?:password|passwd|secret|token|api[_-]?key|client[_-]?secret|proof[_-]?key)["']?\s*[:=]\s*)["']?[^\s"',&]+`),
	regexp.MustCompile(`://[^/\s:@]+:[^/\s@]+@`),
}

// redactCredentials removes credential-looking values from text bound for the log.
func redactCredentials(text string) string {
	text = credentialPatterns[0].ReplaceAllString(text, "$1 [redacted]")
	text = credentialPatterns[1].ReplaceAllString(text, "${1}[redacted]")
	return credentialPatterns[2].ReplaceAllString(text, "://[redacted]@")
}

// backgroundWork tracks goroutines the start hook starts, so shutdown can wait for them.
type backgroundWork struct {
	ctx   context.Context
	group sync.WaitGroup
}

func (b *backgroundWork) Go(fn func(ctx context.Context)) {
	b.group.Add(1)
	go func() {
		defer b.group.Done()
		fn(b.ctx)
	}()
}

// done is closed when every started function has returned.
func (b *backgroundWork) done() <-chan struct{} {
	channel := make(chan struct{})
	go func() { b.group.Wait(); close(channel) }()
	return channel
}

// newDatastore is the start hook's datastore: the route adapter's pool and schema, with
// no request transaction (sdk.md S11.3).
func newDatastore(db *sql.DB, token string) modulesdk.Datastore {
	if db == nil {
		return noDatastore{}
	}
	return &poolDatastore{db: db, token: token}
}

type poolDatastore struct {
	db    *sql.DB
	token string
}

// begin opens a transaction scoped to the module schema.
func (p *poolDatastore) begin(ctx context.Context) (*sql.Tx, error) {
	transaction, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("db begin: %w", err)
	}
	if _, err := transaction.ExecContext(ctx, "SET LOCAL search_path TO "+p.token); err != nil { // token validated at boot
		_ = transaction.Rollback()
		return nil, fmt.Errorf("search_path: %w", err)
	}
	return transaction, nil
}

func (p *poolDatastore) Transaction(ctx context.Context, fn func(modulesdk.Statements) error) error {
	transaction, err := p.begin(ctx)
	if err != nil {
		return err
	}
	if err := fn(transactionStatements{tx: transaction}); err != nil {
		_ = transaction.Rollback()
		return err
	}
	return transaction.Commit()
}

func (p *poolDatastore) Query(ctx context.Context, query string, params []string) (rows []modulesdk.Row, err error) {
	err = p.Transaction(ctx, func(statements modulesdk.Statements) error {
		rows, err = statements.Query(ctx, query, params)
		return err
	})
	return rows, err
}

func (p *poolDatastore) Exec(ctx context.Context, query string, params []string) (affected uint64, err error) {
	err = p.Transaction(ctx, func(statements modulesdk.Statements) error {
		affected, err = statements.Exec(ctx, query, params)
		return err
	})
	return affected, err
}

// transactionStatements runs statements inside one open transaction.
type transactionStatements struct{ tx *sql.Tx }

func (s transactionStatements) Query(ctx context.Context, query string, params []string) ([]modulesdk.Row, error) {
	host := txHost{ctx: ctx, tx: s.tx}
	return host.Query(query, params)
}

func (s transactionStatements) Exec(ctx context.Context, query string, params []string) (uint64, error) {
	host := txHost{ctx: ctx, tx: s.tx}
	return host.Exec(query, params)
}

// noDatastore is the start hook's datastore when DATABASE_URL is unset.
type noDatastore struct{}

func (noDatastore) Query(context.Context, string, []string) ([]modulesdk.Row, error) {
	return nil, errNoDB
}
func (noDatastore) Exec(context.Context, string, []string) (uint64, error) { return 0, errNoDB }
func (noDatastore) Transaction(context.Context, func(modulesdk.Statements) error) error {
	return errNoDB
}
