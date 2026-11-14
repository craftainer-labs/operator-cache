package valkeyacl

import (
	"context"
	"errors"
	"strings"

	"github.com/valkey-io/valkey-go"
)

// maxVersion bounds the version text: a longer one is no version.
const maxVersion = 64

// Version asks the server for its version. The connection already proved
// itself with ACL WHOAMI, so this is best effort: INFO server is asked first
// (valkey_version on Valkey, redis_version on Redis), and when the
// administrator's ACL does not allow it, HELLO, which sits in the connection
// category and so stays open to a narrow ACL. A refused or unreadable answer, a
// version that is not plain ASCII, or one over 64 bytes gives "" and no error.
// Only a connection that fails is an error.
func (a *Admin) Version(ctx context.Context) (string, error) {
	msg, err := a.do(ctx, "INFO", false, "server")
	if err == nil {
		if text, terr := msg.ToString(); terr == nil {
			if v := cleanVersion(infoVersion(text)); v != "" {
				return v, nil
			}
		}
	} else if lost(err) {
		return "", err
	}
	msg, err = a.do(ctx, "HELLO", false)
	if err != nil {
		if lost(err) {
			return "", err
		}
		return "", nil
	}
	return cleanVersion(helloVersion(msg)), nil
}

// lost reports a failure of the connection itself, as opposed to an answer of
// the server.
func lost(err error) bool {
	return errors.Is(err, ErrUnavailable) || errors.Is(err, ErrTimeout)
}

// infoVersion reads the version out of the text of INFO server: valkey_version
// when there is one (Valkey keeps redis_version at 7.2.4 for compatibility),
// otherwise redis_version.
func infoVersion(text string) string {
	var redis string
	for line := range strings.Lines(text) {
		line = strings.TrimSpace(line)
		if v, ok := strings.CutPrefix(line, "valkey_version:"); ok {
			return v
		}
		if v, ok := strings.CutPrefix(line, "redis_version:"); ok {
			redis = v
		}
	}
	return redis
}

// helloVersion reads "version" from the flat field list HELLO answers in RESP2.
func helloVersion(msg valkey.ValkeyMessage) string {
	items, err := msg.ToArray()
	if err != nil {
		return ""
	}
	for i := 0; i+1 < len(items); i += 2 {
		if k, err := items[i].ToString(); err == nil && k == "version" {
			v, _ := items[i+1].ToString()
			return v
		}
	}
	return ""
}

// cleanVersion returns v when it is 1 to 64 bytes of printable ASCII without
// spaces, and "" for anything else.
func cleanVersion(v string) string {
	if v == "" || len(v) > maxVersion {
		return ""
	}
	for i := range len(v) {
		if v[i] <= 0x20 || v[i] > 0x7e {
			return ""
		}
	}
	return v
}

// Probe is the health probe of a server: a connection of its own, made and
// closed here and never taken from a cache, that logs in as the administrator
// (ACL WHOAMI, which proves the server is reachable over verified TLS and
// accepts the login) and reads the version. The connection is closed on every
// path. Errors are those of Connect and of the version read.
func Probe(ctx context.Context, cfg Config) (string, error) {
	a, err := Connect(ctx, cfg)
	if err != nil {
		return "", err
	}
	defer a.Close()
	return a.Version(ctx)
}
