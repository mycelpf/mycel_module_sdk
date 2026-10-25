package wire

import (
	"fmt"
	"net/http"
	"sort"

	modulesdk "github.com/mycelpf/mycel_module_sdk"
	modulerpc "github.com/mycelpf/mycel_module_sdk/contracts/modulerpc/v1"
	"google.golang.org/protobuf/proto"
)

// HeaderFieldsToWire encodes header fields for HandleRequest/HandleResponse.header_fields.
func HeaderFieldsToWire(fields []modulesdk.HeaderField) []*modulerpc.HeaderField {
	if len(fields) == 0 {
		return nil
	}
	encoded := make([]*modulerpc.HeaderField, 0, len(fields))
	for _, field := range fields {
		encoded = append(encoded, &modulerpc.HeaderField{Name: field.Name, Values: append([]string(nil), field.Values...)})
	}
	return encoded
}

func headerFieldsFromWire(fields []*modulerpc.HeaderField) []modulesdk.HeaderField {
	decoded := make([]modulesdk.HeaderField, 0, len(fields))
	for _, field := range fields {
		decoded = append(decoded, modulesdk.HeaderField{Name: field.GetName(), Values: field.GetValues()})
	}
	return decoded
}

// decodeHeaders reads the authoritative header fields when present and otherwise the
// legacy map through the named compatibility adapter, then normalizes and validates.
func decodeHeaders(fields []*modulerpc.HeaderField, legacy map[string]string) ([]modulesdk.HeaderField, error) {
	if len(fields) > 0 {
		return modulesdk.NormalizeHeaderFields(headerFieldsFromWire(fields))
	}
	return modulesdk.NormalizeHeaderFields(modulesdk.HeaderFieldsFromLegacyMap(legacy))
}

// HeaderFieldsFromHandleRequest returns a request's headers (sdk.md S4): header_fields
// when present, else the legacy map. It fails with modulesdk.ErrInvalidHeader on an
// invalid name or CR, LF or NUL in a value.
func HeaderFieldsFromHandleRequest(request *modulerpc.HandleRequest) ([]modulesdk.HeaderField, error) {
	return decodeHeaders(request.GetHeaderFields(), request.GetHeaders())
}

// HeaderFieldsFromHandleResponse returns a module response's headers (sdk.md S4):
// header_fields when present, else the legacy map, whose newline-joined Set-Cookie value
// becomes separate cookies. It fails with modulesdk.ErrInvalidHeader on an invalid name or
// CR, LF or NUL in a value. app_core then calls modulesdk.ApplyModuleHeaders and applies
// its own security policy.
func HeaderFieldsFromHandleResponse(response *modulerpc.HandleResponse) ([]modulesdk.HeaderField, error) {
	return decodeHeaders(response.GetHeaderFields(), response.GetHeaders())
}

// HandleRequestHeaders converts app_core's inbound HTTP headers into both wire forms: the
// authoritative header fields and the legacy map for modules built before them. Hop-by-hop
// headers and the names a Connection header lists are removed; header is not changed.
func HandleRequestHeaders(header http.Header) ([]*modulerpc.HeaderField, map[string]string, error) {
	cleaned := header.Clone()
	modulesdk.RemoveHopByHopHeaders(cleaned)
	names := make([]string, 0, len(cleaned))
	for name := range cleaned {
		names = append(names, name)
	}
	sort.Strings(names)
	fields := make([]modulesdk.HeaderField, 0, len(names))
	for _, name := range names {
		fields = append(fields, modulesdk.HeaderField{Name: name, Values: cleaned[name]})
	}
	normalized, err := modulesdk.NormalizeHeaderFields(fields)
	if err != nil {
		return nil, nil, err
	}
	return HeaderFieldsToWire(normalized), modulesdk.LegacyHeaderMap(normalized), nil
}

// wireHeaderBytes counts the authoritative headers of a message: header_fields when
// present, else the legacy map.
func wireHeaderBytes(fields []*modulerpc.HeaderField, legacy map[string]string) int {
	if len(fields) > 0 {
		return modulesdk.HeaderBytes(headerFieldsFromWire(fields))
	}
	total := 0
	for name, value := range legacy {
		total += len(name) + len(value) + 4
	}
	return total
}

func checkBudget(kind error, message proto.Message, bodyBytes, headerBytes int) error {
	switch {
	case bodyBytes > modulesdk.MaximumBodyBytes:
		return fmt.Errorf("%w: body is %d bytes; the limit is %d", kind, bodyBytes, modulesdk.MaximumBodyBytes)
	case headerBytes > modulesdk.MaximumHeaderBytes:
		return fmt.Errorf("%w: headers are %d bytes; the limit is %d", kind, headerBytes, modulesdk.MaximumHeaderBytes)
	}
	if size := proto.Size(message); size > modulesdk.TransportCeilingBytes {
		return fmt.Errorf("%w: encoded message is %d bytes; the ceiling is %d", kind, size, modulesdk.TransportCeilingBytes)
	}
	return nil
}

// CheckHandleRequestBudget checks a request against the sdk.md S5 budgets: body, headers
// and the encoded message. app_core calls it before dispatch and answers HTTP 413 with
// modulesdk.ErrorCodeRequestTooLarge; the SDK server checks again before the module runs.
func CheckHandleRequestBudget(request *modulerpc.HandleRequest) error {
	return checkBudget(modulesdk.ErrRequestTooLarge, request, len(request.GetBody()),
		wireHeaderBytes(request.GetHeaderFields(), request.GetHeaders()))
}

// CheckHandleResponseBudget checks a module response against the sdk.md S5 budgets. A
// failure becomes HTTP 502 with modulesdk.ErrorCodeModuleResponseTooLarge; it is never
// retried.
func CheckHandleResponseBudget(response *modulerpc.HandleResponse) error {
	return checkBudget(modulesdk.ErrResponseTooLarge, response, len(response.GetBody()),
		wireHeaderBytes(response.GetHeaderFields(), response.GetHeaders()))
}
