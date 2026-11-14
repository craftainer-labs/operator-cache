package valkeyacl

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/valkey-io/valkey-go"
)

// The kinds of error the client returns. Callers match them with errors.Is and
// show only coarse text: the wrapped text holds what the server said, for the
// operator's log. It never holds a password: a server echoes the modifier it
// refused ("Error in ACL SETUSER modifier '>secret'"), so the text of a refused
// ACL SETUSER is dropped.
var (
	// ErrNotFound means the user the command names does not exist.
	ErrNotFound = errors.New("valkey: user not found")
	// ErrInvalid means the server refused the command's arguments, or the
	// client refused them before sending (an empty or reserved user name, a
	// namespace that would make a glob pattern).
	ErrInvalid = errors.New("valkey: invalid request")
	// ErrServer means the server answered with another error.
	ErrServer = errors.New("valkey: server error")
	// ErrAuth means the server refused the admin login (WRONGPASS, NOAUTH) or
	// the command (NOPERM): the credentials are wrong or the admin's ACL is
	// too narrow.
	ErrAuth = errors.New("valkey: not authorized")
	// ErrUnavailable means no connection to the server could be made or kept:
	// the dial (egress policy included), the TLS handshake or the connection
	// itself failed.
	ErrUnavailable = errors.New("valkey: server unavailable")
	// ErrTimeout means the server did not answer in time. The command may or
	// may not have been applied, so the caller observes before it retries.
	ErrTimeout = errors.New("valkey: timed out")
)

// transport wraps an error of the dial, the handshake or the connection: a
// timeout when the deadline ran out, otherwise unavailable.
func transport(op string, err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%w: %s: %w", ErrTimeout, op, err)
	}
	return fmt.Errorf("%w: %s: %w", ErrUnavailable, op, err)
}

// reply turns the error of a command into an error kind. The server's own
// words are kept unless secret says the command carried a password. valkey-go
// strips the "ERR " of a generic error, so what is left starts with lower-case
// or capitalised text, while the codes of a server's state or of a refusal
// (WRONGPASS, NOPERM, LOADING, READONLY) stay upper-case.
func reply(command string, err error, secret bool) error {
	verr, ok := valkey.IsValkeyErr(err)
	if !ok {
		return transport(command, err)
	}
	msg := verr.Error()
	code, _, _ := strings.Cut(msg, " ")
	kind := ErrInvalid
	switch {
	case code == "WRONGPASS" || code == "NOAUTH" || code == "NOPERM":
		kind = ErrAuth
	case len(code) >= 4 && strings.ToUpper(code) == code:
		kind = ErrServer
	}
	if secret {
		return fmt.Errorf("%w: %s: the server refused the command", kind, command)
	}
	return fmt.Errorf("%w: %s: %s", kind, command, msg)
}
