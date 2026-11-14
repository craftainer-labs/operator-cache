package valkeyacl_test

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"

	"github.com/craftainer-labs/operator-cache/internal/valkeyacl"
)

// with sets the fake's version behaviour under its lock.
func (f *fake) with(fn func(*fake)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func TestVersionPrefersValkeyVersionOfInfo(t *testing.T) {
	f := newFake(t)
	got, err := connect(t, f).Version(context.Background())
	if err != nil || got != "9.1.2" {
		t.Fatalf("%q, %v", got, err)
	}
}

func TestVersionFallsBackToRedisVersion(t *testing.T) {
	f := newFake(t)
	f.with(func(f *fake) { f.infoText = "# Server\r\nredis_version:7.4.1\r\nos:Linux\r\n" })
	got, err := connect(t, f).Version(context.Background())
	if err != nil || got != "7.4.1" {
		t.Fatalf("%q, %v", got, err)
	}
}

// An admin whose ACL refuses INFO is asked HELLO.
func TestVersionFallsBackToHelloWhenInfoIsDenied(t *testing.T) {
	f := newFake(t)
	f.with(func(f *fake) { f.denied = map[string]bool{"INFO": true}; f.helloVersion = "8.0.1" })
	got, err := connect(t, f).Version(context.Background())
	if err != nil || got != "8.0.1" {
		t.Fatalf("%q, %v", got, err)
	}
}

// INFO without a version line is no answer, so HELLO is asked.
func TestVersionFallsBackToHelloWhenInfoNamesNone(t *testing.T) {
	f := newFake(t)
	f.with(func(f *fake) { f.infoText = "# Server\r\nos:Linux\r\n" })
	got, err := connect(t, f).Version(context.Background())
	if err != nil || got != "9.1.2" {
		t.Fatalf("%q, %v", got, err)
	}
}

// A server that gives no usable version is still healthy: "" and no error.
func TestVersionIsEmptyWhenNoSourceAnswers(t *testing.T) {
	f := newFake(t)
	// The client's own HELLO happens at connect, so the refusals come after it.
	a := connect(t, f)
	f.with(func(f *fake) { f.denied = map[string]bool{"INFO": true, "HELLO": true} })
	got, err := a.Version(context.Background())
	if err != nil || got != "" {
		t.Fatalf("%q, %v", got, err)
	}
}

// What the server reports is bounded and sanitised: odd or over-long values
// are dropped, not truncated.
func TestVersionRejectsOddAndOverlongValues(t *testing.T) {
	long := strings.Repeat("1", 65)
	for name, text := range map[string]string{
		"overlong":  "valkey_version:" + long + "\r\n",
		"control":   "valkey_version:9.\x01.2\r\n",
		"non-ascii": "valkey_version:9.é.2\r\n",
		"spaces":    "valkey_version:9 .1\r\n",
		"empty":     "valkey_version:\r\n",
	} {
		f := newFake(t)
		a := connect(t, f)
		f.with(func(f *fake) { f.infoText = text; f.denied = map[string]bool{"HELLO": true} })
		got, err := a.Version(context.Background())
		if err != nil || got != "" {
			t.Errorf("%s: %q, %v", name, got, err)
		}
	}
	f := newFake(t)
	f.with(func(f *fake) { f.infoText = "valkey_version:" + strings.Repeat("1", 64) + "\r\n" })
	if got, _ := connect(t, f).Version(context.Background()); len(got) != 64 {
		t.Errorf("a 64-byte version is kept: %q", got)
	}
}

// A connection that is lost while the version is read is an error, from INFO or
// from HELLO.
func TestVersionReportsALostConnection(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*fake)
	}{
		{"info", func(f *fake) { f.dropOn = "INFO" }},
		{"hello", func(f *fake) { f.denied = map[string]bool{"INFO": true}; f.dropOn = "HELLO" }},
	} {
		f := newFake(t)
		a := connect(t, f)
		f.with(tc.mutate)
		_, err := a.Version(context.Background())
		if !errors.Is(err, valkeyacl.ErrUnavailable) {
			t.Errorf("%s: %v", tc.name, err)
		}
	}
}

// A HELLO answer whose version is not plain gives "".
func TestVersionIgnoresAMalformedHello(t *testing.T) {
	f := newFake(t)
	f.with(func(f *fake) { f.denied = map[string]bool{"INFO": true}; f.helloVersion = "bad version" })
	got, err := connect(t, f).Version(context.Background())
	if err != nil || got != "" {
		t.Fatalf("%q, %v", got, err)
	}
}

func TestProbeReturnsTheVersionAndClosesTheConnection(t *testing.T) {
	f := newFake(t)
	got, err := valkeyacl.Probe(context.Background(), f.cfg())
	if err != nil || got != "9.1.2" {
		t.Fatalf("%q, %v", got, err)
	}
}

func TestProbeReportsAWrongPasswordWithoutLeaking(t *testing.T) {
	f := newFake(t)
	cfg := f.cfg()
	cfg.Password = "not-the-password"
	_, err := valkeyacl.Probe(context.Background(), cfg)
	if !errors.Is(err, valkeyacl.ErrAuth) {
		t.Fatalf("%v", err)
	}
	host, _, _ := net.SplitHostPort(f.addr())
	for _, secret := range []string{"not-the-password", f.adminPW, host} {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("the error leaks %q: %v", secret, err)
		}
	}
}

func TestProbeReportsAConnectionThatFailsInTheVersionRead(t *testing.T) {
	f := newFake(t)
	f.with(func(f *fake) { f.dropOn = "INFO" })
	if _, err := valkeyacl.Probe(context.Background(), f.cfg()); !errors.Is(err, valkeyacl.ErrUnavailable) {
		t.Fatalf("%v", err)
	}
}

// An answer of the wrong shape is no version, and not an error: INFO as a
// number leaves HELLO to answer, and HELLO as a plain OK, or as a list with no
// version in it, gives "".
func TestVersionIgnoresAnswersOfTheWrongShape(t *testing.T) {
	for name, raw := range map[string]map[string]string{
		"info is a number":     {"INFO": ":1\r\n"},
		"hello is not a list":  {"INFO": ":1\r\n", "HELLO": "+OK\r\n"},
		"hello has no version": {"INFO": ":1\r\n", "HELLO": list(bulk("server"), bulk("valkey"))},
	} {
		f := newFake(t)
		a := connect(t, f)
		f.with(func(f *fake) { f.raw = raw })
		got, err := a.Version(context.Background())
		want := ""
		if name == "info is a number" {
			want = "9.1.2"
		}
		if err != nil || got != want {
			t.Errorf("%s: %q, %v", name, got, err)
		}
	}
}
