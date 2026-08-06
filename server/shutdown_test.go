package server

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	modulesdk "github.com/mycelpf/mycel_module_sdk"
)

// The order in cleanup is load-bearing: the hook runs after the gRPC server has stopped
// accepting (so nothing can enqueue into a queue that is already draining) and before the
// datastore closes (so the hook may still write).
func TestCleanupRunsShutdownHookBeforeClosingResources(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "test.sock")
	if err := os.WriteFile(sock, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	var order []string
	d := modulesdk.Descriptor{
		Token: "test",
		Shutdown: func(ctx context.Context) error {
			// The socket must still exist when the hook runs: a hook that wants to make a
			// final call over it has to be able to.
			if _, err := os.Stat(sock); err != nil {
				t.Errorf("socket already removed before the hook ran: %v", err)
			}
			if _, ok := ctx.Deadline(); !ok {
				t.Error("hook received a context with no deadline — a slow hook would hang exit")
			}
			order = append(order, "hook")
			return nil
		},
	}

	var flushed bool
	cleanup(d, (*sql.DB)(nil), "unix", sock, func(context.Context) error {
		flushed = true
		order = append(order, "telemetry")
		return nil
	})

	if len(order) != 2 || order[0] != "hook" || order[1] != "telemetry" {
		t.Errorf("cleanup order = %v, want [hook telemetry]", order)
	}
	if !flushed {
		t.Error("telemetry was not flushed — spans from the failing run would be lost")
	}
	if _, err := os.Stat(sock); !os.IsNotExist(err) {
		t.Error("the unix socket was not removed")
	}
}

// A hook that overruns must not hang the process: the deadline is what bounds it, and the
// error it returns is the record of what could not be delivered.
func TestCleanupBoundsASlowShutdownHook(t *testing.T) {
	t.Setenv("MYCEL_SHUTDOWN_TIMEOUT_MS", "80")

	var gotErr error
	d := modulesdk.Descriptor{
		Token: "test",
		Shutdown: func(ctx context.Context) error {
			<-ctx.Done() // never finishes on its own
			gotErr = errors.New("3 event(s) undelivered")
			return gotErr
		},
	}

	start := time.Now()
	cleanup(d, nil, "tcp", "127.0.0.1:0", nil)
	elapsed := time.Since(start)

	if elapsed > 3*time.Second {
		t.Errorf("cleanup took %s — a stuck hook must not hold the process open", elapsed)
	}
	if gotErr == nil {
		t.Error("the hook never observed its deadline")
	}
}

func TestCleanupWithNoHookIsSafe(t *testing.T) {
	// Every module that predates Descriptor.Shutdown takes this path.
	cleanup(modulesdk.Descriptor{Token: "test"}, nil, "tcp", "127.0.0.1:0", nil)
}
