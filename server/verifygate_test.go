package server

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	modulesdk "github.com/mycelpf/mycel_module_sdk"
	modulerpc "github.com/mycelpf/mycel_module_sdk/contracts/modulerpc/v1"
)

// shrinkVerifyBackoff makes the gate's sleeps negligible so these tests exercise the
// control flow, not the clock.
func shrinkVerifyBackoff(t *testing.T) {
	t.Helper()
	base, max := verifyBackoffBase, verifyBackoffMax
	verifyBackoffBase, verifyBackoffMax = time.Millisecond, 2*time.Millisecond
	t.Cleanup(func() { verifyBackoffBase, verifyBackoffMax = base, max })
}

// The regression: runVerifyGate gave up after 12 attempts and pinned the gate forever.
// Nothing cleared that pin, and module pods carry no livenessProbe, so a collaborator that
// took a few seconds too long to boot left the module answering 503 on every route until
// a human redeployed it. It must now recover on its own.
func TestVerifyGateRecoversOnceTheCollaboratorComesUp(t *testing.T) {
	quietLog(t)
	shrinkVerifyBackoff(t)

	var calls atomic.Int32
	d := modulesdk.Descriptor{
		Token: "testmod",
		// Fails well past the old 12-attempt cutoff, then succeeds.
		Verify: func(modulesdk.Host) error {
			if calls.Add(1) < 20 {
				return errors.New("extension not reachable yet")
			}
			return nil
		},
	}
	hg := newHealthGate(nil, "testmod")
	hg.forceUnready("startup verification pending")

	done := make(chan bool, 1)
	go func() { done <- runVerifyGate(context.Background(), d, hg, 0) }()

	select {
	case passed := <-done:
		if !passed {
			t.Error("runVerifyGate reported failure after Verify started passing")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("runVerifyGate never returned — it gave up or wedged")
	}
	if got := calls.Load(); got < 20 {
		t.Errorf("Verify ran %d times, want at least 20 — it stopped retrying too early", got)
	}
}

// While Verify is failing the module must stay out of rotation: fail-closed is the point
// of the gate, and making it recoverable must not weaken that.
func TestVerifyGateHoldsNotServingWhileFailing(t *testing.T) {
	quietLog(t)
	shrinkVerifyBackoff(t)

	block := make(chan struct{})
	d := modulesdk.Descriptor{
		Token:  "testmod",
		Verify: func(modulesdk.Host) error { <-block; return nil },
	}
	hg := newHealthGate(nil, "testmod")
	hg.forceUnready("startup verification pending")

	done := make(chan struct{})
	go func() { runVerifyGate(context.Background(), d, hg, 0); close(done) }()

	if hg.serving(context.Background()) {
		t.Error("the module served while its startup gate was unresolved")
	}

	// Let it finish and WAIT for it: the gate goroutine reads the backoff vars, and
	// t.Cleanup restores them, so leaving it running past the test is a real race.
	close(block)
	<-done
}

// SDK-T7 and SDK-T18 (sdk.md S3, S8): a gated Handle answers 503 with the stable
// verification_pending code and a fixed message. The verification report can name
// collaborators, addresses or credentials, so it is logged, never returned.
func TestGatedHandleReturnsASafeEnvelope(t *testing.T) {
	h := &handler{
		d:      modulesdk.Descriptor{Token: "testmod"},
		health: newHealthGate(nil, "testmod"),
	}
	h.health.forceUnready("acme_iam_ext at grpc://10.0.0.7:9000 returned 500 for /_enrich_identity token=abc123")

	resp, err := h.Handle(context.Background(), &modulerpc.HandleRequest{Method: "GET", Path: "/anything"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetStatus() != 503 {
		t.Fatalf("status = %d, want 503", resp.GetStatus())
	}
	envelope, err := modulesdk.DecodeErrorEnvelope(resp.GetBody())
	if err != nil {
		t.Fatal(err)
	}
	if envelope.Code != modulesdk.ErrorCodeVerificationPending {
		t.Errorf("code = %q, want %q", envelope.Code, modulesdk.ErrorCodeVerificationPending)
	}
	for _, secret := range []string{"acme_iam_ext", "10.0.0.7", "abc123", "_enrich_identity"} {
		if strings.Contains(string(resp.GetBody()), secret) {
			t.Errorf("the public body leaks %q: %s", secret, resp.GetBody())
		}
	}
}

// A cancelled caller must not be able to mark the module unhealthy. The verdict is cached
// for healthPingTTL and shared with every later probe, so inheriting the caller's context
// meant one browser refresh could report a healthy module as down for 2 seconds.
func TestHealthCheckIgnoresCallerCancellation(t *testing.T) {
	db := fakeDB(t, "fake_health_ok", nil)
	g := newHealthGate(db, "testmod")

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already dead before the probe runs

	if ok, _ := g.check(ctx); !ok {
		t.Error("a cancelled caller made a healthy module report NOT_SERVING")
	}
}

// A genuinely unreachable schema must still fail — detaching the context must not make
// check() unconditionally optimistic.
func TestHealthCheckStillFailsOnARealDBError(t *testing.T) {
	db := failingQueryDB(t, "fake_health_broken")
	g := newHealthGate(db, "testmod")
	if ok, _ := g.check(context.Background()); ok {
		t.Error("check() reported healthy despite the schema query failing")
	}
}

type failingConn struct{ fakeConn }

func (c *failingConn) QueryContext(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
	return nil, errors.New(`relation "testmod._mycel_migrations" does not exist`)
}

type failingDriver struct{}

func (failingDriver) Open(string) (driver.Conn, error) { return &failingConn{}, nil }

func failingQueryDB(t *testing.T, name string) *sql.DB {
	t.Helper()
	sql.Register(name, failingDriver{})
	db, err := sql.Open(name, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}
