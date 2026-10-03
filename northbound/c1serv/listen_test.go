package c1serv

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ANetResearch/ANetLink/internal/peercred"
	"github.com/ANetResearch/ANetLink/runtime"
)

func sockIn(t *testing.T, sub ...string) string {
	t.Helper()
	return filepath.Join(append([]string{t.TempDir()}, append(sub, "c1.sock")...)...)
}

func listenOK(t *testing.T, o Options) net.Listener {
	t.Helper()
	if o.Logf == nil {
		o.Logf = (&logSink{}).logf
	}
	ln, err := Listen(o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	return ln
}

func modeOf(t *testing.T, p string) (os.FileMode, int) {
	t.Helper()
	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	_, gid, _ := peercred.OwnerOf(fi)
	return fi.Mode(), gid
}

// AL-1, AL-2, M-AL-2: with nobody else allowed, the directory is created
// 0700 and the socket is 0600 before anyone can be accepted.
func TestSocketIsPrivateByDefault(t *testing.T) {
	sock := sockIn(t, "fresh")
	listenOK(t, Options{Socket: sock})
	m, _ := modeOf(t, sock)
	if m&os.ModeSocket == 0 || m.Perm() != 0o600 {
		t.Fatalf("socket mode %v, want 0600 socket", m)
	}
	dm, _ := modeOf(t, filepath.Dir(sock))
	if dm.Perm() != 0o700 {
		t.Fatalf("created directory mode %v, want 0700", dm.Perm())
	}
}

// AL-2: one other user — the socket opens to a group (0660), and the
// directory anetlinkd created becomes enterable by that group (0710), only
// after the socket mode is set.
func TestSocketOpensToOneOtherUserThroughAGroup(t *testing.T) {
	other := otherUser(t)
	gid := os.Getgid()
	sock := sockIn(t, "shared")
	listenOK(t, Options{Socket: sock, AllowUsers: []string{other.Username}, Group: strconv.Itoa(gid)})
	m, g := modeOf(t, sock)
	if m.Perm() != 0o660 || g != gid {
		t.Fatalf("socket mode %v gid %d, want 0660 gid %d", m.Perm(), g, gid)
	}
	dm, dg := modeOf(t, filepath.Dir(sock))
	if dm.Perm() != 0o710 || dg != gid {
		t.Fatalf("directory mode %v gid %d, want 0710 gid %d", dm.Perm(), dg, gid)
	}
}

// AL-2: with two other users the group must be named; there is no
// primary group they share by default.
func TestTwoOtherUsersNeedAGroup(t *testing.T) {
	other := otherUser(t)
	second := "root"
	if os.Getuid() == 0 {
		t.Skip("needs a non-root test user")
	}
	_, err := Listen(Options{Socket: sockIn(t, "g"), AllowUsers: []string{other.Username, second}, Logf: (&logSink{}).logf})
	if err == nil || !strings.Contains(err.Error(), "c1.group") {
		t.Fatalf("two other users without c1.group: %v", err)
	}
	listenOK(t, Options{Socket: sockIn(t, "g2"), AllowUsers: []string{other.Username, second, other.Uid},
		Group: strconv.Itoa(os.Getgid())})
}

func TestOverlongSocketPathIsRefusedWithTheFix(t *testing.T) {
	long := filepath.Join(t.TempDir(), strings.Repeat("d", 120), "c1.sock")
	_, err := Listen(Options{Socket: long, Logf: (&logSink{}).logf})
	if err == nil || !strings.Contains(err.Error(), "shorter") {
		t.Fatalf("over-long path: %v", err)
	}
}

func TestUnknownUsersAndGroupsAreRefused(t *testing.T) {
	for _, o := range []Options{
		{AllowUsers: []string{"no-such-user-c1t"}},
		{AllowUsers: []string{""}},
		{AllowUsers: []string{"-5"}},
		{AllowUsers: []string{otherUserName(t)}, Group: "no-such-group-c1t"},
	} {
		o.Socket = sockIn(t, "u")
		o.Logf = (&logSink{}).logf
		if _, err := Listen(o); err == nil {
			t.Errorf("%+v must be refused", o)
		}
	}
}

// AL-1, M-AL-2: the directory is a trust boundary. Anyone who can write in
// it can replace the socket.
func TestListenRefusesAnUntrustedDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "open")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	_, err := Listen(Options{Socket: filepath.Join(dir, "c1.sock"), Logf: (&logSink{}).logf})
	if err == nil || !strings.Contains(err.Error(), "writable by other users") {
		t.Fatalf("world-writable directory: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "c1.sock")); !os.IsNotExist(err) {
		t.Fatal("no socket may be created in a refused directory")
	}
}

