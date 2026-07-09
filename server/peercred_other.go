//go:build !linux && !darwin

package server

import "net"

// peerUID is a no-op on platforms without a supported peer-credential syscall. ok is
// false, so the listener allows the connection (the 0700 socket dir remains the gate).
func peerUID(*net.UnixConn) (int, bool, error) { return 0, false, nil }
