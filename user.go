package modulesdk

import (
	"context"
	"slices"
)

// User is the verified user app_core attaches to a request (sdk.md S11.4, login §8).
// app_core sets it only from a credential it verified; the SDK never builds one from a
// header. The zero User means no verified user.
type User struct {
	// Access is the IAM-owned decision; Roles alone never satisfy Authorize.
	Access *AuthorizedContext
	Sub    string
	Email  string
	Roles  []string
	Tenant string
}

// Present reports whether a verified user is attached.
func (u User) Present() bool {
	return u.Sub != "" || u.Email != "" || len(u.Roles) > 0 || u.Tenant != ""
}

type userContextKey struct{}

// ContextWithUser returns ctx carrying user. The server's http.Handler adapter uses it,
// so a Descriptor.Handler module reads the same user a Route module gets in Request.User.
func ContextWithUser(ctx context.Context, user User) context.Context {
	user.Access = user.Access.Clone()
	user.Roles = slices.Clone(user.Roles)
	return context.WithValue(ctx, userContextKey{}, user)
}

// UserFromContext returns the verified user carried by ctx, or the zero User.
func UserFromContext(ctx context.Context) User {
	user, _ := ctx.Value(userContextKey{}).(User)
	user.Access = user.Access.Clone()
	user.Roles = slices.Clone(user.Roles)
	return user
}
