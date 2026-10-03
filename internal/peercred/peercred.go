// Package peercred tells a Unix socket server who is on the other end of a
// connection, and holds the rules for the directories such sockets live in.
//
// Both halves exist for one reason: a Unix socket is an authorisation
// boundary only if the kernel's word on the caller is checked at accept
// time and nobody else can swap the socket file underneath it. File modes
// alone are not enough — they say who may connect, not who did — so every
// listener in ANetLink asks the kernel (Of) and refuses before reading a
// byte.
//
// The semantics follow getpeereid(3), with the pid when the platform has
// it. Linux, macOS and FreeBSD are supported; everywhere else Of answers
// ErrUnsupported, and the servers built on it refuse to start rather than
// serve without the check.
//
// This mirrors the daemon's own package of the same name on purpose. It is
// a copy, not an import: an internal package of another module cannot be
// imported, and the rule is short enough that two copies stay honest.
package peercred

import (
	"errors"
	"net"
)

// ErrUnsupported means this platform cannot report a Unix socket peer's
// user, so a caller must fail closed.
var ErrUnsupported = errors.New("peercred: this platform cannot report who is on the other end of a Unix socket")

// Cred is what the kernel recorded about the peer process. PID is -1 when
// the platform does not report it.
type Cred struct {
	UID, GID, PID int
	// Source names the mechanism that answered, for logs.
	Source string
}

// unknown is returned with every error: uid 0 is root, so a zero Cred
// must never stand for "could not tell".
var unknown = Cred{UID: -1, GID: -1, PID: -1}

// Supported reports whether Of can answer on this platform.
func Supported() bool { return supported }

// Of returns the credentials of the process on the other end of c, which
// must be a Unix socket connection.
func Of(c net.Conn) (Cred, error) {
	if !supported {
		return unknown, ErrUnsupported
	}
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return unknown, errors.New("peercred: not a Unix socket connection")
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return unknown, err
	}
	cred, cerr := unknown, error(nil)
	if err := raw.Control(func(fd uintptr) { cred, cerr = fromFD(int(fd)) }); err != nil {
		return unknown, err
	}
	if cerr != nil {
		return unknown, cerr
	}
	return cred, nil
}
