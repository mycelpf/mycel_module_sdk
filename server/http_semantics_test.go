package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	modulesdk "github.com/mycelpf/mycel_module_sdk"
	modulerpc "github.com/mycelpf/mycel_module_sdk/contracts/modulerpc/v1"
	"github.com/mycelpf/mycel_module_sdk/wire"
)

const (
	cookieOne = "session=abc; Expires=Wed, 21 Oct 2026 07:28:00 GMT; Path=/; HttpOnly"
	cookieTwo = "theme=dark; Expires=Thu, 22 Oct 2026 07:28:00 GMT; Path=/"
)

// binaryBody holds every byte value, including invalid UTF-8.
func binaryBody() []byte {
	body := make([]byte, 256)
	for index := range body {
		body[index] = byte(index)
	}
	return body
}

func routeHandler(route modulesdk.RouteFunc) *handler {
	return &handler{d: modulesdk.Descriptor{Token: "clc", Route: route}, health: newHealthGate(nil, "clc")}
}

func httpHandler(serve http.HandlerFunc) *handler {
	return &handler{d: modulesdk.Descriptor{Token: "clc", Handler: serve}, health: newHealthGate(nil, "clc")}
}

func repeatedRequest(body []byte) *modulerpc.HandleRequest {
	return &modulerpc.HandleRequest{
		Method: "POST", Path: "/upload", Body: body,
		HeaderFields: []*modulerpc.HeaderField{
			{Name: "accept", Values: []string{"text/html", "application/json"}},
			{Name: "x-forwarded-for", Values: []string{"10.0.0.1", "10.0.0.2"}},
		},
	}
}

