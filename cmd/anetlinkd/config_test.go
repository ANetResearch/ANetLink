package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "anetlinkd.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// AL-11, ux §2.4: the c1 section, and the command line winning over it.
func TestC1ConfigAndFlags(t *testing.T) {
	cfg, err := loadConfig(writeConfig(t, `{
	  "c1": {"socket": "/run/anetlink/c1.sock", "allow_users": ["alice", "1002"],
	         "group": "anet", "remote": ["light.onoff@sim/*"]}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	o := cfg.c1Options(c1Flags{})
	if o.Socket != "/run/anetlink/c1.sock" || o.Group != "anet" ||
		!reflect.DeepEqual(o.AllowUsers, []string{"alice", "1002"}) {
		t.Fatalf("from file: %+v", o)
	}
	rc, err := cfg.runtimeConfig()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(rc.Remote, []string{"light.onoff@sim/*"}) {
		t.Fatalf("remote: %v", rc.Remote)
	}

	o = cfg.c1Options(c1Flags{socket: "/tmp/x/c1.sock", allowUsers: stringList{"bob"}, group: "staff"})
	if o.Socket != "/tmp/x/c1.sock" || o.Group != "staff" || !reflect.DeepEqual(o.AllowUsers, []string{"bob"}) {
		t.Fatalf("flags must win: %+v", o)
	}
}

// The defaults are the safe ones: no section means own user only and no
// remote access.
func TestC1DefaultsAreClosed(t *testing.T) {
	cfg, err := loadConfig("")
	if err != nil {
		t.Fatal(err)
	}
	o := cfg.c1Options(c1Flags{})
	if o.Socket != "" || len(o.AllowUsers) != 0 || o.Group != "" {
		t.Fatalf("defaults: %+v", o)
	}
	rc, err := cfg.runtimeConfig()
	if err != nil {
		t.Fatal(err)
	}
	if len(rc.Remote) != 0 {
		t.Fatalf("remote must default to none: %v", rc.Remote)
	}
}

// A typo in the c1 section is an error, not a silently ignored key: the
// operator meant to configure access and would get something else.
func TestC1SectionIsStrict(t *testing.T) {
	_, err := loadConfig(writeConfig(t, `{"c1": {"remotes": ["*/*"]}}`))
	if err == nil || !strings.Contains(err.Error(), "remotes") {
		t.Fatalf("unknown c1 key: %v", err)
	}
	_, err = loadConfig(writeConfig(t, `{"c1": {"allow_users": "alice"}}`))
	if err == nil {
		t.Fatal("allow_users must be a list")
	}
}

func TestC1RemotePatternsAreChecked(t *testing.T) {
	for _, bad := range []string{`["["]`, `["light.onoff"]`, `[""]`} {
		cfg, err := loadConfig(writeConfig(t, `{"c1": {"remote": `+bad+`}}`))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := cfg.runtimeConfig(); err == nil {
			t.Errorf("remote %s must be refused at startup", bad)
		}
	}
}

func TestRepeatableAllowUserFlag(t *testing.T) {
	var l stringList
	for _, v := range []string{"alice", "bob"} {
		if err := l.Set(v); err != nil {
			t.Fatal(err)
		}
	}
	if !reflect.DeepEqual([]string(l), []string{"alice", "bob"}) || l.String() != "alice,bob" {
		t.Fatalf("stringList: %v %q", l, l.String())
	}
}

func TestVersion(t *testing.T) {
	if version != "0.2.0" {
		t.Fatalf("version %s, want 0.2.0 (C1 v1)", version)
	}
}
