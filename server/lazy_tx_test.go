package server

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"strings"
	"sync"
	"testing"

	modulesdk "github.com/mycelpf/mycel_module_sdk"
	modulerpc "github.com/mycelpf/mycel_module_sdk/contracts/modulerpc/v1"
)

// Every Route-style request used to open a transaction before Route ran: BEGIN, SET LOCAL
// search_path, COMMIT — three database round trips and a pooled connection held for the
// whole route, even for /openapi.json, /info, /health or a console route that only calls
// other services. The transaction now opens on the route's first Query/Exec.

// countingDriver records the database work a request actually caused.
type countingDriver struct {
	mu        sync.Mutex
	begins    int
	commits   int
	rollbacks int
	stmts     []string
	beginErr  error
}

func (d *countingDriver) Open(string) (driver.Conn, error) { return &countingConn{d: d}, nil }

func (d *countingDriver) snapshot() (begins, commits, rollbacks int, stmts []string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.begins, d.commits, d.rollbacks, append([]string(nil), d.stmts...)
}

type countingConn struct{ d *countingDriver }

func (c *countingConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("prepare unused") }
func (c *countingConn) Close() error                        { return nil }
func (c *countingConn) Begin() (driver.Tx, error) {
	c.d.mu.Lock()
	defer c.d.mu.Unlock()
	if c.d.beginErr != nil {
		return nil, c.d.beginErr
	}
	c.d.begins++
	return &countingTx{d: c.d}, nil
}
func (c *countingConn) ExecContext(_ context.Context, q string, _ []driver.NamedValue) (driver.Result, error) {
	c.d.mu.Lock()
	c.d.stmts = append(c.d.stmts, q)
	c.d.mu.Unlock()
	return driver.RowsAffected(1), nil
}
func (c *countingConn) QueryContext(_ context.Context, q string, _ []driver.NamedValue) (driver.Rows, error) {
	c.d.mu.Lock()
	c.d.stmts = append(c.d.stmts, q)
	c.d.mu.Unlock()
	return &emptyRows{}, nil
}

type countingTx struct{ d *countingDriver }

func (t *countingTx) Commit() error {
	t.d.mu.Lock()
	defer t.d.mu.Unlock()
	t.d.commits++
	return nil
}
func (t *countingTx) Rollback() error {
	t.d.mu.Lock()
	defer t.d.mu.Unlock()
	t.d.rollbacks++
	return nil
}

