package server

import (
	"context"
	modulesdk "github.com/mycelpf/mycel_module_sdk"
	modulerpc "github.com/mycelpf/mycel_module_sdk/contracts/modulerpc/v1"
	"google.golang.org/grpc/codes"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func TestAuditReadRetryPolicy(t *testing.T) {
	for _, tc := range []struct {
		method    string
		code      codes.Code
		failFirst int32
		want      int32
		ok        bool
	}{
		{"GET", codes.Unavailable, 1, 2, true},
		{"HEAD", codes.Unavailable, 1, 2, true},
		{"OPTIONS", codes.Unavailable, 1, 2, true},
		{"GET", codes.Unavailable, 0, 3, false},
		{"GET", codes.PermissionDenied, 0, 1, false},
	} {
		t.Run(tc.method+tc.code.String()+string(rune(tc.want)), func(t *testing.T) {
			es := &extServer{failWith: tc.code, failFirst: tc.failFirst}
			startExt(t, "SDK_AUDIT_EXT", es)
			_, _, _, err := callExt(context.Background(), "SDK_AUDIT_EXT", tc.method, "/read", nil)
			if (err == nil) != tc.ok || es.calls.Load() != tc.want {
				t.Fatalf("calls=%d err=%v", es.calls.Load(), err)
			}
		})
	}
}

func TestAuditBridgeRepeatedOrdinaryHeaders(t *testing.T) {
	r := bridgeHTTP(context.Background(), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Add("Vary", "Accept-Encoding")
		w.Header().Add("Vary", "Origin")
	}), &modulerpc.HandleRequest{Path: "/"})
	if r.Headers["Vary"] != "Accept-Encoding, Origin" {
		t.Fatal(r.Headers)
	}
}

func TestAuditConcurrentSharedResponseHeaders(t *testing.T) {
	headers := map[string]string{"x-shared": "yes"}
	h := &handler{health: newHealthGate(nil, "audit"), d: modulesdk.Descriptor{Route: func(modulesdk.Request, modulesdk.Host) modulesdk.Response {
		return modulesdk.Response{Status: 200, Headers: headers}
	}}}
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := h.Handle(context.Background(), &modulerpc.HandleRequest{Path: "/"})
			if err != nil || resp.GetStatus() != 200 {
				t.Error("Handle failed")
			}
		}()
	}
	wg.Wait()
	if len(headers) != 1 {
		t.Fatal("shared map mutated")
	}
}

func TestAuditExtensionWritesAreNeverReplayed(t *testing.T) {
	for _, method := range []string{"POST", "PATCH", "PUT", "DELETE"} {
		t.Run(method, func(t *testing.T) {
			es := &extServer{failWith: codes.Unavailable, failFirst: 1}
			startExt(t, "SDK_AUDIT_EXT", es)
			_, _, _, err := callExt(context.Background(), "SDK_AUDIT_EXT", method, "/write", nil)
			if err == nil || es.calls.Load() != 1 {
				t.Fatalf("write replayed: calls=%d err=%v", es.calls.Load(), err)
			}
		})
	}
}

func TestAuditBridgePreservesCookiesAndCommittedHeaders(t *testing.T) {
	resp := bridgeHTTP(context.Background(), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Set-Cookie", "session=one; Path=/")
		w.Header().Add("Set-Cookie", "csrf=two; Path=/")
		w.Header().Set("X-State", "committed")
		w.WriteHeader(201)
		w.Header().Set("X-State", "late")
	}), &modulerpc.HandleRequest{Method: "GET", Path: "/"})
	if resp.Headers["Set-Cookie"] != "session=one; Path=/\ncsrf=two; Path=/" {
		t.Errorf("cookies lost: %v", resp.Headers)
	}
	if resp.Headers["X-State"] != "committed" {
		t.Errorf("late header leaked: %v", resp.Headers)
	}
}

func TestAuditVitalsAcrossHandlerTypes(t *testing.T) {
	for _, httpMode := range []bool{false, true} {
		called := false
		d := modulesdk.Descriptor{Token: "audit", Route: func(modulesdk.Request, modulesdk.Host) modulesdk.Response {
			called = true
			return modulesdk.Response{Status: 404}
		}}
		if httpMode {
			d.Route = nil
			d.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true; w.WriteHeader(404) })
		}
		h := &handler{d: d, health: newHealthGate(nil, "audit")}
		resp, err := h.Handle(context.Background(), &modulerpc.HandleRequest{Method: "GET", Path: "/_mycel/vitals"})
		if err != nil || resp.GetStatus() != 200 || called {
			t.Errorf("httpMode=%v status=%d called=%v err=%v", httpMode, resp.GetStatus(), called, err)
		}
	}
}

func TestAuditResponseHeadersRemainOwnedByRoute(t *testing.T) {
	for _, headers := range []map[string]string{{"x-test": "yes"}, {"Content-Type": "text/plain"}} {
		original := map[string]string{}
		for k, v := range headers {
			original[k] = v
		}
		h := &handler{health: newHealthGate(nil, "audit"), d: modulesdk.Descriptor{Route: func(modulesdk.Request, modulesdk.Host) modulesdk.Response {
			return modulesdk.Response{Status: 200, Headers: headers}
		}}}
		resp, err := h.Handle(context.Background(), &modulerpc.HandleRequest{Method: "GET", Path: "/"})
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(headers, original) {
			t.Errorf("route headers mutated: %v", headers)
		}
		count := 0
		for k := range resp.Headers {
			if strings.EqualFold(k, "content-type") {
				count++
			}
		}
		if count != 1 {
			t.Errorf("duplicate content types: %v", resp.Headers)
		}
	}
}
