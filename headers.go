package modulesdk

import (
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
)

// HeaderField is one header name with all of its values in order (sdk.md S4). It is the
// lossless form: repeated request values and separate Set-Cookie values survive. The
// wire carries the same shape as HandleRequest/HandleResponse.header_fields.
type HeaderField struct {
	Name   string
	Values []string
}

// ErrInvalidHeader wraps every header refused by NormalizeHeaderFields: an invalid name,
// or CR, LF or NUL in a value (header injection).
var ErrInvalidHeader = errors.New("invalid header")

// ErrRequestTooLarge and ErrResponseTooLarge wrap budget failures (sdk.md S5). The first
// maps to HTTP 413 with ErrorCodeRequestTooLarge, the second to HTTP 502 with
// ErrorCodeModuleResponseTooLarge.
var (
	ErrRequestTooLarge  = errors.New("request too large")
	ErrResponseTooLarge = errors.New("module response too large")
)

// Message budgets (sdk.md S5). The transport ceiling is the gRPC limit at both ends; the
// body and header limits leave room for the protobuf envelope. A request above a limit is
// refused with HTTP 413 and ErrorCodeRequestTooLarge before the module runs; a module
// response above one becomes HTTP 502 with ErrorCodeModuleResponseTooLarge. Sizes are
// uncompressed bytes.
const (
	TransportCeilingBytes = 32 << 20
	MaximumBodyBytes      = 30 << 20
	MaximumHeaderBytes    = 64 << 10
)

// HeaderBytes is the size of a header set as counted against MaximumHeaderBytes: for each
// value, its name, the value and four bytes of separators.
func HeaderBytes(fields []HeaderField) int {
	total := 0
	for _, field := range fields {
		for _, value := range field.Values {
			total += len(field.Name) + len(value) + 4
		}
	}
	return total
}

// validHeaderName reports whether name is an RFC 9110 token.
func validHeaderName(name string) bool {
	if name == "" {
		return false
	}
	for index := 0; index < len(name); index++ {
		character := name[index]
		switch {
		case character >= 'a' && character <= 'z', character >= 'A' && character <= 'Z', character >= '0' && character <= '9':
		case strings.IndexByte("!#$%&'*+-.^_`|~", character) >= 0:
		default:
			return false
		}
	}
	return true
}

// NormalizeHeaderFields validates fields and returns them with lower-case names, the
// values of a repeated name merged in order under its first position, and names with no
// values dropped. It refuses an invalid name and CR, LF or NUL in any value.
func NormalizeHeaderFields(fields []HeaderField) ([]HeaderField, error) {
	var normalized []HeaderField
	position := map[string]int{}
	for _, field := range fields {
		if !validHeaderName(field.Name) {
			return nil, fmt.Errorf("%w: name %q", ErrInvalidHeader, field.Name)
		}
		name := strings.ToLower(field.Name)
		for _, value := range field.Values {
			if strings.ContainsAny(value, "\r\n\x00") {
				return nil, fmt.Errorf("%w: %s has CR, LF or NUL in a value", ErrInvalidHeader, name)
			}
		}
		if len(field.Values) == 0 {
			continue
		}
		if index, seen := position[name]; seen {
			normalized[index].Values = append(normalized[index].Values, field.Values...)
			continue
		}
		position[name] = len(normalized)
		normalized = append(normalized, HeaderField{Name: name, Values: append([]string(nil), field.Values...)})
	}
	return normalized, nil
}