func countingHandler(t *testing.T, name string, d *countingDriver, route modulesdk.RouteFunc) *handler {
	t.Helper()
	sql.Register(name, d)
	db, err := sql.Open(name, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return &handler{
		d:      modulesdk.Descriptor{Token: "testmod", Route: route},
		db:     db,
		health: newHealthGate(db, "testmod"),
	}
}

func handle(t *testing.T, h *handler, method, path string) *modulerpc.HandleResponse {
	t.Helper()
	resp, err := h.Handle(context.Background(), &modulerpc.HandleRequest{Method: method, Path: path})
	if err != nil {
		t.Fatalf("Handle returned a transport error: %v", err)
	}
	return resp
}

// The regression: a route that never touches the datastore still cost BEGIN + SET LOCAL
// + COMMIT.
func TestHandleOpensNoTransactionForARouteThatNeverTouchesTheDatabase(t *testing.T) {
	d := &countingDriver{}
	h := countingHandler(t, "counting_static", d, func(modulesdk.Request, modulesdk.Host) modulesdk.Response {
		return modulesdk.Response{Status: 200, Body: `{"status":"ok"}`}
	})

	if resp := handle(t, h, "GET", "/info"); resp.GetStatus() != 200 {
		t.Fatalf("status = %d, want 200", resp.GetStatus())
	}
	begins, commits, _, stmts := d.snapshot()
	if begins != 0 || commits != 0 || len(stmts) != 0 {
		t.Errorf("a route that never touched the host cost begins=%d commits=%d statements=%q; want none",
			begins, commits, stmts)
	}
}

// A route that does use the datastore still gets exactly one transaction, scoped to the
// module's schema BEFORE its first statement, and committed once.
func TestHandleOpensOneSchemaScopedTransactionOnFirstUse(t *testing.T) {
	d := &countingDriver{}
	h := countingHandler(t, "counting_rw", d, func(_ modulesdk.Request, host modulesdk.Host) modulesdk.Response {
		if _, err := host.Exec("INSERT INTO things VALUES ($1)", []string{"a"}); err != nil {
			return modulesdk.Response{Status: 500}
		}
		if _, err := host.Query("SELECT id FROM things", nil); err != nil {
			return modulesdk.Response{Status: 500}
		}
		return modulesdk.Response{Status: 201}
	})

	if resp := handle(t, h, "POST", "/things"); resp.GetStatus() != 201 {
		t.Fatalf("status = %d, want 201", resp.GetStatus())
	}
	begins, commits, _, stmts := d.snapshot()
	if begins != 1 || commits != 1 {
		t.Errorf("begins=%d commits=%d, want exactly one transaction committed once", begins, commits)
	}
	want := []string{"SET LOCAL search_path TO testmod", "INSERT INTO things VALUES ($1)", "SELECT id FROM things"}
	if strings.Join(stmts, " | ") != strings.Join(want, " | ") {
		t.Errorf("statements = %q, want %q (schema scoped before the route's first statement)", stmts, want)
	}
}

// A failing route's writes are rolled back, never committed.
func TestHandleRollsBackWritesWhenTheRouteFails(t *testing.T) {
	d := &countingDriver{}
	h := countingHandler(t, "counting_fail", d, func(_ modulesdk.Request, host modulesdk.Host) modulesdk.Response {
		_, _ = host.Exec("INSERT INTO things VALUES ($1)", []string{"a"})
		return modulesdk.Response{Status: 500}
	})

	handle(t, h, "POST", "/things")
	_, commits, rollbacks, _ := d.snapshot()
	if commits != 0 || rollbacks == 0 {
		t.Errorf("commits=%d rollbacks=%d after a 500, want 0 commits and a rollback", commits, rollbacks)
	}
}

// Opening the transaction can fail. That must reach the route as the error of the call
// that needed it, so the route answers in its own terms — and nothing is committed.
func TestHandleSurfacesABeginFailureToTheRoute(t *testing.T) {
	d := &countingDriver{beginErr: errors.New("too many clients already")}
	var routeErr error
	h := countingHandler(t, "counting_beginfail", d, func(_ modulesdk.Request, host modulesdk.Host) modulesdk.Response {
		if _, err := host.Exec("INSERT INTO things VALUES ($1)", []string{"a"}); err != nil {
			routeErr = err
			return modulesdk.Response{Status: 503, Body: `{"error":"retry"}`}
		}
		return modulesdk.Response{Status: 201}
	})

	resp := handle(t, h, "POST", "/things")
	if routeErr == nil {
		t.Fatal("the route never saw the begin failure: either it was not called, or its Exec succeeded without a transaction")
	}
	if resp.GetStatus() != 503 {
		t.Errorf("status = %d, want the route's own 503", resp.GetStatus())
	}
	if _, commits, _, _ := d.snapshot(); commits != 0 {
		t.Errorf("commits = %d after a failed begin", commits)
	}
}

// A route that panics after writing must not leak its pooled connection: the transaction
// it opened is rolled back on the way out, through the same interceptor production uses.
func TestHandleReleasesTheTransactionWhenTheRoutePanics(t *testing.T) {
	quietLog(t)
	d := &countingDriver{}
	h := countingHandler(t, "counting_panic", d, func(_ modulesdk.Request, host modulesdk.Host) modulesdk.Response {
		_, _ = host.Exec("INSERT INTO things VALUES ($1)", []string{"a"})
		panic("module author dereferenced nil")
	})

	req := &modulerpc.HandleRequest{Method: "POST", Path: "/things"}
	_, _ = recoverUnary("testmod")(context.Background(), req, unaryInfo(handleFullMethod),
		func(ctx context.Context, r any) (any, error) {
			return h.Handle(ctx, r.(*modulerpc.HandleRequest))
		})

	if _, commits, rollbacks, _ := d.snapshot(); commits != 0 || rollbacks != 1 {
		t.Errorf("commits=%d rollbacks=%d after a panic, want the opened transaction rolled back once",
			commits, rollbacks)
	}
}
