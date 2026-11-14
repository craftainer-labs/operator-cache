// Package valkeyacl is the operator's client of the ACL commands of Valkey and
// Redis (ACL SETUSER, GETUSER, DELUSER and WHOAMI), over TLS that verifies the
// server's certificate chain and host name, through the dialer it is given (the
// egress policy's HostDialer, which refuses blocked addresses after name
// resolution and never reads the proxy environment variables).
//
// It is built on valkey-go (Apache-2.0). The connection speaks RESP2, so
// ACL GETUSER answers the same flat array on Valkey and on Redis, and has no
// client-side cache. valkey-go multiplexes every command of a client over a
// pipelined connection that it re-dials when it breaks, so one Admin is safe
// for concurrent use; what it does not do is replace the connection when the
// credentials, the CA or the proxy change, or close it when nobody uses it any
// more, which is what the controller's connection cache does.
package valkeyacl

import (
	"cmp"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"net"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/valkey-io/valkey-go"
)

const defaultTimeout = 10 * time.Second

// Config says how to reach the server and log in as its ACL administrator.
type Config struct {
	// Address is host:port of the server's TLS port.
	Address string
	// CA is the PEM bundle that signed the server's certificate. Empty means
	// the system roots.
	CA []byte
	// ServerName is the name the server's certificate must be valid for; empty
	// means the host of Address.
	ServerName string
	// MinTLS is the lowest TLS version accepted (a crypto/tls constant); zero
	// means TLS 1.2, which is also the floor when a lower one is given.
	MinTLS uint16
	// Username and Password are the administrator's login. An empty Username
	// logs in as "default".
	Username string
	Password string `json:"-"`
	// Dial connects to Address and is required: the operator passes the egress
	// policy's dialer (an egress.DialFunc, assignable because this type is
	// unnamed), and there is no default that could dial around it.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
	// Timeout bounds the connect and every command; zero means ten seconds.
	Timeout time.Duration
}

// String shows the config without the password, so that logging a Config (or a
// struct that holds one) with %v or %+v does not write the password out.
func (c Config) String() string {
	return fmt.Sprintf("valkeyacl.Config{Address: %q, ServerName: %q, Username: %q, Password: <redacted>}",
		c.Address, c.ServerName, c.Username)
}

// GoString is String for %#v.
func (c Config) GoString() string { return c.String() }

func (c Config) withDefaults() Config {
	if c.Timeout == 0 {
		c.Timeout = defaultTimeout
	}
	c.MinTLS = max(c.MinTLS, tls.VersionTLS12)
	return c
}

// Admin is a logged-in connection to the server. It is safe for concurrent use.
type Admin struct {
	cli     valkey.Client
	timeout time.Duration
	// user is the administrator's own login, which no command may target.
	user string
	// down is set when a command failed for want of a connection, and says
	// nothing else: valkey-go dials again by itself, but a cache that sees it
	// replaces the Admin rather than wait for that.
	down   atomic.Bool
	closed atomic.Bool
}

// roots returns the pool that verifies the server: the PEM bundle, or nil for
// the system roots.
func roots(pem []byte) (*x509.CertPool, error) {
	if len(pem) == 0 {
		return nil, nil
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("%w: the CA bundle holds no certificate", ErrInvalid)
	}
	return pool, nil
}

