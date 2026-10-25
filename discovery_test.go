package modulesdk

import (
	"errors"
	"strings"
	"testing"
)

// validShellUI is a typed shell declaration for the clc module that passes validation.
func validShellUI() *UIDescriptor {
	return &UIDescriptor{
		Kind:             UIKindShell,
		ElementName:      "clc-app",
		EntryAsset:       "/m/clc/assets/clc-app.4f2a9c.js",
		ContractMajor:    1,
		MinContractMinor: 0,
		BrowserRoutes:    []string{"/", "/workloads/:name", "/history/*"},
		APIPrefix:        "/m/clc/api/",
		AssetPrefix:      "/m/clc/assets/",
		Slots:            []Slot{{ID: "jobs", SchemaMajor: 1, Title: "clc.jobs.title", ProviderPath: "/m/clc/api/jobs"}},
	}
}

func typedDescriptor(ui *UIDescriptor) Descriptor {
	return Descriptor{Token: "clc", DescriptorSchemaMajor: 1, UI: ui}
}

func TestValidDescriptorsPass(t *testing.T) {
	for name, d := range map[string]Descriptor{
		"legacy":         {Token: "mycel_kpi", MenuGroup: "Ops", Nav: []NavEntry{{Label: "KPIs", Path: "/"}}, UIEntry: "/ui/kpi.js"},
		"typed shell":    typedDescriptor(validShellUI()),
		"typed API only": typedDescriptor(nil),
	} {
		if err := ValidateDiscovery(d.Discovery()); err != nil {
			t.Errorf("%s: unexpected error: %v", name, err)
		}
	}
}

// SDK-T4 (sdk.md S2): invalid routes, slots, prefixes and schema majors fail; a typed
// descriptor with a conflicting ui_entry fails. (Oversized metadata is checked on the
// encoded message by the discovery package.)
func TestInvalidTypedDescriptorsFail(t *testing.T) {
	cases := map[string]func(d *Descriptor){
		"unknown schema major":        func(d *Descriptor) { d.DescriptorSchemaMajor = 2 },
		"typed UI under legacy":       func(d *Descriptor) { d.DescriptorSchemaMajor = 0 },
		"element name mismatch":       func(d *Descriptor) { d.UI.ElementName = "other-app" },
		"unsupported contract major":  func(d *Descriptor) { d.UI.ContractMajor = 2 },
		"unspecified kind":            func(d *Descriptor) { d.UI.Kind = UIKindUnspecified },
		"API prefix outside module":   func(d *Descriptor) { d.UI.APIPrefix = "/m/other/api/" },
		"asset prefix not beneath":    func(d *Descriptor) { d.UI.AssetPrefix = "/m/clc/" },
		"prefix with dot segment":     func(d *Descriptor) { d.UI.APIPrefix = "/m/clc/../iam/" },
		"prefix without slash":        func(d *Descriptor) { d.UI.APIPrefix = "/m/clc/api" },
		"overlapping prefixes":        func(d *Descriptor) { d.UI.AssetPrefix = "/m/clc/api/assets/" },
		"entry outside asset prefix":  func(d *Descriptor) { d.UI.EntryAsset = "/m/clc/api/app.js" },
		"entry is a remote URL":       func(d *Descriptor) { d.UI.EntryAsset = "https://cdn.example.com/app.js" },
		"route not rooted":            func(d *Descriptor) { d.UI.BrowserRoutes = []string{"workloads"} },
		"route wildcard not terminal": func(d *Descriptor) { d.UI.BrowserRoutes = []string{"/a/*/b"} },
		"route bad parameter":         func(d *Descriptor) { d.UI.BrowserRoutes = []string{"/a/:"} },
		"route duplicate shape":       func(d *Descriptor) { d.UI.BrowserRoutes = []string{"/w/:name", "/w/:id"} },
		"route matches API prefix":    func(d *Descriptor) { d.UI.BrowserRoutes = []string{"/api/*"} },
		"parameter covers API prefix": func(d *Descriptor) { d.UI.BrowserRoutes = []string{"/:section/x"} },
		"slot unknown ID":             func(d *Descriptor) { d.UI.Slots[0].ID = "weather" },
		"slot duplicate ID": func(d *Descriptor) {
			d.UI.Slots = append(d.UI.Slots, Slot{ID: "jobs", SchemaMajor: 1, Title: "t", ProviderPath: "/m/clc/api/jobs2"})
		},
		"slot unsupported schema":    func(d *Descriptor) { d.UI.Slots[0].SchemaMajor = 2 },
		"slot outside API prefix":    func(d *Descriptor) { d.UI.Slots[0].ProviderPath = "/m/clc/assets/jobs" },
		"slot without title":         func(d *Descriptor) { d.UI.Slots[0].Title = " " },
		"conflicting legacy entry":   func(d *Descriptor) { d.UIEntry = "/m/clc/assets/old.js" },
		"empty capability":           func(d *Descriptor) { d.Capabilities = []string{""} },
		"duplicate capability":       func(d *Descriptor) { d.Capabilities = []string{"a", "a"} },
		"invalid token":              func(d *Descriptor) { d.Token = "Clc" },
		"shell without entry asset":  func(d *Descriptor) { d.UI.EntryAsset = "" },
		"shell without element name": func(d *Descriptor) { d.UI.ElementName = "" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			d := typedDescriptor(validShellUI())
			mutate(&d)
			err := ValidateDiscovery(d.Discovery())
			if !errors.Is(err, ErrInvalidDescriptor) {
				t.Fatalf("error = %v, want ErrInvalidDescriptor", err)
			}
		})
	}
}

