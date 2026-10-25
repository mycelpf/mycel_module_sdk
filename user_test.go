package modulesdk

import (
	"context"
	"reflect"
	"testing"
)

func TestUserContextRoundTrip(t *testing.T) {
	user := User{Sub: "user-1", Email: "a@example.com", Roles: []string{"operator"}, Tenant: "acme"}
	got := UserFromContext(ContextWithUser(context.Background(), user))
	if !reflect.DeepEqual(got, user) {
		t.Fatalf("user = %+v, want %+v", got, user)
	}
	if !got.Present() {
		t.Error("a verified user reports not present")
	}
	if UserFromContext(context.Background()).Present() {
		t.Error("a context without a user reports a present user")
	}
}
