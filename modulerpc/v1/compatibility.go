// Package modulerpc is a deprecated compatibility path for the module wire contract.
//
// Deprecated: import github.com/mycelpf/mycel_module_sdk/contracts/modulerpc/v1.
//
// This directory used to hold a second generated copy of the contract, written by a
// misdirected protoc output path. That copy registered the same protobuf names from a
// different file path, so importing it next to the canonical package was a registration
// conflict. It now forwards to the canonical package with aliases and registers nothing
// (sdk.md S1, S10). It is deleted only in an explicitly incompatible SDK release.
package modulerpc

import (
	canonical "github.com/mycelpf/mycel_module_sdk/contracts/modulerpc/v1"
	"google.golang.org/grpc"
)

// Message, enumeration and service types of the contract.
type (
	DescribeRequest                  = canonical.DescribeRequest
	DescribeResponse                 = canonical.DescribeResponse
	NavEntry                         = canonical.NavEntry
	HandleRequest                    = canonical.HandleRequest
	HandleResponse                   = canonical.HandleResponse
	HealthRequest                    = canonical.HealthRequest
	HealthResponse                   = canonical.HealthResponse
	HealthResponse_Status            = canonical.HealthResponse_Status
	ModuleHandlerClient              = canonical.ModuleHandlerClient
	ModuleHandlerServer              = canonical.ModuleHandlerServer
	UnimplementedModuleHandlerServer = canonical.UnimplementedModuleHandlerServer
	UnsafeModuleHandlerServer        = canonical.UnsafeModuleHandlerServer
)

// Health status values.
const (
	HealthResponse_UNKNOWN     = canonical.HealthResponse_UNKNOWN
	HealthResponse_SERVING     = canonical.HealthResponse_SERVING
	HealthResponse_NOT_SERVING = canonical.HealthResponse_NOT_SERVING
)

// Full gRPC method names.
const (
	ModuleHandler_Describe_FullMethodName = canonical.ModuleHandler_Describe_FullMethodName
	ModuleHandler_Handle_FullMethodName   = canonical.ModuleHandler_Handle_FullMethodName
	ModuleHandler_Health_FullMethodName   = canonical.ModuleHandler_Health_FullMethodName
)

// Enumeration name tables and the service description, shared with the canonical package.
var (
	HealthResponse_Status_name  = canonical.HealthResponse_Status_name
	HealthResponse_Status_value = canonical.HealthResponse_Status_value
	ModuleHandler_ServiceDesc   = canonical.ModuleHandler_ServiceDesc
)

// File_modulerpc_v1_modulerpc_proto is the canonical file descriptor.
var File_modulerpc_v1_modulerpc_proto = canonical.File_contracts_modulerpc_v1_modulerpc_proto

// NewModuleHandlerClient forwards to the canonical package.
func NewModuleHandlerClient(connection grpc.ClientConnInterface) ModuleHandlerClient {
	return canonical.NewModuleHandlerClient(connection)
}

// RegisterModuleHandlerServer forwards to the canonical package.
func RegisterModuleHandlerServer(registrar grpc.ServiceRegistrar, server ModuleHandlerServer) {
	canonical.RegisterModuleHandlerServer(registrar, server)
}
