package modulesdk

import (
	"errors"
	"testing"
	"time"
)

func authorizedUser() User {
	return User{Sub: "principal-1", Tenant: "account-1", Access: &AuthorizedContext{
		Version: 1, Issuer: "https://iam.example", Subject: "principal-1", Account: "account-1",
		Application: "helix", Audience: "helix-client", PolicyVersion: "policy-7", NotBefore: 100, ExpiresAt: 200,
		Grants: []PermissionGrant{{Permission: "helix:read", Resource: "kb:one"}},
	}}
}

func TestIAMAuthorityCannotBeReplacedByRolesOrWrongBinding(t *testing.T) {
	b := AccessBinding{Issuer: "https://iam.example", Account: "account-1", Application: "helix", Audience: "helix-client"}
	now := time.Unix(150, 0)
	if err := authorizedUser().Authorize(b, "helix:read", "kb:one", now); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		change func(*User, *AccessBinding)
	}{
		{"role only", func(u *User, _ *AccessBinding) { u.Access = nil; u.Roles = []string{"admin"} }},
		{"subject", func(u *User, _ *AccessBinding) { u.Sub = "other" }},
		{"account", func(_ *User, b *AccessBinding) { b.Account = "other" }},
		{"tenant", func(u *User, _ *AccessBinding) { u.Tenant = "other" }},
		{"application", func(_ *User, b *AccessBinding) { b.Application = "other" }},
		{"audience", func(_ *User, b *AccessBinding) { b.Audience = "other" }},
		{"issuer", func(_ *User, b *AccessBinding) { b.Issuer = "other" }},
		{"unsupported version", func(u *User, _ *AccessBinding) { u.Access.Version = 99 }},
		{"not yet valid", func(u *User, _ *AccessBinding) { u.Access.NotBefore = 151 }},
		{"expired", func(u *User, _ *AccessBinding) { u.Access.ExpiresAt = 150 }},
		{"policy missing", func(u *User, _ *AccessBinding) { u.Access.PolicyVersion = "" }},
		{"no permission", func(u *User, _ *AccessBinding) { u.Access.Grants = nil }},
		{"different resource", func(u *User, _ *AccessBinding) { u.Access.Grants[0].Resource = "kb:two" }},
		{"no wildcard expansion", func(u *User, _ *AccessBinding) { u.Access.Grants[0].Resource = "*" }},
		{"invalid interval", func(u *User, _ *AccessBinding) { u.Access.NotBefore = 201 }},
		{"empty binding", func(_ *User, b *AccessBinding) { b.Account = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := authorizedUser()
			binding := b
			tc.change(&u, &binding)
			if err := u.Authorize(binding, "helix:read", "kb:one", now); !errors.Is(err, ErrAccessDenied) {
				t.Fatalf("got %v", err)
			}
		})
	}
	if err := authorizedUser().Authorize(b, "helix:write", "kb:one", now); !errors.Is(err, ErrAccessDenied) {
		t.Fatal(err)
	}
}

func TestAuthorizedContextCloneDoesNotShareGrants(t *testing.T) {
	original := authorizedUser().Access
	cloned := original.Clone()
	cloned.Grants[0].Permission = "helix:write"
	if original.Grants[0].Permission != "helix:read" {
		t.Fatal("grant alias crosses request boundary")
	}
	var absent *AuthorizedContext
	if absent.Clone() != nil {
		t.Fatal("nil context changed")
	}
}