func TestTypedDescriptorWithAgreeingLegacyEntryPasses(t *testing.T) {
	d := typedDescriptor(validShellUI())
	d.UIEntry = d.UI.EntryAsset
	if err := ValidateDiscovery(d.Discovery()); err != nil {
		t.Fatal(err)
	}
}

// SDK-T5 (sdk.md S2): schema 0 resolves only through the legacy adapter; schema 1 with no
// UI means no module UI.
func TestResolveUI(t *testing.T) {
	legacy := ResolveUI(Descriptor{Token: "kpi", UIEntry: "/ui/kpi.js"}.Discovery())
	if legacy.Kind != UIResolutionLegacy || legacy.LegacyEntry != "/ui/kpi.js" || legacy.Typed != nil {
		t.Errorf("legacy resolution = %+v", legacy)
	}
	none := ResolveUI(typedDescriptor(nil).Discovery())
	if none.Kind != UIResolutionNone || none.Typed != nil || none.LegacyEntry != "" {
		t.Errorf("typed API-only resolution = %+v", none)
	}
	typed := ResolveUI(typedDescriptor(validShellUI()).Discovery())
	if typed.Kind != UIResolutionTyped || typed.Typed == nil || typed.Typed.ElementName != "clc-app" {
		t.Errorf("typed resolution = %+v", typed)
	}
}

func ownIAMDescriptor(classes ...RouteClassDeclaration) Descriptor {
	return Descriptor{
		Token:                 "mycel_iam",
		DescriptorSchemaMajor: 1,
		UI:                    &UIDescriptor{Kind: UIKindOwn, ContractMajor: 1},
		RouteClasses:          classes,
	}
}

func TestValidRouteClassesPass(t *testing.T) {
	d := ownIAMDescriptor(
		RouteClassDeclaration{Method: "GET", Path: "/m/mycel_iam/signin", Class: RouteClassIAMSignIn},
		RouteClassDeclaration{Method: "GET", Path: "/m/mycel_iam/oauth/authorize", Class: RouteClassIAMAuthorization},
		RouteClassDeclaration{Method: "POST", Path: "/m/mycel_iam/oauth/token", Class: RouteClassIAMToken},
		RouteClassDeclaration{Method: "GET", Path: "/.well-known/jwks.json", Class: RouteClassIAMKeySet},
	)
	if err := ValidateDiscovery(d.Discovery()); err != nil {
		t.Fatal(err)
	}
}

