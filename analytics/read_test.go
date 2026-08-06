package analytics

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// readTransport answers GET /kpi/{key} with a scripted status and body, and records the
// deadline it was called with (the 3s budget is a correctness property, not a nicety).
type readTransport struct {
	status   uint16
	body     string
	calls    int
	deadline time.Time
	lastPath string
}

func (r *readTransport) fn(ctx context.Context, envKey, method, path string, body []byte) (uint16, []byte, bool, error) {
	r.calls++
	r.lastPath = path
	r.deadline, _ = ctx.Deadline()
	return r.status, []byte(r.body), true, nil
}

const oneKPI = `{"key":"orders_submitted","name":"Orders Submitted","area":"operations",
"unit":"count","format":"number","direction":"higher_is_better","definition":"d","version":1,
"grain":"day","current":{"period_start":"2026-08-05T00:00:00Z","value":42},
"prior":{"period_start":"2026-08-04T00:00:00Z","value":37},"delta_pct":13.5,
"series":[{"period_start":"2026-08-04T00:00:00Z","value":37},{"period_start":"2026-08-05T00:00:00Z","value":42}]}`

func newReadClient(t *testing.T, rt *readTransport) *Client {
	t.Helper()
	t.Setenv(envURL, "127.0.0.1:1")
	c := New(Config{Service: "test", Transport: rt.fn, Log: func(string, string) {}})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = c.Shutdown(ctx)
	})
	return c
}

