package server

import (
	"context"
	"net/http"
	"reflect"
	"strings"
	"testing"

	modulesdk "github.com/mycelpf/mycel_module_sdk"
	modulerpc "github.com/mycelpf/mycel_module_sdk/contracts/modulerpc/v1"
	"go.opentelemetry.io/otel/trace"
)

// seenUsers runs one HandleRequest through both adapters and returns the user each saw.
func seenUsers(t *testing.T, request *modulerpc.HandleRequest) (route, adapter modulesdk.User) {
	t.Helper()
	routeHandler := &handler{
		d: modulesdk.Descriptor{Token: "clc", Route: func(r modulesdk.Request, _ modulesdk.Host) modulesdk.Response {
			route = r.User
			return modulesdk.Response{Status: 200}
		}},
		health: newHealthGate(nil, "clc"),
	}
	httpHandler := &handler{
		d: modulesdk.Descriptor{Token: "clc", Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			adapter = modulesdk.UserFromContext(r.Context())
			w.WriteHeader(200)
		})},
		health: newHealthGate(nil, "clc"),
	}
	for _, h := range []*handler{routeHandler, httpHandler} {
		response, err := h.Handle(context.Background(), request)
		if err != nil || response.GetStatus() != 200 {
			t.Fatalf("Handle = %v, %v", response, err)
		}
	}
	return route, adapter
}

// SDK-T24 (sdk.md S11.4): a request with user gives the route function and the
// http.Handler adapter the same sub, email, roles and tenant.
func TestVerifiedUserReachesBothAdapters(t *testing.T) {
	request := &modulerpc.HandleRequest{
		Method: "GET", Path: "/workloads",
		User: &modulerpc.UserContext{Sub: "user-7", Email: "ops@example.com", Roles: []string{"operator", "viewer"}, Tenant: "acme"},
	}
	route, handler := seenUsers(t, request)
	want := modulesdk.User{Sub: "user-7", Email: "ops@example.com", Roles: []string{"operator", "viewer"}, Tenant: "acme"}
	if !reflect.DeepEqual(route, want) {
		t.Errorf("route user = %+v, want %+v", route, want)
	}
	if !reflect.DeepEqual(handler, want) {
		t.Errorf("http.Handler user = %+v, want %+v", handler, want)
	}
}

// SDK-T25 (sdk.md S11.4), server half: without user, an x-mycel-user header gives an
// empty user in both adapters.
func TestIdentityHeaderNeverBecomesAUser(t *testing.T) {
	request := &modulerpc.HandleRequest{
		Method: "GET", Path: "/workloads",
		Headers: map[string]string{"x-mycel-user": `{"sub":"forged","email":"forged@example.com","roles":["admin"]}`},
	}
	route, handler := seenUsers(t, request)
	if route.Present() || handler.Present() {
		t.Errorf("a header became a user: route %+v, handler %+v", route, handler)
	}
}