func TestListenRefusesSymlinkLiveAndForeignPaths(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "d")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.sock")
	if err := os.Symlink(filepath.Join(dir, "target.sock"), link); err != nil {
		t.Fatal(err)
	}
	if _, err := Listen(Options{Socket: link, Logf: (&logSink{}).logf}); err == nil {
		t.Fatal("a symlinked socket path must be refused")
	}

	file := filepath.Join(dir, "file.sock")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Listen(Options{Socket: file, Logf: (&logSink{}).logf}); err == nil {
		t.Fatal("a regular file at the socket path must be refused")
	}

	live := filepath.Join(dir, "live.sock")
	first := listenOK(t, Options{Socket: live})
	go func() {
		for {
			c, err := first.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	if _, err := Listen(Options{Socket: live, Logf: (&logSink{}).logf}); err == nil || !strings.Contains(err.Error(), "in use") {
		t.Fatalf("a second anetlinkd on a live socket: %v", err)
	}

	// a stale socket from a crashed run is replaced
	stale := filepath.Join(dir, "stale.sock")
	sl, err := net.Listen("unix", stale)
	if err != nil {
		t.Fatal(err)
	}
	sl.(*net.UnixListener).SetUnlinkOnClose(false)
	sl.Close()
	listenOK(t, Options{Socket: stale})
}

// A directory that already exists keeps its mode, and if the allowed users
// cannot enter it the start is refused with the fix, not left to fail on
// the daemon's side.
func TestExistingPrivateDirectoryIsReportedForOtherUsers(t *testing.T) {
	other := otherUser(t)
	dir := filepath.Join(t.TempDir(), "mine")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := Listen(Options{Socket: filepath.Join(dir, "c1.sock"), AllowUsers: []string{other.Username},
		Group: strconv.Itoa(os.Getgid()), Logf: (&logSink{}).logf})
	if err == nil || !strings.Contains(err.Error(), "cannot enter") {
		t.Fatalf("private existing directory with other users: %v", err)
	}
	if m, _ := modeOf(t, dir); m.Perm() != 0o700 {
		t.Fatalf("an existing directory's mode must not change, now %v", m.Perm())
	}
}

// resolveAccess is the AL-2/AL-3 table: who is accepted, the socket mode,
// and its group. Root is never implied.
func TestResolveAccess(t *testing.T) {
	self := os.Getuid()
	if self == 0 {
		t.Skip("needs a non-root test user")
	}
	ac, err := resolveAccess(Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(ac.allowed) != 1 || !ac.allowed[self] || ac.allowed[0] || ac.mode != 0o600 || ac.gid != -1 {
		t.Fatalf("default access: %+v", ac)
	}
	ac, err = resolveAccess(Options{AllowUsers: []string{strconv.Itoa(self)}})
	if err != nil || len(ac.allowed) != 1 || ac.mode != 0o600 {
		t.Fatalf("self listed explicitly is still nobody else: %+v %v", ac, err)
	}
	ac, err = resolveAccess(Options{AllowUsers: []string{"root"}})
	if err != nil || !ac.allowed[0] || ac.mode != 0o660 || ac.gid != 0 {
		t.Fatalf("root listed by name: %+v %v", ac, err)
	}
	// a numeric uid with no account still works when the group is named
	ac, err = resolveAccess(Options{AllowUsers: []string{"424242"}, Group: strconv.Itoa(os.Getgid())})
	if err != nil || !ac.allowed[424242] || ac.gid != os.Getgid() {
		t.Fatalf("numeric uid: %+v %v", ac, err)
	}
	if _, err := resolveAccess(Options{AllowUsers: []string{"424242"}}); err == nil || !strings.Contains(err.Error(), "c1.group") {
		t.Fatalf("numeric uid without a known primary group needs c1.group: %v", err)
	}
}

// AL-3, M-AL-1: a connection from a uid that is not allowed is closed at
// accept, before a byte is read, and the refusal is logged at most once a
// minute per uid.
func TestPeerCheckClosesOtherUIDs(t *testing.T) {
	saved := peerCredOf
	t.Cleanup(func() { peerCredOf = saved })
	intruder := os.Getuid() + 7
	peerCredOf = func(c net.Conn) (peercred.Cred, error) {
		return peercred.Cred{UID: intruder, GID: intruder, PID: -1, Source: "test"}, nil
	}
	r := newRig(t)
	for i := 0; i < 3; i++ {
		c, err := net.Dial("unix", r.sock)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = c.Write([]byte("GET /v1/health HTTP/1.1\r\nHost: anetlink\r\n\r\n"))
		_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, err = bufio.NewReader(c).ReadByte()
		c.Close()
		if err == nil {
			t.Fatal("a refused uid got an answer")
		}
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			t.Fatal("a refused connection must be closed, not left hanging")
		}
		if !errors.Is(err, io.EOF) && !errors.Is(err, syscall.ECONNRESET) {
			t.Fatalf("unexpected read error %v", err)
		}
	}
	want := "uid " + strconv.Itoa(intruder)
	waitLog(t, r.logs, want)
	if n := r.logs.count(want); n != 1 {
		t.Fatalf("c1_uid_refused logged %d times for one uid within a minute:\n%s", n, r.logs.all())
	}
}

// The same check lets the right uid through: the rig's own requests work
// with the real kernel answer, and a cred lookup that fails closes.
func TestPeerCheckFailsClosedWhenCredentialsAreUnreadable(t *testing.T) {
	saved := peerCredOf
	t.Cleanup(func() { peerCredOf = saved })
	peerCredOf = func(net.Conn) (peercred.Cred, error) {
		return peercred.Cred{UID: -1}, errors.New("no creds")
	}
	r := newRig(t)
	_, err := r.cli.Get("http://anetlink/v1/health")
	if err == nil {
		t.Fatal("unreadable credentials must close the connection")
	}
}

func TestPeerCheckAdmitsTheOwnUser(t *testing.T) {
	r := newRig(t)
	resp, err := r.cli.Get("http://anetlink/v1/health")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("health %d", resp.StatusCode)
	}
}

