//go:build darwin

package server

import (
	"net"

	"golang.org/x/sys/unix"
)

// peerUID reads the connecting peer's uid from a unix-socket connection via the
// LOCAL_PEERCRED socket option (the getpeereid mechanism on darwin/BSD). A syscall
// error is returned for the caller to log (non-fatal); the connection is allowed when
// creds can't be read.
func peerUID(uc *net.UnixConn) (int, bool, error) {
	raw, err := uc.SyscallConn()
	if err != nil {
		return 0, false, err
	}
	var xucred *unix.Xucred
	var serr error
	if err := raw.Control(func(fd uintptr) {
		xucred, serr = unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
	}); err != nil {
		return 0, false, err
	}
	if serr != nil {
		return 0, false, serr
	}
	return int(xucred.Uid), true, nil
}
