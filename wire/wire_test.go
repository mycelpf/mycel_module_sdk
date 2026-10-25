package wire

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	modulesdk "github.com/mycelpf/mycel_module_sdk"
	modulerpc "github.com/mycelpf/mycel_module_sdk/contracts/modulerpc/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// fixture is a typed shell descriptor that uses every discovery field.
func fixture() modulesdk.Discovery {
	return modulesdk.Discovery{
		Token:           "clc",
		ContractVersion: "1.2.0",
		MenuGroup:       "Cluster",
		Nav: []modulesdk.NavEntry{
			{Label: "Overview", Path: "/"},
			{Label: "Workloads", Path: "/workloads", Group: "Runtime"},
			{Label: "History", Path: "/history"},
		},
		Capabilities:          []string{"jobs"},
		LegacyUIEntry:         "/m/clc/assets/clc-app.4f2a9c.js",
		OpenAPI:               []byte(`{"openapi":"3.0.3"}`),
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
	}
}

// throughHostJSON sends a DescribeResponse through protobuf bytes and the host's JSON
// representation and back.
func throughHostJSON(t *testing.T, response *modulerpc.DescribeResponse) *modulerpc.DescribeResponse {
	t.Helper()
	encoded, err := proto.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	decoded := &modulerpc.DescribeResponse{}
	if err := proto.Unmarshal(encoded, decoded); err != nil {
		t.Fatal(err)
	}
	json, err := protojson.Marshal(decoded)
	if err != nil {
		t.Fatal(err)
	}
	fromJSON := &modulerpc.DescribeResponse{}
	if err := protojson.Unmarshal(json, fromJSON); err != nil {
		t.Fatal(err)
	}
	return fromJSON
}

// SDK-T3 (sdk.md S2), Go half: a descriptor survives pure data, protobuf and host JSON
// without changing meaning.
func TestDiscoverySurvivesProtobufAndHostJSON(t *testing.T) {
	want := fixture()
	got, err := DiscoveryFromDescribeResponse(throughHostJSON(t, DescribeResponseFromDiscovery(want)))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip changed the descriptor:\n got %+v\nwant %+v", got, want)
	}
}

// SDK-T19 (sdk.md S11.1): a valid route-class declaration survives pure data, protobuf
// and host JSON.
func TestRouteClassesSurviveProtobufAndHostJSON(t *testing.T) {
	want := modulesdk.Discovery{
		Token:                 "mycel_iam",
		DescriptorSchemaMajor: 1,
		UI:                    &modulesdk.UIDescriptor{Kind: modulesdk.UIKindOwn, ContractMajor: 1},
		RouteClasses: []modulesdk.RouteClassDeclaration{
			{Method: "GET", Path: "/m/mycel_iam/signin", Class: modulesdk.RouteClassIAMSignIn},
			{Method: "GET", Path: "/m/mycel_iam/oauth/authorize", Class: modulesdk.RouteClassIAMAuthorization},
			{Method: "POST", Path: "/m/mycel_iam/oauth/token", Class: modulesdk.RouteClassIAMToken},
			{Method: "GET", Path: "/.well-known/jwks.json", Class: modulesdk.RouteClassIAMKeySet},
			{Method: "POST", Path: "/m/mycel_iam/directory/sync", Class: modulesdk.RouteClassNetworkService},
		},
	}
	response := throughHostJSON(t, DescribeResponseFromDiscovery(want))
	if got := response.GetRouteClasses()[3].GetClass(); got != modulerpc.RouteClass_ROUTE_CLASS_IAM_KEY_SET {
		t.Fatalf("wire class = %v", got)
	}
	if got := response.GetRouteClasses()[4].GetClass(); got != modulerpc.RouteClass_ROUTE_CLASS_NETWORK_SERVICE {
		t.Fatalf("wire class = %v", got)
	}
	got, err := DiscoveryFromDescribeResponse(response)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.RouteClasses, want.RouteClasses) {
		t.Fatalf("route classes = %+v, want %+v", got.RouteClasses, want.RouteClasses)
	}
}