// Connect dials the server, verifies its certificate, logs in and asks ACL
// WHOAMI, so a refused login or a missing ACL right is reported here. A wrong
// password is ErrAuth.
func Connect(ctx context.Context, cfg Config) (*Admin, error) {
	cfg = cfg.withDefaults()
	if cfg.Dial == nil {
		return nil, fmt.Errorf("%w: no dialer: every connection goes through the egress policy's", ErrInvalid)
	}
	pool, err := roots(cfg.CA)
	if err != nil {
		return nil, err
	}
	host, _, err := net.SplitHostPort(cfg.Address)
	if err != nil {
		return nil, fmt.Errorf("%w: address %q: %w", ErrInvalid, cfg.Address, err)
	}
	tlsConfig := &tls.Config{ServerName: cmp.Or(cfg.ServerName, host), RootCAs: pool, MinVersion: cfg.MinTLS}
	// What the last dial failed with: valkey-go reports an unreachable server
	// as its own "unable to connect" error, so the cause is kept here.
	var dialErr atomic.Pointer[error]
	cause := func(err error) error {
		if e := dialErr.Load(); e != nil {
			return *e
		}
		return err
	}
	a := &Admin{timeout: cfg.Timeout, user: cmp.Or(cfg.Username, "default")}
	cli, err := valkey.NewClient(valkey.ClientOption{
		InitAddress:       []string{cfg.Address},
		Username:          cfg.Username,
		Password:          cfg.Password,
		ClientName:        "operator-cache",
		ForceSingleClient: true,
		AlwaysRESP2:       true,
		DisableCache:      true,
		DisableRetry:      true,
		Dialer:            net.Dialer{Timeout: cfg.Timeout},
		DialCtxFn: func(ctx context.Context, addr string, _ *net.Dialer, _ *tls.Config) (net.Conn, error) {
			conn, err := dialTLS(ctx, cfg, addr, tlsConfig)
			if err != nil {
				dialErr.Store(&err)
			}
			return conn, err
		},
	})
	if err != nil {
		return nil, cause(reply("connect", err, true))
	}
	a.cli = cli
	if _, err := a.Whoami(ctx); err != nil {
		a.Close()
		return nil, cause(err)
	}
	return a, nil
}

// dialTLS connects through the dialer of the config and verifies the server.
func dialTLS(ctx context.Context, cfg Config, addr string, tlsConfig *tls.Config) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()
	raw, err := cfg.Dial(ctx, "tcp", addr)
	if err != nil {
		return nil, transport("dial", err)
	}
	tc := tls.Client(raw, tlsConfig)
	if err := tc.HandshakeContext(ctx); err != nil {
		_ = raw.Close()
		return nil, transport("tls", err)
	}
	return tc, nil
}

// Alive reports whether the connection is usable: not closed and not one that
// failed since it last answered.
func (a *Admin) Alive() bool { return !a.closed.Load() && !a.down.Load() }

// Close ends the connection. It may be called more than once.
func (a *Admin) Close() {
	if a.closed.CompareAndSwap(false, true) {
		a.cli.Close()
	}
}

// do runs a command and returns its reply. secret says the command carries a
// password, so a refusal does not repeat the server's words.
func (a *Admin) do(ctx context.Context, name string, secret bool, args ...string) (valkey.ValkeyMessage, error) {
	parent := ctx
	ctx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()
	cmd := a.cli.B().Arbitrary(name).Args(args...).Build()
	msg, err := a.cli.Do(ctx, cmd).ToMessage()
	if valkey.IsValkeyNil(err) {
		// A nil answer is not a failure: ACL GETUSER says so for a user that
		// does not exist.
		err = nil
	}
	if err != nil {
		// The caller's own cancellation or deadline says nothing of the
		// connection, which others share, so it does not mark it down.
		if _, ok := valkey.IsValkeyErr(err); !ok && parent.Err() == nil {
			a.down.Store(true)
		}
		return msg, reply(name, err, secret)
	}
	a.down.Store(false)
	return msg, nil
}

// Whoami is ACL WHOAMI: the name of the user the connection is logged in as.
// An answer proves that the server is reachable over verified TLS and accepts
// the login.
func (a *Admin) Whoami(ctx context.Context) (string, error) {
	msg, err := a.do(ctx, "ACL", false, "WHOAMI")
	if err != nil {
		return "", err
	}
	return msg.ToString()
}

// UserInfo is what ACL GETUSER says of a user.
type UserInfo struct {
	// Flags are the user's flags: "on" or "off", "nopass", and so on.
	Flags []string
	// PasswordHashes are the SHA-256 hashes (hex) of the user's passwords.
	PasswordHashes []string
	// Commands is the user's command rules as the server normalises them.
	Commands string
	// Keys and Channels are the key and channel patterns.
	Keys     string
	Channels string
	// Selectors is how many selectors the user has: each is a further set of
	// permissions beside the root ones above.
	Selectors int
}

// Has reports whether the user has a flag.
func (u UserInfo) Has(flag string) bool { return slices.Contains(u.Flags, flag) }

// checkName is validName, and also refuses the administrator's own login: a
// command that deleted or turned off that user would lock the operator out.
func (a *Admin) checkName(name string) error {
	if name == a.user {
		return fmt.Errorf("%w: the user name %q is the operator's own login", ErrInvalid, name)
	}
	return validName(name)
}