func mustHandle(t *testing.T, h *handler, request *modulerpc.HandleRequest) *modulerpc.HandleResponse {
	t.Helper()
	response, err := h.Handle(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func responseFields(t *testing.T, response *modulerpc.HandleResponse) map[string][]string {
	t.Helper()
	fields, err := wire.HeaderFieldsFromHandleResponse(response)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string][]string{}
	for _, field := range fields {
		byName[field.Name] = field.Values
	}
	return byName
}

// SDK-T11 (sdk.md S4), Route adapter: repeated request values reach the module; two
// cookies with expiry commas, an HTML content type and a binary body come back intact.
func TestRepeatedHeadersRoundTripThroughRoute(t *testing.T) {
	var seen modulesdk.Request
	h := routeHandler(func(r modulesdk.Request, _ modulesdk.Host) modulesdk.Response {
		seen = r
		return modulesdk.Response{Status: 200, Body: string(r.Body), HeaderFields: []modulesdk.HeaderField{
			{Name: "Set-Cookie", Values: []string{cookieOne, cookieTwo}},
			{Name: "Content-Type", Values: []string{"application/octet-stream"}},
		}}
	})
	response := mustHandle(t, h, repeatedRequest(binaryBody()))
	wantRequest := []modulesdk.HeaderField{
		{Name: "accept", Values: []string{"text/html", "application/json"}},
		{Name: "x-forwarded-for", Values: []string{"10.0.0.1", "10.0.0.2"}},
	}
	if !reflect.DeepEqual(seen.HeaderFields, wantRequest) {
		t.Errorf("request fields = %#v", seen.HeaderFields)
	}
	if seen.Headers["accept"] != "text/html, application/json" {
		t.Errorf("legacy request view = %#v", seen.Headers)
	}
	fields := responseFields(t, response)
	if !reflect.DeepEqual(fields["set-cookie"], []string{cookieOne, cookieTwo}) {
		t.Errorf("set-cookie = %q", fields["set-cookie"])
	}
	if !reflect.DeepEqual(fields["content-type"], []string{"application/octet-stream"}) {
		t.Errorf("content-type = %q", fields["content-type"])
	}
	if string(response.GetBody()) != string(binaryBody()) {
		t.Error("binary body changed")
	}
	// Hosts that predate header fields still get the newline form they split.
	if response.GetHeaders()["set-cookie"] != cookieOne+"\n"+cookieTwo {
		t.Errorf("legacy set-cookie = %q", response.GetHeaders()["set-cookie"])
	}
}

// SDK-T11 (sdk.md S4), migration: a Route module that still joins cookies with a newline
// in the legacy map gets separate cookies on the wire.
func TestLegacyRouteSetCookieIsTranslated(t *testing.T) {
	h := routeHandler(func(modulesdk.Request, modulesdk.Host) modulesdk.Response {
		return modulesdk.Response{Status: 200, Headers: map[string]string{"Set-Cookie": cookieOne + "\n" + cookieTwo}}
	})
	response := mustHandle(t, h, &modulerpc.HandleRequest{Method: "GET", Path: "/"})
	if got := responseFields(t, response)["set-cookie"]; !reflect.DeepEqual(got, []string{cookieOne, cookieTwo}) {
		t.Errorf("set-cookie = %q", got)
	}
	if response.GetHeaders()["Set-Cookie"] != cookieOne+"\n"+cookieTwo {
		t.Errorf("legacy map changed: %#v", response.GetHeaders())
	}
}

// SDK-T11 (sdk.md S4), http.Handler adapter: the same fixture through the bridge.
func TestRepeatedHeadersRoundTripThroughHandler(t *testing.T) {
	var seenAccept []string
	h := httpHandler(func(w http.ResponseWriter, r *http.Request) {
		seenAccept = r.Header.Values("Accept")
		body, _ := io.ReadAll(r.Body)
		w.Header().Add("Set-Cookie", cookieOne)
		w.Header().Add("Set-Cookie", cookieTwo)
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(body)
	})
	response := mustHandle(t, h, repeatedRequest(binaryBody()))
	if !reflect.DeepEqual(seenAccept, []string{"text/html", "application/json"}) {
		t.Errorf("Accept = %q", seenAccept)
	}
	fields := responseFields(t, response)
	if !reflect.DeepEqual(fields["set-cookie"], []string{cookieOne, cookieTwo}) {
		t.Errorf("set-cookie = %q", fields["set-cookie"])
	}
	if string(response.GetBody()) != string(binaryBody()) {
		t.Error("binary body changed")
	}
}

// SDK-T11 (sdk.md S4): an explicit HTML type is kept, and 204/304 get no injected JSON
// content type; status and cache validators are preserved in both adapters.
func TestBinaryAndHTMLBodiesArePreserved(t *testing.T) {
	html := "<!doctype html><p>café</p>"
	route := routeHandler(func(r modulesdk.Request, _ modulesdk.Host) modulesdk.Response {
		switch r.Path {
		case "/page":
			return modulesdk.Response{Status: 200, Headers: map[string]string{"Content-Type": "text/html; charset=utf-8", "ETag": `"v1"`}, Body: html}
		case "/empty":
			return modulesdk.Response{Status: 204}
		}
		return modulesdk.Response{Status: 304, Headers: map[string]string{"ETag": `"v1"`}}
	})
	bridged := httpHandler(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"v1"`)
		switch r.URL.Path {
		case "/page":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte(html))
		case "/empty":
			w.WriteHeader(204)
		default:
			w.WriteHeader(304)
		}
	})
	for name, h := range map[string]*handler{"route": route, "http.Handler": bridged} {
		page := mustHandle(t, h, &modulerpc.HandleRequest{Method: "GET", Path: "/page"})
		fields := responseFields(t, page)
		if !reflect.DeepEqual(fields["content-type"], []string{"text/html; charset=utf-8"}) || string(page.GetBody()) != html || !reflect.DeepEqual(fields["etag"], []string{`"v1"`}) {
			t.Errorf("%s page: %v %q", name, fields, page.GetBody())
		}
		for path, status := range map[string]uint32{"/empty": 204, "/conditional": 304} {
			response := mustHandle(t, h, &modulerpc.HandleRequest{Method: "GET", Path: path, HeaderFields: []*modulerpc.HeaderField{{Name: "if-none-match", Values: []string{`"v1"`}}}})
			if response.GetStatus() != status || responseFields(t, response)["content-type"] != nil {
				t.Errorf("%s %s: status %d, fields %v", name, path, response.GetStatus(), responseFields(t, response))
			}
		}
	}
}

func envelopeCode(t *testing.T, response *modulerpc.HandleResponse) string {
	t.Helper()
	envelope, err := modulesdk.DecodeErrorEnvelope(response.GetBody())
	if err != nil {
		t.Fatalf("not an envelope: %q", response.GetBody())
	}
	return envelope.Code
}

// SDK-T12 (sdk.md S4): CR/LF in a request header value is refused before the module runs.
func TestRequestWithCRLFHeaderIsRefused(t *testing.T) {
	called := false
	h := routeHandler(func(modulesdk.Request, modulesdk.Host) modulesdk.Response {
		called = true
		return modulesdk.Response{Status: 200}
	})
	for _, request := range []*modulerpc.HandleRequest{
		{Method: "GET", Path: "/", HeaderFields: []*modulerpc.HeaderField{{Name: "x-a", Values: []string{"a\r\nx-mycel-user: forged"}}}},
		{Method: "GET", Path: "/", Headers: map[string]string{"x-a": "a\nb"}},
	} {
		response := mustHandle(t, h, request)
		if response.GetStatus() != 400 || envelopeCode(t, response) != modulesdk.ErrorCodeInvalidHeader || called {
			t.Errorf("status %d, called %v, body %q", response.GetStatus(), called, response.GetBody())
		}
	}
}

// SDK-T12 (sdk.md S4): a module response header with CR/LF becomes a safe error, in both
// adapters, and the injected text never reaches the wire.
func TestResponseWithCRLFHeaderBecomesASafeError(t *testing.T) {
	quietLog(t)
	route := routeHandler(func(modulesdk.Request, modulesdk.Host) modulesdk.Response {
		return modulesdk.Response{Status: 200, HeaderFields: []modulesdk.HeaderField{{Name: "location", Values: []string{"/a\r\nset-cookie: admin=1"}}}}
	})
	bridged := httpHandler(func(w http.ResponseWriter, _ *http.Request) {
		w.Header()["Location"] = []string{"/a\r\nset-cookie: admin=1"}
		w.WriteHeader(302)
	})
	for name, h := range map[string]*handler{"route": route, "http.Handler": bridged} {
		response := mustHandle(t, h, &modulerpc.HandleRequest{Method: "GET", Path: "/"})
		if response.GetStatus() != 500 || envelopeCode(t, response) != modulesdk.ErrorCodeInvalidHeader || strings.Contains(string(response.GetBody()), "admin") {
			t.Errorf("%s: status %d body %q", name, response.GetStatus(), response.GetBody())
		}
	}
}

// SDK-T13 (sdk.md S5): an oversized request answers 413 before the module runs.
func TestOversizedRequestIsRefusedBeforeDispatch(t *testing.T) {
	quietLog(t)
	calls := 0
	h := routeHandler(func(modulesdk.Request, modulesdk.Host) modulesdk.Response {
		calls++
		return modulesdk.Response{Status: 200}
	})
	atLimit := mustHandle(t, h, &modulerpc.HandleRequest{Method: "POST", Path: "/", Body: make([]byte, modulesdk.MaximumBodyBytes)})
	if atLimit.GetStatus() != 200 || calls != 1 {
		t.Fatalf("at the limit: status %d, calls %d", atLimit.GetStatus(), calls)
	}
	for name, request := range map[string]*modulerpc.HandleRequest{
		"body":    {Method: "POST", Path: "/", Body: make([]byte, modulesdk.MaximumBodyBytes+1)},
		"headers": {Method: "GET", Path: "/", HeaderFields: []*modulerpc.HeaderField{{Name: "x-a", Values: []string{strings.Repeat("v", modulesdk.MaximumHeaderBytes)}}}},
	} {
		response := mustHandle(t, h, request)
		if response.GetStatus() != 413 || envelopeCode(t, response) != modulesdk.ErrorCodeRequestTooLarge || calls != 1 {
			t.Errorf("%s: status %d, calls %d", name, response.GetStatus(), calls)
		}
	}
}

// SDK-T13 (sdk.md S5): an oversized module response answers 502 with the stable code,
// its transaction is rolled back, and the mutation runs exactly once (no retry).
func TestOversizedResponseRollsBackAndAnswers502Once(t *testing.T) {
	quietLog(t)
	driver := &countingDriver{}
	calls := 0
	h := countingHandler(t, "counting_oversized", driver, func(_ modulesdk.Request, host modulesdk.Host) modulesdk.Response {
		calls++
		if _, err := host.Exec("INSERT INTO t VALUES (1)", nil); err != nil {
			return modulesdk.Response{Status: 500}
		}
		return modulesdk.Response{Status: 201, Body: strings.Repeat("a", modulesdk.MaximumBodyBytes+1)}
	})
	response := handle(t, h, "POST", "/write")
	if response.GetStatus() != 502 || envelopeCode(t, response) != modulesdk.ErrorCodeModuleResponseTooLarge {
		t.Fatalf("status %d body %.80q", response.GetStatus(), response.GetBody())
	}
	begins, commits, rollbacks, _ := driver.snapshot()
	if calls != 1 || begins != 1 || commits != 0 || rollbacks != 1 {
		t.Errorf("calls %d begins %d commits %d rollbacks %d", calls, begins, commits, rollbacks)
	}

	bridged := httpHandler(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Large", strings.Repeat("v", modulesdk.MaximumHeaderBytes))
	})
	if response := mustHandle(t, bridged, &modulerpc.HandleRequest{Method: "GET", Path: "/"}); response.GetStatus() != 502 {
		t.Errorf("bridged oversized headers: status %d", response.GetStatus())
	}
}

// SDK-D4: the route function gets the call's context through Request.Context, and Host
// calls for the request carry it.
func TestRouteReceivesTheRequestContext(t *testing.T) {
	type key struct{}
	var seen any
	h := routeHandler(func(r modulesdk.Request, _ modulesdk.Host) modulesdk.Response {
		seen = r.Context().Value(key{})
		return modulesdk.Response{Status: 200}
	})
	if _, err := h.Handle(context.WithValue(context.Background(), key{}, "call"), &modulerpc.HandleRequest{Path: "/"}); err != nil {
		t.Fatal(err)
	}
	if seen != "call" {
		t.Fatalf("route saw %v", seen)
	}
}

// SDK-D4: when app_core gives up on the call, the route sees its context cancelled.
func TestRouteContextIsCancelledWithTheCall(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	h := routeHandler(func(r modulesdk.Request, _ modulesdk.Host) modulesdk.Response {
		close(started)
		select {
		case <-r.Context().Done():
			return modulesdk.Response{Status: 499}
		case <-time.After(5 * time.Second):
			return modulesdk.Response{Status: 200}
		}
	})
	done := make(chan *modulerpc.HandleResponse, 1)
	go func() {
		response, _ := h.Handle(ctx, &modulerpc.HandleRequest{Path: "/slow"})
		done <- response
	}()
	<-started
	cancel()
	if response := <-done; response.GetStatus() != 499 {
		t.Fatalf("status %d", response.GetStatus())
	}
}

// SDK-T14 (sdk.md S6): two federated module fixtures; every documented operation, with
// its parameters filled in, reaches the module at the path app_core dispatches (the
// /m/<token> prefix stripped).
func TestFederatedOperationsResolveToDispatchedPaths(t *testing.T) {
	documents := map[string][]byte{
		"clc":       []byte(`{"openapi":"3.0.3","paths":{"/m/clc/api/workloads/{name}":{"get":{"operationId":"clc_workload"}},"/m/clc/api/history":{"get":{"operationId":"clc_history"}}}}`),
		"mycel_iam": []byte(`{"openapi":"3.0.3","paths":{"/m/mycel_iam/users/{id}/roles":{"post":{"operationId":"mycel_iam_user_roles"}}}}`),
	}
	served := map[string]map[string]string{
		"clc":       {"/api/workloads/web": "GET", "/api/history": "GET"},
		"mycel_iam": {"/users/42/roles": "POST"},
	}
	merged, err := modulesdk.FederateOpenAPI("app", "0.2", []modulesdk.OpenAPIModule{{Token: "clc", Document: documents["clc"]}, {Token: "mycel_iam", Document: documents["mycel_iam"]}})
	if err != nil {
		t.Fatal(err)
	}
	var federated struct {
		Paths map[string]map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal(merged, &federated); err != nil {
		t.Fatal(err)
	}
	handlers := map[string]*handler{}
	for token, routes := range served {
		routes := routes
		handlers[token] = &handler{d: modulesdk.Descriptor{Token: token, DescriptorSchemaMajor: 1, OpenAPI: documents[token], Route: func(r modulesdk.Request, _ modulesdk.Host) modulesdk.Response {
			if routes[r.Path] == r.Method {
				return modulesdk.Response{Status: 200}
			}
			return modulesdk.Response{Status: 404}
		}}, health: newHealthGate(nil, token)}
		if _, err := validateBoot(handlers[token].d); err != nil {
			t.Fatalf("%s: %v", token, err)
		}
	}
	samples := map[string]string{"{name}": "web", "{id}": "42"}
	operations := 0
	for path, item := range federated.Paths {
		for method := range item {
			concrete := path
			for template, value := range samples {
				concrete = strings.ReplaceAll(concrete, template, value)
			}
			// app_core's dispatch: /m/<token>/<rest> goes to <token> with /<rest>.
			rest := strings.TrimPrefix(concrete, "/m/")
			token, modulePath, _ := strings.Cut(rest, "/")
			h := handlers[token]
			if h == nil {
				t.Fatalf("%s %s resolves to no module", method, path)
			}
			response := mustHandle(t, h, &modulerpc.HandleRequest{Method: strings.ToUpper(method), Path: "/" + modulePath})
			if response.GetStatus() != 200 {
				t.Errorf("%s %s reached %s as /%s and got %d", method, path, token, modulePath, response.GetStatus())
			}
			operations++
		}
	}
	if operations != 3 {
		t.Fatalf("operations = %d", operations)
	}
}

// SDK-T14 (sdk.md S6): a typed module with an unprefixed OpenAPI path does not boot; a
// legacy module keeps booting.
func TestBootRefusesATypedModuleWithAnUnprefixedOpenAPIPath(t *testing.T) {
	d := modulesdk.Descriptor{Token: "clc", Route: okRoute, DescriptorSchemaMajor: 1, OpenAPI: []byte(`{"paths":{"/workloads":{}}}`)}
	if code := runBoot(t, d, testDependencies()); code == 0 {
		t.Error("a typed module with an unprefixed path booted")
	}
	d.DescriptorSchemaMajor = 0
	if _, err := validateBoot(d); err != nil {
		t.Errorf("legacy module refused: %v", err)
	}
}
