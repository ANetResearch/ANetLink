//go:build unix

package peercred

import (
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The trust rule as a pure function, so the owner cases that need a second
// account to reproduce on disk are still pinned.
func TestCheckDirTrust(t *testing.T) {
	const self = 1000
	cases := []struct {
		name string
		uid  int
		mode fs.FileMode
		ok   bool
	}{
		{"own 0700", self, 0o700, true},
		{"own 0710", self, 0o710, true},
		{"own 0755", self, 0o755, true},
		{"root 0755", 0, 0o755, true},
		{"root sticky 1777", 0, 0o777 | fs.ModeSticky, false},
		{"own group-writable", self, 0o770, false},
		{"own other-writable", self, 0o703, false},
		{"another user 0700", 1001, 0o700, false},
		{"another user 0755", 1001, 0o755, false},
	}
	for _, c := range cases {
		err := checkDirTrust("/d", c.uid, c.mode, self)
		if (err == nil) != c.ok {
			t.Errorf("%s: err=%v, want ok=%v", c.name, err, c.ok)
		}
	}
}

func TestPrepareDirCreates0700(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a", "b")
	created, err := PrepareDir(dir)
	if err != nil || !created {
		t.Fatalf("PrepareDir: created=%v err=%v", created, err)
	}
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o700 {
		t.Fatalf("mode %v, want 0700", fi.Mode().Perm())
	}
	created, err = PrepareDir(dir)
	if err != nil || created {
		t.Fatalf("second PrepareDir: created=%v err=%v", created, err)
	}
}

func TestPrepareDirRefusesWritableByOthers(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "open")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareDir(dir); err == nil {
		t.Fatal("a world-writable directory must not hold the socket")
	}
	if err := os.Chmod(dir, 0o770); err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareDir(dir); err == nil {
		t.Fatal("a group-writable directory must not hold the socket")
	}
}

func TestPrepareDirRefusesAFile(t *testing.T) {
	f := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(f, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareDir(f); err == nil {
		t.Fatal("a regular file is not a directory")
	}
}

func TestClearPath(t *testing.T) {
	dir := t.TempDir()

	// nothing there: fine
	if err := ClearPath(filepath.Join(dir, "none.sock")); err != nil {
		t.Fatalf("absent path: %v", err)
	}

	// a symlink is refused even when it points at nothing
	link := filepath.Join(dir, "link.sock")
	if err := os.Symlink(filepath.Join(dir, "elsewhere"), link); err != nil {
		t.Fatal(err)
	}
	if err := ClearPath(link); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("symlink: %v", err)
	}
	if _, err := os.Lstat(link); err != nil {
		t.Fatal("ClearPath must not remove a symlink it refused")
	}

	// a regular file is refused and left alone
	file := filepath.Join(dir, "file.sock")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ClearPath(file); err == nil {
		t.Fatal("a regular file at the socket path must be refused")
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatal("ClearPath must not remove a file it refused")
	}

	// a live socket is refused and left alone
	live := filepath.Join(dir, "live.sock")
	ln, err := net.Listen("unix", live)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	if err := ClearPath(live); err == nil || !strings.Contains(err.Error(), "in use") {
		t.Fatalf("live socket: %v", err)
	}
	if _, err := os.Lstat(live); err != nil {
		t.Fatal("ClearPath must not remove a socket somebody listens on")
	}

	// a stale socket (nobody listening) is removed
	stale := filepath.Join(dir, "stale.sock")
	sl, err := net.Listen("unix", stale)
	if err != nil {
		t.Fatal(err)
	}
	sl.(*net.UnixListener).SetUnlinkOnClose(false)
	sl.Close()
	if _, err := os.Lstat(stale); err != nil {
		t.Fatalf("test setup: stale socket should still exist: %v", err)
	}
	if err := ClearPath(stale); err != nil {
		t.Fatalf("stale socket: %v", err)
	}
	if _, err := os.Lstat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale socket should be gone, Lstat err=%v", err)
	}
}

func TestOwnerOf(t *testing.T) {
	fi, err := os.Stat(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	uid, gid, ok := OwnerOf(fi)
	if !ok || uid != os.Getuid() || gid < 0 {
		t.Fatalf("OwnerOf: uid=%d gid=%d ok=%v", uid, gid, ok)
	}
}
