package modulesdk

import (
	"errors"
	"slices"
	"strings"
	"time"
)

// ErrAccessDenied deliberately does not disclose which binding failed.
var ErrAccessDenied = errors.New("IAM authorization required")

// PermissionGrant is an exact operation/resource grant evaluated by IAM.
// Consumers never expand roles, patterns or wildcard resources.
type PermissionGrant struct {
	Permission string `json:"permission"`
	Resource   string `json:"resource"`
}

// AccessBinding is trusted deployment configuration, never request headers.
type AccessBinding struct {
	Issuer      string
	Account     string
	Application string
	Audience    string
}

// AuthorizedContext is carried only inside an IAM-verified credential and the
// authenticated host/module transport. Constructing this value does not verify it.
// Version 1 uses exact grants; an empty or unsupported context grants nothing.
type AuthorizedContext struct {
	Version       uint32            `json:"version"`
	Issuer        string            `json:"issuer"`
	Subject       string            `json:"subject"`
	Account       string            `json:"account"`
	Application   string            `json:"application"`
	Audience      string            `json:"audience"`
	PolicyVersion string            `json:"policy_version"`
	NotBefore     int64             `json:"not_before"`
	ExpiresAt     int64             `json:"expires_at"`
	Grants        []PermissionGrant `json:"grants"`
}

// Clone prevents a module or request from mutating another request's grants.
func (a *AuthorizedContext) Clone() *AuthorizedContext {
	if a == nil {
		return nil
	}
	result := *a
	result.Grants = slices.Clone(a.Grants)
	return &result
}

// ValidateAccess checks the binding of an IAM decision already verified by app_core.
// It is not a
// policy evaluator: roles and email cannot create or widen a grant. Expiration is
// exclusive and no module-local grace period extends the authority.
func (u User) ValidateAccess(b AccessBinding, now time.Time) error {
	a := u.Access
	if a == nil || a.Version != 1 || u.Sub == "" || a.Subject != u.Sub ||
		b.Issuer == "" || b.Account == "" || b.Application == "" || b.Audience == "" ||
		a.Issuer != b.Issuer || a.Account != b.Account || a.Application != b.Application || a.Audience != b.Audience ||
		(u.Tenant != "" && u.Tenant != a.Account) || strings.TrimSpace(a.PolicyVersion) == "" ||
		a.NotBefore < 0 || a.ExpiresAt <= a.NotBefore || now.Unix() < a.NotBefore || now.Unix() >= a.ExpiresAt {
		return ErrAccessDenied
	}
	return nil
}

// Authorize matches an exact IAM grant after validating its complete binding.
func (u User) Authorize(b AccessBinding, permission, resource string, now time.Time) error {
	if err := u.ValidateAccess(b, now); err != nil {
		return err
	}
	if strings.TrimSpace(permission) == "" || strings.TrimSpace(resource) == "" {
		return ErrAccessDenied
	}
	for _, grant := range u.Access.Grants {
		if grant.Permission == permission && grant.Resource == resource {
			return nil
		}
	}
	return ErrAccessDenied
}