// SDK-T20 (sdk.md S11.1), validation half: each invalid declaration fails.
func TestInvalidRouteClassesFail(t *testing.T) {
	signin := RouteClassDeclaration{Method: "GET", Path: "/m/mycel_iam/signin", Class: RouteClassIAMSignIn}
	cases := map[string]Descriptor{
		"unknown class":     ownIAMDescriptor(RouteClassDeclaration{Method: "GET", Path: "/m/mycel_iam/x", Class: RouteClass(9)}),
		"unspecified class": ownIAMDescriptor(RouteClassDeclaration{Method: "GET", Path: "/m/mycel_iam/x", Class: RouteClassUnspecified}),
		"other method":      ownIAMDescriptor(RouteClassDeclaration{Method: "PUT", Path: "/m/mycel_iam/x", Class: RouteClassIAMToken}),
		"lower-case method": ownIAMDescriptor(RouteClassDeclaration{Method: "get", Path: "/m/mycel_iam/x", Class: RouteClassIAMToken}),
		"foreign prefix":    ownIAMDescriptor(RouteClassDeclaration{Method: "GET", Path: "/m/clc/signin", Class: RouteClassIAMSignIn}),
		"duplicate":         ownIAMDescriptor(signin, signin),
		"reserved livez":    ownIAMDescriptor(RouteClassDeclaration{Method: "GET", Path: "/livez", Class: RouteClassIAMSignIn}),
		"reserved healthz":  ownIAMDescriptor(RouteClassDeclaration{Method: "GET", Path: "/healthz", Class: RouteClassIAMSignIn}),
		"reserved callback": ownIAMDescriptor(RouteClassDeclaration{Method: "GET", Path: "/auth/callback", Class: RouteClassIAMSignIn}),
		"reserved system":   ownIAMDescriptor(RouteClassDeclaration{Method: "GET", Path: "/api/system", Class: RouteClassIAMSignIn}),
		"reserved license":  ownIAMDescriptor(RouteClassDeclaration{Method: "GET", Path: "/api/license", Class: RouteClassIAMSignIn}),
		"reserved kit":      ownIAMDescriptor(RouteClassDeclaration{Method: "GET", Path: "/_mycel/ui/context", Class: RouteClassIAMSignIn}),
		"pattern path":      ownIAMDescriptor(RouteClassDeclaration{Method: "GET", Path: "/m/mycel_iam/:id", Class: RouteClassIAMSignIn}),
		"query in path":     ownIAMDescriptor(RouteClassDeclaration{Method: "GET", Path: "/m/mycel_iam/x?y=1", Class: RouteClassIAMSignIn}),
		"bare path from shell module": func() Descriptor {
			d := typedDescriptor(validShellUI())
			d.RouteClasses = []RouteClassDeclaration{{Method: "GET", Path: "/.well-known/jwks.json", Class: RouteClassIAMKeySet}}
			return d
		}(),
		"bare path from legacy module": {
			Token:        "mycel_iam",
			RouteClasses: []RouteClassDeclaration{{Method: "GET", Path: "/.well-known/jwks.json", Class: RouteClassIAMKeySet}},
		},
	}
	for name, d := range cases {
		t.Run(name, func(t *testing.T) {
			if err := ValidateDiscovery(d.Discovery()); !errors.Is(err, ErrInvalidDescriptor) {
				t.Fatalf("error = %v, want ErrInvalidDescriptor", err)
			}
		})
	}
}

// SDK-T21 (sdk.md S11.2), validation half: a group that is only spaces fails.
func TestNavigationGroupValidation(t *testing.T) {
	good := Descriptor{Token: "clc", MenuGroup: "Cluster", Nav: []NavEntry{
		{Label: "Overview", Path: "/"},
		{Label: "Workloads", Path: "/workloads", Group: "Runtime"},
	}}
	if err := ValidateDiscovery(good.Discovery()); err != nil {
		t.Fatal(err)
	}
	bad := Descriptor{Token: "clc", Nav: []NavEntry{{Label: "Workloads", Path: "/workloads", Group: "   "}}}
	err := ValidateDiscovery(bad.Discovery())
	if !errors.Is(err, ErrInvalidDescriptor) || !strings.Contains(err.Error(), "group") {
		t.Fatalf("error = %v, want an invalid navigation group", err)
	}
}
