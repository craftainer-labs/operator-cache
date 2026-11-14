package valkeyacl_test

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/craftainer-labs/operator-cache/internal/valkeyacl"
)

var bg = context.Background()

func TestConnectAndWhoami(t *testing.T) {
	f := newFake(t)
	a := connect(t, f)
	if !a.Alive() {
		t.Fatal("a fresh connection is not alive")
	}
	who, err := a.Whoami(bg)
	if err != nil || who != "admin" {
		t.Fatalf("Whoami = %q, %v", who, err)
	}
}

func TestConnectDefaultTimeout(t *testing.T) {
	f := newFake(t)
	cfg := f.cfg()
	cfg.Timeout = 0
	a, err := valkeyacl.Connect(bg, cfg)
	if err != nil {
		t.Fatal(err)
	}
	a.Close()
}

func TestConnectRefusesWhatItCannotUse(t *testing.T) {
	f := newFake(t)
	for name, mutate := range map[string]func(*valkeyacl.Config){
		"no dialer":       func(c *valkeyacl.Config) { c.Dial = nil },
		"a CA of no cert": func(c *valkeyacl.Config) { c.CA = []byte("not pem") },
		"no port":         func(c *valkeyacl.Config) { c.Address = "valkey.test" },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := f.cfg()
			mutate(&cfg)
			if _, err := valkeyacl.Connect(bg, cfg); !errors.Is(err, valkeyacl.ErrInvalid) {
				t.Fatalf("err = %v, want ErrInvalid", err)
			}
		})
	}
}

func TestConnectWrongPasswordIsAuthAndLeaksNothing(t *testing.T) {
	f := newFake(t)
	cfg := f.cfg()
	cfg.Password = "not-the-password"
	_, err := valkeyacl.Connect(bg, cfg)
	if !errors.Is(err, valkeyacl.ErrAuth) {
		t.Fatalf("err = %v, want ErrAuth", err)
	}
	if strings.Contains(err.Error(), cfg.Password) || strings.Contains(err.Error(), f.adminPW) {
		t.Fatalf("the error holds a password: %v", err)
	}
}

func TestConnectNeedsAnACLRight(t *testing.T) {
	f := newFake(t)
	f.failWith = "NOPERM this user has no permissions to run the 'acl' command"
	if _, err := valkeyacl.Connect(bg, f.cfg()); !errors.Is(err, valkeyacl.ErrAuth) {
		t.Fatalf("err = %v, want ErrAuth", err)
	}
}

func TestConnectVerifiesTheCertificate(t *testing.T) {
	f := newFake(t)
	t.Run("a CA that did not sign it", func(t *testing.T) {
		cfg := f.cfg()
		cfg.CA = newPKI(t).ca
		_, err := valkeyacl.Connect(bg, cfg)
		var verify *tls.CertificateVerificationError
		if !errors.Is(err, valkeyacl.ErrUnavailable) || !errors.As(err, &verify) {
			t.Fatalf("err = %v, want ErrUnavailable wrapping a verification error", err)
		}
	})
	t.Run("the system roots when there is no CA", func(t *testing.T) {
		cfg := f.cfg()
		cfg.CA = nil
		if _, err := valkeyacl.Connect(bg, cfg); !errors.Is(err, valkeyacl.ErrUnavailable) {
			t.Fatalf("err = %v, want ErrUnavailable", err)
		}
	})
	t.Run("another host name", func(t *testing.T) {
		cfg := f.cfg()
		cfg.ServerName = "other.test"
		if _, err := valkeyacl.Connect(bg, cfg); !errors.Is(err, valkeyacl.ErrUnavailable) {
			t.Fatalf("err = %v, want ErrUnavailable", err)
		}
	})
	t.Run("the host of the address by default", func(t *testing.T) {
		cfg := f.cfg()
		cfg.ServerName = ""
		a, err := valkeyacl.Connect(bg, cfg)
		if err != nil {
			t.Fatal(err)
		}
		a.Close()
	})
}

