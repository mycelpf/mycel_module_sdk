package server

import (
	"context"
	"testing"
)

// A failed startup Verify pins the module NOT_SERVING (fail-closed) regardless of DB
// health, and ready() clears it — the mechanism behind the extension-conformance gate.
func TestHealthGateForceUnready(t *testing.T) {
	ctx := context.Background()
	g := newHealthGate(nil, "test") // no DB → serves by default

	if !g.serving(ctx) {
		t.Fatal("a no-DB gate should serve by default")
	}
	g.forceUnready("extension non-conformant")
	if g.serving(ctx) {
		t.Fatal("forceUnready must pin NOT_SERVING")
	}
	g.ready()
	if !g.serving(ctx) {
		t.Fatal("ready() must clear the gate back to SERVING")
	}
}
