//go:build integration

// Package integration runs the operator's ACL client against a real Valkey with
// TLS and ACLs on (see README.md here): the same valkeyacl.Admin the controller
// uses, so what the fake server of the unit tests only claims is checked against
// the server. Every command the operator sends, the answers for missing things,
// the overlap of the two passwords of a rotation, and what a tenant's ACL lets a
// real client do (and, above all, not do) are asked of the real thing.
package integration

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/valkey-io/valkey-go"

	"github.com/craftainer-labs/operator-cache/internal/valkeyacl"
)

var bg = context.Background()

// server is the Valkey under test, from the environment.
type server struct {
	address  string
	host     string
	ca       []byte
	user     string
	password string
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// readFile reads a file named by the test environment through an os.Root on
// its directory, so the read cannot leave that directory.
func readFile(t *testing.T, path string) []byte {
	t.Helper()
	dir, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dir.Close() }()
	b, err := dir.ReadFile(filepath.Base(path))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// setting is the value of an environment variable, or the content of the file
// that KEY_FILE names. A missing one fails the test: running the tag without a
// server is a mistake, not a reason to skip.
func setting(t *testing.T, key string) string {
	t.Helper()
	if v := os.Getenv(key); v != "" {
		return v
	}
	file := os.Getenv(key + "_FILE")
	if file == "" {
		t.Fatalf("%s or %s_FILE is not set; see test/integration/README.md", key, key)
	}
	return strings.TrimSpace(string(readFile(t, file)))
}

// load reads the connection settings and waits for the server.
func load(t *testing.T) server {
	t.Helper()
	caFile := os.Getenv("VALKEY_CA_FILE")
	if caFile == "" {
		t.Fatal("VALKEY_CA_FILE is not set; see test/integration/README.md")
	}
	address := env("VALKEY_ADDRESS", "localhost:6379")
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		t.Fatal(err)
	}
	s := server{
		address:  address,
		host:     host,
		ca:       readFile(t, caFile),
		user:     env("VALKEY_ADMIN_USERNAME", "admin"),
		password: setting(t, "VALKEY_ADMIN_PASSWORD"),
	}
	s.waitReady(t)
	return s
}

