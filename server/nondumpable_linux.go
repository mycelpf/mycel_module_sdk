//go:build linux

package server

import "golang.org/x/sys/unix"

// markNonDumpable clears the process's dumpable flag (core I12, sdk.md S7), which hides
// its environment and memory from other processes of the same user.
func markNonDumpable() error {
	return unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0)
}
