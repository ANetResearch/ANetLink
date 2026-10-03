//go:build unix

package peercred

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"syscall"
	"time"
)

// PrepareDir makes dir fit to hold a socket that is an authorisation
// boundary. A missing directory is created 0700 (so nobody else can reach a
// socket in it between listen and chmod). An existing one must be a
// directory owned by this process's user or root, writable by nobody else:
// whoever can write in it can replace the socket with their own.
//
// created reports whether this call made the directory, which tells the
// caller it may set the directory's final mode; an existing directory's
// mode is never changed (it may be /run).
func PrepareDir(dir string) (created bool, err error) {
	fi, err := os.Stat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return false, fmt.Errorf("cannot create %s: %w", dir, err)
		}
		// MkdirAll's mode is filtered by the umask; say exactly what we mean.
		if err := os.Chmod(dir, 0o700); err != nil {
			return false, fmt.Errorf("cannot set mode 0700 on %s: %w", dir, err)
		}
		created = true
		fi, err = os.Stat(dir)
	}
	if err != nil {
		return false, fmt.Errorf("cannot inspect %s: %w", dir, err)
	}
	if !fi.IsDir() {
		return false, fmt.Errorf("%s is not a directory; choose a socket path inside a directory", dir)
	}
	uid, _, ok := OwnerOf(fi)
	if !ok {
		return false, fmt.Errorf("cannot read the owner of %s", dir)
	}
	if err := checkDirTrust(dir, uid, fi.Mode(), os.Getuid()); err != nil {
		return false, err
	}
	return created, nil
}

// checkDirTrust is the rule PrepareDir applies, kept pure so every owner
// case can be tested without a second account.
func checkDirTrust(dir string, ownerUID int, mode fs.FileMode, selfUID int) error {
	if ownerUID != selfUID && ownerUID != 0 {
		return fmt.Errorf("%s belongs to uid %d, so that user could replace the socket; "+
			"use a directory owned by this user or by root", dir, ownerUID)
	}
	if mode.Perm()&0o022 != 0 {
		return fmt.Errorf("%s is writable by other users (mode %04o), so they could replace the socket; "+
			"run chmod go-w %s or choose another directory", dir, mode.Perm(), dir)
	}
	return nil
}

// ClearPath makes path free for a new socket. Nothing there is fine. A
// stale socket (one nobody listens on) is removed. Everything else is
// refused and left in place: a symlink (a socket path must not be one, or
// whoever controls the link decides where we listen), a socket somebody
// is listening on, and anything that is not a socket at all.
func ClearPath(path string) error {
	fi, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("cannot inspect %s: %w", path, err)
	}
	if fi.Mode()&fs.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symbolic link; a socket path must not be one, so point the configuration at the real location", path)
	}
	if fi.Mode()&fs.ModeSocket == 0 {
		return fmt.Errorf("%s exists and is not a socket; move it away or choose another path", path)
	}
	c, err := net.DialTimeout("unix", path, time.Second)
	if err == nil {
		c.Close()
		return fmt.Errorf("%s is in use by another process; stop that process or choose another path", path)
	}
	if !errors.Is(err, syscall.ECONNREFUSED) {
		return fmt.Errorf("cannot tell whether %s is still in use (%v); if it is stale, remove it by hand", path, err)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("cannot remove the stale socket %s: %w", path, err)
	}
	return nil
}

// OwnerOf returns the uid and gid that own fi.
func OwnerOf(fi fs.FileInfo) (uid, gid int, ok bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return -1, -1, false
	}
	return int(st.Uid), int(st.Gid), true
}
