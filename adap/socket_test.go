package adap

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ANetResearch/ANetLink/internal/peercred"
	"github.com/ANetResearch/ANetLink/sdk"
)

func startHost(t *testing.T, sock string) (*Host, error) {
	t.Helper()
	h := NewHost(HostConfig{Name: "test", Socket: sock, Timeout: 2 * time.Second})
	b, err := sdk.Select(context.Background(), "", h.Backends()...)
	if err != nil {
		return nil, err
	}
	if err := h.Start(context.Background(), b, newSink()); err != nil {
		return nil, err
	}
	t.Cleanup(func() { h.Stop(context.Background()) })
	return h, nil
}

// AL-10: the host creates its directory 0700 and the socket 0600.
func TestHostSocketIsPrivate(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "adap", "a.sock")
	if _, err := startHost(t, sock); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(sock)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSocket == 0 || fi.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode %v", fi.Mode())
	}
	di, err := os.Stat(filepath.Dir(sock))
	if err != nil {
		t.Fatal(err)
	}
	if di.Mode().Perm() != 0o700 {
		t.Fatalf("directory mode %v, want 0700", di.Mode().Perm())
	}
}

// AL-10: starting no longer removes whatever is at the path. A live socket
// belongs to someone; only a stale one is replaced.
func TestHostDoesNotTakeOverALiveSocket(t *testing.T) {
	sock := filepath.Join(privDir(t), "a.sock")
	other, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	go func() {
		for {
			c, err := other.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	if _, err := startHost(t, sock); err == nil || !strings.Contains(err.Error(), "in use") {
		t.Fatalf("a live socket must be refused, got %v", err)
	}
	if _, err := os.Lstat(sock); err != nil {
		t.Fatal("the other process's socket was removed")
	}

	stale := filepath.Join(privDir(t), "stale.sock")
	sl, err := net.Listen("unix", stale)
	if err != nil {
		t.Fatal(err)
	}
	sl.(*net.UnixListener).SetUnlinkOnClose(false)
	sl.Close()
	if _, err := startHost(t, stale); err != nil {
		t.Fatalf("a stale socket must be replaced: %v", err)
	}
}

func TestHostRefusesUntrustedDirectoryAndSymlink(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "open")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	if _, err := startHost(t, filepath.Join(dir, "a.sock")); err == nil {
		t.Fatal("a world-writable directory must be refused")
	}
	safe := privDir(t)
	link := filepath.Join(safe, "link.sock")
	if err := os.Symlink(filepath.Join(safe, "x.sock"), link); err != nil {
		t.Fatal(err)
	}
	if _, err := startHost(t, link); err == nil {
		t.Fatal("a symlinked socket path must be refused")
	}
}

// AL-10: only anetlinkd's own user may connect. Another uid is closed at
// accept and never gets a session.
func TestHostRefusesOtherUIDs(t *testing.T) {
	saved := peerCredOf
	t.Cleanup(func() { peerCredOf = saved })
	peerCredOf = func(net.Conn) (peercred.Cred, error) {
		return peercred.Cred{UID: os.Getuid() + 3, PID: -1}, nil
	}
	sock := filepath.Join(privDir(t), "a.sock")
	h, err := startHost(t, sock)
	if err != nil {
		t.Fatal(err)
	}
	p := &Plugin{Name: "intruder", Protocol: "demo"}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := p.Dial(ctx, sock); err == nil {
		p.Close()
		t.Fatal("a connection from another uid completed the handshake")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.conn != nil {
		t.Fatal("the host adopted a connection from another uid")
	}
}

// privDir is a 0700 directory whatever the umask.
func privDir(t *testing.T) string {
	t.Helper()
	d := filepath.Join(t.TempDir(), "p")
	if err := os.Mkdir(d, 0o700); err != nil {
		t.Fatal(err)
	}
	return d
}
