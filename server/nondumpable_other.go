//go:build !linux && !darwin

package server

import "errors"

// markNonDumpable fails on platforms with no supported protection, so a typed module
// cannot boot there without it (sdk.md S7).
func markNonDumpable() error {
	return errors.New("non-dumpable protection is not supported on this platform")
}
