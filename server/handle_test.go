package server

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"log"
	"os"
	"strings"
	"testing"

	modulesdk "github.com/mycelpf/mycel_module_sdk"
	modulerpc "github.com/mycelpf/mycel_module_sdk/contracts/modulerpc/v1"
	"google.golang.org/grpc"
)

func unaryInfo(method string) *grpc.UnaryServerInfo {
	return &grpc.UnaryServerInfo{FullMethod: method}
}

// quietLog silences the expected panic stacks / commit errors these tests provoke.
// It restores os.Stderr (log's actual default) rather than nil, which would leave the
// package logger unusable for every test that runs after this one.
func quietLog(t *testing.T) {
	t.Helper()
	log.SetOutput(io.Discard)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
}

// A stdlib-only fake driver. The SDK keeps a deliberately lean dependency set (it is the
// artifact every module compiles against), so forcing a commit failure is worth ~50 lines
// here rather than a mock library in go.mod.
type fakeDriver struct{ commitErr error }

func (d *fakeDriver) Open(string) (driver.Conn, error) { return &fakeConn{d: d}, nil }

type fakeConn struct{ d *fakeDriver }

func (c *fakeConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("prepare unused") }
func (c *fakeConn) Close() error                        { return nil }
func (c *fakeConn) Begin() (driver.Tx, error)           { return &fakeTx{d: c.d}, nil }

// ExecerContext keeps `SET LOCAL search_path` from needing a prepared statement.
func (c *fakeConn) ExecContext(context.Context, string, []driver.NamedValue) (driver.Result, error) {
	return driver.RowsAffected(0), nil
}

// QueryerContext serves the health gate's schema probe. It returns zero rows, which
// database/sql surfaces as sql.ErrNoRows — what check() treats as "reachable and empty",
// i.e. healthy. Without this, database/sql falls back to Prepare and the probe fails for
// reasons that have nothing to do with what a test is asserting.
func (c *fakeConn) QueryContext(ctx context.Context, _ string, _ []driver.NamedValue) (driver.Rows, error) {
	if err := ctx.Err(); err != nil {
		return nil, err // honour a live cancellation, so the detach test means something
	}
	return &emptyRows{}, nil
}

type emptyRows struct{}

func (*emptyRows) Columns() []string         { return []string{"?column?"} }
func (*emptyRows) Close() error              { return nil }
func (*emptyRows) Next([]driver.Value) error { return io.EOF }

type fakeTx struct{ d *fakeDriver }

func (t *fakeTx) Commit() error   { return t.d.commitErr }
func (t *fakeTx) Rollback() error { return nil }

func fakeDB(t *testing.T, name string, commitErr error) *sql.DB {
	t.Helper()
	sql.Register(name, &fakeDriver{commitErr: commitErr})
	db, err := sql.Open(name, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// The bug this pins: a connection that dies during COMMIT — what a Postgres failover
// produces — rolled the transaction back while the route's 200 still went to the client,
// reporting a write that does not exist.
func TestHandleReportsACommitFailureInsteadOfSuccess(t *testing.T) {
	quietLog(t)

	db := fakeDB(t, "fake_commit_fails", errors.New("connection reset by peer"))
	h := &handler{
		d: modulesdk.Descriptor{
			Token: "testmod",
			// The route WRITES: the transaction opens on first use, so a commit failure is
			// only possible — and only matters — once the route has done something to lose.
			Route: func(_ modulesdk.Request, host modulesdk.Host) modulesdk.Response {
				if _, err := host.Exec("INSERT INTO things VALUES ($1)", []string{"a"}); err != nil {
					return modulesdk.Response{Status: 500}
				}
				return modulesdk.Response{Status: 201, Body: `{"created":true}`}
			},
		},
		db:     db,
		health: newHealthGate(db, "testmod"),
	}

	resp, err := h.Handle(context.Background(), &modulerpc.HandleRequest{Method: "POST", Path: "/things"})
	if err != nil {
		t.Fatalf("Handle returned a transport error: %v", err)
	}
	if resp.GetStatus() != 500 {
		t.Errorf("status = %d, want 500 — the commit failed, so the 201 was a lie", resp.GetStatus())
	}
	if !strings.Contains(string(resp.GetBody()), "not applied") {
		t.Errorf("body %q does not tell the caller the write was not applied", resp.GetBody())
	}
}

// The success path must still commit and pass the route's own status through untouched.
func TestHandleReturnsTheRouteStatusWhenCommitSucceeds(t *testing.T) {
	db := fakeDB(t, "fake_commit_ok", nil)
	h := &handler{
		d: modulesdk.Descriptor{
			Token: "testmod",
			Route: func(modulesdk.Request, modulesdk.Host) modulesdk.Response {
				return modulesdk.Response{Status: 201, Body: `{"created":true}`}
			},
		},
		db:     db,
		health: newHealthGate(db, "testmod"),
	}

	resp, err := h.Handle(context.Background(), &modulerpc.HandleRequest{Method: "POST", Path: "/things"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetStatus() != 201 {
		t.Errorf("status = %d, want the route's 201", resp.GetStatus())
	}
}

// grpc-go does not recover handler panics, so before recoverUnary a single bad input
// unwound past Serve and killed the module — every concurrent request with it, and
// Descriptor.Shutdown skipped, so an analytics queue was lost rather than drained.
func TestRecoverUnaryTurnsAPanicIntoA500(t *testing.T) {
	quietLog(t)

	interceptor := recoverUnary("testmod")
	resp, err := interceptor(context.Background(), &modulerpc.HandleRequest{},
		unaryInfo(handleFullMethod), func(context.Context, any) (any, error) {
			panic("module author dereferenced nil")
		})

	if err != nil {
		t.Fatalf("Handle panics must surface as an HTTP status, not a transport error: %v", err)
	}
	hr, ok := resp.(*modulerpc.HandleResponse)
	if !ok {
		t.Fatalf("resp is %T, want *modulerpc.HandleResponse", resp)
	}
	// 500 not 502: the module is up and answering — one route misbehaved. That is the
	// difference between "look at this handler" and "redeploy the module".
	if hr.GetStatus() != 500 {
		t.Errorf("status = %d, want 500", hr.GetStatus())
	}
}

// Describe/Health carry no HTTP status of their own, so a panic there is a transport
// error — but still must not take the process down.
func TestRecoverUnaryReturnsInternalForNonHandleRPCs(t *testing.T) {
	quietLog(t)

	interceptor := recoverUnary("testmod")
	_, err := interceptor(context.Background(), &modulerpc.DescribeRequest{},
		unaryInfo("/mycel.modulerpc.v1.ModuleHandler/Describe"),
		func(context.Context, any) (any, error) { panic("boom") })

	if err == nil {
		t.Fatal("a Describe panic must surface as an error")
	}
}

func TestRecoverUnaryPassesThroughWhenNothingPanics(t *testing.T) {
	interceptor := recoverUnary("testmod")
	want := &modulerpc.HandleResponse{Status: 204}
	got, err := interceptor(context.Background(), &modulerpc.HandleRequest{},
		unaryInfo(handleFullMethod),
		func(context.Context, any) (any, error) { return want, nil })
	if err != nil {
		t.Fatal(err)
	}
	if got.(*modulerpc.HandleResponse).GetStatus() != 204 {
		t.Error("the handler's own response was not returned untouched")
	}
}
