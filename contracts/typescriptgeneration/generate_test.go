package typescriptgeneration

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	modulesdk "github.com/mycelpf/mycel_module_sdk"
	modulerpc "github.com/mycelpf/mycel_module_sdk/contracts/modulerpc/v1"
	"github.com/mycelpf/mycel_module_sdk/wire"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

const generatedPath = "../typescript/src/contract.ts"

// SDK-T3 (sdk.md S2): the committed TypeScript contract is exactly what the generator
// produces from the compiled .proto files. Run `go generate ./contracts/typescriptgeneration`
// after changing a .proto file.
func TestGeneratedTypeScriptMatchesTheContract(t *testing.T) {
	committed, err := os.ReadFile(generatedPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(committed, Generate()) {
		t.Fatalf("%s is stale; run: go generate ./contracts/typescriptgeneration", generatedPath)
	}
}

// Every message, field and enum value of the contract appears in the generated source.
func TestGeneratedTypeScriptCoversEveryField(t *testing.T) {
	source := string(Generate())
	c := collect()
	for _, message := range c.messages {
		for index := 0; index < message.Fields().Len(); index++ {
			field := message.Fields().Get(index)
			if !strings.Contains(source, "\t"+field.JSONName()+"?: ") {
				t.Errorf("%s.%s is missing", message.FullName(), field.JSONName())
			}
		}
	}
	for _, enum := range c.enums {
		for index := 0; index < enum.Values().Len(); index++ {
			if name := string(enum.Values().Get(index).Name()); !strings.Contains(source, `"`+name+`"`) {
				t.Errorf("%s is missing", name)
			}
		}
	}
}

// runValidator runs the TypeScript validator on input and returns its JSON answer.
func runValidator(t *testing.T, message string, input []byte) map[string]json.RawMessage {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not on PATH; the TypeScript half of SDK-T3 needs Node.js 22.6 or later")
	}
	index, err := filepath.Abs("../typescript/src/index.ts")
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(t.TempDir(), "run.ts")
	program := `import { readFileSync } from 'node:fs';
import { validate } from ` + strconvQuote("file://"+index) + `;
const result = validate(` + strconvQuote(message) + `, JSON.parse(readFileSync(0, 'utf8')));
process.stdout.write(JSON.stringify(result));
`
	if err := os.WriteFile(script, []byte(program), 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(node, "--experimental-strip-types", "--no-warnings", script)
	command.Stdin = bytes.NewReader(input)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, stderr.String())
	}
	var result map[string]json.RawMessage
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("node output %q: %v", output, err)
	}
	return result
}

func strconvQuote(text string) string {
	encoded, _ := json.Marshal(text)
	return string(encoded)
}

// fixture is a typed shell descriptor that uses every discovery field.
func fixture() modulesdk.Discovery {
	return modulesdk.Discovery{
		Token:           "clc",
		ContractVersion: "1.2.0",
		MenuGroup:       "Cluster",
		Nav: []modulesdk.NavEntry{
			{Label: "Overview", Path: "/"},
			{Label: "Workloads", Path: "/workloads", Group: "Runtime"},
		},
		Capabilities:          []string{"jobs"},
		LegacyUIEntry:         "/m/clc/assets/clc-app.4f2a9c.js",
		OpenAPI:               []byte(`{"openapi":"3.0.3","paths":{"/m/clc/api/jobs":{"get":{"operationId":"clc_jobs"}}}}`),
		DescriptorSchemaMajor: 1,
		UI: &modulesdk.UIDescriptor{
			Kind:             modulesdk.UIKindShell,
			ElementName:      "clc-app",
			EntryAsset:       "/m/clc/assets/clc-app.4f2a9c.js",
			ContractMajor:    1,
			MinContractMinor: 2,
			BrowserRoutes:    []string{"/", "/workloads/:name", "/history/*"},
			APIPrefix:        "/m/clc/api/",
			AssetPrefix:      "/m/clc/assets/",
			Slots:            []modulesdk.Slot{{ID: "jobs", SchemaMajor: 1, Title: "clc.jobs", ProviderPath: "/m/clc/api/jobs"}},
		},
		NeededMycelLicense: []string{"clc"},
		RouteClasses:       []modulesdk.RouteClassDeclaration{{Method: "POST", Path: "/m/clc/api/ingest", Class: modulesdk.RouteClassNetworkService}},
	}
}