// config is a verified connection as the administrator. It dials directly: the
// egress policy refuses loopback and is tested on its own; this lane tests the
// server's answers and the TLS handshake.
func (s server) config() valkeyacl.Config {
	return valkeyacl.Config{
		Address: s.address, CA: s.ca, Username: s.user, Password: s.password,
		Dial:    (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
		Timeout: 10 * time.Second,
	}
}

// waitReady waits until the administrator can log in: the server takes a moment
// to load its ACL file and open the port after the container starts.
func (s server) waitReady(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for {
		a, err := valkeyacl.Connect(bg, s.config())
		if err == nil {
			a.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the server at %s is not ready: %v", s.address, err)
		}
		time.Sleep(time.Second)
	}
}

// admin connects as the administrator for the length of the test.
func admin(t *testing.T) (*valkeyacl.Admin, server) {
	t.Helper()
	s := load(t)
	a, err := valkeyacl.Connect(bg, s.config())
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(a.Close)
	return a, s
}

// random is a short random lowercase hex string, for names of a test's own.
func random(t *testing.T) string {
	t.Helper()
	b := make([]byte, 5)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

// newTenantUser makes a user from the tenant's rules with one password and
// removes it when the test ends. It returns the user's name.
func newTenantUser(t *testing.T, a *valkeyacl.Admin, tenant valkeyacl.Tenant, password string) string {
	t.Helper()
	name := "it-" + random(t)
	rules, err := tenant.Rules()
	if err != nil {
		t.Fatal(err)
	}
	if err := a.SetUser(bg, name, append([]string{"reset"}, append(rules, valkeyacl.AddPassword(password))...)...); err != nil {
		t.Fatalf("SetUser: %v", err)
	}
	t.Cleanup(func() { _, _ = a.DelUser(bg, name) })
	return name
}

// login is a client of an application: a verified connection as the user, which
// may run only what its ACL allows.
func (s server) login(user, password string) (valkey.Client, error) {
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(s.ca)
	return valkey.NewClient(valkey.ClientOption{
		InitAddress:       []string{s.address},
		Username:          user,
		Password:          password,
		ForceSingleClient: true,
		AlwaysRESP2:       true,
		DisableCache:      true,
		DisableRetry:      true,
		TLSConfig:         &tls.Config{RootCAs: pool, ServerName: s.host, MinVersion: tls.VersionTLS12},
	})
}

// run is one command of an application's client; the error is the server's.
func run(c valkey.Client, args ...string) error {
	ctx, cancel := context.WithTimeout(bg, 10*time.Second)
	defer cancel()
	return c.Do(ctx, c.B().Arbitrary(args[0]).Args(args[1:]...).Build()).Error()
}

func denied(t *testing.T, c valkey.Client, args ...string) {
	t.Helper()
	err := run(c, args...)
	if verr, ok := valkey.IsValkeyErr(err); !ok || !strings.HasPrefix(verr.Error(), "NOPERM") {
		t.Errorf("%v: err = %v, want NOPERM", args, err)
	}
}

func allowed(t *testing.T, c valkey.Client, args ...string) {
	t.Helper()
	if err := run(c, args...); err != nil {
		t.Errorf("%v: %v", args, err)
	}
}

func loginOK(t *testing.T, s server, user, password string) valkey.Client {
	t.Helper()
	c, err := s.login(user, password)
	if err != nil {
		t.Fatalf("login as %s: %v", user, err)
	}
	t.Cleanup(c.Close)
	return c
}

func TestAdminLogin(t *testing.T) {
	a, s := admin(t)
	who, err := a.Whoami(bg)
	if err != nil || who != s.user {
		t.Fatalf("Whoami = %q, %v", who, err)
	}
	if !a.Alive() {
		t.Fatal("not alive")
	}
}

func TestWrongCredentialsAreAuthAndLeakNothing(t *testing.T) {
	s := load(t)
	for name, mutate := range map[string]func(*valkeyacl.Config){
		"a wrong password": func(c *valkeyacl.Config) { c.Password = "wrong-password-0123" },
		"an unknown user":  func(c *valkeyacl.Config) { c.Username = "nobody-0123" },
		"the default user": func(c *valkeyacl.Config) { c.Username = "default" },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := s.config()
			mutate(&cfg)
			_, err := valkeyacl.Connect(bg, cfg)
			if !errors.Is(err, valkeyacl.ErrAuth) {
				t.Fatalf("err = %v, want ErrAuth", err)
			}
			for _, secret := range []string{cfg.Password, s.password} {
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("the error holds a password: %v", err)
				}
			}
		})
	}
}

func TestTLSIsVerified(t *testing.T) {
	s := load(t)
	t.Run("the system roots do not know the CA", func(t *testing.T) {
		cfg := s.config()
		cfg.CA = nil
		var unknown x509.UnknownAuthorityError
		_, err := valkeyacl.Connect(bg, cfg)
		if !errors.Is(err, valkeyacl.ErrUnavailable) || !errors.As(err, &unknown) {
			t.Fatalf("err = %v, want an unknown authority", err)
		}
	})
	t.Run("a wrong host name", func(t *testing.T) {
		cfg := s.config()
		cfg.ServerName = "not-the-server.example"
		var host x509.HostnameError
		_, err := valkeyacl.Connect(bg, cfg)
		if !errors.Is(err, valkeyacl.ErrUnavailable) || !errors.As(err, &host) {
			t.Fatalf("err = %v, want a host name error", err)
		}
	})
	t.Run("a plain connection is not served", func(t *testing.T) {
		c, err := net.DialTimeout("tcp", s.address, 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = c.Close() }()
		_ = c.SetDeadline(time.Now().Add(3 * time.Second))
		if _, err := c.Write([]byte("PING\r\n")); err != nil {
			return
		}
		buf := make([]byte, 16)
		if n, _ := c.Read(buf); strings.Contains(string(buf[:n]), "PONG") {
			t.Fatalf("a plain PING was answered: %q", buf[:n])
		}
	})
}

