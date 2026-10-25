//go:build linux

package server

import (
	"testing"

	"golang.org/x/sys/unix"
)

// SDK-T15 (sdk.md S7, core I12), platform half: on linux the process really becomes
// non-dumpable.
func TestMarkNonDumpableOnLinux(t *testing.T) {
	if err := markNonDumpable(); err != nil {
		t.Fatal(err)
	}
	dumpable, err := unix.PrctlRetInt(unix.PR_GET_DUMPABLE, 0, 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if dumpable != 0 {
		t.Errorf("PR_GET_DUMPABLE = %d after markNonDumpable, want 0", dumpable)
	}
}
