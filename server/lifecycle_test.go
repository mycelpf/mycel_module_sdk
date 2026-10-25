package server

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	modulesdk "github.com/mycelpf/mycel_module_sdk"
	modulerpc "github.com/mycelpf/mycel_module_sdk/contracts/modulerpc/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

// runningModule is one module process run in-process through run(), served on a real
// unix socket.
type runningModule struct {
	socket   string
	stop     context.CancelFunc
	exitCode chan int
	conn     *grpc.ClientConn
	module   modulerpc.ModuleHandlerClient
	health   healthpb.HealthClient
	database *sql.DB
}

// shortSocket returns a unix socket path short enough for darwin's 104-byte limit.
func shortSocket(t *testing.T) string {
	t.Helper()
	directory, err := os.MkdirTemp("/tmp", "sdk")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(directory) })
	return filepath.Join(directory, "m.sock")
}

// registerDriver registers driver under a name unique to this run, so the test can run
// repeatedly in one process (sql.Register panics on a repeated name).
func registerDriver(t *testing.T, prefix string, driver *countingDriver) string {
	t.Helper()
	name := prefix + "_" + strconv.FormatInt(time.Now().UnixNano(), 10)
	sql.Register(name, driver)
	return name
}

func testDependencies() bootDependencies {
	return bootDependencies{
		openDatabase:     func(string) (*sql.DB, error) { return nil, errors.New("no database in this test") },
		markNonDumpable:  func() error { return nil },
		developmentBuild: false,
	}
}

// startModule runs d through run() and returns once its socket accepts connections.
// With driverName set, DATABASE_URL is set and the datastore is a fresh *sql.DB on that
// registered driver.
func startModule(t *testing.T, d modulesdk.Descriptor, dependencies bootDependencies, driverName string) *runningModule {
	t.Helper()
	quietLog(t)
	socket := shortSocket(t)
	t.Setenv("MYCEL_LISTEN", "unix://"+socket)
	module := &runningModule{socket: socket, exitCode: make(chan int, 1)}
	if driverName != "" {
		t.Setenv("DATABASE_URL", "fake://")
		dependencies.openDatabase = func(string) (*sql.DB, error) {
			db, err := sql.Open(driverName, "")
			module.database = db
			return db, err
		}
	} else {
		t.Setenv("DATABASE_URL", "")
	}
	ctx, stop := context.WithCancel(context.Background())
	module.stop = stop
	go func() { module.exitCode <- run(ctx, d, dependencies) }()

	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(socket); err == nil {
			break
		}
		select {
		case code := <-module.exitCode:
			module.exitCode <- code
			return module
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("the module never listened")
		}
		time.Sleep(5 * time.Millisecond)
	}
	conn, err := grpc.NewClient("unix://"+socket, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	module.conn = conn
	module.module = modulerpc.NewModuleHandlerClient(conn)
	module.health = healthpb.NewHealthClient(conn)
	t.Cleanup(func() {
		stop()
		select {
		case <-module.exitCode:
		case <-time.After(20 * time.Second):
		}
	})
	return module
}

// exit stops the module and returns its exit code.
func (m *runningModule) exit(t *testing.T) int {
	t.Helper()
	m.stop()
	select {
	case code := <-m.exitCode:
		m.exitCode <- code
		return code
	case <-time.After(20 * time.Second):
		t.Fatal("the module did not exit")
		return -1
	}
}

// waitExit waits for the module to exit on its own.
func (m *runningModule) waitExit(t *testing.T, within time.Duration) (int, bool) {
	t.Helper()
	select {
	case code := <-m.exitCode:
		m.exitCode <- code
		return code, true
	case <-time.After(within):
		return 0, false
	}
}

func (m *runningModule) healthStatus(t *testing.T) healthpb.HealthCheckResponse_ServingStatus {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	response, err := m.health.Check(ctx, &healthpb.HealthCheckRequest{})
	if err != nil {
		t.Fatalf("health check: %v", err)
	}
	return response.GetStatus()
}