func TestUserLifecycle(t *testing.T) {
	a, _ := admin(t)
	name := "it-" + random(t)
	if _, err := a.GetUser(bg, name); !errors.Is(err, valkeyacl.ErrNotFound) {
		t.Fatalf("GetUser of nobody: %v, want ErrNotFound", err)
	}
	if deleted, err := a.DelUser(bg, name); err != nil || deleted {
		t.Fatalf("DelUser of nobody = %v, %v", deleted, err)
	}
	tenant := valkeyacl.Tenant{Prefix: "team-a", Write: true, Channels: true}
	rules, err := tenant.Rules()
	if err != nil {
		t.Fatal(err)
	}
	if err := a.SetUser(bg, name, append(rules, valkeyacl.AddPassword("first-password-0123456789"))...); err != nil {
		t.Fatal(err)
	}
	u, err := a.GetUser(bg, name)
	if err != nil {
		t.Fatal(err)
	}
	if !tenant.Matches(u) {
		t.Fatalf("the server shows the user as %+v; the tenant expects keys %q, channels %q, commands %q",
			u, tenant.Keys(), tenant.ChannelPattern(), tenant.Commands())
	}
	if len(u.PasswordHashes) != 1 || u.PasswordHashes[0] != valkeyacl.HashPassword("first-password-0123456789") {
		t.Fatalf("password hashes = %v", u.PasswordHashes)
	}
	// The same rules again change nothing, and the rules of a read-only
	// tenant are shown as theirs.
	if err := a.SetUser(bg, name, rules...); err != nil {
		t.Fatal(err)
	}
	if u, _ = a.GetUser(bg, name); !tenant.Matches(u) || len(u.PasswordHashes) != 1 {
		t.Fatalf("after the same rules: %+v", u)
	}
	ro := valkeyacl.Tenant{Prefix: "team-a"}
	roRules, _ := ro.Rules()
	if err := a.SetUser(bg, name, roRules...); err != nil {
		t.Fatal(err)
	}
	if u, _ = a.GetUser(bg, name); !ro.Matches(u) || tenant.Matches(u) {
		t.Fatalf("after the read-only rules: %+v (want keys %q, commands %q)", u, ro.Keys(), ro.Commands())
	}
	if deleted, err := a.DelUser(bg, name); err != nil || !deleted {
		t.Fatalf("DelUser = %v, %v", deleted, err)
	}
	if _, err := a.GetUser(bg, name); !errors.Is(err, valkeyacl.ErrNotFound) {
		t.Fatalf("GetUser after delete: %v, want ErrNotFound", err)
	}
}