// HeaderFieldsFromLegacyMap is the named compatibility adapter from the legacy
// single-value header map (sdk.md S4). Names are lower-cased and sorted. A Set-Cookie
// value is split on newlines, the form modules used before header fields existed; every
// other value stays one value. The result is not validated; pass it to
// NormalizeHeaderFields.
func HeaderFieldsFromLegacyMap(headers map[string]string) []HeaderField {
	names := make([]string, 0, len(headers))
	for name := range headers {
		names = append(names, name)
	}
	sort.Slice(names, func(left, right int) bool {
		return strings.ToLower(names[left]) < strings.ToLower(names[right])
	})
	var fields []HeaderField
	for _, name := range names {
		value := headers[name]
		lower := strings.ToLower(name)
		if lower == "set-cookie" {
			var cookies []string
			for _, cookie := range strings.Split(value, "\n") {
				if cookie = strings.TrimSuffix(cookie, "\r"); strings.TrimSpace(cookie) != "" {
					cookies = append(cookies, cookie)
				}
			}
			if len(cookies) > 0 {
				fields = append(fields, HeaderField{Name: lower, Values: cookies})
			}
			continue
		}
		fields = append(fields, HeaderField{Name: lower, Values: []string{value}})
	}
	return fields
}

// LegacyHeaderMap is the named compatibility adapter to the legacy single-value header
// map, for hosts and modules that predate header fields (sdk.md S4). Cookie values join
// with "; ", Set-Cookie values with a newline (the form app_core 0.2 splits), and every
// other repeated value with ", ". Names are lower-cased.
func LegacyHeaderMap(fields []HeaderField) map[string]string {
	if len(fields) == 0 {
		return nil
	}
	legacy := make(map[string]string, len(fields))
	for _, field := range fields {
		name := strings.ToLower(field.Name)
		separator := ", "
		switch name {
		case "cookie":
			separator = "; "
		case "set-cookie":
			separator = "\n"
		}
		values := field.Values
		if existing, present := legacy[name]; present {
			values = append([]string{existing}, values...)
		}
		legacy[name] = strings.Join(values, separator)
	}
	return legacy
}

// HostSecurityHeaderNames are the response headers app_core owns (sdk.md S4, UX A12). A
// module value for one of them never reaches the client.
func HostSecurityHeaderNames() []string {
	return []string{
		"Content-Security-Policy", "Content-Security-Policy-Report-Only", "Cross-Origin-Embedder-Policy",
		"Cross-Origin-Opener-Policy", "Cross-Origin-Resource-Policy", "Permissions-Policy",
		"Referrer-Policy", "Strict-Transport-Security", "X-Content-Type-Options", "X-Frame-Options",
	}
}

// hopByHopHeaderNames are connection-level headers that never cross the app_core
// boundary (RFC 9110 section 7.6.1).
var hopByHopHeaderNames = []string{
	"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Proxy-Connection",
	"Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

// RemoveHopByHopHeaders removes the hop-by-hop headers from header, including every name
// a Connection header lists. app_core calls it on inbound requests before dispatch.
func RemoveHopByHopHeaders(header http.Header) {
	for _, listed := range header.Values("Connection") {
		for _, name := range strings.Split(listed, ",") {
			if name = strings.TrimSpace(name); name != "" {
				header.Del(name)
			}
		}
	}
	for _, name := range hopByHopHeaderNames {
		header.Del(name)
	}
}

// ApplyModuleHeaders adds a module's response headers to destination, keeping every value
// (each Set-Cookie separately). It drops hop-by-hop headers, the names a module
// Connection header lists, and the host security headers, so the policy app_core applies
// afterwards is what the client sees (sdk.md S4). fields should be normalized.
func ApplyModuleHeaders(destination http.Header, fields []HeaderField) {
	dropped := map[string]bool{}
	for _, name := range append(HostSecurityHeaderNames(), hopByHopHeaderNames...) {
		dropped[http.CanonicalHeaderKey(name)] = true
	}
	for _, field := range fields {
		if http.CanonicalHeaderKey(field.Name) != "Connection" {
			continue
		}
		for _, listed := range field.Values {
			for _, name := range strings.Split(listed, ",") {
				if name = strings.TrimSpace(name); name != "" {
					dropped[http.CanonicalHeaderKey(name)] = true
				}
			}
		}
	}
	for _, field := range fields {
		name := http.CanonicalHeaderKey(field.Name)
		if dropped[name] {
			continue
		}
		for _, value := range field.Values {
			destination.Add(name, value)
		}
	}
}
