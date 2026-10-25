package modulesdk

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"testing"
)

const (
	cookieOne = "session=abc; Expires=Wed, 21 Oct 2026 07:28:00 GMT; Path=/; HttpOnly"
	cookieTwo = "theme=dark; Expires=Thu, 22 Oct 2026 07:28:00 GMT; Path=/"
)

func TestNormalizeHeaderFieldsMergesNamesInOrder(t *testing.T) {
	got, err := NormalizeHeaderFields([]HeaderField{
		{Name: "Set-Cookie", Values: []string{cookieOne}},
		{Name: "Accept", Values: []string{"text/html"}},
		{Name: "set-cookie", Values: []string{cookieTwo}},
		{Name: "Empty", Values: nil},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []HeaderField{
		{Name: "set-cookie", Values: []string{cookieOne, cookieTwo}},
		{Name: "accept", Values: []string{"text/html"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

// SDK-T12 (sdk.md S4): CR or LF in a value, and an invalid name, are refused.
func TestHeaderValidationRefusesCRLFAndBadNames(t *testing.T) {
	for name, field := range map[string]HeaderField{
		"carriage return":   {Name: "x-a", Values: []string{"one\rtwo"}},
		"line feed":         {Name: "x-a", Values: []string{"one\ntwo"}},
		"response split":    {Name: "set-cookie", Values: []string{"a=b\r\nlocation: https://evil.example"}},
		"null byte":         {Name: "x-a", Values: []string{"a\x00b"}},
		"empty name":        {Name: "", Values: []string{"x"}},
		"space in name":     {Name: "x a", Values: []string{"x"}},
		"colon in name":     {Name: "x:a", Values: []string{"x"}},
		"line feed in name": {Name: "x\na", Values: []string{"x"}},
	} {
		if _, err := NormalizeHeaderFields([]HeaderField{field}); !errors.Is(err, ErrInvalidHeader) {
			t.Errorf("%s: err = %v, want ErrInvalidHeader", name, err)
		}
	}
	if _, err := NormalizeHeaderFields([]HeaderField{{Name: "x-a", Values: []string{"tab\tis fine, commas too"}}}); err != nil {
		t.Errorf("valid value refused: %v", err)
	}
}

// SDK-T11 (sdk.md S4): the legacy newline-joined Set-Cookie form is accepted and
// translated to separate values; other names keep their single value.
func TestLegacyHeaderMapTranslation(t *testing.T) {
	fields := HeaderFieldsFromLegacyMap(map[string]string{
		"Set-Cookie":   cookieOne + "\n" + cookieTwo,
		"Content-Type": "text/html; charset=utf-8",
	})
	want := []HeaderField{
		{Name: "content-type", Values: []string{"text/html; charset=utf-8"}},
		{Name: "set-cookie", Values: []string{cookieOne, cookieTwo}},
	}
	if !reflect.DeepEqual(fields, want) {
		t.Fatalf("got %#v, want %#v", fields, want)
	}
	legacy := LegacyHeaderMap([]HeaderField{
		{Name: "set-cookie", Values: []string{cookieOne, cookieTwo}},
		{Name: "vary", Values: []string{"accept", "cookie"}},
		{Name: "cookie", Values: []string{"a=1", "b=2"}},
	})
	wantLegacy := map[string]string{"set-cookie": cookieOne + "\n" + cookieTwo, "vary": "accept, cookie", "cookie": "a=1; b=2"}
	if !reflect.DeepEqual(legacy, wantLegacy) {
		t.Fatalf("legacy = %#v, want %#v", legacy, wantLegacy)
	}
}

// SDK-T12 (sdk.md S4): host security headers and hop-by-hop headers from a module never
// reach the client; the host's policy, applied afterwards, is what the client sees.
func TestModuleCannotWeakenHostSecurityHeaders(t *testing.T) {
	destination := http.Header{}
	ApplyModuleHeaders(destination, []HeaderField{
		{Name: "content-security-policy", Values: []string{"default-src *"}},
		{Name: "x-frame-options", Values: []string{"ALLOWALL"}},
		{Name: "strict-transport-security", Values: []string{"max-age=0"}},
		{Name: "connection", Values: []string{"keep-alive, x-secret"}},
		{Name: "x-secret", Values: []string{"leak"}},
		{Name: "transfer-encoding", Values: []string{"chunked"}},
		{Name: "set-cookie", Values: []string{cookieOne, cookieTwo}},
		{Name: "content-type", Values: []string{"text/html"}},
	})
	for _, name := range append(HostSecurityHeaderNames(), "Connection", "Transfer-Encoding", "X-Secret") {
		if values := destination.Values(name); len(values) != 0 {
			t.Errorf("%s reached the client from the module: %q", name, values)
		}
	}
	if got := destination.Values("Set-Cookie"); !reflect.DeepEqual(got, []string{cookieOne, cookieTwo}) {
		t.Errorf("Set-Cookie = %q", got)
	}
	if got := destination.Get("Content-Type"); got != "text/html" {
		t.Errorf("Content-Type = %q", got)
	}
}

func TestHeaderBytes(t *testing.T) {
	if got := HeaderBytes([]HeaderField{{Name: "ab", Values: []string{"cd", "e"}}}); got != (2+2+4)+(2+1+4) {
		t.Fatalf("HeaderBytes = %d", got)
	}
}

// SDK-D4: a Request built without the server has a usable context.
func TestRequestContextDefaultsToBackground(t *testing.T) {
	var request Request
	if request.Context() == nil {
		t.Fatal("nil context")
	}
	type key struct{}
	ctx := context.WithValue(context.Background(), key{}, "v")
	carried := request.WithContext(ctx)
	if carried.Context().Value(key{}) != "v" {
		t.Fatal("WithContext lost the context")
	}
	if request.Context().Value(key{}) != nil {
		t.Fatal("WithContext changed the original request")
	}
}

// SDK-SR1-NS: the network-service class is accepted with the same path rules.
func TestNetworkServiceRouteClass(t *testing.T) {
	d := Descriptor{Token: "clc", DescriptorSchemaMajor: 1, RouteClasses: []RouteClassDeclaration{
		{Method: "POST", Path: "/m/clc/api/ingest", Class: RouteClassNetworkService},
	}}
	if err := ValidateDiscovery(d.Discovery()); err != nil {
		t.Fatalf("valid network-service route refused: %v", err)
	}
	d.RouteClasses[0].Path = "/api/system"
	if err := ValidateDiscovery(d.Discovery()); err == nil {
		t.Fatal("a reserved host route was accepted as a network-service route")
	}
	d.RouteClasses[0] = RouteClassDeclaration{Method: "GET", Path: "/m/clc/x", Class: RouteClassNetworkService + 1}
	if err := ValidateDiscovery(d.Discovery()); err == nil {
		t.Fatal("an unknown class was accepted")
	}
}
