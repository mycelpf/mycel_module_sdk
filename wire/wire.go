// Package wire maps between the SDK's plain data types (package modulesdk) and the
// protobuf contract (contracts/modulerpc/v1). The root package stays free of protobuf;
// these explicit adapters are the only place the two meet (sdk.md S2).
//
// The module side (package server) uses DescribeResponseFromDiscovery and
// UserFromUserContext. The host side (app_core) uses DiscoveryFromDescribeResponse, which
// also validates, and UserContextFromUser.
package wire

import (
	"fmt"
	"strings"

	modulesdk "github.com/mycelpf/mycel_module_sdk"
	modulerpc "github.com/mycelpf/mycel_module_sdk/contracts/modulerpc/v1"
	"google.golang.org/protobuf/proto"
)

// DescribeResponseFromDiscovery encodes a module's discovery data for Describe.
func DescribeResponseFromDiscovery(d modulesdk.Discovery) *modulerpc.DescribeResponse {
	response := &modulerpc.DescribeResponse{
		Token:                 d.Token,
		ContractVersion:       d.ContractVersion,
		MenuGroup:             d.MenuGroup,
		Capabilities:          nonEmpty(d.Capabilities),
		UiEntry:               d.LegacyUIEntry,
		Openapi:               d.OpenAPI,
		DescriptorSchemaMajor: d.DescriptorSchemaMajor,
		NeededMycelLicense:    nonEmpty(d.NeededMycelLicense),
	}
	for _, entry := range d.Nav {
		wireEntry := &modulerpc.NavEntry{Label: entry.Label, Path: entry.Path}
		if entry.Group != "" {
			group := entry.Group
			wireEntry.Group = &group
		}
		response.Nav = append(response.Nav, wireEntry)
	}
	if d.UI != nil {
		ui := &modulerpc.UIDescriptor{
			Kind:             modulerpc.UIKind(d.UI.Kind),
			ElementName:      d.UI.ElementName,
			EntryAsset:       d.UI.EntryAsset,
			ContractMajor:    d.UI.ContractMajor,
			MinContractMinor: d.UI.MinContractMinor,
			BrowserRoutes:    nonEmpty(d.UI.BrowserRoutes),
			ApiPrefix:        d.UI.APIPrefix,
			AssetPrefix:      d.UI.AssetPrefix,
		}
		for _, slot := range d.UI.Slots {
			ui.Slots = append(ui.Slots, &modulerpc.SlotDescriptor{
				Id: slot.ID, SchemaMajor: slot.SchemaMajor, Title: slot.Title, ProviderPath: slot.ProviderPath,
			})
		}
		response.Ui = ui
	}
	for _, declaration := range d.RouteClasses {
		response.RouteClasses = append(response.RouteClasses, &modulerpc.RouteClassDeclaration{
			Method: declaration.Method, Path: declaration.Path, Class: modulerpc.RouteClass(declaration.Class),
		})
	}
	return response
}

// DiscoveryFromDescribeResponse decodes and validates what a module returned from
// Describe: ValidateDescribeResponse, then modulesdk.ValidateDiscovery. app_core calls
// this once per module at boot; an error is a compatibility failure.
func DiscoveryFromDescribeResponse(response *modulerpc.DescribeResponse) (modulesdk.Discovery, error) {
	if err := ValidateDescribeResponse(response); err != nil {
		return modulesdk.Discovery{}, err
	}
	d := decode(response)
	if err := modulesdk.ValidateDiscovery(d); err != nil {
		return modulesdk.Discovery{}, err
	}
	return d, nil
}

// ValidateDescribeResponse checks what only the wire form shows: the encoded size of the
// discovery metadata (sdk.md S5; the OpenAPI document is not counted) and navigation
// groups that are present but empty (sdk.md S11.2).
func ValidateDescribeResponse(response *modulerpc.DescribeResponse) error {
	withoutOpenAPI := proto.Clone(response).(*modulerpc.DescribeResponse)
	withoutOpenAPI.Openapi = nil
	if size := proto.Size(withoutOpenAPI); size > modulesdk.MaximumDiscoveryBytes {
		return fmt.Errorf("%w: discovery metadata is %d bytes; the limit is %d", modulesdk.ErrInvalidDescriptor, size, modulesdk.MaximumDiscoveryBytes)
	}
	for index, entry := range response.GetNav() {
		if entry.Group != nil && strings.TrimSpace(entry.GetGroup()) == "" {
			return fmt.Errorf("%w: navigation entry %d (%q): group is present but empty", modulesdk.ErrInvalidDescriptor, index, entry.GetLabel())
		}
	}
	return nil
}

