package wire

import (
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"

	modulesdk "github.com/mycelpf/mycel_module_sdk"
	modulerpc "github.com/mycelpf/mycel_module_sdk/contracts/modulerpc/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

const (
	cookieOne = "session=abc; Expires=Wed, 21 Oct 2026 07:28:00 GMT; Path=/; HttpOnly"
	cookieTwo = "theme=dark; Expires=Thu, 22 Oct 2026 07:28:00 GMT; Path=/"
)

// SDK-T11 (sdk.md S4): repeated values and two Set-Cookie values with expiry commas survive
// protobuf and host JSON; header fields are authoritative over the legacy map.
func TestHeaderFieldsRoundTripLossless(t *testing.T) {
	fields := []modulesdk.HeaderField{
		{Name: "set-cookie", Values: []string{cookieOne, cookieTwo}},
		{Name: "vary", Values: []string{"accept", "cookie"}},
		{Name: "content-type", Values: []string{"text/html; charset=utf-8"}},
	}
	response := &modulerpc.HandleResponse{Status: 200, HeaderFields: HeaderFieldsToWire(fields), Headers: map[string]string{"x-stale": "ignored"}}
	encoded, err := proto.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	decoded := &modulerpc.HandleResponse{}
	if err := proto.Unmarshal(encoded, decoded); err != nil {
		t.Fatal(err)
	}
	json, err := protojson.Marshal(decoded)
	if err != nil {
		t.Fatal(err)
	}
	fromJSON := &modulerpc.HandleResponse{}
	if err := protojson.Unmarshal(json, fromJSON); err != nil {
		t.Fatal(err)
	}
	got, err := HeaderFieldsFromHandleResponse(fromJSON)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, fields) {
		t.Fatalf("got %#v, want %#v", got, fields)
	}

	request := &modulerpc.HandleRequest{HeaderFields: HeaderFieldsToWire([]modulesdk.HeaderField{{Name: "Accept", Values: []string{"text/html", "application/json"}}})}
	gotRequest, err := HeaderFieldsFromHandleRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	if want := []modulesdk.HeaderField{{Name: "accept", Values: []string{"text/html", "application/json"}}}; !reflect.DeepEqual(gotRequest, want) {
		t.Fatalf("request fields = %#v", gotRequest)
	}
}

