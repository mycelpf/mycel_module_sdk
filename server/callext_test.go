package server

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	modulerpc "github.com/mycelpf/mycel_module_sdk/contracts/modulerpc/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
)

// extServer is a real ModuleHandler standing in for an extension service.
type extServer struct {
	modulerpc.UnimplementedModuleHandlerServer
	calls     atomic.Int32
	failWith  codes.Code
	failFirst int32
	status    uint32
	delay     time.Duration
}

func (e *extServer) Handle(ctx context.Context, _ *modulerpc.HandleRequest) (*modulerpc.HandleResponse, error) {
	n := e.calls.Add(1)
	if e.delay > 0 {
		select {
		case <-time.After(e.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if e.failWith != codes.OK && (e.failFirst == 0 || n <= e.failFirst) {
		return nil, grpcstatus.Error(e.failWith, "induced")
	}
	st := e.status
	if st == 0 {
		st = 200
	}
	return &modulerpc.HandleResponse{Status: st, Body: []byte(`{"roles":["admin"]}`)}, nil
}

// startExt serves es on loopback TCP, points envKey at it, and clears the pooled conn so
// each test dials fresh (the pool is process-global by design).
func startExt(t *testing.T, envKey string, es *extServer) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	modulerpc.RegisterModuleHandlerServer(srv, es)
	go srv.Serve(lis)
	t.Setenv(envKey, lis.Addr().String())
	t.Cleanup(func() { srv.Stop(); closeExtConns() })
	closeExtConns()
}

// An unwired collaborator is a legitimate choice, not a failure — configured=false, no
// error, and nothing dialled.
func TestCallExtReportsAnUnwiredExtension(t *testing.T) {
	t.Setenv("MYCEL_TEST_EXT_URL", "")
	status, out, configured, err := callExt(context.Background(), "MYCEL_TEST_EXT_URL", "POST", "/x", nil)
	if configured || err != nil || status != 0 || out != nil {
		t.Errorf("unwired ext gave status=%d out=%v configured=%v err=%v", status, out, configured, err)
	}
}

// The degradation this hardening exists for: an extension that is momentarily
// unreachable used to fail the call outright, and IAM's best-effort call site then minted
// an identity WITHOUT its enriched roles — silently. Only safe read methods may
// retry: UNAVAILABLE can also be returned after processing a write.
func TestCallExtRetriesAnUnavailableExtension(t *testing.T) {
	quietLog(t)
	es := &extServer{failWith: codes.Unavailable, failFirst: 1}
	startExt(t, "MYCEL_TEST_EXT_URL", es)

	status, _, configured, err := callExt(context.Background(), "MYCEL_TEST_EXT_URL", "GET", "/identity", nil)
	if err != nil {
		t.Fatalf("a blip was not retried: %v", err)
	}
	if !configured || status != 200 {
		t.Errorf("status=%d configured=%v, want 200/true", status, configured)
	}
	if got := es.calls.Load(); got < 2 {
		t.Errorf("extension saw %d calls — the retry policy did not engage", got)
	}
}

// A non-2xx is the TARGET's status and arrives with err == nil. Pinning this because it
// is the trap in this API: a caller that checks only err treats a 503 as a successful
// empty answer.
func TestCallExtReturnsANon2xxWithoutAnError(t *testing.T) {
	quietLog(t)
	es := &extServer{status: 503}
	startExt(t, "MYCEL_TEST_EXT_URL", es)

	status, _, configured, err := callExt(context.Background(), "MYCEL_TEST_EXT_URL", "POST", "/x", nil)
	if err != nil {
		t.Errorf("err = %v, want nil — the hop succeeded; the TARGET returned 503", err)
	}
	if !configured || status != 503 {
		t.Errorf("status=%d configured=%v, want 503/true", status, configured)
	}
}

// The hop must fit inside the caller's budget. The host allows a module 5s in total, so
// an extension call that also took 5s guaranteed the host timed out first.
func TestCallExtTimeoutLeavesRoomForTheCaller(t *testing.T) {
	if extCallTimeout >= 5*time.Second {
		t.Errorf("extCallTimeout = %s, but the host's whole module budget is 5s — the module could never answer gracefully",
			extCallTimeout)
	}
}

// The pool must be reusable and lock-free on the hot path; it was a single process-wide
// mutex taken on every call, serializing calls to unrelated targets.
func TestCallExtPoolsAndIsConcurrencySafe(t *testing.T) {
	quietLog(t)
	es := &extServer{}
	startExt(t, "MYCEL_TEST_EXT_URL", es)

	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, _, err := callExt(context.Background(), "MYCEL_TEST_EXT_URL", "GET", "/x", nil); err != nil {
				t.Errorf("concurrent CallExt: %v", err)
			}
		}()
	}
	wg.Wait()

	if got := es.calls.Load(); got != 30 {
		t.Errorf("extension saw %d calls, want 30", got)
	}
	// One pooled connection for one address, however many callers.
	n := 0
	extConns.Range(func(any, any) bool { n++; return true })
	if n != 1 {
		t.Errorf("%d pooled connections for one address, want 1", n)
	}
}

// closeExtConns must actually empty the pool — cleanup relies on it to release sockets
// rather than leaving them to process exit.
func TestCloseExtConnsEmptiesThePool(t *testing.T) {
	quietLog(t)
	startExt(t, "MYCEL_TEST_EXT_URL", &extServer{})
	if _, _, _, err := callExt(context.Background(), "MYCEL_TEST_EXT_URL", "GET", "/x", nil); err != nil {
		t.Fatal(err)
	}

	closeExtConns()
	n := 0
	extConns.Range(func(any, any) bool { n++; return true })
	if n != 0 {
		t.Errorf("%d connections left after closeExtConns", n)
	}
}
