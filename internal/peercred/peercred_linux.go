package peercred

import "golang.org/x/sys/unix"

const supported = true

// fromFD reads SO_PEERCRED: the uid, gid and pid of the peer as the kernel
// recorded them at connect(2) (or at listen(2), seen from the client).
func fromFD(fd int) (Cred, error) {
	u, err := unix.GetsockoptUcred(fd, unix.SOL_SOCKET, unix.SO_PEERCRED)
	if err != nil {
		return unknown, err
	}
	return Cred{UID: int(u.Uid), GID: int(u.Gid), PID: int(u.Pid), Source: "SO_PEERCRED"}, nil
}
