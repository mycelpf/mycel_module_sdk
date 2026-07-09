//go:build linux

package server

import (
	"net"

	"golang.org/x/sys/unix"
)

// peerUID reads the connecting peer's uid from a unix-socket connection via the
// SO_PEERCRED socket option (Linux). ok is false only when the credential struct is
// unexpectedly empty; a syscall error is returned for the caller to log (non-fatal).
func peerUID(uc *net.UnixConn) (int, bool, error) {
	raw, err := uc.SyscallConn()
	if err != nil {
		return 0, false, err
	}
	var ucred *unix.Ucred
	var serr error
	if err := raw.Control(func(fd uintptr) {
		ucred, serr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return 0, false, err
	}
	if serr != nil {
		return 0, false, serr
	}
	return int(ucred.Uid), true, nil
}
