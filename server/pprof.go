package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/pprof"
	"strconv"
	"time"
)

// pprofEnv is the opt-in switch for the debug listener. Unset or empty → no listener.
const pprofEnv = "MYCEL_PPROF_ADDR"

// validatePprofAddr fails closed: only 127.0.0.1, [::1] or localhost with an explicit
// numeric port pass. An empty host (":6060" binds every interface), 0.0.0.0, any other
// address or hostname, or a missing port is refused — a debug endpoint must never be
// reachable off-host, and a typo must not widen it.
func validatePprofAddr(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("%s=%q: want host:port: %w", pprofEnv, addr, err)
	}
	switch host {
	case "127.0.0.1", "::1", "localhost":
	default:
		return fmt.Errorf("%s=%q: host %q is not loopback (want 127.0.0.1, [::1] or localhost)", pprofEnv, addr, host)
	}
	if _, err := strconv.ParseUint(port, 10, 16); err != nil {
		return fmt.Errorf("%s=%q: port %q is not a number in 0-65535", pprofEnv, addr, port)
	}
	return nil
}

// startPprof serves net/http/pprof on a DEDICATED mux at addr and returns a stop func
// and the bound address (useful with port 0). An empty addr is the default-off case: no
// listener, nil error, nil stop. A non-loopback addr returns an error and binds nothing.
// The module's gRPC/Handle path never reaches this mux, and this mux never sees
// http.DefaultServeMux (which importing net/http/pprof also registers on).
func startPprof(addr string) (stop func(context.Context) error, boundAddr string, err error) {
	if addr == "" {
		return nil, "", nil
	}
	if err := validatePprofAddr(addr); err != nil {
		return nil, "", err
	}
	host, port, _ := net.SplitHostPort(addr)
	if host == "localhost" {
		host = "127.0.0.1" // bind the literal, not whatever "localhost" resolves to
	}
	lis, err := net.Listen("tcp", net.JoinHostPort(host, port))
	if err != nil {
		return nil, "", err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(lis) }()
	return func(ctx context.Context) error {
		// Close, not Shutdown: a 30s CPU profile in flight must not hold up pod exit.
		err := srv.Close()
		if serr := <-errCh; serr != nil && !errors.Is(serr, http.ErrServerClosed) {
			return serr
		}
		return err
	}, lis.Addr().String(), nil
}
