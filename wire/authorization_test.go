package wire

import (
	modulesdk "github.com/mycelpf/mycel_module_sdk"
	modulerpc "github.com/mycelpf/mycel_module_sdk/contracts/modulerpc/v1"
	"google.golang.org/protobuf/proto"
	"reflect"
	"testing"
)

func TestIAMDecisionSurvivesCanonicalWire(t *testing.T) {
	user := modulesdk.User{Sub: "p-1", Tenant: "a-1", Access: &modulesdk.AuthorizedContext{
		Version: 1, Issuer: "https://iam.example", Subject: "p-1", Account: "a-1", Application: "helix", Audience: "helix-client", PolicyVersion: "7", NotBefore: 100, ExpiresAt: 200,
		Grants: []modulesdk.PermissionGrant{{Permission: "helix:read", Resource: "kb:one"}},
	}}
	encoded, err := proto.Marshal(&modulerpc.HandleRequest{User: UserContextFromUser(user)})
	if err != nil {
		t.Fatal(err)
	}
	request := new(modulerpc.HandleRequest)
	if err := proto.Unmarshal(encoded, request); err != nil {
		t.Fatal(err)
	}
	got := UserFromUserContext(request.User)
	if !reflect.DeepEqual(got.Access, user.Access) {
		t.Fatalf("lost IAM decision: %#v", got.Access)
	}
	got.Access.Grants[0].Resource = "kb:other"
	if user.Access.Grants[0].Resource != "kb:one" {
		t.Fatal("wire conversion aliases caller grants")
	}
	legacy := UserFromUserContext(UserContextFromUser(modulesdk.User{Sub: "legacy", Roles: []string{"admin"}}))
	if legacy.Access != nil {
		t.Fatal("legacy roles invented IAM authority")
	}
}
