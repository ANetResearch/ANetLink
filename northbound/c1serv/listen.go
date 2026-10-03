package c1serv

import (
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"os"
	"os/user"
	"path/filepath"
	goruntime "runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ANetResearch/ANetLink/internal/peercred"
)

// Options says where C1 listens and who may connect (ux §2.4).
type Options struct {
	// Socket is the socket path (c1.socket, --c1-socket). DefaultSocket
	// gives the default.
	Socket string
	// AllowUsers are the users besides anetlinkd's own that may connect
	// (c1.allow_users, --c1-allow-user): names or numeric uids. Empty means
	// only anetlinkd's own user. Root is not implied.
	AllowUsers []string
	// Group is the group that opens the socket to those users (c1.group,
	// --c1-group): a name or numeric gid. With exactly one other user it
	// defaults to that user's primary group; with more it is required.
	Group string
	// Logf receives refusals and startup notes; nil means log.Printf.
	Logf func(format string, args ...any)
}

// DefaultSocket is $XDG_RUNTIME_DIR/anetlink/c1.sock, the same default the
// daemon's modules.anetlink.socket uses, so neither side needs configuring.
func DefaultSocket() (string, error) {
	rd := os.Getenv("XDG_RUNTIME_DIR")
	if rd == "" {
		return "", errors.New("c1: XDG_RUNTIME_DIR is not set, so there is no default socket path; " +
			"pass --c1-socket PATH (for example /run/anetlink/c1.sock) and set the same path as modules.anetlink.socket in anet")
	}
	return filepath.Join(rd, "anetlink", "c1.sock"), nil
}

// peerCredOf is peercred.Of; a variable only so a test can stand in for a
// connection from another user.
var peerCredOf = peercred.Of

// access is who may connect and how the socket file is set up (AL-2).
type access struct {
	allowed map[int]bool // uids accepted at accept time (AL-3)
	others  []int        // allowed uids other than our own
	gid     int          // group for the socket; -1 leaves it alone
	mode    fs.FileMode  // socket mode
}

func resolveAccess(o Options) (access, error) {
	self := os.Getuid()
	ac := access{allowed: map[int]bool{self: true}, gid: -1, mode: 0o600}
	primary := map[int]int{} // uid -> primary gid, when known
	for _, name := range o.AllowUsers {
		uid, gid, err := lookupUser(name)
		if err != nil {
			return access{}, err
		}
		if ac.allowed[uid] {
			continue
		}
		ac.allowed[uid] = true
		ac.others = append(ac.others, uid)
		primary[uid] = gid
	}
	sort.Ints(ac.others)
	if len(ac.others) == 0 {
		return ac, nil
	}
	ac.mode = 0o660
	switch {
	case o.Group != "":
		gid, err := lookupGroup(o.Group)
		if err != nil {
			return access{}, err
		}
		ac.gid = gid
	case len(ac.others) == 1:
		gid := primary[ac.others[0]]
		if gid < 0 {
			return access{}, fmt.Errorf("c1: uid %d in c1.allow_users has no account here, so its primary group is unknown; "+
				"set c1.group (--c1-group) to a group that user belongs to", ac.others[0])
		}
		ac.gid = gid
	default:
		return access{}, fmt.Errorf("c1: c1.allow_users names %d other users, so the socket needs a group they all belong to; "+
			"set c1.group (--c1-group)", len(ac.others))
	}
	return ac, nil
}

// lookupUser accepts a user name or a numeric uid. A numeric uid needs no
// account (containers often have none); its primary gid is then -1.
func lookupUser(name string) (uid, gid int, err error) {
	if name == "" {
		return 0, 0, errors.New("c1: c1.allow_users has an empty entry; remove it or give a user name or uid")
	}
	if n, err := strconv.Atoi(name); err == nil {
		if n < 0 {
			return 0, 0, fmt.Errorf("c1: c1.allow_users entry %q is not a uid; give a user name or a uid of 0 or more", name)
		}
		if u, err := user.LookupId(name); err == nil {
			if g, err := strconv.Atoi(u.Gid); err == nil {
				return n, g, nil
			}
		}
		return n, -1, nil
	}
	u, err := user.Lookup(name)
	if err != nil {
		return 0, 0, fmt.Errorf("c1: no user named %q on this machine (c1.allow_users); use an existing user name or a numeric uid", name)
	}
	uid, err = strconv.Atoi(u.Uid)
	if err != nil {
		return 0, 0, fmt.Errorf("c1: user %q has a non-numeric uid %q, which this platform cannot check", name, u.Uid)
	}
	gid, err = strconv.Atoi(u.Gid)
	if err != nil {
		gid = -1
	}
	return uid, gid, nil
}

func lookupGroup(name string) (int, error) {
	if n, err := strconv.Atoi(name); err == nil {
		if n < 0 {
			return 0, fmt.Errorf("c1: c1.group %q is not a gid; give a group name or a gid of 0 or more", name)
		}
		return n, nil
	}
	g, err := user.LookupGroup(name)
	if err != nil {
		return 0, fmt.Errorf("c1: no group named %q on this machine (c1.group); use an existing group name or a numeric gid", name)
	}
	return strconv.Atoi(g.Gid)
}

