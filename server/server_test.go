package server

import (
	"encoding/json"
	"runtime"
	"strings"
	"testing"
)

// TestErrResponse pins that the error envelope is always VALID JSON — including when
// the message carries a quote or backslash (e.g. a pq error naming a "quoted"
// identifier). The old string-concatenated body produced un-parseable JSON in exactly
// that case; this is the shared SDK, so every module inherits the fix.
func TestErrResponse(t *testing.T) {
	cases := []string{"boom", `bad "quoted" value`, `path C:\temp\x`, "line1\nline2"}
	for _, msg := range cases {
		r := errResponse(500, msg)
		if r.Status != 500 {
			t.Errorf("status = %d, want 500", r.Status)
		}
		if r.Headers["content-type"] != "application/json" {
			t.Errorf("content-type = %q", r.Headers["content-type"])
		}
		var got map[string]string
		if err := json.Unmarshal(r.Body, &got); err != nil {
			t.Fatalf("errResponse(%q) body is not valid JSON: %v (body: %s)", msg, err, r.Body)
		}
		if got["error"] != msg {
			t.Errorf("round-trip: got %q, want %q", got["error"], msg)
		}
	}
}

// TestValidateUDSPath pins the boot-critical guard against a MYCEL_LISTEN socket
// path that would overflow the kernel's fixed sockaddr_un.sun_path and SILENTLY
// truncate — the exact failure mode validateUDSPath exists to turn into a loud
// boot error. This is the shared SDK's most silent-failure-prone helper, so it is
// the first thing worth a test now that the SDK is one module every app depends on.
func TestValidateUDSPath(t *testing.T) {
	limit := 104 // darwin sun_path
	if runtime.GOOS == "linux" {
		limit = 108
	}
	cases := []struct {
		name    string
		path    string
		wantErr bool
	}{
		{"typical socket", "/tmp/mycel_app_core/workflow_manager.sock", false},
		{"empty", "", false},
		{"one under limit", strings.Repeat("a", limit-1), false},
		{"exactly at limit rejected", strings.Repeat("a", limit), true}, // >= limit fails: no room for the NUL terminator
		{"well over limit", strings.Repeat("a", limit+64), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := validateUDSPath(c.path); (err != nil) != c.wantErr {
				t.Errorf("validateUDSPath(len=%d) err=%v, wantErr=%v", len(c.path), err, c.wantErr)
			}
		})
	}
}
