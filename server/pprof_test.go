package server

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	modulesdk "github.com/mycelpf/mycel_module_sdk"
	modulerpc "github.com/mycelpf/mycel_module_sdk/contracts/modulerpc/v1"
)

func TestValidatePprofAddr(t *testing.T) {
	for _, ok := range []string{"127.0.0.1:6060", "[::1]:6060", "localhost:6060", "127.0.0.1:0"} {
		if err := validatePprofAddr(ok); err != nil {
			t.Errorf("validatePprofAddr(%q) = %v, want accepted", ok, err)
		}
	}
	for _, bad := range []string{
		"0.0.0.0:6060", ":6060", "10.1.2.3:6060", "example.com:6060", "127.0.0.1", "",
		"[::]:6060", "127.0.0.2:6060", "localhost", "localhost:", "127.0.0.1:http", "127.0.0.1:99999",
		"::1:6060", "LOCALHOST.evil.com:6060",
	} {
		if err := validatePprofAddr(bad); err == nil {
			t.Errorf("validatePprofAddr(%q) accepted, want refused", bad)
		}
	}
}

func TestStartPprofEmptyAddrIsOff(t *testing.T) {
	stop, bound, err := startPprof("")
	if err != nil {
		t.Fatalf("startPprof(\"\") error = %v, want nil", err)
	}
	if bound != "" {
		t.Errorf("bound = %q, want no listener", bound)
	}
	if stop != nil {
		if err := stop(context.Background()); err != nil {
			t.Errorf("stop: %v", err)
		}
	}
}

func TestStartPprofRefusesNonLoopback(t *testing.T) {
	// Learn a free port, release it, then ask for it on a wildcard host: a fail-open
	// implementation would bind it and the dial below would succeed.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	for _, addr := range []string{"0.0.0.0:0", net.JoinHostPort("0.0.0.0", itoa(port)), net.JoinHostPort("", itoa(port))} {
		stop, bound, err := startPprof(addr)
		if err == nil {
			t.Errorf("startPprof(%q) error = nil, want refusal", addr)
		}
		if bound != "" {
			t.Errorf("startPprof(%q) bound %q", addr, bound)
		}
		if stop != nil {
			_ = stop(context.Background())
		}
	}
	if c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", itoa(port)), 300*time.Millisecond); err == nil {
		c.Close()
		t.Errorf("something is listening on %d after refused starts", port)
	}
}

func itoa(n int) string { return fmtInt(n) }

func fmtInt(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for ; n > 0; n /= 10 {
		b = append([]byte{byte('0' + n%10)}, b...)
	}
	return string(b)
}

func getBody(t *testing.T, url string) (int, string) {
	t.Helper()
	c := &http.Client{Timeout: 10 * time.Second}
	resp, err := c.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestStartPprofServesProfilesOnLoopbackAndStops(t *testing.T) {
	stop, bound, err := startPprof("127.0.0.1:0")
	if err != nil {
		t.Fatalf("startPprof: %v", err)
	}
	if bound == "" {
		t.Fatal("no bound address returned")
	}
	stopped := false
	defer func() {
		if !stopped {
			_ = stop(context.Background())
		}
	}()
	host, _, _ := net.SplitHostPort(bound)
	if host != "127.0.0.1" {
		t.Errorf("bound host = %q, want 127.0.0.1", host)
	}

	code, body := getBody(t, "http://"+bound+"/debug/pprof/heap?debug=1")
	if code != 200 || !strings.HasPrefix(body, "heap profile:") {
		t.Errorf("heap: code=%d body=%.60q", code, body)
	}
	code, body = getBody(t, "http://"+bound+"/debug/pprof/goroutine?debug=1")
	if code != 200 || !strings.Contains(body, "goroutine profile: total") {
		t.Errorf("goroutine: code=%d body=%.60q", code, body)
	}
	// Dedicated mux: anything outside /debug/pprof/ (e.g. whatever else was registered
	// on http.DefaultServeMux) must not be served.
	if code, _ := getBody(t, "http://"+bound+"/"); code != 404 {
		t.Errorf("GET / = %d, want 404", code)
	}

	if err := stop(context.Background()); err != nil {
		t.Errorf("stop: %v", err)
	}
	stopped = true
	if c, err := net.DialTimeout("tcp", bound, 300*time.Millisecond); err == nil {
		c.Close()
		t.Error("port still accepts connections after stop")
	}
}

// pprof registers on http.DefaultServeMux at import; the debug listener must not serve
// that mux (anything else registered there would leak onto the debug port).
func TestStartPprofDoesNotServeDefaultServeMux(t *testing.T) {
	http.HandleFunc("/leak-canary", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("leaked")) })
	stop, bound, err := startPprof("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer stop(context.Background())
	if code, body := getBody(t, "http://"+bound+"/leak-canary"); code == 200 || body == "leaked" {
		t.Errorf("DefaultServeMux reachable on the debug listener: %d %q", code, body)
	}
}

// The module's normal traffic path must never reach pprof, whatever the path.
func TestHandleNeverRoutesToPprof(t *testing.T) {
	db := fakeDB(t, "fake_pprof_guard", nil)
	h := &handler{
		d: modulesdk.Descriptor{
			Token: "testmod",
			Route: func(req modulesdk.Request, _ modulesdk.Host) modulesdk.Response {
				return modulesdk.Response{Status: 404, Body: `{"route":"` + req.Path + `"}`}
			},
		},
		db:     db,
		health: newHealthGate(db, "testmod"),
	}
	for _, p := range []string{"/debug/pprof/heap", "/debug/pprof/", "/debug/pprof/profile"} {
		resp, err := h.Handle(context.Background(), &modulerpc.HandleRequest{Method: "GET", Path: p})
		if err != nil {
			t.Fatal(err)
		}
		if resp.GetStatus() != 404 || string(resp.GetBody()) != `{"route":"`+p+`"}` {
			t.Errorf("%s: status=%d body=%.80q, want the Route's own response", p, resp.GetStatus(), resp.GetBody())
		}
	}
}
