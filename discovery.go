package modulesdk

import (
	"errors"
	"fmt"
	"strings"
)

// Discovery schema majors (sdk.md S2). DescribeResponse.descriptor_schema_major carries one.
const (
	DescriptorSchemaLegacy uint32 = 0 // legacy descriptor; UI inferred only by the legacy adapter
	DescriptorSchemaTyped  uint32 = 1 // v0.2 typed declaration
)

// UIContractMajor is the UI element contract major this SDK supports (UX A10).
const UIContractMajor uint32 = 1

// MaximumDiscoveryBytes bounds the encoded discovery metadata (sdk.md S5): the
// DescribeResponse without its OpenAPI document.
const MaximumDiscoveryBytes = 256 << 10

// UIKind is the module's UI kind (UX "UI kind and runtime-mode matrix").
type UIKind int

// UI kinds; the numbers equal the wire enum UIKind.
const (
	UIKindUnspecified UIKind = 0
	UIKindShell       UIKind = 1 // mounted as <token>-app inside the host shell
	UIKindOwn         UIKind = 2 // the module owns / and every non-reserved page
)

// Slot is one optional shell slot provider (UX A6).
type Slot struct {
	ID           string // "chat" or "jobs"
	SchemaMajor  uint32 // 1
	Title        string // localisation key
	ProviderPath string // same-origin path under the UI's APIPrefix
}

// UIDescriptor is the typed UI declaration reported as Describe.ui (UX A10, A11).
type UIDescriptor struct {
	Kind             UIKind
	ElementName      string   // must be <token>-app; required for UIKindShell
	EntryAsset       string   // same-origin path under AssetPrefix; required for UIKindShell
	ContractMajor    uint32   // UIContractMajor
	MinContractMinor uint32   // the host's minor must be at least this
	BrowserRoutes    []string // patterns relative to /m/<token>: literals, :parameter, terminal *
	APIPrefix        string   // beneath /m/<token>/, ending in "/"
	AssetPrefix      string   // beneath /m/<token>/, ending in "/", disjoint from APIPrefix
	Slots            []Slot
}

// RouteClass marks a route for app_core's admission matrix (sdk.md S11.1). The numbers
// equal the wire enum RouteClass.
type RouteClass int

// Route classes.
const (
	RouteClassUnspecified      RouteClass = 0
	RouteClassIAMSignIn        RouteClass = 1 // sign-in initiation, callback and signed-out pages
	RouteClassIAMAuthorization RouteClass = 2 // /oauth/authorize
	RouteClassIAMToken         RouteClass = 3 // /oauth/token and /oauth/check
	RouteClassIAMKeySet        RouteClass = 4 // the published key set
	// RouteClassNetworkService is a provisioned network-service route (login route
	// admission matrix, core I9, T3): no service-caller authentication and no user
	// context; required products are still checked, and deployment configuration must
	// allowlist the route and isolate the network. Not for routes with per-user permissions.
	RouteClassNetworkService RouteClass = 5
)

// RouteClassDeclaration marks one exact route (sdk.md S11.1).
type RouteClassDeclaration struct {
	Method string // GET or POST
	Path   string // complete host-visible path: /m/<token>/..., or a bare path for a UIKindOwn module
	Class  RouteClass
}

// Discovery is the data a module reports through Describe, without its handlers and
// hooks. The server builds it from a Descriptor; app_core builds it from a
// DescribeResponse with the discovery package. Both validate it with ValidateDiscovery.
type Discovery struct {
	Token                 string
	ContractVersion       string
	MenuGroup             string
	Nav                   []NavEntry
	Capabilities          []string
	LegacyUIEntry         string // DescribeResponse.ui_entry
	OpenAPI               []byte
	DescriptorSchemaMajor uint32
	UI                    *UIDescriptor
	NeededMycelLicense    []string
	RouteClasses          []RouteClassDeclaration
}

// Discovery returns the descriptor's discovery data.
func (d Descriptor) Discovery() Discovery {
	return Discovery{
		Token:                 d.Token,
		ContractVersion:       d.ContractVersion,
		MenuGroup:             d.MenuGroup,
		Nav:                   d.Nav,
		Capabilities:          d.Capabilities,
		LegacyUIEntry:         d.UIEntry,
		OpenAPI:               d.OpenAPI,
		DescriptorSchemaMajor: d.DescriptorSchemaMajor,
		UI:                    d.UI,
		NeededMycelLicense:    d.NEEDED_MYCEL_LICENSE,
		RouteClasses:          d.RouteClasses,
	}
}