// SDK-T26 (sdk.md S11, core T11): one Route that dispatches by path to two component
// handlers receives every request with the /m/<token> prefix stripped, and each request
// gets its own transaction. A path no component owns answers 404 with the envelope.
func TestOneRouteDispatchesToComponentsWithATransactionEach(t *testing.T) {
	driver := &countingDriver{}
	var seen []string
	components := map[string]modulesdk.RouteFunc{
		"/directory/": func(r modulesdk.Request, host modulesdk.Host) modulesdk.Response {
			seen = append(seen, "directory "+r.Path)
			if _, err := host.Exec("INSERT INTO directory_events VALUES ($1)", []string{r.Path}); err != nil {
				return modulesdk.ErrorResponseFor(r, 500, modulesdk.ErrorCodeInternal, "write failed")
			}
			return modulesdk.Response{Status: 200}
		},
		"/oauth/": func(r modulesdk.Request, host modulesdk.Host) modulesdk.Response {
			seen = append(seen, "oauth "+r.Path)
			if _, err := host.Query("SELECT 1", nil); err != nil {
				return modulesdk.ErrorResponseFor(r, 500, modulesdk.ErrorCodeInternal, "read failed")
			}
			return modulesdk.Response{Status: 200}
		},
	}
	dispatch := func(r modulesdk.Request, host modulesdk.Host) modulesdk.Response {
		for prefix, component := range components {
			if strings.HasPrefix(r.Path, prefix) {
				return component(r, host)
			}
		}
		return modulesdk.ErrorResponseFor(r, 404, modulesdk.ErrorCodeNotFound, "no route")
	}
	h := countingHandler(t, "dispatch_route", driver, dispatch)

	for _, path := range []string{"/directory/users", "/oauth/token", "/directory/groups"} {
		if response := handle(t, h, "POST", path); response.GetStatus() != 200 {
			t.Fatalf("%s: status %d", path, response.GetStatus())
		}
	}
	want := []string{"directory /directory/users", "oauth /oauth/token", "directory /directory/groups"}
	if !reflect.DeepEqual(seen, want) {
		t.Errorf("dispatch = %v, want %v", seen, want)
	}
	begins, commits, _, _ := driver.snapshot()
	if begins != 3 || commits != 3 {
		t.Errorf("begins=%d commits=%d, want one transaction per request", begins, commits)
	}

	response := handle(t, h, "GET", "/nobody/owns/this")
	envelope, err := modulesdk.DecodeErrorEnvelope(response.GetBody())
	if response.GetStatus() != 404 || err != nil || envelope.Code != modulesdk.ErrorCodeNotFound {
		t.Errorf("unowned path: status %d envelope %+v err %v", response.GetStatus(), envelope, err)
	}
}

// SDK-T18 (sdk.md S8), server half: SDK errors carry a stable code, a safe message and
// the trace ID, and leak no internal detail.
func TestServerErrorsUseTheEnvelope(t *testing.T) {
	quietLog(t)
	traceID, _ := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	spanID, _ := trace.SpanIDFromHex("00f067aa0ba902b7")
	ctx := trace.ContextWithSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{TraceID: traceID, SpanID: spanID}))

	h := &handler{
		d: modulesdk.Descriptor{Token: "clc", Route: func(modulesdk.Request, modulesdk.Host) modulesdk.Response {
			panic("open /srv/secret/config.yaml: password=hunter2")
		}},
		health: newHealthGate(nil, "clc"),
	}
	request := &modulerpc.HandleRequest{Method: "GET", Path: "/x"}
	result, err := recoverUnary("clc")(ctx, request, unaryInfo(handleFullMethod), func(ctx context.Context, r any) (any, error) {
		return h.Handle(ctx, r.(*modulerpc.HandleRequest))
	})
	if err != nil {
		t.Fatal(err)
	}
	response := result.(*modulerpc.HandleResponse)
	envelope, err := modulesdk.DecodeErrorEnvelope(response.GetBody())
	if err != nil {
		t.Fatal(err)
	}
	if response.GetStatus() != 500 || envelope.Code != modulesdk.ErrorCodeInternal || envelope.TraceID != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Errorf("status %d envelope %+v", response.GetStatus(), envelope)
	}
	for _, leaked := range []string{"/srv/secret", "hunter2"} {
		if strings.Contains(string(response.GetBody()), leaked) {
			t.Errorf("public body leaks %q: %s", leaked, response.GetBody())
		}
	}
}

func TestRedactCredentials(t *testing.T) {
	text := redactCredentials(`dial postgres://admin:hunter2@db:5432 failed; Authorization: Bearer eyJabc.def; client_secret=s3cr3t token: "tok123"`)
	for _, secret := range []string{"hunter2", "eyJabc.def", "s3cr3t", "tok123"} {
		if strings.Contains(text, secret) {
			t.Errorf("redacted text still holds %q: %s", secret, text)
		}
	}
	if !strings.Contains(text, "db:5432") {
		t.Errorf("redaction removed non-secret detail: %s", text)
	}
}