// The refusal sentence is the one in the reason table (ux §1.4 J), in the
// operator's language.
func TestUIDRefusedSentence(t *testing.T) {
	t.Setenv("LC_ALL", "")
	t.Setenv("LC_MESSAGES", "")
	t.Setenv("LANG", "zh_CN.UTF-8")
	if got, want := uidRefusedSentence(1002), "拒绝 uid 1002 连接 c1.sock(不在 allow_users 中);如需允许,在 ANetLink 配置 c1.allow_users 加入该用户。"; got != want {
		t.Fatalf("zh: %q", got)
	}
	t.Setenv("LANG", "en_US.UTF-8")
	if got, want := uidRefusedSentence(1002), "Refused uid 1002 on c1.sock (not in allow_users); to allow it, add the user to c1.allow_users in the ANetLink config."; got != want {
		t.Fatalf("en: %q", got)
	}
	t.Setenv("LC_ALL", "zh_TW.UTF-8")
	if !strings.HasPrefix(uidRefusedSentence(1), "拒绝") {
		t.Fatal("LC_ALL wins over LANG")
	}
}

// Serve stops with its context and takes the socket with it.
func TestServeRemovesTheSocketOnShutdown(t *testing.T) {
	sock := sockIn(t, "bye")
	ln, err := Listen(Options{Socket: sock, Logf: (&logSink{}).logf})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, runtime.New(runtime.Config{}), ln, nil) }()
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(sock); !os.IsNotExist(err) {
		t.Fatalf("socket left behind: %v", err)
	}
}

func waitLog(t *testing.T, l *logSink, sub string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if l.count(sub) > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("log never mentioned %q:\n%s", sub, l.all())
}

// otherUser is an account that is not the test's own, so the group paths
// can be exercised without root.
func otherUser(t *testing.T) *user.User {
	t.Helper()
	for _, name := range []string{"nobody", "daemon", "bin"} {
		u, err := user.Lookup(name)
		if err == nil && u.Uid != strconv.Itoa(os.Getuid()) {
			return u
		}
	}
	t.Skip("no second account on this machine")
	return nil
}

func otherUserName(t *testing.T) string { return otherUser(t).Username }
