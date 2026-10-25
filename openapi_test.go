package modulesdk

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func openAPIFixture(token string, paths map[string]string, schemas ...string) []byte {
	document := map[string]any{"openapi": "3.0.3", "info": map[string]any{"title": token, "version": "1"}}
	pathItems := map[string]any{}
	for path, operationID := range paths {
		pathItems[path] = map[string]any{"get": map[string]any{"operationId": operationID, "responses": map[string]any{"200": map[string]any{"description": "OK"}}}}
	}
	document["paths"] = pathItems
	if len(schemas) > 0 {
		schemaItems := map[string]any{}
		for _, name := range schemas {
			schemaItems[name] = map[string]any{"type": "object"}
		}
		document["components"] = map[string]any{"schemas": schemaItems}
	}
	encoded, _ := json.Marshal(document)
	return encoded
}

// SDK-T14 (sdk.md S6): paths are complete host-visible paths under the module's own prefix.
func TestValidateOpenAPI(t *testing.T) {
	valid := openAPIFixture("clc", map[string]string{"/m/clc/api/workloads/{name}": "clc_workload", "/m/clc": "clc_root"})
	if err := ValidateOpenAPI("clc", valid); err != nil {
		t.Fatalf("valid document refused: %v", err)
	}
	for name, document := range map[string][]byte{
		"unprefixed path":        openAPIFixture("clc", map[string]string{"/api/workloads": "clc_a"}),
		"foreign module prefix":  openAPIFixture("clc", map[string]string{"/m/mycel_iam/users": "clc_a"}),
		"prefix without slash":   openAPIFixture("clc", map[string]string{"/m/clcx/api": "clc_a"}),
		"templated prefix":       openAPIFixture("clc", map[string]string{"/m/{token}/api": "clc_a"}),
		"dot segment":            openAPIFixture("clc", map[string]string{"/m/clc/../mycel_iam/users": "clc_a"}),
		"partial template":       openAPIFixture("clc", map[string]string{"/m/clc/a{b}": "clc_a"}),
		"query in path":          openAPIFixture("clc", map[string]string{"/m/clc/a?b=1": "clc_a"}),
		"duplicate operation id": openAPIFixture("clc", map[string]string{"/m/clc/a": "clc_same", "/m/clc/b": "clc_same"}),
		"not JSON":               []byte("openapi: 3"),
		"no paths object":        []byte(`{"openapi":"3.0.3","paths":[]}`),
	} {
		if err := ValidateOpenAPI("clc", document); !errors.Is(err, ErrInvalidOpenAPI) {
			t.Errorf("%s: err = %v, want ErrInvalidOpenAPI", name, err)
		}
	}
}

// SDK-T14 (sdk.md S6): merging rejects a duplicate operation ID or component name across
// modules, and keeps every path verbatim.
func TestFederateOpenAPIRefusesDuplicates(t *testing.T) {
	clc := OpenAPIModule{Token: "clc", Document: openAPIFixture("clc", map[string]string{"/m/clc/api/workloads/{name}": "clc_workload"}, "clc_error")}
	iam := OpenAPIModule{Token: "mycel_iam", Document: openAPIFixture("mycel_iam", map[string]string{"/m/mycel_iam/users": "mycel_iam_users"}, "mycel_iam_user")}
	merged, err := FederateOpenAPI("app", "0.2", []OpenAPIModule{clc, iam, {Token: "empty"}})
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Paths      map[string]json.RawMessage            `json:"paths"`
		Components map[string]map[string]json.RawMessage `json:"components"`
	}
	if err := json.Unmarshal(merged, &document); err != nil {
		t.Fatal(err)
	}
	if len(document.Paths) != 2 || document.Paths["/m/clc/api/workloads/{name}"] == nil || document.Paths["/m/mycel_iam/users"] == nil {
		t.Fatalf("paths = %v", document.Paths)
	}
	if len(document.Components["schemas"]) != 2 {
		t.Fatalf("schemas = %v", document.Components["schemas"])
	}

	duplicateOperation := OpenAPIModule{Token: "other", Document: openAPIFixture("other", map[string]string{"/m/other/x": "clc_workload"})}
	if _, err := FederateOpenAPI("app", "0.2", []OpenAPIModule{clc, duplicateOperation}); !errors.Is(err, ErrInvalidOpenAPI) || !strings.Contains(err.Error(), "clc_workload") {
		t.Errorf("duplicate operation ID: err = %v", err)
	}
	duplicateSchema := OpenAPIModule{Token: "other", Document: openAPIFixture("other", map[string]string{"/m/other/x": "other_x"}, "clc_error")}
	if _, err := FederateOpenAPI("app", "0.2", []OpenAPIModule{clc, duplicateSchema}); !errors.Is(err, ErrInvalidOpenAPI) || !strings.Contains(err.Error(), "clc_error") {
		t.Errorf("duplicate schema: err = %v", err)
	}
	foreign := OpenAPIModule{Token: "other", Document: openAPIFixture("other", map[string]string{"/m/clc/api/x": "other_x"})}
	if _, err := FederateOpenAPI("app", "0.2", []OpenAPIModule{clc, foreign}); !errors.Is(err, ErrInvalidOpenAPI) {
		t.Errorf("foreign prefix: err = %v", err)
	}
	if _, err := FederateOpenAPI("app", "0.2", []OpenAPIModule{clc, clc}); !errors.Is(err, ErrInvalidOpenAPI) {
		t.Errorf("same module twice: err = %v", err)
	}
}

// SDK-T14: a typed descriptor with an unprefixed OpenAPI path fails validation; a legacy
// descriptor keeps its current behaviour (the server logs a warning instead).
func TestTypedDescriptorValidatesOpenAPI(t *testing.T) {
	d := Descriptor{Token: "clc", DescriptorSchemaMajor: 1, OpenAPI: openAPIFixture("clc", map[string]string{"/workloads": "clc_w"})}
	if err := ValidateDiscovery(d.Discovery()); !errors.Is(err, ErrInvalidDescriptor) || !errors.Is(err, ErrInvalidOpenAPI) {
		t.Fatalf("typed: err = %v", err)
	}
	d.DescriptorSchemaMajor = 0
	if err := ValidateDiscovery(d.Discovery()); err != nil {
		t.Fatalf("legacy: err = %v", err)
	}
}
