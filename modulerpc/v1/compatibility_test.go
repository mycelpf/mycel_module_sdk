package modulerpc_test

import (
	"testing"

	canonical "github.com/mycelpf/mycel_module_sdk/contracts/modulerpc/v1"
	old "github.com/mycelpf/mycel_module_sdk/modulerpc/v1"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
)

// SDK-T2 (sdk.md S1, S10): importing the old and canonical packages together registers
// the contract's descriptors once, and old-path types are the canonical types.
func TestOldAndCanonicalPackagesShareOneRegistration(t *testing.T) {
	count := 0
	protoregistry.GlobalFiles.RangeFilesByPackage("mycel.modulerpc.v1", func(file protoreflect.FileDescriptor) bool {
		if file.Path() != "contracts/modulerpc/v1/modulerpc.proto" && file.Path() != "contracts/modulerpc/v1/ui_contract.proto" {
			t.Errorf("unexpected registered file %s", file.Path())
		}
		count++
		return true
	})
	if count != 2 {
		t.Errorf("registered files in mycel.modulerpc.v1 = %d, want 2", count)
	}

	var request any = &old.DescribeResponse{Token: "x"}
	if _, ok := request.(*canonical.DescribeResponse); !ok {
		t.Error("old DescribeResponse is not the canonical type")
	}
	if old.ModuleHandler_ServiceDesc.ServiceName != canonical.ModuleHandler_ServiceDesc.ServiceName {
		t.Error("service descriptions differ")
	}
}
