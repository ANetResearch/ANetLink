package peercred

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// A connection made by this very process must come back as this process:
// that is the one fact every check built on Of relies on.
func TestOfReportsTheConnectingProcess(t *testing.T) {
	if !Supported() {
		t.Skip("peer credentials are not readable on " + runtime.GOOS)
	}
	sock := filepath.Join(t.TempDir(), "p.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	done := make(chan Cred, 1)
	errc := make(chan error, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			errc <- err
			return
		}
		defer c.Close()
		cr, err := Of(c)
		if err != nil {
			errc <- err
			return
		}
		done <- cr
	}()
	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	select {
	case err := <-errc:
		t.Fatal(err)
	case cr := <-done:
		if cr.UID != os.Getuid() {
			t.Fatalf("uid %d, want %d", cr.UID, os.Getuid())
		}
		if cr.GID != os.Getgid() {
			t.Fatalf("gid %d, want %d", cr.GID, os.Getgid())
		}
		if runtime.GOOS == "linux" && cr.PID != os.Getpid() {
			t.Fatalf("pid %d, want %d", cr.PID, os.Getpid())
		}
		if cr.Source == "" {
			t.Fatal("Source must name the mechanism that answered")
		}
	}
}

// Anything that is not a Unix socket has no peer credentials, and saying so
// is an error, never a zero Cred (uid 0 is root).
func TestOfRefusesNonUnixConn(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	cr, err := Of(a)
	if err == nil {
		t.Fatalf("a pipe has no peer, got %+v", cr)
	}
	if cr.UID != -1 {
		t.Fatalf("a failed lookup must not report uid %d", cr.UID)
	}
}

func TestSupportedMatchesThePlatformList(t *testing.T) {
	want := runtime.GOOS == "linux" || runtime.GOOS == "darwin" || runtime.GOOS == "freebsd"
	if Supported() != want {
		t.Fatalf("Supported() = %v on %s", Supported(), runtime.GOOS)
	}
	if !want {
		if _, err := Of(&net.UnixConn{}); !errors.Is(err, ErrUnsupported) {
			t.Fatalf("unsupported platform must answer ErrUnsupported, got %v", err)
		}
	}
}
