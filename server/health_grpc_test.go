package server

import (
	"context"
	"net"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	grpcstatus "google.golang.org/grpc/status"
)

// serveWithHealth starts a module server exactly as Serve wires it, and returns a health
// client. End to end on a real connection, because the thing being verified is that a
// k8s probe — which speaks grpc.health.v1 and nothing else — gets an answer at all.
func serveWithHealth(t *testing.T, hg *healthGate) healthpb.HealthClient {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	healthpb.RegisterHealthServer(srv, &grpcHealth{gate: hg})
	go srv.Serve(lis)
	t.Cleanup(srv.Stop)

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return healthpb.NewHealthClient(conn)
}

// Before this, module Deployments carried no probes because there was nothing to probe:
// modules registered only ModuleHandler, so k8s's native `readinessProbe: grpc:` got
// Unimplemented.
func TestGrpcHealthAnswersServingForAHealthyModule(t *testing.T) {
	cl := serveWithHealth(t, newHealthGate(nil, "testmod"))
	resp, err := cl.Check(context.Background(), &healthpb.HealthCheckRequest{})
	if err != nil {
		t.Fatalf("a k8s grpc probe would fail outright: %v", err)
	}
	if resp.GetStatus() != healthpb.HealthCheckResponse_SERVING {
		t.Errorf("status = %v, want SERVING", resp.GetStatus())
	}
}

// A module held by its startup verify gate must report NOT_SERVING — as a STATUS, not an
// error. The difference matters: an error reads to the probe as "endpoint broken", losing
// the distinction between a module that is up and honestly not ready and one that is gone.
func TestGrpcHealthReportsNotServingWhenGated(t *testing.T) {
	hg := newHealthGate(nil, "testmod")
	hg.forceUnready("startup verification pending")
	cl := serveWithHealth(t, hg)

	resp, err := cl.Check(context.Background(), &healthpb.HealthCheckRequest{})
	if err != nil {
		t.Fatalf("NOT_SERVING must be a status, not an error: %v", err)
	}
	if resp.GetStatus() != healthpb.HealthCheckResponse_NOT_SERVING {
		t.Errorf("status = %v, want NOT_SERVING", resp.GetStatus())
	}
}

// Both surfaces read the same gate, so k8s and the console can never disagree.
func TestGrpcHealthAndModuleHandlerHealthAgree(t *testing.T) {
	hg := newHealthGate(nil, "testmod")
	h := &handler{health: hg}

	for _, gated := range []bool{false, true} {
		if gated {
			hg.forceUnready("gated")
		} else {
			hg.ready()
		}
		own, err := h.Health(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		std := (&grpcHealth{gate: hg})
		resp, err := std.Check(context.Background(), &healthpb.HealthCheckRequest{})
		if err != nil {
			t.Fatal(err)
		}
		ownServing := own.GetStatus() == 1 // HealthResponse_SERVING
		stdServing := resp.GetStatus() == healthpb.HealthCheckResponse_SERVING
		if ownServing != stdServing {
			t.Errorf("gated=%v: ModuleHandler says serving=%v but grpc.health says %v",
				gated, ownServing, stdServing)
		}
	}
}

// Watch is honestly unimplemented rather than faked with a response a client would wait
// on forever.
func TestGrpcHealthWatchIsUnimplemented(t *testing.T) {
	cl := serveWithHealth(t, newHealthGate(nil, "testmod"))
	stream, err := cl.Watch(context.Background(), &healthpb.HealthCheckRequest{})
	if err == nil {
		_, err = stream.Recv()
	}
	if grpcstatus.Code(err) != codes.Unimplemented {
		t.Errorf("Watch error = %v, want Unimplemented", err)
	}
}