func (m *runningModule) waitServing(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for m.healthStatus(t) != healthpb.HealthCheckResponse_SERVING {
		if time.Now().After(deadline) {
			t.Fatal("the module never reported SERVING")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func okRoute(modulesdk.Request, modulesdk.Host) modulesdk.Response {
	return modulesdk.Response{Status: 200, Body: `{}`}
}

// runBoot runs a descriptor that must be refused at boot and returns the exit code.
func runBoot(t *testing.T, d modulesdk.Descriptor, dependencies bootDependencies) int {
	t.Helper()
	quietLog(t)
	t.Setenv("MYCEL_LISTEN", "unix://"+shortSocket(t))
	t.Setenv("DATABASE_URL", "")
	done := make(chan int, 1)
	go func() { done <- run(context.Background(), d, dependencies) }()
	select {
	case code := <-done:
		return code
	case <-time.After(5 * time.Second):
		t.Fatal("boot was not refused: run is still serving")
		return -1
	}
}

// SDK-T6 (license T12), boot half: an unknown or repeated product ID stops boot.
func TestBootRefusesAnInvalidLicenseDeclaration(t *testing.T) {
	for _, ids := range [][]string{{"clcx"}, {"clc", "clc"}} {
		d := modulesdk.Descriptor{Token: "clc", Route: okRoute, NEEDED_MYCEL_LICENSE: ids}
		if code := runBoot(t, d, testDependencies()); code == 0 {
			t.Errorf("NEEDED_MYCEL_LICENSE %q booted", ids)
		}
	}
}

// SDK-T9 (sdk.md S3): a malformed local descriptor fails boot without entering the
// verification retry loop; declaring both Verify and VerifyContext fails boot.
func TestBootRefusesMalformedDescriptorsWithoutRetrying(t *testing.T) {
	var attempts atomic.Int32
	malformed := modulesdk.Descriptor{
		Token: "clc", Route: okRoute, DescriptorSchemaMajor: 9,
		VerifyContext: func(context.Context, modulesdk.Host) error { attempts.Add(1); return nil },
	}
	if code := runBoot(t, malformed, testDependencies()); code == 0 {
		t.Error("a malformed descriptor booted")
	}
	if attempts.Load() != 0 {
		t.Errorf("verification ran %d times for a malformed descriptor", attempts.Load())
	}
	both := modulesdk.Descriptor{
		Token: "clc", Route: okRoute,
		Verify:        func(modulesdk.Host) error { return nil },
		VerifyContext: func(context.Context, modulesdk.Host) error { return nil },
	}
	if code := runBoot(t, both, testDependencies()); code == 0 {
		t.Error("a descriptor with both verification hooks booted")
	}
}

// SDK-T20 (sdk.md S11.1), boot half: an invalid route-class declaration fails boot.
func TestBootRefusesAnInvalidRouteClass(t *testing.T) {
	d := modulesdk.Descriptor{
		Token: "mycel_iam", Route: okRoute,
		RouteClasses: []modulesdk.RouteClassDeclaration{{Method: "GET", Path: "/healthz", Class: modulesdk.RouteClassIAMSignIn}},
	}
	if code := runBoot(t, d, testDependencies()); code == 0 {
		t.Error("a reserved host route class booted")
	}
}

// SDK-T7 (sdk.md S3): while verification is pending, Describe and both health services
// answer and Handle answers 503.
func TestDescribeAndHealthAnswerWhileVerificationIsPending(t *testing.T) {
	release := make(chan struct{})
	d := modulesdk.Descriptor{
		Token: "clc", Route: okRoute, MenuGroup: "Cluster",
		VerifyContext: func(ctx context.Context, _ modulesdk.Host) error {
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
	}
	module := startModule(t, d, testDependencies(), "")
	defer close(release)

	ctx := context.Background()
	describe, err := module.module.Describe(ctx, &modulerpc.DescribeRequest{})
	if err != nil || describe.GetToken() != "clc" || describe.GetMenuGroup() != "Cluster" {
		t.Fatalf("Describe while pending: %v, %v", describe, err)
	}
	if status := module.healthStatus(t); status != healthpb.HealthCheckResponse_NOT_SERVING {
		t.Errorf("grpc health = %v, want NOT_SERVING", status)
	}
	legacyHealth, err := module.module.Health(ctx, &modulerpc.HealthRequest{})
	if err != nil || legacyHealth.GetStatus() != modulerpc.HealthResponse_NOT_SERVING {
		t.Errorf("ModuleHandler health = %v, %v", legacyHealth, err)
	}
	response, err := module.module.Handle(ctx, &modulerpc.HandleRequest{Method: "GET", Path: "/"})
	if err != nil || response.GetStatus() != 503 {
		t.Fatalf("Handle while pending = %v, %v; want 503", response, err)
	}
}

// SDK-T8 (sdk.md S3): shutdown cancels a running probe.
func TestShutdownCancelsARunningVerificationProbe(t *testing.T) {
	probeCancelled := make(chan struct{})
	dependencies := testDependencies()
	dependencies.attemptTimeout = time.Minute
	d := modulesdk.Descriptor{
		Token: "clc", Route: okRoute,
		VerifyContext: func(ctx context.Context, _ modulesdk.Host) error {
			<-ctx.Done()
			close(probeCancelled)
			return ctx.Err()
		},
	}
	module := startModule(t, d, dependencies, "")
	start := time.Now()
	if code := module.exit(t); code != 0 {
		t.Errorf("exit code = %d, want 0 after a signal", code)
	}
	select {
	case <-probeCancelled:
	default:
		t.Error("the probe never saw its context cancelled")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("shutdown took %s while a probe was running", elapsed)
	}
}

// SDK-T8: shutdown cancels a backoff wait.
func TestShutdownCancelsAVerificationBackoffWait(t *testing.T) {
	base, maximum := verifyBackoffBase, verifyBackoffMax
	verifyBackoffBase, verifyBackoffMax = time.Hour, time.Hour
	t.Cleanup(func() { verifyBackoffBase, verifyBackoffMax = base, maximum })

	var attempts atomic.Int32
	d := modulesdk.Descriptor{
		Token: "clc", Route: okRoute,
		VerifyContext: func(context.Context, modulesdk.Host) error {
			attempts.Add(1)
			return errors.New("collaborator not up")
		},
	}
	module := startModule(t, d, testDependencies(), "")
	deadline := time.Now().Add(5 * time.Second)
	for attempts.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	start := time.Now()
	module.exit(t)
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("shutdown waited %s on a backoff", elapsed)
	}
}

// SDK-T8: each attempt has a deadline, and a delayed collaborator recovers without a
// redeploy.
func TestADelayedCollaboratorRecoversAndAttemptsHaveADeadline(t *testing.T) {
	shrinkVerifyBackoff(t)
	dependencies := testDependencies()
	dependencies.attemptTimeout = 30 * time.Millisecond
	var attempts atomic.Int32
	d := modulesdk.Descriptor{
		Token: "clc", Route: okRoute,
		VerifyContext: func(ctx context.Context, _ modulesdk.Host) error {
			if _, ok := ctx.Deadline(); !ok {
				t.Error("a verification attempt has no deadline")
			}
			if attempts.Add(1) < 3 {
				<-ctx.Done() // a collaborator that hangs until the attempt deadline
				return ctx.Err()
			}
			return nil
		},
	}
	module := startModule(t, d, dependencies, "")
	module.waitServing(t)
	if attempts.Load() < 3 {
		t.Errorf("attempts = %d, want at least 3", attempts.Load())
	}
}

// SDK-T8: the verification Host carries the attempt context into extension calls.
func TestVerificationHostCarriesCancellationToCallExt(t *testing.T) {
	t.Setenv("SDK_VERIFY_EXT", "unix:///tmp/sdk-no-such-extension.sock")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	quietLog(t)
	start := time.Now()
	_, _, configured, err := verificationHost{ctx: ctx}.CallExt("SDK_VERIFY_EXT", "GET", "/probe", nil)
	if !configured || err == nil {
		t.Fatalf("configured=%v err=%v, want a configured call that fails on the cancelled context", configured, err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("a cancelled verification context still took %s", elapsed)
	}
}

// SDK-T10 (sdk.md S3, core I10, I21): the SDK never checks licenses. A module that
// declares a product with no license anywhere reports SERVING and answers requests;
// app_core's admission readiness is separate.
func TestLicenseDeclarationDoesNotChangeProcessHealth(t *testing.T) {
	t.Setenv("MYCEL_LICENSE_MODE", "enforce")
	d := modulesdk.Descriptor{Token: "clc", Route: okRoute, NEEDED_MYCEL_LICENSE: []string{"clc"}}
	module := startModule(t, d, testDependencies(), "")
	module.waitServing(t)
	describe, err := module.module.Describe(context.Background(), &modulerpc.DescribeRequest{})
	if err != nil || !reflect.DeepEqual(describe.GetNeededMycelLicense(), []string{"clc"}) {
		t.Fatalf("Describe needed_mycel_license = %v, %v", describe.GetNeededMycelLicense(), err)
	}
	response, err := module.module.Handle(context.Background(), &modulerpc.HandleRequest{Method: "GET", Path: "/"})
	if err != nil || response.GetStatus() != 200 {
		t.Fatalf("Handle = %v, %v", response, err)
	}
}

// SDK-T15 (sdk.md S7): a typed module refuses host-reserved variables and a failed
// non-dumpable protection.
func TestTypedModuleRefusesHostReservedVariables(t *testing.T) {
	for _, name := range HostReservedVariables() {
		t.Run(name, func(t *testing.T) {
			t.Setenv(name, "value-that-must-not-be-logged")
			d := modulesdk.Descriptor{Token: "clc", Route: okRoute, DescriptorSchemaMajor: 1}
			if code := runBoot(t, d, testDependencies()); code == 0 {
				t.Errorf("a typed module booted with %s in its environment", name)
			}
		})
	}
}

func TestTypedModuleFailsBootWhenNonDumpableFails(t *testing.T) {
	dependencies := testDependencies()
	dependencies.markNonDumpable = func() error { return errors.New("prctl refused") }
	d := modulesdk.Descriptor{Token: "clc", Route: okRoute, DescriptorSchemaMajor: 1}
	if code := runBoot(t, d, dependencies); code == 0 {
		t.Error("a typed module booted without the non-dumpable protection")
	}
}

func TestLegacyModuleKeepsItsCurrentBoot(t *testing.T) {
	t.Setenv("MYCEL_HOST_IAM_URL", "http://iam")
	dependencies := testDependencies()
	dependencies.markNonDumpable = func() error { return errors.New("must not be called for a legacy module") }
	module := startModule(t, modulesdk.Descriptor{Token: "clc", Route: okRoute}, dependencies, "")
	module.waitServing(t)
}

// SDK-T16 (sdk.md S7): a released typed module refuses TCP; a development build allows it.
func TestReleaseBoundaryRefusesTCPForTypedModules(t *testing.T) {
	typed := modulesdk.Descriptor{Token: "clc", DescriptorSchemaMajor: 1}
	dependencies := testDependencies()
	if err := checkReleaseBoundary(typed, "tcp", dependencies); err == nil {
		t.Error("a released typed module may listen on TCP")
	}
	if err := checkReleaseBoundary(typed, "unix", dependencies); err != nil {
		t.Errorf("unix socket refused: %v", err)
	}
	dependencies.developmentBuild = true
	if err := checkReleaseBoundary(typed, "tcp", dependencies); err != nil {
		t.Errorf("a development build refused TCP: %v", err)
	}
	if err := checkReleaseBoundary(modulesdk.Descriptor{Token: "clc"}, "tcp", testDependencies()); err != nil {
		t.Errorf("a legacy module lost its TCP path: %v", err)
	}
}

// orderLog records lifecycle events from several goroutines.
type orderLog struct {
	mu     sync.Mutex
	events []string
}

func (o *orderLog) add(event string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.events = append(o.events, event)
}

func (o *orderLog) snapshot() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.events...)
}

// SDK-T22 (sdk.md S11.3): Start runs once, after verification succeeds and before
// SERVING, with a working datastore.
func TestStartRunsOnceAfterVerificationAndBeforeServing(t *testing.T) {
	driver := &countingDriver{}
	driverName := registerDriver(t, "start_hook_runs", driver)
	events := &orderLog{}
	var starts atomic.Int32
	var module *runningModule
	ready := make(chan struct{})
	d := modulesdk.Descriptor{
		Token: "clc", Route: okRoute,
		VerifyContext: func(context.Context, modulesdk.Host) error {
			events.add("verify")
			return nil
		},
		Start: func(ctx context.Context, store modulesdk.Datastore, _ modulesdk.Background) error {
			<-ready
			starts.Add(1)
			events.add("start")
			if status := module.healthStatus(t); status != healthpb.HealthCheckResponse_NOT_SERVING {
				t.Errorf("health during Start = %v, want NOT_SERVING", status)
			}
			if _, err := store.Exec(ctx, "INSERT INTO samples VALUES ($1)", []string{"1"}); err != nil {
				t.Errorf("start datastore Exec: %v", err)
			}
			return store.Transaction(ctx, func(statements modulesdk.Statements) error {
				_, err := statements.Query(ctx, "SELECT 1", nil)
				return err
			})
		},
	}
	module = startModule(t, d, testDependencies(), driverName)
	close(ready)
	module.waitServing(t)
	if got := events.snapshot(); !reflect.DeepEqual(got, []string{"verify", "start"}) {
		t.Errorf("events = %v, want [verify start]", got)
	}
	if starts.Load() != 1 {
		t.Errorf("Start ran %d times", starts.Load())
	}
	begins, commits, _, statements := driver.snapshot()
	if begins != 2 || commits != 2 {
		t.Errorf("begins=%d commits=%d, want each statement and the transaction committed on their own", begins, commits)
	}
	joined := strings.Join(statements, " | ")
	if !strings.Contains(joined, "SET LOCAL search_path TO clc | INSERT INTO samples VALUES ($1)") {
		t.Errorf("statements = %q, want schema-scoped statements", statements)
	}
}

// SDK-T22: an error from Start exits non-zero; Start with Handler fails boot.
func TestStartFailureExitsNonZero(t *testing.T) {
	d := modulesdk.Descriptor{
		Token: "clc", Route: okRoute,
		Start: func(context.Context, modulesdk.Datastore, modulesdk.Background) error {
			return errors.New("sampler cannot reach kubernetes")
		},
	}
	module := startModule(t, d, testDependencies(), "")
	code, exited := module.waitExit(t, 10*time.Second)
	if !exited || code == 0 {
		t.Fatalf("exited=%v code=%d, want a non-zero exit", exited, code)
	}

	withHandler := modulesdk.Descriptor{
		Token: "clc", Handler: http.NotFoundHandler(),
		Start: func(context.Context, modulesdk.Datastore, modulesdk.Background) error { return nil },
	}
	if code := runBoot(t, withHandler, testDependencies()); code == 0 {
		t.Error("Start with Handler booted")
	}
}

// SDK-T16 and SDK-T23 (sdk.md S7, S11.3): at shutdown an in-flight RPC drains first,
// then the start-hook context is cancelled and its work finishes, then Shutdown runs
// while the datastore is still open, then the datastore closes — inside the grace.
func TestShutdownOrder(t *testing.T) {
	driver := &countingDriver{}
	driverName := registerDriver(t, "shutdown_order", driver)
	events := &orderLog{}
	inFlight := make(chan struct{})
	releaseRPC := make(chan struct{})
	var module *runningModule
	d := modulesdk.Descriptor{
		Token: "clc",
		Route: func(request modulesdk.Request, _ modulesdk.Host) modulesdk.Response {
			if request.Path == "/slow" {
				close(inFlight)
				<-releaseRPC
				events.add("rpc finished")
			}
			return modulesdk.Response{Status: 200}
		},
		Start: func(_ context.Context, _ modulesdk.Datastore, background modulesdk.Background) error {
			background.Go(func(ctx context.Context) {
				<-ctx.Done()
				events.add("start context cancelled")
				time.Sleep(20 * time.Millisecond) // work that finishes after cancellation
				events.add("start work finished")
			})
			return nil
		},
		Shutdown: func(ctx context.Context) error {
			events.add("shutdown hook")
			if _, err := module.database.ExecContext(ctx, "SELECT 1"); err != nil {
				t.Errorf("the datastore was closed before the shutdown hook: %v", err)
			}
			return nil
		},
	}
	module = startModule(t, d, testDependencies(), driverName)
	module.waitServing(t)

	rpcDone := make(chan error, 1)
	go func() {
		_, err := module.module.Handle(context.Background(), &modulerpc.HandleRequest{Method: "GET", Path: "/slow"})
		rpcDone <- err
	}()
	<-inFlight
	exited := make(chan int, 1)
	start := time.Now()
	go func() { exited <- module.exit(t) }()
	time.Sleep(50 * time.Millisecond)
	if got := events.snapshot(); len(got) != 0 {
		t.Errorf("events before the RPC drained = %v, want none", got)
	}
	close(releaseRPC)
	if err := <-rpcDone; err != nil {
		t.Errorf("the in-flight RPC failed during drain: %v", err)
	}
	<-exited
	if elapsed := time.Since(start); elapsed > shutdownGrace {
		t.Errorf("shutdown took %s, longer than the grace", elapsed)
	}
	want := []string{"rpc finished", "start context cancelled", "start work finished", "shutdown hook"}
	if got := events.snapshot(); !reflect.DeepEqual(got, want) {
		t.Errorf("shutdown order = %v, want %v", got, want)
	}
	if err := module.database.Ping(); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Errorf("the datastore is still open after exit: %v", err)
	}
}

// SDK-T19 and SDK-T21 (sdk.md S11.1, S11.2): Describe carries route classes and the
// navigation entries with and without group in declared order.
func TestDescribeCarriesRouteClassesAndNavigationGroups(t *testing.T) {
	d := modulesdk.Descriptor{
		Token: "mycel_iam", Route: okRoute, MenuGroup: "Directory", DescriptorSchemaMajor: 1,
		UI: &modulesdk.UIDescriptor{Kind: modulesdk.UIKindOwn, ContractMajor: 1},
		Nav: []modulesdk.NavEntry{
			{Label: "Users", Path: "/users"},
			{Label: "Clients", Path: "/clients", Group: "Access"},
			{Label: "Audit", Path: "/audit"},
		},
		RouteClasses: []modulesdk.RouteClassDeclaration{
			{Method: "GET", Path: "/m/mycel_iam/oauth/authorize", Class: modulesdk.RouteClassIAMAuthorization},
			{Method: "GET", Path: "/.well-known/jwks.json", Class: modulesdk.RouteClassIAMKeySet},
		},
		Capabilities: []string{"directory"},
	}
	module := startModule(t, d, testDependencies(), "")
	describe, err := module.module.Describe(context.Background(), &modulerpc.DescribeRequest{})
	if err != nil {
		t.Fatal(err)
	}
	var labels, groups []string
	for _, entry := range describe.GetNav() {
		labels = append(labels, entry.GetLabel())
		groups = append(groups, entry.GetGroup())
	}
	if !reflect.DeepEqual(labels, []string{"Users", "Clients", "Audit"}) || !reflect.DeepEqual(groups, []string{"", "Access", ""}) {
		t.Errorf("navigation labels %v groups %v", labels, groups)
	}
	if describe.GetNav()[0].Group != nil {
		t.Error("an entry without group carries a group on the wire")
	}
	classes := describe.GetRouteClasses()
	if len(classes) != 2 || classes[1].GetClass() != modulerpc.RouteClass_ROUTE_CLASS_IAM_KEY_SET || classes[1].GetPath() != "/.well-known/jwks.json" {
		t.Errorf("route classes = %v", classes)
	}
	if describe.GetDescriptorSchemaMajor() != 1 || describe.GetUi().GetKind() != modulerpc.UIKind_UI_KIND_OWN {
		t.Errorf("schema %d ui %v", describe.GetDescriptorSchemaMajor(), describe.GetUi())
	}
	if !reflect.DeepEqual(describe.GetCapabilities(), []string{"directory"}) {
		t.Errorf("capabilities = %v", describe.GetCapabilities())
	}
}