// UIResolutionKind says how a host must treat a module's UI (sdk.md S2, UX A10).
type UIResolutionKind int

// UI resolutions.
const (
	UIResolutionNone   UIResolutionKind = iota // schema 1 without ui: no module UI, navigation fallback or entry asset
	UIResolutionTyped                          // schema 1 with ui: use Typed
	UIResolutionLegacy                         // schema 0: only the named legacy adapter may infer a UI from LegacyEntry
)

// UIResolution is the result of ResolveUI.
type UIResolution struct {
	Kind        UIResolutionKind
	Typed       *UIDescriptor
	LegacyEntry string
}

// ResolveUI is the one place that tells a typed API-only module from a legacy module.
// Call it only on a Discovery that passed ValidateDiscovery.
func ResolveUI(d Discovery) UIResolution {
	if d.DescriptorSchemaMajor == DescriptorSchemaLegacy {
		return UIResolution{Kind: UIResolutionLegacy, LegacyEntry: d.LegacyUIEntry}
	}
	if d.UI == nil {
		return UIResolution{Kind: UIResolutionNone}
	}
	return UIResolution{Kind: UIResolutionTyped, Typed: d.UI}
}

// ErrInvalidDescriptor wraps every validation failure of ValidateDiscovery. A license
// failure also wraps the license error (ErrUnknownProduct and the others).
var ErrInvalidDescriptor = errors.New("invalid module descriptor")

func invalid(format string, arguments ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidDescriptor, fmt.Sprintf(format, arguments...))
}

// ReservedHostRoutes are app_core routes no module may declare (sdk.md S11.1). A path
// ending in "/" reserves everything beneath it as well as the path without the slash.
func ReservedHostRoutes() []string {
	return []string{"/livez", "/healthz", "/auth/callback", "/api/system", "/api/license", "/_mycel/ui/"}
}

// ValidateDiscovery checks a module's discovery data (sdk.md S2, S6, S11.1, S11.2;
// license §5.2). A typed (schema 1) descriptor's OpenAPI document must pass
// ValidateOpenAPI; a legacy one is not checked here. The server runs it once at boot, before any retry loop, and a failure is a
// permanent boot error. app_core runs it again on what Describe returned.
func ValidateDiscovery(d Discovery) error {
	if !validToken(d.Token) {
		return invalid("token %q must match [a-z][a-z0-9_]*", d.Token)
	}
	switch d.DescriptorSchemaMajor {
	case DescriptorSchemaLegacy:
		if d.UI != nil {
			return invalid("a typed UI needs descriptor schema major %d", DescriptorSchemaTyped)
		}
	case DescriptorSchemaTyped:
	default:
		return invalid("unsupported descriptor schema major %d", d.DescriptorSchemaMajor)
	}
	for index, entry := range d.Nav {
		if entry.Group != "" && strings.TrimSpace(entry.Group) == "" {
			return invalid("navigation entry %d (%q): group is only spaces", index, entry.Label)
		}
	}
	seen := map[string]bool{}
	for _, capability := range d.Capabilities {
		if strings.TrimSpace(capability) == "" {
			return invalid("empty capability")
		}
		if seen[capability] {
			return invalid("duplicate capability %q", capability)
		}
		seen[capability] = true
	}
	if err := ValidateNeededMycelLicense(d.NeededMycelLicense); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidDescriptor, err)
	}
	if d.UI != nil {
		if err := validateUI(d.Token, d.LegacyUIEntry, d.UI); err != nil {
			return err
		}
	}
	if d.DescriptorSchemaMajor == DescriptorSchemaTyped && len(d.OpenAPI) > 0 {
		if err := ValidateOpenAPI(d.Token, d.OpenAPI); err != nil {
			return fmt.Errorf("%w: %w", ErrInvalidDescriptor, err)
		}
	}
	return validateRouteClasses(d)
}

func validToken(token string) bool {
	if token == "" || token[0] < 'a' || token[0] > 'z' {
		return false
	}
	for _, character := range token {
		if !(character >= 'a' && character <= 'z' || character >= '0' && character <= '9' || character == '_') {
			return false
		}
	}
	return true
}

