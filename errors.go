package modulesdk

import (
	"encoding/json"
	"errors"
	"strings"
)

// ErrorEnvelopeVersion is the version of the shared error body (sdk.md S8). Version 1 is
// {code, message, traceId?, outcome?}. A field is added only in a backward-compatible way;
// any other change is a new version.
const ErrorEnvelopeVersion = 1

// ErrorEnvelope is the shared error body every module and app_core return (sdk.md S8).
// Code is stable and machine-readable; Message is safe to show to the caller and never
// holds credentials, internal paths or raw collaborator output; TraceID correlates the
// failure with logs. Outcome is set only by app_core's license gate (license §5.11).
type ErrorEnvelope struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	TraceID string `json:"traceId,omitempty"`
	Outcome string `json:"outcome,omitempty"`
}

// Stable error codes the SDK itself produces.
const (
	ErrorCodeVerificationPending = "verification_pending" // startup verification has not passed
	ErrorCodeInternal            = "internal_error"       // a route panicked or failed unexpectedly
	ErrorCodeCommitFailed        = "commit_failed"        // the request's transaction did not commit
	ErrorCodeNotFound            = "not_found"            // no route owns the path
	ErrorCodeLegacy              = "legacy_error"         // decoded from a pre-envelope {"error": ...} body

	ErrorCodeInvalidHeader          = "invalid_header"            // a header name is invalid or a value holds CR, LF or NUL (sdk.md S4)
	ErrorCodeRequestTooLarge        = "request_too_large"         // HTTP 413: the request is above a budget (sdk.md S5)
	ErrorCodeModuleResponseTooLarge = "module_response_too_large" // HTTP 502: the module's response is above a budget (sdk.md S5)
)

// ErrorResponse builds a Response carrying the error envelope as JSON.
func ErrorResponse(status uint16, code, message, traceID string) Response {
	body, _ := json.Marshal(ErrorEnvelope{Code: code, Message: message, TraceID: traceID})
	return Response{Status: status, Headers: map[string]string{"content-type": "application/json"}, Body: string(body)}
}

// ErrorResponseFor builds an error Response for request, taking the trace ID from the
// request's W3C traceparent header, which the server adapter injects.
func ErrorResponseFor(request Request, status uint16, code, message string) Response {
	return ErrorResponse(status, code, message, TraceIDFromHeaders(request.Headers))
}

// TraceIDFromHeaders returns the trace ID of a valid W3C traceparent header, or "" when
// the header is missing or malformed. A wrong trace ID is worse than none.
func TraceIDFromHeaders(headers map[string]string) string {
	parts := strings.Split(headers["traceparent"], "-")
	if len(parts) != 4 || len(parts[1]) != 32 {
		return ""
	}
	traceID := parts[1]
	allZero := true
	for _, character := range traceID {
		if !(character >= '0' && character <= '9' || character >= 'a' && character <= 'f') {
			return ""
		}
		if character != '0' {
			allZero = false
		}
	}
	if allZero {
		return ""
	}
	return traceID
}

// ErrNotAnErrorEnvelope is returned by DecodeErrorEnvelope for a body that is neither
// the envelope nor the legacy error body.
var ErrNotAnErrorEnvelope = errors.New("body is not an error envelope")

// DecodeErrorEnvelope reads an error body. It accepts the envelope and, during the
// transition, the legacy {"error": "..."} body, which it returns with Code ErrorCodeLegacy.
func DecodeErrorEnvelope(body []byte) (ErrorEnvelope, error) {
	var decoded struct {
		ErrorEnvelope
		Legacy *string `json:"error"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		return ErrorEnvelope{}, errors.Join(ErrNotAnErrorEnvelope, err)
	}
	if decoded.Code != "" {
		return decoded.ErrorEnvelope, nil
	}
	if decoded.Legacy != nil {
		return ErrorEnvelope{Code: ErrorCodeLegacy, Message: *decoded.Legacy}, nil
	}
	return ErrorEnvelope{}, ErrNotAnErrorEnvelope
}