// SDK-T21 (sdk.md S11.2): entries with and without group keep their declared order; an
// empty group present on the wire fails.
func TestNavigationGroupsOnTheWire(t *testing.T) {
	response := DescribeResponseFromDiscovery(fixture())
	if response.GetNav()[0].Group != nil || response.GetNav()[1].GetGroup() != "Runtime" || response.GetNav()[2].Group != nil {
		t.Fatalf("wire navigation = %v", response.GetNav())
	}
	empty := ""
	response.Nav[2].Group = &empty
	if _, err := DiscoveryFromDescribeResponse(response); !errors.Is(err, modulesdk.ErrInvalidDescriptor) || !strings.Contains(err.Error(), "group") {
		t.Fatalf("error = %v, want an invalid empty group", err)
	}
}

// SDK-T5 (sdk.md S2), wire half: a legacy response has schema 0 and resolves only through
// the legacy adapter; schema 1 without ui resolves to no UI.
func TestLegacyAndAPIOnlyResponses(t *testing.T) {
	legacy, err := DiscoveryFromDescribeResponse(&modulerpc.DescribeResponse{Token: "mycel_kpi", UiEntry: "/ui/kpi.js"})
	if err != nil {
		t.Fatal(err)
	}
	if resolution := modulesdk.ResolveUI(legacy); resolution.Kind != modulesdk.UIResolutionLegacy || resolution.LegacyEntry != "/ui/kpi.js" {
		t.Errorf("legacy resolution = %+v", resolution)
	}
	apiOnly, err := DiscoveryFromDescribeResponse(&modulerpc.DescribeResponse{Token: "mycel_kpi", DescriptorSchemaMajor: 1})
	if err != nil {
		t.Fatal(err)
	}
	if resolution := modulesdk.ResolveUI(apiOnly); resolution.Kind != modulesdk.UIResolutionNone {
		t.Errorf("API-only resolution = %+v", resolution)
	}
	if _, err := DiscoveryFromDescribeResponse(&modulerpc.DescribeResponse{Token: "x", DescriptorSchemaMajor: 7}); !errors.Is(err, modulesdk.ErrInvalidDescriptor) {
		t.Errorf("unknown schema major: error = %v", err)
	}
}

// SDK-T4 (sdk.md S2, S5), size half: discovery metadata above 256 KiB fails; the OpenAPI
// document does not count.
func TestOversizedDiscoveryMetadataFails(t *testing.T) {
	d := fixture()
	d.OpenAPI = []byte(strings.Repeat("x", 300<<10))
	if err := ValidateDescribeResponse(DescribeResponseFromDiscovery(d)); err != nil {
		t.Fatalf("a large OpenAPI document counted toward the metadata budget: %v", err)
	}
	for len(d.Nav) < 12000 {
		d.Nav = append(d.Nav, modulesdk.NavEntry{Label: strings.Repeat("l", 20), Path: "/p"})
	}
	if err := ValidateDescribeResponse(DescribeResponseFromDiscovery(d)); !errors.Is(err, modulesdk.ErrInvalidDescriptor) {
		t.Fatalf("error = %v, want oversized metadata to fail", err)
	}
}

func TestUnknownWireEnumsFailValidation(t *testing.T) {
	response := DescribeResponseFromDiscovery(fixture())
	response.Ui.Kind = modulerpc.UIKind(9)
	if _, err := DiscoveryFromDescribeResponse(response); !errors.Is(err, modulesdk.ErrInvalidDescriptor) {
		t.Errorf("unknown UI kind: error = %v", err)
	}
	response = DescribeResponseFromDiscovery(fixture())
	response.RouteClasses = []*modulerpc.RouteClassDeclaration{{Method: "GET", Path: "/m/clc/x", Class: modulerpc.RouteClass(9)}}
	if _, err := DiscoveryFromDescribeResponse(response); !errors.Is(err, modulesdk.ErrInvalidDescriptor) {
		t.Errorf("unknown route class: error = %v", err)
	}
}

func TestUserConversion(t *testing.T) {
	user := modulesdk.User{Sub: "s", Email: "e@example.com", Roles: []string{"a", "b"}, Tenant: "t"}
	if got := UserFromUserContext(UserContextFromUser(user)); !reflect.DeepEqual(got, user) {
		t.Fatalf("user = %+v, want %+v", got, user)
	}
	if UserContextFromUser(modulesdk.User{}) != nil {
		t.Error("the zero user must be absent on the wire")
	}
	if UserFromUserContext(nil).Present() {
		t.Error("an absent wire user gave a present user")
	}
}
