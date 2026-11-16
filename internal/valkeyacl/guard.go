package valkeyacl

import "net"

// The limits on one reply of the server. valkey-go has none: it allocates a
// buffer of the size a bulk string or an array declares before it reads the
// body, so a hostile or broken server could make the operator allocate
// gigabytes with one header. The connection is therefore wrapped, and the
// stream is checked against these limits before valkey-go sees a header that
// breaks them. Every reply the operator reads (INFO, HELLO, ACL GETUSER, ...)
// is a few kilobytes, and the connection speaks RESP2 only.
const (
	// maxReplyBytes bounds the bytes of one reply, its headers and bodies together.
	maxReplyBytes = 1 << 20
	// maxReplyItems bounds the elements the arrays of one reply declare, which also
	// bounds how deep they nest.
	maxReplyItems = 4096
	// maxReplyLine bounds a simple string, an error or an integer.
	maxReplyLine = 4096
)

type guardState uint8

const (
	gType   guardState = iota // the type byte of a value
	gLine                     // the rest of a simple string, an error or an integer
	gLength                   // the length of a bulk string or an array, up to its CR
	gLenEnd                   // the LF after that CR
	gBody                     // the body of a bulk string and its CRLF
)

// replyGuard is a streaming check of the RESP2 the server sends. It accepts the
// five RESP2 types, a null (a length of -1) and nothing else, and refuses a
// reply over the limits above. It keeps no data, only counters, and handles
// replies that arrive in any chunks, one after another.
type replyGuard struct {
	state   guardState
	kind    byte  // '$' or '*' while a length is read
	pending int64 // values of the current reply still to read; 0 between replies
	total   int64 // bytes of the current reply, counting a bulk string's body at its header
	items   int64 // elements the arrays of the current reply declared
	length  int64 // the length being read
	digits  int   // digits of that length so far
	neg     bool  // that length has a minus sign
	line    int   // bytes of the current line
	left    int64 // bytes of the current body still to skip
}

// feed checks the next bytes of the stream. An error means the reply is over a
// limit or is not RESP2, and the stream must not be read on.
func (g *replyGuard) feed(p []byte) error {
	for i := 0; i < len(p); {
		if g.state == gBody {
			skip := min(int64(len(p)-i), g.left)
			g.left -= skip
			i += int(skip)
			if g.left == 0 {
				g.state = gType
				g.pending--
			}
			continue
		}
		if err := g.step(p[i]); err != nil {
			return err
		}
		i++
	}
	return nil
}

// step reads one byte that is not part of a body.
func (g *replyGuard) step(b byte) error {
	if g.pending == 0 {
		g.pending, g.total, g.items = 1, 0, 0
	}
	if g.total++; g.total > maxReplyBytes {
		return ErrReplyLimit
	}
	switch g.state {
	case gType:
		switch b {
		case '+', '-', ':':
			g.state, g.line = gLine, 0
		case '$', '*':
			g.state, g.kind = gLength, b
		default:
			return ErrReplyLimit
		}
	case gLine:
		if g.line++; g.line > maxReplyLine {
			return ErrReplyLimit
		}
		if b == '\n' {
			g.state = gType
			g.pending--
		}
	case gLength:
		switch {
		case b == '-' && g.digits == 0 && !g.neg:
			g.neg = true
		case b >= '0' && b <= '9':
			g.digits++
			if g.length = g.length*10 + int64(b-'0'); g.length > maxReplyBytes {
				return ErrReplyLimit
			}
		case b == '\r' && g.digits > 0:
			g.state = gLenEnd
		default:
			return ErrReplyLimit
		}
	default: // gLenEnd
		if b != '\n' {
			return ErrReplyLimit
		}
		return g.header()
	}
	return nil
}

// header acts on a complete length.
func (g *replyGuard) header() error {
	n, neg := g.length, g.neg
	g.length, g.digits, g.neg, g.state = 0, 0, false, gType
	switch {
	case neg:
		// Only the null, -1, has a sign.
		if n != 1 {
			return ErrReplyLimit
		}
		g.pending--
	case g.kind == '$':
		if g.total += n + 2; g.total > maxReplyBytes {
			return ErrReplyLimit
		}
		g.state, g.left = gBody, n+2
	default:
		if g.items += n; g.items > maxReplyItems {
			return ErrReplyLimit
		}
		g.pending += n - 1
	}
	return nil
}

// guardedConn checks everything the server sends against replyGuard. On a
// violation it reports it, closes the connection and returns the error to
// valkey-go, whose pipe then fails the command.
type guardedConn struct {
	net.Conn
	guard   replyGuard
	onLimit func()
	// err is the violation, kept so that no later Read parses past it.
	err error
}

func (c *guardedConn) Read(p []byte) (int, error) {
	if c.err != nil {
		return 0, c.err
	}
	n, err := c.Conn.Read(p)
	if gerr := c.guard.feed(p[:n]); gerr != nil {
		c.err = gerr
		c.onLimit()
		_ = c.Close()
		return 0, gerr
	}
	return n, err
}