// SDK-T11 (sdk.md S4, migration): a module or host that still sends only the legacy map,
// with Set-Cookie values joined by a newline, is translated to separate values.
func TestLegacyNewlineSetCookieIsTranslated(t *testing.T) {
	response := &modulerpc.HandleResponse{Status: 200, Headers: map[string]string{"Set-Cookie": cookieOne + "\n" + cookieTwo, "content-type": "text/plain"}}
	got, err := HeaderFieldsFromHandleResponse(response)
	if err != nil {
		t.Fatal(err)
	}
	want := []modulesdk.HeaderField{{Name: "content-type", Values: []string{"text/plain"}}, {Name: "set-cookie", Values: []string{cookieOne, cookieTwo}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v", got)
	}
	request := &modulerpc.HandleRequest{Headers: map[string]string{"Accept": "text/html, application/json"}}
	gotRequest, err := HeaderFieldsFromHandleRequest(request)
	if err != nil || !reflect.DeepEqual(gotRequest, []modulesdk.HeaderField{{Name: "accept", Values: []string{"text/html, application/json"}}}) {
		t.Fatalf("legacy request: %#v, %v", gotRequest, err)
	}
}

// SDK-T12 (sdk.md S4): CR/LF on either wire form is refused.
func TestWireHeadersRefuseCRLF(t *testing.T) {
	for name, response := range map[string]*modulerpc.HandleResponse{
		"fields": {HeaderFields: []*modulerpc.HeaderField{{Name: "location", Values: []string{"/a\r\nset-cookie: x=1"}}}},
		"legacy": {Headers: map[string]string{"location": "/a\r\nx: y"}},
	} {
		if _, err := HeaderFieldsFromHandleResponse(response); !errors.Is(err, modulesdk.ErrInvalidHeader) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

// app_core's side: inbound HTTP headers become both wire forms, without hop-by-hop headers.
func TestHandleRequestHeadersFromHTTP(t *testing.T) {
	header := http.Header{}
	header.Add("Accept", "text/html")
	header.Add("Accept", "application/json")
	header.Add("Cookie", "a=1")
	header.Add("Cookie", "b=2")
	header.Add("Connection", "keep-alive, X-Private")
	header.Add("X-Private", "secret")
	header.Add("Upgrade", "h2c")
	fields, legacy, err := HandleRequestHeaders(header)
	if err != nil {
		t.Fatal(err)
	}
	got, err := HeaderFieldsFromHandleRequest(&modulerpc.HandleRequest{HeaderFields: fields})
	if err != nil {
		t.Fatal(err)
	}
	want := []modulesdk.HeaderField{{Name: "accept", Values: []string{"text/html", "application/json"}}, {Name: "cookie", Values: []string{"a=1", "b=2"}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("fields = %#v", got)
	}
	if !reflect.DeepEqual(legacy, map[string]string{"accept": "text/html, application/json", "cookie": "a=1; b=2"}) {
		t.Fatalf("legacy = %#v", legacy)
	}
	if header.Get("X-Private") == "" {
		t.Fatal("HandleRequestHeaders changed its argument")
	}
}

func handleRequest(bodyBytes, headerBytes int) *modulerpc.HandleRequest {
	request := &modulerpc.HandleRequest{Method: "POST", Path: "/x", Body: make([]byte, bodyBytes)}
	if headerBytes > 0 {
		// One value: name (3) + value + 4 separator bytes.
		request.HeaderFields = []*modulerpc.HeaderField{{Name: "x-a", Values: []string{strings.Repeat("v", headerBytes-7)}}}
	}
	return request
}

// SDK-T13 (sdk.md S5): just below, at and above the body and header limits; envelope
// overhead at both limits stays under the transport ceiling.
func TestRequestBudgetBoundaries(t *testing.T) {
	for name, testCase := range map[string]struct {
		request *modulerpc.HandleRequest
		refused bool
	}{
		"body below":            {handleRequest(modulesdk.MaximumBodyBytes-1, 0), false},
		"body at":               {handleRequest(modulesdk.MaximumBodyBytes, 0), false},
		"body above":            {handleRequest(modulesdk.MaximumBodyBytes+1, 0), true},
		"headers below":         {handleRequest(0, modulesdk.MaximumHeaderBytes-1), false},
		"headers at":            {handleRequest(0, modulesdk.MaximumHeaderBytes), false},
		"headers above":         {handleRequest(0, modulesdk.MaximumHeaderBytes+1), true},
		"both at with envelope": {handleRequest(modulesdk.MaximumBodyBytes, modulesdk.MaximumHeaderBytes), false},
		"legacy headers above":  {&modulerpc.HandleRequest{Headers: map[string]string{"x-a": strings.Repeat("v", modulesdk.MaximumHeaderBytes)}}, true},
	} {
		err := CheckHandleRequestBudget(testCase.request)
		if testCase.refused != errors.Is(err, modulesdk.ErrRequestTooLarge) {
			t.Errorf("%s: err = %v, refused want %v", name, err, testCase.refused)
		}
	}
	// Repetitive data compresses well; the budget counts uncompressed bytes.
	repetitive := &modulerpc.HandleRequest{Body: []byte(strings.Repeat("a", modulesdk.MaximumBodyBytes+1))}
	if err := CheckHandleRequestBudget(repetitive); !errors.Is(err, modulesdk.ErrRequestTooLarge) {
		t.Errorf("compressible body: err = %v", err)
	}
}

func TestResponseBudgetBoundaries(t *testing.T) {
	for name, testCase := range map[string]struct {
		size    int
		refused bool
	}{"below": {modulesdk.MaximumBodyBytes - 1, false}, "at": {modulesdk.MaximumBodyBytes, false}, "above": {modulesdk.MaximumBodyBytes + 1, true}} {
		err := CheckHandleResponseBudget(&modulerpc.HandleResponse{Status: 200, Body: make([]byte, testCase.size)})
		if testCase.refused != errors.Is(err, modulesdk.ErrResponseTooLarge) {
			t.Errorf("body %s: err = %v", name, err)
		}
	}
	headers := &modulerpc.HandleResponse{HeaderFields: []*modulerpc.HeaderField{{Name: "x-a", Values: []string{strings.Repeat("v", modulesdk.MaximumHeaderBytes)}}}}
	if err := CheckHandleResponseBudget(headers); !errors.Is(err, modulesdk.ErrResponseTooLarge) {
		t.Errorf("headers above: err = %v", err)
	}
	// The encoded message is checked too: bytes the budget fields do not see (here unknown
	// fields) still count against the transport ceiling.
	envelope := &modulerpc.HandleResponse{Body: make([]byte, modulesdk.MaximumBodyBytes)}
	unknown := protowireBytesField(99, modulesdk.TransportCeilingBytes-modulesdk.MaximumBodyBytes)
	envelope.ProtoReflect().SetUnknown(unknown)
	if err := CheckHandleResponseBudget(envelope); !errors.Is(err, modulesdk.ErrResponseTooLarge) {
		t.Errorf("envelope above the ceiling: err = %v", err)
	}
}

// protowireBytesField encodes an unknown length-delimited field of size bytes.
func protowireBytesField(number protowire.Number, size int) []byte {
	encoded := protowire.AppendTag(nil, number, protowire.BytesType)
	return protowire.AppendBytes(encoded, make([]byte, size))
}