func TestKPIParsesAndCarriesDefinitionMetadata(t *testing.T) {
	rt := &readTransport{status: 200, body: oneKPI}
	c := newReadClient(t, rt)

	res, err := c.KPI(context.Background(), "orders_submitted", Params{Grain: "day"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Current == nil || res.Current.Value != 42 {
		t.Errorf("current = %+v, want 42", res.Current)
	}
	if res.Prior == nil || res.Prior.Value != 37 {
		t.Errorf("prior = %+v, want 37", res.Prior)
	}
	if len(res.Series) != 2 {
		t.Errorf("series has %d points, want 2", len(res.Series))
	}
	// The render metadata is the point of the definition columns: without direction and
	// format the card cannot colour a delta or format a number.
	if res.Direction != "higher_is_better" || res.Format != "number" || res.Version != 1 {
		t.Errorf("render metadata lost: %+v", res)
	}
	if !strings.Contains(rt.lastPath, "grain=day") {
		t.Errorf("params did not reach the wire: path = %q", rt.lastPath)
	}
}

// The read budget must stay strictly under the host's 5s MYCEL_HANDLE_TIMEOUT, or the
// host cancels first and the user gets a 504 instead of the designed degraded state.
func TestKPIDeadlineStaysUnderTheHostBudget(t *testing.T) {
	rt := &readTransport{status: 200, body: oneKPI}
	c := newReadClient(t, rt)
	_, _ = c.KPI(context.Background(), "k", Params{})

	budget := time.Until(rt.deadline)
	if budget > 4*time.Second {
		t.Errorf("read budget %s leaves no room under the host's 5s deadline", budget)
	}
	if budget <= 0 {
		t.Error("no deadline was applied to the read")
	}
}

func TestKPICacheAvoidsASecondCall(t *testing.T) {
	rt := &readTransport{status: 200, body: oneKPI}
	c := newReadClient(t, rt)
	for i := 0; i < 3; i++ {
		if _, err := c.KPI(context.Background(), "k", Params{Grain: "day"}); err != nil {
			t.Fatal(err)
		}
	}
	if rt.calls != 1 {
		t.Errorf("transport called %d times, want 1 — per-render reads must be free", rt.calls)
	}
	// Different params are a different question and must not hit the same entry.
	if _, err := c.KPI(context.Background(), "k", Params{Grain: "month"}); err != nil {
		t.Fatal(err)
	}
	if rt.calls != 2 {
		t.Errorf("transport called %d times, want 2 — the cache key ignores params", rt.calls)
	}
}

// A blip should age the number, not blank the card.
func TestKPIServesStaleWhenTheStoreFails(t *testing.T) {
	rt := &readTransport{status: 200, body: oneKPI}
	t.Setenv(envCacheTTLMS, "1") // expire immediately so the next call refreshes
	c := newReadClient(t, rt)

	if _, err := c.KPI(context.Background(), "k", Params{Grain: "day"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	rt.status = 503

	res, err := c.KPI(context.Background(), "k", Params{Grain: "day"})
	if err != nil {
		t.Fatalf("a store blip must degrade, not error: %v", err)
	}
	if !res.Stale {
		t.Error("stale results must be marked, so the UI can say so")
	}
	if res.Current == nil || res.Current.Value != 42 {
		t.Error("the last-good value was not preserved")
	}
}

func TestKPITypedErrors(t *testing.T) {
	rt := &readTransport{status: 404, body: `{"error":"unknown"}`}
	c := newReadClient(t, rt)
	if _, err := c.KPI(context.Background(), "nope", Params{}); !errors.Is(err, ErrNotFound) {
		t.Errorf("404 → %v, want ErrNotFound", err)
	}

	rt2 := &readTransport{status: 503}
	c2 := newReadClient(t, rt2)
	if _, err := c2.KPI(context.Background(), "k", Params{}); !errors.Is(err, ErrUnavailable) {
		t.Errorf("503 with a cold cache → %v, want ErrUnavailable", err)
	}
}

func TestKPIWithNoModuleConfigured(t *testing.T) {
	t.Setenv(envURL, "")
	c := New(Config{Service: "t", Log: func(string, string) {}})
	if _, err := c.KPI(context.Background(), "k", Params{}); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("got %v, want ErrNotConfigured", err)
	}
}

func TestWithHeaders(t *testing.T) {
	valid := "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	cases := []struct {
		name, traceparent, user string
		wantTrace, wantActor    string
	}{
		{"valid", valid, `{"sub":"user-1","email":"a@b.c"}`, "4bf92f3577b34da6a3ce929d0e0e4736", "user-1"},
		{"missing", "", "", "", ""},
		{"malformed", "not-a-traceparent", "", "", ""},
		{"short trace id", "00-abc-def-01", "", "", ""},
		// An all-zero trace id is invalid per W3C; a bogus id is worse than none because
		// it links the event into somebody else's trace.
		{"all zero", "00-00000000000000000000000000000000-00f067aa0ba902b7-01", "", "", ""},
		{"non-hex", "00-zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz-00f067aa0ba902b7-01", "", "", ""},
		{"actor by email only", "", `{"email":"a@b.c"}`, "", "a@b.c"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := Event{Type: "T"}.WithHeaders(map[string]string{
				"traceparent":  tc.traceparent,
				"x-mycel-user": tc.user,
			})
			if e.TraceID != tc.wantTrace {
				t.Errorf("TraceID = %q, want %q", e.TraceID, tc.wantTrace)
			}
			if e.ActorID != tc.wantActor {
				t.Errorf("ActorID = %q, want %q", e.ActorID, tc.wantActor)
			}
		})
	}
}

func TestWithHeadersDoesNotOverwriteExplicitValues(t *testing.T) {
	e := Event{ActorID: "explicit", TraceID: "deadbeefdeadbeefdeadbeefdeadbeef"}.
		WithHeaders(map[string]string{
			"traceparent":  "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
			"x-mycel-user": `{"sub":"other"}`,
		})
	if e.ActorID != "explicit" || e.TraceID != "deadbeefdeadbeefdeadbeefdeadbeef" {
		t.Errorf("explicit values were overwritten: %+v", e)
	}
}

func TestUUIDv4Shape(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		id := uuidV4()
		if len(id) != 36 || id[14] != '4' {
			t.Fatalf("not a v4 uuid: %q", id)
		}
		if seen[id] {
			t.Fatalf("duplicate id %q — the dedup key must be unique", id)
		}
		seen[id] = true
	}
}
