//go:build darwin

package server

import "log"

// markNonDumpable has no darwin equivalent of PR_SET_DUMPABLE. darwin is a development
// platform only; released images are linux, where a failure stops boot.
func markNonDumpable() error {
	log.Printf("non-dumpable protection is not available on darwin (development only)")
	return nil
}