func TestConnectDialFailures(t *testing.T) {
	f := newFake(t)
	t.Run("the dialer refuses", func(t *testing.T) {
		cfg := f.cfg()
		cfg.Dial = func(context.Context, string, string) (net.Conn, error) { return nil, errors.New("blocked") }
		if _, err := valkeyacl.Connect(bg, cfg); !errors.Is(err, valkeyacl.ErrUnavailable) {
			t.Fatalf("err = %v, want ErrUnavailable", err)
		}
	})
	t.Run("the dialer is too slow", func(t *testing.T) {
		cfg := f.cfg()
		cfg.Timeout = 100 * time.Millisecond
		cfg.Dial = func(ctx context.Context, _, _ string) (net.Conn, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		if _, err := valkeyacl.Connect(bg, cfg); !errors.Is(err, valkeyacl.ErrTimeout) {
			t.Fatalf("err = %v, want ErrTimeout", err)
		}
	})
}

func TestUserRoundTrip(t *testing.T) {
	f := newFake(t)
	a := connect(t, f)
	if _, err := a.GetUser(bg, "u1"); !errors.Is(err, valkeyacl.ErrNotFound) {
		t.Fatalf("GetUser of nobody: %v, want ErrNotFound", err)
	}
	tenant := valkeyacl.Tenant{Prefix: "team-a", Write: true, Channels: true}
	rules, err := tenant.Rules()
	if err != nil {
		t.Fatal(err)
	}
	if err := a.SetUser(bg, "u1", append(rules, valkeyacl.AddPassword("pw1"))...); err != nil {
		t.Fatal(err)
	}
	u, err := a.GetUser(bg, "u1")
	if err != nil {
		t.Fatal(err)
	}
	if !u.Has("on") || u.Has("nopass") || len(u.PasswordHashes) != 1 || u.PasswordHashes[0] != valkeyacl.HashPassword("pw1") {
		t.Fatalf("user = %+v", u)
	}
	if !tenant.Matches(u) {
		t.Fatalf("the user does not match its rules: %+v", u)
	}
	// Rotation: a second password, then only the new one.
	if err := a.SetUser(bg, "u1", valkeyacl.AddPassword("pw2")); err != nil {
		t.Fatal(err)
	}
	if u, _ = a.GetUser(bg, "u1"); len(u.PasswordHashes) != 2 {
		t.Fatalf("hashes = %v, want both passwords", u.PasswordHashes)
	}
	if err := a.SetUser(bg, "u1", valkeyacl.KeepOnly(valkeyacl.HashPassword("pw2"))...); err != nil {
		t.Fatal(err)
	}
	if u, _ = a.GetUser(bg, "u1"); len(u.PasswordHashes) != 1 || u.PasswordHashes[0] != valkeyacl.HashPassword("pw2") {
		t.Fatalf("hashes = %v, want only the new one", u.PasswordHashes)
	}
	if deleted, err := a.DelUser(bg, "u1"); err != nil || !deleted {
		t.Fatalf("DelUser = %v, %v", deleted, err)
	}
	if deleted, err := a.DelUser(bg, "u1"); err != nil || deleted {
		t.Fatalf("DelUser of nobody = %v, %v", deleted, err)
	}
}

func TestInvalidNamesNeverReachTheServer(t *testing.T) {
	f := newFake(t)
	a := connect(t, f)
	before := len(f.commands())
	for _, name := range []string{"", "default", "a b", "a\nb", "a\"b", "a'b", "a\x7fb"} {
		if _, err := a.GetUser(bg, name); !errors.Is(err, valkeyacl.ErrInvalid) {
			t.Errorf("GetUser(%q) = %v", name, err)
		}
		if err := a.SetUser(bg, name, "on"); !errors.Is(err, valkeyacl.ErrInvalid) {
			t.Errorf("SetUser(%q) = %v", name, err)
		}
		if _, err := a.DelUser(bg, name); !errors.Is(err, valkeyacl.ErrInvalid) {
			t.Errorf("DelUser(%q) = %v", name, err)
		}
	}
	if after := len(f.commands()); after != before {
		t.Fatalf("%d commands reached the server", after-before)
	}
}

func TestRefusedModifierDoesNotEchoThePassword(t *testing.T) {
	f := newFake(t)
	a := connect(t, f)
	err := a.SetUser(bg, "u1", "bad-modifier-with-secret-pw")
	if !errors.Is(err, valkeyacl.ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
	if strings.Contains(err.Error(), "secret") {
		t.Fatalf("the error echoes the modifier: %v", err)
	}
}

func TestServerErrorsAreKept(t *testing.T) {
	f := newFake(t)
	a := connect(t, f)
	f.mu.Lock()
	f.failWith = "NOPERM this user has no permissions to run the 'acl|getuser' command"
	f.mu.Unlock()
	if _, err := a.GetUser(bg, "u1"); !errors.Is(err, valkeyacl.ErrAuth) || !strings.Contains(err.Error(), "NOPERM") {
		t.Fatalf("err = %v, want ErrAuth with the server's words", err)
	}
	for line, kind := range map[string]error{
		"ERR something is wrong": valkeyacl.ErrInvalid,
		"LOADING in progress":    valkeyacl.ErrServer,
	} {
		f.mu.Lock()
		f.failWith = line
		f.mu.Unlock()
		if _, err := a.DelUser(bg, "u1"); !errors.Is(err, kind) {
			t.Errorf("%q: err = %v, want %v", line, err, kind)
		}
	}
	if !a.Alive() {
		t.Fatal("an error reply is not a lost connection")
	}
}

func TestMalformedAnswers(t *testing.T) {
	f := newFake(t)
	a := connect(t, f)
	for _, c := range []struct{ name, garbage string }{
		{"an odd list", list(bulk("flags"))},
		{"a name that is not text", list(":1\r\n", bulk("x"))},
		{"flags that are not a list", list(bulk("flags"), bulk("on"))},
		{"a number for commands", list(bulk("commands"), ":1\r\n")},
		{"not a list", bulk("hello")},
	} {
		f.mu.Lock()
		f.garbage = c.garbage
		f.mu.Unlock()
		if _, err := a.GetUser(bg, "u1"); !errors.Is(err, valkeyacl.ErrServer) {
			t.Errorf("%s: err = %v, want ErrServer", c.name, err)
		}
	}
}

func TestDelUserAnswerNotANumber(t *testing.T) {
	f := newFake(t)
	a := connect(t, f)
	f.mu.Lock()
	f.garbage = "+OK\r\n" // a simple string where a number belongs
	f.mu.Unlock()
	if _, err := a.DelUser(bg, "u1"); !errors.Is(err, valkeyacl.ErrServer) {
		t.Fatalf("err = %v, want ErrServer", err)
	}
}

func TestSlowServerIsATimeoutAndDropsAreNotAlive(t *testing.T) {
	f := newFake(t)
	cfg := f.cfg()
	cfg.Timeout = 300 * time.Millisecond
	a, err := valkeyacl.Connect(bg, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	f.mu.Lock()
	f.hang = "GETUSER"
	f.mu.Unlock()
	if _, err := a.GetUser(bg, "u1"); !errors.Is(err, valkeyacl.ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
	if a.Alive() {
		t.Fatal("a command that got no answer leaves the connection alive")
	}
	a.Close()
	a.Close()
	if a.Alive() {
		t.Fatal("a closed connection is alive")
	}
}

func TestTenantRules(t *testing.T) {
	cases := []struct {
		name   string
		tenant valkeyacl.Tenant
		want   []string
	}{
		{"read only", valkeyacl.Tenant{Prefix: "a"}, []string{
			"on", "clearselectors", "resetkeys", "%R~a:*", "resetchannels",
			"-@all", "+@connection", "+@read", "-@dangerous", "-@admin", "-scan", "-randomkey",
		}},
		{"read write", valkeyacl.Tenant{Prefix: "team-1", Write: true}, []string{
			"on", "clearselectors", "resetkeys", "~team-1:*", "resetchannels",
			"-@all", "+@connection", "+@read", "+@write", "-@dangerous", "-@admin", "-scan", "-randomkey",
		}},
		{"channels", valkeyacl.Tenant{Prefix: "x1", Write: true, Channels: true}, []string{
			"on", "clearselectors", "resetkeys", "~x1:*", "resetchannels", "&x1:*",
			"-@all", "+@connection", "+@read", "+@write", "+@pubsub", "-@dangerous", "-@admin", "-scan", "-randomkey", "-pubsub",
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := c.tenant.Rules()
			if err != nil {
				t.Fatal(err)
			}
			if strings.Join(got, " ") != strings.Join(c.want, " ") {
				t.Fatalf("rules = %v\nwant    %v", got, c.want)
			}
			// What no tenant is ever granted.
			for _, r := range got {
				if r == "+@all" || r == "+@dangerous" || r == "+@admin" || r == "allkeys" || r == "allchannels" ||
					r == "allcommands" || r == "~*" || r == "&*" || strings.HasPrefix(r, ">") || r == "nopass" {
					t.Fatalf("rule %q is never granted", r)
				}
			}
		})
	}
}

func TestTenantRefusesPrefixesThatWouldBeGlobs(t *testing.T) {
	for _, p := range []string{"", "*", "a*", "a?b", "a[b]", "A", "-a", "a-", "a:b", "a b", "%", "a\\b", "a.b"} {
		if _, err := (valkeyacl.Tenant{Prefix: p}).Rules(); !errors.Is(err, valkeyacl.ErrInvalid) {
			t.Errorf("prefix %q: err = %v, want ErrInvalid", p, err)
		}
	}
}

func TestTenantMatches(t *testing.T) {
	tenant := valkeyacl.Tenant{Prefix: "a", Write: true}
	ok := valkeyacl.UserInfo{Flags: []string{"on"}, Keys: tenant.Keys(), Commands: tenant.Commands()}
	if !tenant.Matches(ok) {
		t.Fatal("a user made from the rules does not match")
	}
	for name, mutate := range map[string]func(*valkeyacl.UserInfo){
		"off":            func(u *valkeyacl.UserInfo) { u.Flags = []string{"off"} },
		"nopass":         func(u *valkeyacl.UserInfo) { u.Flags = []string{"on", "nopass"} },
		"other keys":     func(u *valkeyacl.UserInfo) { u.Keys = "~*" },
		"other channels": func(u *valkeyacl.UserInfo) { u.Channels = "&*" },
		"other commands": func(u *valkeyacl.UserInfo) { u.Commands = "+@all" },
	} {
		u := ok
		mutate(&u)
		if tenant.Matches(u) {
			t.Errorf("%s: matches", name)
		}
	}
}

func TestHashPassword(t *testing.T) {
	// SHA-256 of "abc".
	if got := valkeyacl.HashPassword("abc"); got != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Fatalf("hash = %s", got)
	}
}

func TestLostConnectionIsUnavailableAndNotAlive(t *testing.T) {
	f := newFake(t)
	f.dropAfter = 1 // the login's WHOAMI is the first ACL command
	a := connect(t, f)
	if _, err := a.GetUser(bg, "u1"); !errors.Is(err, valkeyacl.ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
	if a.Alive() {
		t.Fatal("a connection the server dropped is alive")
	}
}