// safePath reports whether p is an absolute, same-origin path with no scheme, host,
// query, fragment, backslash, control character, space, empty segment or dot segment.
func safePath(p string) bool {
	if !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") {
		return false
	}
	for _, character := range p {
		if character <= ' ' || character == 0x7f || strings.ContainsRune(`?#\%`, character) {
			return false
		}
	}
	segments := strings.Split(strings.TrimSuffix(p[1:], "/"), "/")
	for index, segment := range segments {
		if segment == "." || segment == ".." || (segment == "" && !(index == 0 && len(segments) == 1)) {
			return false
		}
	}
	return true
}

// validPrefix reports whether prefix is a safe path strictly beneath /m/<token>/ that
// ends in "/".
func validPrefix(token, prefix string) bool {
	root := "/m/" + token + "/"
	return safePath(prefix) && strings.HasSuffix(prefix, "/") && strings.HasPrefix(prefix, root) && len(prefix) > len(root)
}

func validateUI(token, legacyEntry string, ui *UIDescriptor) error {
	switch ui.Kind {
	case UIKindShell, UIKindOwn:
	default:
		return invalid("unknown UI kind %d", ui.Kind)
	}
	if ui.ContractMajor != UIContractMajor {
		return invalid("unsupported UI contract major %d (supported: %d)", ui.ContractMajor, UIContractMajor)
	}
	element := token + "-app"
	if ui.ElementName != "" && ui.ElementName != element {
		return invalid("element name %q must be %q", ui.ElementName, element)
	}
	if ui.Kind == UIKindShell {
		if ui.ElementName == "" {
			return invalid("a shell UI must name its element %q", element)
		}
		if ui.EntryAsset == "" || ui.APIPrefix == "" || ui.AssetPrefix == "" {
			return invalid("a shell UI needs an entry asset, an API prefix and an asset prefix")
		}
	}
	if ui.APIPrefix != "" && !validPrefix(token, ui.APIPrefix) {
		return invalid("API prefix %q must be a path beneath /m/%s/ ending in /", ui.APIPrefix, token)
	}
	if ui.AssetPrefix != "" && !validPrefix(token, ui.AssetPrefix) {
		return invalid("asset prefix %q must be a path beneath /m/%s/ ending in /", ui.AssetPrefix, token)
	}
	if ui.APIPrefix != "" && ui.AssetPrefix != "" &&
		(strings.HasPrefix(ui.APIPrefix, ui.AssetPrefix) || strings.HasPrefix(ui.AssetPrefix, ui.APIPrefix)) {
		return invalid("API prefix %q and asset prefix %q overlap", ui.APIPrefix, ui.AssetPrefix)
	}
	if ui.EntryAsset != "" {
		if ui.AssetPrefix == "" || !safePath(ui.EntryAsset) || !strings.HasPrefix(ui.EntryAsset, ui.AssetPrefix) || strings.HasSuffix(ui.EntryAsset, "/") {
			return invalid("entry asset %q must be a same-origin file path under the asset prefix %q", ui.EntryAsset, ui.AssetPrefix)
		}
	}
	if legacyEntry != "" && legacyEntry != ui.EntryAsset {
		return invalid("legacy ui_entry %q disagrees with the typed entry asset %q", legacyEntry, ui.EntryAsset)
	}
	if err := validateBrowserRoutes(token, ui); err != nil {
		return err
	}
	return validateSlots(ui)
}

// routeSegments splits a browser-route pattern; "/" has no segments.
func routeSegments(pattern string) []string {
	trimmed := strings.Trim(pattern, "/")
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "/")
}

func validParameterName(name string) bool {
	if name == "" || !(name[0] >= 'a' && name[0] <= 'z' || name[0] >= 'A' && name[0] <= 'Z') {
		return false
	}
	for _, character := range name {
		if !(character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || character == '_') {
			return false
		}
	}
	return true
}

func validLiteralSegment(segment string) bool {
	if segment == "" || segment == "." || segment == ".." {
		return false
	}
	for _, character := range segment {
		if !(character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || strings.ContainsRune("-._~", character)) {
			return false
		}
	}
	return true
}

// patternReaches reports whether a browser-route pattern can match some path at or
// beneath the module-relative prefix segments.
func patternReaches(pattern, prefix []string) bool {
	for index, segment := range prefix {
		if index >= len(pattern) {
			return false
		}
		switch {
		case pattern[index] == "*":
			return true
		case strings.HasPrefix(pattern[index], ":"):
		case pattern[index] != segment:
			return false
		}
	}
	return true
}

