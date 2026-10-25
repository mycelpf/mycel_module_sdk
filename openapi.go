package modulesdk

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// ErrInvalidOpenAPI wraps every failure of ValidateOpenAPI and FederateOpenAPI.
var ErrInvalidOpenAPI = errors.New("invalid module OpenAPI document")

func invalidOpenAPI(format string, arguments ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidOpenAPI, fmt.Sprintf(format, arguments...))
}

// openAPIOperationMethods are the operation keys of an OpenAPI 3 path item.
var openAPIOperationMethods = []string{"get", "put", "post", "delete", "options", "head", "patch", "trace"}

type openAPIDocument struct {
	Paths      map[string]map[string]json.RawMessage `json:"paths"`
	Components map[string]map[string]json.RawMessage `json:"components"`
}

// ValidateOpenAPI checks a module's OpenAPI document against the one path convention of
// sdk.md S6: every path is a complete host-visible path, /m/<token> or beneath it, so the
// documented URL is the URL app_core dispatches (prefix stripped before Handle). A path
// template parameter takes a whole segment. Operation IDs are unique.
func ValidateOpenAPI(token string, document []byte) error {
	_, err := parseOpenAPI(token, document)
	return err
}

func parseOpenAPI(token string, document []byte) (openAPIDocument, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(document, &raw); err != nil {
		return openAPIDocument{}, invalidOpenAPI("module %s: not a JSON object: %v", token, err)
	}
	var parsed openAPIDocument
	if err := json.Unmarshal(document, &parsed); err != nil {
		return openAPIDocument{}, invalidOpenAPI("module %s: paths and components must be objects: %v", token, err)
	}
	root := "/m/" + token
	operationIDs := map[string]string{}
	for _, path := range sortedKeys(parsed.Paths) {
		if path != root && !strings.HasPrefix(path, root+"/") {
			if strings.HasPrefix(path, "/m/") {
				return openAPIDocument{}, invalidOpenAPI("module %s: path %q is under another module's prefix", token, path)
			}
			return openAPIDocument{}, invalidOpenAPI("module %s: path %q must be a complete host path under %s/", token, path, root)
		}
		if !validOpenAPIPath(path) {
			return openAPIDocument{}, invalidOpenAPI("module %s: path %q is not a safe path (a template takes a whole segment)", token, path)
		}
		item := parsed.Paths[path]
		for _, method := range openAPIOperationMethods {
			operation, present := item[method]
			if !present {
				continue
			}
			var fields struct {
				OperationID string `json:"operationId"`
			}
			if err := json.Unmarshal(operation, &fields); err != nil {
				return openAPIDocument{}, invalidOpenAPI("module %s: %s %s is not an operation object", token, method, path)
			}
			if fields.OperationID == "" {
				continue
			}
			if other, duplicate := operationIDs[fields.OperationID]; duplicate {
				return openAPIDocument{}, invalidOpenAPI("module %s: operation ID %q is used by %s and %s %s", token, fields.OperationID, other, method, path)
			}
			operationIDs[fields.OperationID] = method + " " + path
		}
	}
	return parsed, nil
}

// validOpenAPIPath accepts literal segments and whole-segment {name} templates, with no
// query, fragment, backslash, control character, empty or dot segment.
func validOpenAPIPath(path string) bool {
	for _, segment := range strings.Split(path[1:], "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
		if strings.HasPrefix(segment, "{") && strings.HasSuffix(segment, "}") {
			if !validParameterName(segment[1 : len(segment)-1]) {
				return false
			}
			continue
		}
		for _, character := range segment {
			if character <= ' ' || character == 0x7f || strings.ContainsRune(`?#\%{}`, character) {
				return false
			}
		}
	}
	return true
}

// OpenAPIModule is one module's OpenAPI document for FederateOpenAPI.
type OpenAPIModule struct {
	Token    string
	Document []byte // empty: the module ships no document and is skipped
}

// FederateOpenAPI merges module documents into one OpenAPI 3.0 document (sdk.md S6).
// Each document must pass ValidateOpenAPI for its own token. Paths are merged verbatim; a
// path, operation ID or component name (in any components section) that two modules
// declare fails, as does the same token twice.
func FederateOpenAPI(title, version string, modules []OpenAPIModule) ([]byte, error) {
	paths := map[string]map[string]json.RawMessage{}
	components := map[string]map[string]json.RawMessage{}
	componentOwner := map[string]string{}
	operationOwner := map[string]string{}
	tokens := map[string]bool{}
	for _, module := range modules {
		if tokens[module.Token] {
			return nil, invalidOpenAPI("module %s appears twice", module.Token)
		}
		tokens[module.Token] = true
		if len(module.Document) == 0 {
			continue
		}
		parsed, err := parseOpenAPI(module.Token, module.Document)
		if err != nil {
			return nil, err
		}
		for _, path := range sortedKeys(parsed.Paths) {
			if _, duplicate := paths[path]; duplicate {
				return nil, invalidOpenAPI("path %q is declared by two modules", path)
			}
			paths[path] = parsed.Paths[path]
			for _, method := range openAPIOperationMethods {
				var fields struct {
					OperationID string `json:"operationId"`
				}
				if operation, present := parsed.Paths[path][method]; present {
					_ = json.Unmarshal(operation, &fields)
				}
				if fields.OperationID == "" {
					continue
				}
				if owner, duplicate := operationOwner[fields.OperationID]; duplicate {
					return nil, invalidOpenAPI("operation ID %q is declared by modules %s and %s", fields.OperationID, owner, module.Token)
				}
				operationOwner[fields.OperationID] = module.Token
			}
		}
		for _, section := range sortedKeys(parsed.Components) {
			if components[section] == nil {
				components[section] = map[string]json.RawMessage{}
			}
			for _, name := range sortedKeys(parsed.Components[section]) {
				key := section + "/" + name
				if owner, duplicate := componentOwner[key]; duplicate {
					return nil, invalidOpenAPI("component %s %q is declared by modules %s and %s", section, name, owner, module.Token)
				}
				componentOwner[key] = module.Token
				components[section][name] = parsed.Components[section][name]
			}
		}
	}
	document := map[string]any{
		"openapi": "3.0.3",
		"info":    map[string]string{"title": title, "version": version},
		"paths":   paths,
	}
	if len(components) > 0 {
		document["components"] = components
	}
	return json.Marshal(document)
}

func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