// validName refuses what must never reach ACL SETUSER or DELUSER: an empty
// name, one with a space, a control character or a quote (the command is
// built from arguments, but the name also goes to logs), and "default", the
// user every connection starts as.
func validName(name string) error {
	if name == "" || name == "default" || strings.IndexFunc(name, func(r rune) bool {
		return r <= ' ' || r == 0x7f || r == '"' || r == '\''
	}) >= 0 {
		return fmt.Errorf("%w: the user name %q is not allowed", ErrInvalid, name)
	}
	return nil
}

// GetUser is ACL GETUSER. A user that does not exist is ErrNotFound.
func (a *Admin) GetUser(ctx context.Context, name string) (UserInfo, error) {
	if err := a.checkName(name); err != nil {
		return UserInfo{}, err
	}
	msg, err := a.do(ctx, "ACL", false, "GETUSER", name)
	if err != nil {
		return UserInfo{}, err
	}
	if msg.IsNil() {
		return UserInfo{}, fmt.Errorf("%w: %s", ErrNotFound, "ACL GETUSER")
	}
	return parseUser(msg)
}

// parseUser reads the flat array of ACL GETUSER: field names and their values.
func parseUser(msg valkey.ValkeyMessage) (UserInfo, error) {
	items, err := msg.ToArray()
	if err != nil || len(items)%2 != 0 {
		return UserInfo{}, fmt.Errorf("%w: ACL GETUSER: the answer is not a list of fields", ErrServer)
	}
	var u UserInfo
	for i := 0; i < len(items); i += 2 {
		field, err := items[i].ToString()
		if err != nil {
			return UserInfo{}, fmt.Errorf("%w: ACL GETUSER: a field name is not text", ErrServer)
		}
		switch field {
		case "flags":
			u.Flags, err = items[i+1].AsStrSlice()
		case "passwords":
			u.PasswordHashes, err = items[i+1].AsStrSlice()
		case "commands":
			u.Commands, err = items[i+1].ToString()
		case "keys":
			u.Keys, err = items[i+1].ToString()
		case "channels":
			u.Channels, err = items[i+1].ToString()
		case "selectors":
			var sel []valkey.ValkeyMessage
			sel, err = items[i+1].ToArray()
			u.Selectors = len(sel)
		}
		if err != nil {
			return UserInfo{}, fmt.Errorf("%w: ACL GETUSER: field %q has an unexpected type", ErrServer, field)
		}
	}
	return u, nil
}

// SetUser is ACL SETUSER: it makes the user if there is none and applies the
// rules in order. Rules that carry a password (">pw", "<pw") are never repeated
// in an error.
func (a *Admin) SetUser(ctx context.Context, name string, rules ...string) error {
	if err := a.checkName(name); err != nil {
		return err
	}
	if slices.Contains(rules, ">") {
		// An empty password would let anyone log in as the user.
		return fmt.Errorf("%w: ACL SETUSER: an empty password", ErrInvalid)
	}
	_, err := a.do(ctx, "ACL", true, append([]string{"SETUSER", name}, rules...)...)
	return err
}

// DelUser is ACL DELUSER, and reports whether a user was deleted. The server
// ends the connections the user has open.
func (a *Admin) DelUser(ctx context.Context, name string) (bool, error) {
	if err := a.checkName(name); err != nil {
		return false, err
	}
	msg, err := a.do(ctx, "ACL", false, "DELUSER", name)
	if err != nil {
		return false, err
	}
	n, err := msg.ToInt64()
	if err != nil {
		return false, fmt.Errorf("%w: ACL DELUSER: the answer is not a number", ErrServer)
	}
	return n > 0, nil
}

// AddPassword is the ACL rule that adds a password to a user.
func AddPassword(password string) string { return ">" + password }

// KeepOnly is the ACL rules that leave the user with the password that hashes
// to hash, and no other.
func KeepOnly(hash string) []string { return []string{"resetpass", "#" + hash} }

// HashPassword is the SHA-256 hash of a password, in hex, as ACL GETUSER lists
// it and as the "#" rule takes it.
func HashPassword(password string) string {
	h := sha256.Sum256([]byte(password))
	return hex.EncodeToString(h[:])
}
