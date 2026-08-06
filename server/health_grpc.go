// The standard gRPC health service (grpc.health.v1), served alongside the module's own
// ModuleHandler.Health.
//
// Why both: ModuleHandler.Health is the HOST's probe — it feeds /api/system and is
// supervisor-internal, as its own doc says. Kubernetes cannot speak it. k8s's native
// `readinessProbe: grpc:` calls grpc.health.v1.Health/Check, and a module that does not
// register that service answers Unimplemented, which is why module Deployments had no
// probes at all: there was nothing for them to probe. Registering it here is what makes
// readiness and liveness expressible in the manifests.
//
// Both surfaces read the SAME healthGate, so k8s and the console can never disagree about
// whether a module is serving.
package server

import (
	"context"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
)

// grpcHealth adapts the module's healthGate to grpc.health.v1.
type grpcHealth struct {
	healthpb.UnimplementedHealthServer
	gate *healthGate
}

// Check is what a k8s probe calls. It answers for the whole process regardless of the
// requested service name: this server hosts exactly one module, so a per-service
// breakdown would be a distinction without a difference.
//
// It deliberately reports NOT_SERVING rather than an error when the module is unhealthy —
// an error would read to the probe as "the endpoint is broken", losing the difference
// between a module that is up and honestly not ready (the startup verify gate holding, a
// datastore it cannot reach) and one that is not there at all.
func (h *grpcHealth) Check(ctx context.Context, _ *healthpb.HealthCheckRequest) (*healthpb.HealthCheckResponse, error) {
	st := healthpb.HealthCheckResponse_SERVING
	if !h.gate.serving(ctx) {
		st = healthpb.HealthCheckResponse_NOT_SERVING
	}
	return &healthpb.HealthCheckResponse{Status: st}, nil
}

// Watch is the streaming form. k8s does not use it, and nothing in mycel does either, so
// it is honestly unimplemented rather than faked with a single-shot response a client
// would wait on forever.
func (h *grpcHealth) Watch(*healthpb.HealthCheckRequest, grpc.ServerStreamingServer[healthpb.HealthCheckResponse]) error {
	return status.Error(codes.Unimplemented, "watch is not supported; use Check")
}
