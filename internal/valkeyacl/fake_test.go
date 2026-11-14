package valkeyacl_test

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/craftainer-labs/operator-cache/internal/valkeyacl"
)

// pki is a throwaway CA and a server certificate for "valkey.test" and
// 127.0.0.1.
type pki struct {
	ca   []byte
	cert tls.Certificate
}

func newPKI(t *testing.T) pki {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "valkey.test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames: []string{"valkey.test"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	return pki{
		ca:   pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		cert: tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key},
	}
}

// fakeUser is an ACL user of the fake server: what ACL GETUSER shows.
type fakeUser struct {
	flags    []string
	hashes   []string
	commands string
	keys     string
	channels string
}

// fake is a Valkey that speaks the subset of RESP2 the client uses over TLS.
type fake struct {
	t   *testing.T
	ln  net.Listener
	pki pki

	adminPW string

	mu    sync.Mutex
	users map[string]*fakeUser
	log   []string
	acls  int

	// hang is a subcommand of ACL the server never answers.
	hang string
	// failWith is an error line sent for every ACL command.
	failWith string
	// garbage is the answer to send to GETUSER and DELUSER instead of the real one.
	garbage string
	// dropAfter closes the connection after that many ACL commands; 0 never.
	dropAfter int
}

func newFake(t *testing.T) *fake {
	t.Helper()
	f := &fake{t: t, pki: newPKI(t), adminPW: "admin-secret", users: map[string]*fakeUser{}}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{f.pki.cert}, MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatal(err)
	}
	f.ln = ln
	t.Cleanup(func() { _ = ln.Close() })
	go f.accept()
	return f
}

func (f *fake) addr() string { return f.ln.Addr().String() }

func (f *fake) cfg() valkeyacl.Config {
	d := &net.Dialer{}
	return valkeyacl.Config{
		Address: f.addr(), CA: f.pki.ca, ServerName: "valkey.test", Username: "admin", Password: f.adminPW,
		Dial: d.DialContext, Timeout: 2 * time.Second,
	}
}

func (f *fake) commands() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.log)
}

func (f *fake) accept() {
	for {
		c, err := f.ln.Accept()
		if err != nil {
			return
		}
		go f.serve(c)
	}
}

func readCommand(r *bufio.Reader) ([]string, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return nil, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(line[1:]))
	if err != nil {
		return nil, err
	}
	args := make([]string, 0, n)
	for range n {
		head, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		size, err := strconv.Atoi(strings.TrimSpace(head[1:]))
		if err != nil {
			return nil, err
		}
		buf := make([]byte, size+2)
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, err
		}
		args = append(args, string(buf[:size]))
	}
	return args, nil
}

func bulk(s string) string {
	return fmt.Sprintf("$%d\r\n%s\r\n", len(s), s)
}

func list(items ...string) string {
	return fmt.Sprintf("*%d\r\n%s", len(items), strings.Join(items, ""))
}

func (f *fake) serve(c net.Conn) {
	defer func() { _ = c.Close() }()
	r := bufio.NewReader(c)
	loggedIn := ""
	for {
		args, err := readCommand(r)
		if err != nil {
			return
		}
		cmd := strings.ToUpper(args[0])
		var reply string
		switch {
		case cmd == "AUTH":
			if len(args) == 3 && args[1] == "admin" && args[2] == f.adminPW {
				loggedIn = "admin"
				reply = "+OK\r\n"
			} else {
				reply = "-WRONGPASS invalid username-password pair or user is disabled.\r\n"
			}
		case loggedIn == "":
			reply = "-NOAUTH Authentication required.\r\n"
		case cmd == "HELLO":
			reply = list(bulk("server"), bulk("valkey"), bulk("version"), bulk("9.1.2"), bulk("proto"), ":2\r\n",
				bulk("id"), ":1\r\n", bulk("mode"), bulk("standalone"), bulk("role"), bulk("master"), bulk("modules"), "*0\r\n")
		case cmd == "INFO":
			reply = bulk("# Server\r\nvalkey_version:9.1.2\r\n")
		case cmd == "ACL":
			f.mu.Lock()
			f.log = append(f.log, strings.Join(args, " "))
			f.acls++
			drop := f.dropAfter > 0 && f.acls > f.dropAfter
			hang := f.hang == strings.ToUpper(args[1])
			f.mu.Unlock()
			if drop {
				return
			}
			if hang {
				time.Sleep(5 * time.Second)
				return
			}
			reply = f.acl(args[1:])
		default:
			reply = "+OK\r\n"
		}
		if _, err := c.Write([]byte(reply)); err != nil {
			return
		}
	}
}

// acl answers an ACL command.
func (f *fake) acl(args []string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failWith != "" {
		return "-" + f.failWith + "\r\n"
	}
	switch strings.ToUpper(args[0]) {
	case "WHOAMI":
		return bulk("admin")
	case "GETUSER":
		if f.garbage != "" {
			return f.garbage
		}
		u, ok := f.users[args[1]]
		if !ok {
			return "$-1\r\n"
		}
		strs := func(v []string) string {
			out := make([]string, len(v))
			for i, s := range v {
				out[i] = bulk(s)
			}
			return list(out...)
		}
		return list(bulk("flags"), strs(u.flags), bulk("passwords"), strs(u.hashes), bulk("commands"), bulk(u.commands),
			bulk("keys"), bulk(u.keys), bulk("channels"), bulk(u.channels), bulk("selectors"), "*0\r\n")
	case "DELUSER":
		if f.garbage != "" {
			return f.garbage
		}
		if _, ok := f.users[args[1]]; ok {
			delete(f.users, args[1])
			return ":1\r\n"
		}
		return ":0\r\n"
	case "SETUSER":
		return f.setUser(args[1], args[2:])
	}
	return "-ERR unknown subcommand\r\n"
}

// setUser applies rules the way the server does, for the rules the client sends.
func (f *fake) setUser(name string, rules []string) string {
	u, ok := f.users[name]
	if !ok {
		u = &fakeUser{flags: []string{"off", "sanitize-payload"}}
		f.users[name] = u
	}
	for _, r := range rules {
		switch {
		case r == "on":
			u.flags = []string{"on", "sanitize-payload"}
		case r == "resetpass":
			u.hashes = nil
		case r == "resetkeys":
			u.keys = ""
		case r == "resetchannels":
			u.channels = ""
		case r == "clearselectors":
		case strings.HasPrefix(r, ">"):
			h := sha(r[1:])
			if !slices.Contains(u.hashes, h) {
				u.hashes = append(u.hashes, h)
			}
		case strings.HasPrefix(r, "#"):
			if !slices.Contains(u.hashes, r[1:]) {
				u.hashes = append(u.hashes, r[1:])
			}
		case strings.HasPrefix(r, "~"), strings.HasPrefix(r, "%R~"):
			u.keys = r
		case strings.HasPrefix(r, "&"):
			u.channels = r
		case r == "-@all":
			u.commands = r
		case strings.HasPrefix(r, "+"), strings.HasPrefix(r, "-"):
			u.commands += " " + r
		default:
			return fmt.Sprintf("-ERR Error in ACL SETUSER modifier '%s': Syntax error\r\n", r)
		}
	}
	return "+OK\r\n"
}

func sha(s string) string { return valkeyacl.HashPassword(s) }

func connect(t *testing.T, f *fake) *valkeyacl.Admin {
	t.Helper()
	a, err := valkeyacl.Connect(context.Background(), f.cfg())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	return a
}
