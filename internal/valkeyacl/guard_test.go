package valkeyacl

import (
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
)

func bulkText(n int) string { return fmt.Sprintf("$%d\r\n%s\r\n", n, strings.Repeat("x", n)) }

// Replies of the five RESP2 types, a null and pipelined replies pass, whole or
// one byte at a time, and the guard is between replies after each.
func TestGuardAcceptsRESP2(t *testing.T) {
	for name, stream := range map[string]string{
		"simple string": "+OK\r\n",
		"error":         "-ERR nope\r\n",
		"integer":       ":42\r\n",
		"null bulk":     "$-1\r\n",
		"empty bulk":    "$0\r\n\r\n",
		"bulk":          "$5\r\nhello\r\n",
		"empty array":   "*0\r\n",
		"null array":    "*-1\r\n",
		"nested":        "*3\r\n$1\r\na\r\n*2\r\n:5\r\n*0\r\n$-1\r\n",
		"pipelined":     "+OK\r\n:1\r\n*1\r\n$2\r\nhi\r\n",
		"big bulk":      bulkText(maxReplyBytes - 16),
	} {
		var whole replyGuard
		if err := whole.feed([]byte(stream)); err != nil || whole.pending != 0 {
			t.Errorf("%s: %v, pending %d", name, err, whole.pending)
		}
		var bytewise replyGuard
		for i := range len(stream) {
			if err := bytewise.feed([]byte{stream[i]}); err != nil {
				t.Fatalf("%s: byte %d: %v", name, i, err)
			}
		}
		if bytewise.pending != 0 || bytewise.state != gType {
			t.Errorf("%s: pending %d, state %d", name, bytewise.pending, bytewise.state)
		}
	}
}

// The limits are per reply: many replies that are each under them are fine.
func TestGuardLimitsAreForOneReply(t *testing.T) {
	var g replyGuard
	for range 3 {
		if err := g.feed([]byte(bulkText(600_000) + "*4096\r\n" + strings.Repeat(":1\r\n", 4096))); err != nil {
			t.Fatal(err)
		}
	}
}

func TestGuardRefuses(t *testing.T) {
	for name, stream := range map[string]string{
		"RESP3 map":          "%1\r\n",
		"RESP3 push":         ">1\r\n",
		"unknown type":       "?\r\n",
		"bulk over limit":    "$1048577\r\n",
		"bulk at limit":      "$1048576\r\n",
		"huge bulk":          "$99999999999999999999\r\n",
		"two bulks together": "*2\r\n" + bulkText(600_000) + bulkText(600_000),
		"too many items":     "*4097\r\n",
		"items add up":       "*4000\r\n*96\r\n*1\r\n",
		"negative":           "$-2\r\n",
		"negative array":     "*-5\r\n",
		"double minus":       "$--1\r\n",
		"minus after digit":  "$1-\r\n",
		"no digits":          "$\r\n",
		"minus alone":        "$-\r\n",
		"not a digit":        "*x\r\n",
		"bare LF":            "$1\n",
		"CR without LF":      "$1\r\x00",
		"long line":          "+" + strings.Repeat("a", maxReplyLine+1),
		"zeros":              "$" + strings.Repeat("0", maxReplyBytes),
	} {
		var g replyGuard
		if err := g.feed([]byte(stream)); !errors.Is(err, ErrReplyLimit) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A line of exactly the limit is fine.
	var g replyGuard
	if err := g.feed([]byte("+" + strings.Repeat("a", maxReplyLine-2) + "\r\n")); err != nil {
		t.Errorf("a line at the limit: %v", err)
	}
}

func TestGuardedConn(t *testing.T) {
	server, client := net.Pipe()
	defer func() { _ = server.Close() }()
	defer func() { _ = client.Close() }()
	limits := 0
	c := &guardedConn{Conn: client, onLimit: func() { limits++ }}
	buf := make([]byte, 64)

	go func() { _, _ = server.Write([]byte("+OK\r\n")) }()
	if n, err := c.Read(buf); err != nil || string(buf[:n]) != "+OK\r\n" || limits != 0 {
		t.Fatalf("%q, %v, %d", buf[:n], err, limits)
	}

	// A header over the limit never reaches the reader, and the connection closes.
	go func() { _, _ = server.Write([]byte("$2000000\r\n")) }()
	if n, err := c.Read(buf); n != 0 || !errors.Is(err, ErrReplyLimit) || limits != 1 {
		t.Fatalf("%d, %v, %d", n, err, limits)
	}
	if _, err := server.Write([]byte("x")); err == nil {
		t.Error("the connection is still open")
	}
}

func TestGuardedConnPassesAnErrorOn(t *testing.T) {
	server, client := net.Pipe()
	c := &guardedConn{Conn: client, onLimit: func() { t.Error("no limit was broken") }}
	_ = server.Close()
	if n, err := c.Read(make([]byte, 8)); n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("%d, %v", n, err)
	}
}