// SDK-T3 (sdk.md S2): a descriptor survives pure DTO → protobuf → host JSON → TypeScript
// validation → back to protobuf and pure DTO without changing meaning.
func TestDescriptorSurvivesTheTypeScriptValidator(t *testing.T) {
	want := fixture()
	if err := modulesdk.ValidateDiscovery(want); err != nil {
		t.Fatal(err)
	}
	encoded, err := proto.Marshal(wire.DescribeResponseFromDiscovery(want))
	if err != nil {
		t.Fatal(err)
	}
	original := &modulerpc.DescribeResponse{}
	if err := proto.Unmarshal(encoded, original); err != nil {
		t.Fatal(err)
	}
	hostJSON, err := protojson.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	result := runValidator(t, "DescribeResponse", hostJSON)
	if string(result["ok"]) != "true" {
		t.Fatalf("validator refused the host JSON: %s", result["errors"])
	}
	back := &modulerpc.DescribeResponse{}
	if err := protojson.Unmarshal(result["value"], back); err != nil {
		t.Fatalf("validated JSON does not decode: %v\n%s", err, result["value"])
	}
	if !proto.Equal(original, back) {
		t.Fatalf("meaning changed:\nbefore %v\nafter  %v", original, back)
	}
	got, err := wire.DiscoveryFromDescribeResponse(back)
	if err != nil {
		t.Fatal(err)
	}
	if string(got.OpenAPI) != string(want.OpenAPI) || got.UI.EntryAsset != want.UI.EntryAsset || got.RouteClasses[0] != want.RouteClasses[0] || got.Nav[1].Group != "Runtime" {
		t.Fatalf("pure DTO changed: %+v", got)
	}

	// Response headers: two cookies with expiry commas survive as separate values.
	response := &modulerpc.HandleResponse{Status: 200, HeaderFields: []*modulerpc.HeaderField{{Name: "set-cookie", Values: []string{
		"a=1; Expires=Wed, 21 Oct 2026 07:28:00 GMT", "b=2; Expires=Thu, 22 Oct 2026 07:28:00 GMT",
	}}}, Body: []byte{0, 1, 2, 255}}
	responseJSON, err := protojson.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	result = runValidator(t, "HandleResponse", responseJSON)
	backResponse := &modulerpc.HandleResponse{}
	if err := protojson.Unmarshal(result["value"], backResponse); err != nil || !proto.Equal(response, backResponse) {
		t.Fatalf("response changed: %v %v", err, backResponse)
	}
}

// SDK-T3: the TypeScript validator refuses what protojson would not produce.
func TestTypeScriptValidatorRefusesWrongShapes(t *testing.T) {
	for name, input := range map[string]string{
		"unknown enum":        `{"ui":{"kind":"UI_KIND_FLOATING"}}`,
		"negative uint32":     `{"descriptorSchemaMajor":-1}`,
		"string for a list":   `{"capabilities":"jobs"}`,
		"number for a string": `{"token":7}`,
	} {
		result := runValidator(t, "DescribeResponse", []byte(input))
		if string(result["ok"]) != "false" {
			t.Errorf("%s: accepted", name)
		}
		// protojson agrees that each input is not a valid DescribeResponse.
		if err := protojson.Unmarshal([]byte(input), &modulerpc.DescribeResponse{}); err == nil {
			t.Errorf("%s: protojson accepted it", name)
		}
	}
}