// Listen prepares the C1 socket (AL-1, AL-2) and returns a listener that
// hands out only connections from allowed users (AL-3). It does all of
// this before the first Accept, so a refusal to start says why at once.
func Listen(o Options) (net.Listener, error) {
	logf := o.Logf
	if logf == nil {
		logf = log.Printf
	}
	if !peercred.Supported() {
		return nil, fmt.Errorf("c1: %s cannot report who connects to a Unix socket, so C1 is not served here; "+
			"run anetlinkd on Linux, macOS or FreeBSD", goruntime.GOOS)
	}
	if o.Socket == "" {
		return nil, errors.New("c1: no socket path; pass --c1-socket PATH or set c1.socket")
	}
	if err := peercred.CheckPathLen(o.Socket); err != nil {
		return nil, fmt.Errorf("c1: %w (--c1-socket, c1.socket)", err)
	}
	ac, err := resolveAccess(o)
	if err != nil {
		return nil, err
	}
	if o.Group != "" && len(ac.others) == 0 {
		logf("c1: c1.group %q has no effect: c1.allow_users names no other user", o.Group)
	}
	dir := filepath.Dir(o.Socket)
	created, err := peercred.PrepareDir(dir)
	if err != nil {
		return nil, fmt.Errorf("c1: %w", err)
	}
	if !created && ac.gid >= 0 {
		if err := checkEnterable(dir, ac.gid); err != nil {
			return nil, err
		}
	}
	if err := peercred.ClearPath(o.Socket); err != nil {
		return nil, fmt.Errorf("c1: %w", err)
	}
	ln, err := net.Listen("unix", o.Socket)
	if err != nil {
		return nil, fmt.Errorf("c1: listen on %s: %w", o.Socket, err)
	}
	fail := func(err error) (net.Listener, error) {
		ln.Close()
		return nil, err
	}
	// Mode first, then group: the socket is never more open than intended.
	if err := os.Chmod(o.Socket, ac.mode); err != nil {
		return fail(fmt.Errorf("c1: cannot set mode %04o on %s: %w", ac.mode, o.Socket, err))
	}
	if ac.gid >= 0 {
		if err := os.Chown(o.Socket, -1, ac.gid); err != nil {
			return fail(fmt.Errorf("c1: cannot give %s to group %d (%v); run anetlinkd as root or as a member of that group, "+
				"or set c1.group to a group it belongs to", o.Socket, ac.gid, err))
		}
		// Only now, with the socket set up, may the other users enter the
		// directory anetlinkd made; an existing directory is left as it is.
		if created {
			if err := os.Chown(dir, -1, ac.gid); err != nil {
				return fail(fmt.Errorf("c1: cannot give %s to group %d: %w", dir, ac.gid, err))
			}
			if err := os.Chmod(dir, 0o710); err != nil {
				return fail(fmt.Errorf("c1: cannot set mode 0710 on %s: %w", dir, err))
			}
		}
	}
	return &peerListener{Listener: ln, allowed: ac.allowed, logf: logf, lastLog: map[int]time.Time{}}, nil
}

// checkEnterable: users reach the socket only through its directory. An
// existing directory keeps its mode (it may be /run), so if it would keep
// the allowed users out, say so now rather than have the daemon fail.
func checkEnterable(dir string, gid int) error {
	fi, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("c1: %w", err)
	}
	_, dgid, _ := peercred.OwnerOf(fi)
	perm := fi.Mode().Perm()
	if perm&0o001 != 0 || (dgid == gid && perm&0o010 != 0) {
		return nil
	}
	return fmt.Errorf("c1: the users in c1.allow_users cannot enter %s (mode %04o, group %d); "+
		"remove that directory so anetlinkd creates it, or run chgrp %d %s && chmod 0710 %s",
		dir, perm, dgid, gid, dir, dir)
}

// peerListener closes every connection whose peer uid is not allowed,
// before a byte of it is read (AL-3).
type peerListener struct {
	net.Listener
	allowed map[int]bool
	logf    func(format string, args ...any)

	mu      sync.Mutex
	lastLog map[int]time.Time
}

func (l *peerListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		cred, err := peerCredOf(c)
		if err != nil {
			c.Close()
			if l.once(-1) {
				l.logf("c1: closed a connection whose peer could not be identified (%v)", err)
			}
			continue
		}
		if !l.allowed[cred.UID] {
			c.Close()
			if l.once(cred.UID) {
				l.logf("c1_uid_refused: %s", uidRefusedSentence(cred.UID))
			}
			continue
		}
		return c, nil
	}
}

// once reports whether a refusal for uid may be logged now: at most one
// line per uid per minute, so a client retrying in a loop cannot fill the
// log.
func (l *peerListener) once(uid int) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if t, ok := l.lastLog[uid]; ok && now.Sub(t) < time.Minute {
		return false
	}
	if len(l.lastLog) > 1024 {
		for k, t := range l.lastLog {
			if now.Sub(t) >= time.Minute {
				delete(l.lastLog, k)
			}
		}
	}
	l.lastLog[uid] = now
	return true
}

// uidRefusedSentence is c1_uid_refused from the reason table (ux §1.4 J),
// in the operator's language.
func uidRefusedSentence(uid int) string {
	if chinese() {
		return fmt.Sprintf("拒绝 uid %d 连接 c1.sock(不在 allow_users 中);如需允许,在 ANetLink 配置 c1.allow_users 加入该用户。", uid)
	}
	return fmt.Sprintf("Refused uid %d on c1.sock (not in allow_users); to allow it, add the user to c1.allow_users in the ANetLink config.", uid)
}

// chinese follows the POSIX locale precedence: LC_ALL, then LC_MESSAGES,
// then LANG.
func chinese() bool {
	for _, k := range []string{"LC_ALL", "LC_MESSAGES", "LANG"} {
		if v := os.Getenv(k); v != "" {
			return strings.HasPrefix(strings.ToLower(v), "zh")
		}
	}
	return false
}