func validateBrowserRoutes(token string, ui *UIDescriptor) error {
	root := "/m/" + token
	var reserved [][]string
	for _, prefix := range []string{ui.APIPrefix, ui.AssetPrefix} {
		if prefix != "" {
			reserved = append(reserved, routeSegments(strings.TrimPrefix(prefix, root)))
		}
	}
	shapes := map[string]string{}
	for _, pattern := range ui.BrowserRoutes {
		if !strings.HasPrefix(pattern, "/") || strings.HasPrefix(pattern, "//") || (pattern != "/" && strings.HasSuffix(pattern, "/")) {
			return invalid("browser route %q must start with one / and not end with /", pattern)
		}
		segments := routeSegments(pattern)
		shape := make([]string, len(segments))
		for index, segment := range segments {
			switch {
			case segment == "*":
				if index != len(segments)-1 {
					return invalid("browser route %q: * must be the last segment", pattern)
				}
				shape[index] = "*"
			case strings.HasPrefix(segment, ":"):
				if !validParameterName(segment[1:]) {
					return invalid("browser route %q: invalid parameter %q", pattern, segment)
				}
				shape[index] = ":"
			default:
				if !validLiteralSegment(segment) {
					return invalid("browser route %q: invalid segment %q", pattern, segment)
				}
				shape[index] = segment
			}
		}
		key := "/" + strings.Join(shape, "/")
		if other, duplicate := shapes[key]; duplicate {
			return invalid("browser routes %q and %q match the same paths", other, pattern)
		}
		shapes[key] = pattern
		for _, prefix := range reserved {
			if patternReaches(segments, prefix) {
				return invalid("browser route %q can match the API or asset prefix", pattern)
			}
		}
	}
	return nil
}

func validateSlots(ui *UIDescriptor) error {
	seen := map[string]bool{}
	for _, slot := range ui.Slots {
		if slot.ID != "chat" && slot.ID != "jobs" {
			return invalid("unknown slot %q (known: chat, jobs)", slot.ID)
		}
		if seen[slot.ID] {
			return invalid("duplicate slot %q", slot.ID)
		}
		seen[slot.ID] = true
		if slot.SchemaMajor != 1 {
			return invalid("slot %q: unsupported schema major %d", slot.ID, slot.SchemaMajor)
		}
		if strings.TrimSpace(slot.Title) == "" {
			return invalid("slot %q has no title", slot.ID)
		}
		if ui.APIPrefix == "" || !safePath(slot.ProviderPath) || !strings.HasPrefix(slot.ProviderPath, ui.APIPrefix) {
			return invalid("slot %q: provider path %q must be under the API prefix %q", slot.ID, slot.ProviderPath, ui.APIPrefix)
		}
	}
	return nil
}

func reservedHostRoute(path string) bool {
	for _, reserved := range ReservedHostRoutes() {
		if path == reserved || (strings.HasSuffix(reserved, "/") && (strings.HasPrefix(path, reserved) || path == strings.TrimSuffix(reserved, "/"))) {
			return true
		}
	}
	return false
}

func validateRouteClasses(d Discovery) error {
	own := d.UI != nil && d.UI.Kind == UIKindOwn
	moduleRoot := "/m/" + d.Token + "/"
	seen := map[string]bool{}
	for _, declaration := range d.RouteClasses {
		if declaration.Class < RouteClassIAMSignIn || declaration.Class > RouteClassNetworkService {
			return invalid("route %s %s: unknown or unspecified route class %d", declaration.Method, declaration.Path, declaration.Class)
		}
		if declaration.Method != "GET" && declaration.Method != "POST" {
			return invalid("route %s %s: method must be GET or POST", declaration.Method, declaration.Path)
		}
		if !safePath(declaration.Path) || strings.ContainsAny(declaration.Path, ":*") {
			return invalid("route %s %q: path must be an exact same-origin path", declaration.Method, declaration.Path)
		}
		if reservedHostRoute(declaration.Path) {
			return invalid("route %s %s: reserved host route", declaration.Method, declaration.Path)
		}
		switch {
		case strings.HasPrefix(declaration.Path, moduleRoot):
		case declaration.Path == "/m" || strings.HasPrefix(declaration.Path, "/m/"):
			return invalid("route %s %s: path is outside this module's prefix %s", declaration.Method, declaration.Path, moduleRoot)
		case !own:
			return invalid("route %s %s: a bare path is allowed only for a module with UI kind own", declaration.Method, declaration.Path)
		}
		key := declaration.Method + " " + declaration.Path
		if seen[key] {
			return invalid("duplicate route class declaration %s", key)
		}
		seen[key] = true
	}
	return nil
}
