package modulesdk

import (
	"encoding/json"
	"testing"
)

// SDK-T18 (sdk.md S8), root half: the envelope carries a stable code, a safe message and
// the trace ID; the legacy {"error": ...} body still decodes.
func TestErrorResponseCarriesTheEnvelope(t *testing.T) {
	request := Request{Headers: map[string]string{"traceparent": "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"}}
	response := ErrorResponseFor(request, 404, ErrorCodeNotFound, "no such route")
	if response.Status != 404 || response.Headers["content-type"] != "application/json" {
		t.Fatalf("status %d headers %v", response.Status, response.Headers)
	}
	var envelope map[string]string
	if err := json.Unmarshal([]byte(response.Body), &envelope); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"code": "not_found", "message": "no such route", "traceId": "4bf92f3577b34da6a3ce929d0e0e4736"}
	for key, value := range want {
		if envelope[key] != value {
			t.Errorf("%s = %q, want %q", key, envelope[key], value)
		}
	}
}

func TestTraceIDFromHeadersRefusesMalformedValues(t *testing.T) {
	for _, value := range []string{"", "garbage", "00-00000000000000000000000000000000-00f067aa0ba902b7-01", "00-4BF92F3577B34DA6A3CE929D0E0E4736-00f067aa0ba902b7-01"} {
		if got := TraceIDFromHeaders(map[string]string{"traceparent": value}); got != "" {
			t.Errorf("traceparent %q gave trace ID %q", value, got)
		}
	}
}

func TestDecodeErrorEnvelope(t *testing.T) {
	envelope, err := DecodeErrorEnvelope([]byte(`{"code":"license_unavailable","message":"License unavailable","traceId":"abc","outcome":"absent"}`))
	if err != nil || envelope.Code != "license_unavailable" || envelope.Outcome != "absent" || envelope.TraceID != "abc" {
		t.Fatalf("envelope %+v err %v", envelope, err)
	}
	legacy, err := DecodeErrorEnvelope([]byte(`{"error":"module not serving"}`))
	if err != nil || legacy.Code != ErrorCodeLegacy || legacy.Message != "module not serving" {
		t.Fatalf("legacy %+v err %v", legacy, err)
	}
	if _, err := DecodeErrorEnvelope([]byte(`{"something":"else"}`)); err == nil {
		t.Error("a body that is neither envelope decoded without error")
	}
	if _, err := DecodeErrorEnvelope([]byte(`not json`)); err == nil {
		t.Error("a non-JSON body decoded without error")
	}
}