func TestRefusedRuleIsInvalidAndLeaksNothing(t *testing.T) {
	a, _ := admin(t)
	name := "it-" + random(t)
	t.Cleanup(func() { _, _ = a.DelUser(bg, name) })
	err := a.SetUser(bg, name, "on", "-@no-such-category-secret-pw")
	if !errors.Is(err, valkeyacl.ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
	if strings.Contains(err.Error(), "secret-pw") {
		t.Fatalf("the error echoes the rule: %v", err)
	}
}

func TestReadWriteTenantIsConfinedToItsPrefix(t *testing.T) {
	a, s := admin(t)
	const pw = "tenant-password-0123456789abcdef"
	name := newTenantUser(t, a, valkeyacl.Tenant{Prefix: "team-a", Write: true}, pw)
	c := loginOK(t, s, name, pw)

	allowed(t, c, "SET", "team-a:k", "v")
	allowed(t, c, "GET", "team-a:k")
	allowed(t, c, "DEL", "team-a:k")
	allowed(t, c, "PING")
	allowed(t, c, "SET", "team-a:", "empty-suffix")
	// Another tenant's keys, and a name that only begins like the prefix.
	denied(t, c, "SET", "team-b:k", "v")
	denied(t, c, "GET", "team-b:k")
	denied(t, c, "SET", "team-ab:k", "v")
	denied(t, c, "SET", "team-a", "v")
	denied(t, c, "SET", "k", "v")
	// A command that reaches several keys is refused when any is outside.
	denied(t, c, "MGET", "team-a:k", "team-b:k")
	denied(t, c, "RENAME", "team-a:k", "team-b:k")
	denied(t, c, "COPY", "team-a:k", "team-b:k")
	// What is never granted.
	for _, cmd := range [][]string{
		{"KEYS", "*"}, {"SCAN", "0"}, {"RANDOMKEY"}, {"FLUSHALL"}, {"FLUSHDB"}, {"INFO"}, {"CONFIG", "GET", "*"},
		{"ACL", "WHOAMI"}, {"ACL", "SETUSER", "x", "on"}, {"CLIENT", "LIST"}, {"SHUTDOWN"},
		{"EVAL", "return 1", "0"}, {"SCRIPT", "FLUSH"}, {"MODULE", "LIST"}, {"SLAVEOF", "NO", "ONE"},
		{"PUBLISH", "team-a:c", "m"}, {"PUBSUB", "CHANNELS"},
		// What the categories hold that reaches beyond the prefix.
		{"DBSIZE"}, {"CLIENT", "TRACKING", "ON", "BCAST"}, {"FUNCTION", "FLUSH"}, {"FUNCTION", "DELETE", "x"},
		{"FUNCTION", "LOAD", "#!lua name=x\nredis.register_function('x', function() return 1 end)"},
		{"OBJECT", "ENCODING", "team-b:k"}, {"DUMP", "team-b:k"}, {"SORT_RO", "team-a:k", "BY", "team-b:*"},
		{"MIGRATE", "localhost", "6379", "team-a:k", "0", "100"}, {"MONITOR"},
	} {
		denied(t, c, cmd...)
	}
}

func TestReadOnlyTenantCannotWrite(t *testing.T) {
	a, s := admin(t)
	const pw = "tenant-password-0123456789abcdef"
	// A key written by someone who may.
	rw := newTenantUser(t, a, valkeyacl.Tenant{Prefix: "team-r", Write: true}, pw)
	allowed(t, loginOK(t, s, rw, pw), "SET", "team-r:k", "v")

	name := newTenantUser(t, a, valkeyacl.Tenant{Prefix: "team-r"}, pw)
	c := loginOK(t, s, name, pw)
	allowed(t, c, "GET", "team-r:k")
	allowed(t, c, "EXISTS", "team-r:k")
	denied(t, c, "SET", "team-r:k", "other")
	denied(t, c, "DEL", "team-r:k")
	denied(t, c, "EXPIRE", "team-r:k", "1")
	denied(t, c, "GET", "team-x:k")
	denied(t, c, "KEYS", "*")
	denied(t, c, "FLUSHALL")
}

func TestChannelsAreConfinedToThePrefix(t *testing.T) {
	a, s := admin(t)
	const pw = "tenant-password-0123456789abcdef"
	name := newTenantUser(t, a, valkeyacl.Tenant{Prefix: "team-c", Write: true, Channels: true}, pw)
	c := loginOK(t, s, name, pw)
	allowed(t, c, "PUBLISH", "team-c:events", "m")
	denied(t, c, "PUBLISH", "team-d:events", "m")
	denied(t, c, "PUBLISH", "events", "m")
	denied(t, c, "PUBSUB", "CHANNELS")
	// Keys still follow the key prefix.
	allowed(t, c, "SET", "team-c:k", "v")
	denied(t, c, "SET", "team-d:k", "v")
	denied(t, c, "KEYS", "*")

	// Without channels there is no publish at all.
	plain := newTenantUser(t, a, valkeyacl.Tenant{Prefix: "team-c", Write: true}, pw)
	denied(t, loginOK(t, s, plain, pw), "PUBLISH", "team-c:events", "m")
}

func TestRotationOverlapsTwoPasswords(t *testing.T) {
	a, s := admin(t)
	const oldPW, newPW = "old-password-0123456789abcdef0123", "new-password-0123456789abcdef0123"
	tenant := valkeyacl.Tenant{Prefix: "team-p", Write: true}
	name := newTenantUser(t, a, tenant, oldPW)
	oldClient := loginOK(t, s, name, oldPW)

	// Phase one: the new password is added; both log in, and the rules and
	// the session in use are untouched.
	if err := a.SetUser(bg, name, valkeyacl.AddPassword(newPW)); err != nil {
		t.Fatal(err)
	}
	u, err := a.GetUser(bg, name)
	if err != nil || len(u.PasswordHashes) != 2 || !tenant.Matches(u) {
		t.Fatalf("after adding: %+v, %v", u, err)
	}
	loginOK(t, s, name, oldPW)
	newClient := loginOK(t, s, name, newPW)
	allowed(t, oldClient, "SET", "team-p:k", "v")

	// Phase two: only the new password is kept, by its hash.
	if err := a.SetUser(bg, name, valkeyacl.KeepOnly(valkeyacl.HashPassword(newPW))...); err != nil {
		t.Fatal(err)
	}
	u, err = a.GetUser(bg, name)
	if err != nil || len(u.PasswordHashes) != 1 || u.PasswordHashes[0] != valkeyacl.HashPassword(newPW) || !tenant.Matches(u) {
		t.Fatalf("after retiring: %+v, %v", u, err)
	}
	if _, err := s.login(name, oldPW); err == nil {
		t.Fatal("the old password still logs in")
	}
	c2, err := s.login(name, newPW)
	if err != nil {
		t.Fatalf("the new password does not log in: %v", err)
	}
	c2.Close()
	allowed(t, newClient, "GET", "team-p:k")
}

func TestUpdateKeepsThePasswordAndRepairsTheRules(t *testing.T) {
	a, s := admin(t)
	const pw = "tenant-password-0123456789abcdef"
	tenant := valkeyacl.Tenant{Prefix: "team-u", Write: true}
	name := newTenantUser(t, a, tenant, pw)

	// Someone widens the user by hand.
	if err := a.SetUser(bg, name, "allkeys", "+@all"); err != nil {
		t.Fatal(err)
	}
	u, err := a.GetUser(bg, name)
	if err != nil || tenant.Matches(u) {
		t.Fatalf("a widened user matches its rules: %+v, %v", u, err)
	}
	rules, _ := tenant.Rules()
	if err := a.SetUser(bg, name, rules...); err != nil {
		t.Fatal(err)
	}
	if u, err = a.GetUser(bg, name); err != nil || !tenant.Matches(u) {
		t.Fatalf("after the rules again: %+v, %v", u, err)
	}
	c := loginOK(t, s, name, pw)
	allowed(t, c, "SET", "team-u:k", "v")
	denied(t, c, "SET", "other:k", "v")
	denied(t, c, "FLUSHALL")
}

func TestDeleteEndsTheSessionsAndLeavesOthersAlone(t *testing.T) {
	a, s := admin(t)
	const pw = "tenant-password-0123456789abcdef"
	tenant := valkeyacl.Tenant{Prefix: "team-g", Write: true}
	mine := newTenantUser(t, a, tenant, pw)
	other := newTenantUser(t, a, tenant, pw)
	mineClient := loginOK(t, s, mine, pw)
	otherClient := loginOK(t, s, other, pw)
	allowed(t, mineClient, "SET", "team-g:k", "v")

	if deleted, err := a.DelUser(bg, mine); err != nil || !deleted {
		t.Fatalf("DelUser = %v, %v", deleted, err)
	}
	deadline := time.Now().Add(settle)
	for run(mineClient, "GET", "team-g:k") == nil {
		if time.Now().After(deadline) {
			t.Fatal("the deleted user's session still works")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, err := s.login(mine, pw); err == nil {
		t.Fatal("the deleted user logs in")
	}
	// Another user of the same rules is untouched.
	allowed(t, otherClient, "GET", "team-g:k")
	if _, err := a.GetUser(bg, other); err != nil {
		t.Fatalf("the other user: %v", err)
	}
}

func TestUsersOfTheServerAreNeverTouchedByOurNames(t *testing.T) {
	a, _ := admin(t)
	// A user someone else made: the client only ever acts on the name it is
	// given, and refuses the reserved ones.
	foreign := "it-foreign-" + random(t)
	if err := a.SetUser(bg, foreign, "on", ">foreign-password-0123", "~foreign:*", "+get"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = a.DelUser(bg, foreign) })
	if _, err := a.DelUser(bg, "default"); !errors.Is(err, valkeyacl.ErrInvalid) {
		t.Fatalf("DelUser(default) = %v, want ErrInvalid", err)
	}
	if _, err := a.GetUser(bg, foreign); err != nil {
		t.Fatalf("the foreign user: %v", err)
	}
}

// settle is how long a test waits for the server to act on a command it has
// answered: a session it ends.
const settle = 5 * time.Second