func decode(response *modulerpc.DescribeResponse) modulesdk.Discovery {
	d := modulesdk.Discovery{
		Token:                 response.GetToken(),
		ContractVersion:       response.GetContractVersion(),
		MenuGroup:             response.GetMenuGroup(),
		Capabilities:          nonEmpty(response.GetCapabilities()),
		LegacyUIEntry:         response.GetUiEntry(),
		OpenAPI:               nonEmpty(response.GetOpenapi()),
		DescriptorSchemaMajor: response.GetDescriptorSchemaMajor(),
		NeededMycelLicense:    nonEmpty(response.GetNeededMycelLicense()),
	}
	for _, entry := range response.GetNav() {
		d.Nav = append(d.Nav, modulesdk.NavEntry{Label: entry.GetLabel(), Path: entry.GetPath(), Group: entry.GetGroup()})
	}
	if ui := response.GetUi(); ui != nil {
		d.UI = &modulesdk.UIDescriptor{
			Kind:             modulesdk.UIKind(ui.GetKind()),
			ElementName:      ui.GetElementName(),
			EntryAsset:       ui.GetEntryAsset(),
			ContractMajor:    ui.GetContractMajor(),
			MinContractMinor: ui.GetMinContractMinor(),
			BrowserRoutes:    nonEmpty(ui.GetBrowserRoutes()),
			APIPrefix:        ui.GetApiPrefix(),
			AssetPrefix:      ui.GetAssetPrefix(),
		}
		for _, slot := range ui.GetSlots() {
			d.UI.Slots = append(d.UI.Slots, modulesdk.Slot{
				ID: slot.GetId(), SchemaMajor: slot.GetSchemaMajor(), Title: slot.GetTitle(), ProviderPath: slot.GetProviderPath(),
			})
		}
	}
	for _, declaration := range response.GetRouteClasses() {
		d.RouteClasses = append(d.RouteClasses, modulesdk.RouteClassDeclaration{
			Method: declaration.GetMethod(), Path: declaration.GetPath(), Class: modulesdk.RouteClass(declaration.GetClass()),
		})
	}
	return d
}

// UserContextFromUser encodes the verified user app_core attaches to a request
// (sdk.md S11.4). The zero User encodes as nil: no user on the wire.
func UserContextFromUser(user modulesdk.User) *modulerpc.UserContext {
	if !user.Present() {
		return nil
	}
	return &modulerpc.UserContext{Sub: user.Sub, Email: user.Email, Roles: nonEmpty(user.Roles), Tenant: user.Tenant, Access: AuthorizedContextToWire(user.Access)}
}

// UserFromUserContext decodes the request's verified user; nil gives the zero User.
func UserFromUserContext(user *modulerpc.UserContext) modulesdk.User {
	if user == nil {
		return modulesdk.User{}
	}
	return modulesdk.User{Sub: user.GetSub(), Email: user.GetEmail(), Roles: nonEmpty(user.GetRoles()), Tenant: user.GetTenant(), Access: AuthorizedContextFromWire(user.GetAccess())}
}

// nonEmpty normalises an empty slice to nil so a round trip compares equal.
func nonEmpty[T any](values []T) []T {
	if len(values) == 0 {
		return nil
	}
	return values
}

// AuthorizedContextToWire preserves the IAM decision without role expansion.
func AuthorizedContextToWire(a *modulesdk.AuthorizedContext) *modulerpc.AuthorizedContext {
	if a == nil {
		return nil
	}
	grants := make([]*modulerpc.PermissionGrant, 0, len(a.Grants))
	for _, g := range a.Grants {
		grants = append(grants, &modulerpc.PermissionGrant{Permission: g.Permission, Resource: g.Resource})
	}
	return &modulerpc.AuthorizedContext{Version: a.Version, Issuer: a.Issuer, Subject: a.Subject, Account: a.Account, Application: a.Application, Audience: a.Audience, PolicyVersion: a.PolicyVersion, NotBefore: a.NotBefore, ExpiresAt: a.ExpiresAt, Grants: grants}
}

// AuthorizedContextFromWire does not verify authority; only trusted host transport may supply it.
func AuthorizedContextFromWire(a *modulerpc.AuthorizedContext) *modulesdk.AuthorizedContext {
	if a == nil {
		return nil
	}
	grants := make([]modulesdk.PermissionGrant, 0, len(a.GetGrants()))
	for _, g := range a.GetGrants() {
		grants = append(grants, modulesdk.PermissionGrant{Permission: g.GetPermission(), Resource: g.GetResource()})
	}
	return &modulesdk.AuthorizedContext{Version: a.GetVersion(), Issuer: a.GetIssuer(), Subject: a.GetSubject(), Account: a.GetAccount(), Application: a.GetApplication(), Audience: a.GetAudience(), PolicyVersion: a.GetPolicyVersion(), NotBefore: a.GetNotBefore(), ExpiresAt: a.GetExpiresAt(), Grants: grants}
}
