package controller

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"

	"github.com/craftainer-labs/lib-operator-go/pkg/lifecycle"
	"github.com/craftainer-labs/lib-operator-go/pkg/server"

	cachev1alpha1 "github.com/craftainer-labs/operator-cache/api/v1alpha1"
	"github.com/craftainer-labs/operator-cache/internal/egress"
	"github.com/craftainer-labs/operator-cache/internal/valkeyacl"
)

// probeFixture is a Prober whose probe connection is a fake that records the
// config it was given.
func probeFixture(version string, err error) (Prober, *[]valkeyacl.Config) {
	var cfgs []valkeyacl.Config
	p := Prober{Config: Config{Egress: egress.NewPolicy(egress.ModeStrict)}}
	p.Check = func(_ context.Context, c valkeyacl.Config) (string, error) {
		cfgs = append(cfgs, c)
		return version, err
	}
	return p, &cfgs
}

func probeConn() server.Connection {
	return server.Connection{
		Admin:    map[string][]byte{AdminUsernameKey: []byte("admin-user"), AdminPasswordKey: []byte("s3cret-pw")},
		CABundle: []byte("ca"), ServerName: "valkey.internal", MinTLSVersion: 0x0304,
	}
}

// Both kinds are probed with the admin login, TLS settings and address of the
// server, through the egress dialer.
func TestProberProbesBothKinds(t *testing.T) {
	cluster := &cachev1alpha1.ClusterCacheServer{}
	cluster.Spec.Host = "valkey.example.test"
	namespaced := &cachev1alpha1.CacheServer{}
	namespaced.Spec.Host = "valkey.example.test"

	for name, srv := range map[string]server.Server{"cluster": cluster, "namespaced": namespaced} {
		p, cfgs := probeFixture("9.1.2", nil)
		got, err := p.Probe(context.Background(), srv, probeConn())
		if err != nil || got != "9.1.2" {
			t.Fatalf("%s: %q, %v", name, got, err)
		}
		if len(*cfgs) != 1 {
			t.Fatalf("%s: %d probes", name, len(*cfgs))
		}
		c := (*cfgs)[0]
		if c.Address != "valkey.example.test:6379" || c.Username != "admin-user" || c.Password != "s3cret-pw" ||
			c.ServerName != "valkey.internal" || string(c.CA) != "ca" || c.MinTLS != 0x0304 || c.Dial == nil {
			t.Errorf("%s: config = %+v", name, c)
		}
	}
}

// A server that answers without a version is healthy with an empty one.
func TestProberAcceptsAnEmptyVersion(t *testing.T) {
	p, _ := probeFixture("", nil)
	got, err := p.Probe(context.Background(), &cachev1alpha1.CacheServer{}, probeConn())
	if err != nil || got != "" {
		t.Errorf("%q, %v", got, err)
	}
}

// Whatever fails, the error is a fixed text of its category: no address, no
// login and no word of the server's own.
func TestProberFailuresAreCategoryOnly(t *testing.T) {
	srv := &cachev1alpha1.ClusterCacheServer{}
	srv.Spec.Host = "valkey.secret-host.test"
	ctx := context.Background()
	leaks := []string{"secret-host", "s3cret-pw", "admin-user", "server said"}
	noLeak := func(what string, err error) {
		t.Helper()
		for _, l := range leaks {
			if strings.Contains(err.Error(), l) {
				t.Errorf("%s: the error leaks %q: %v", what, l, err)
			}
		}
	}

	// A refused login, whose raw text names the host.
	raw := errors.Join(valkeyacl.ErrAuth, errors.New("dial tcp valkey.secret-host.test:6379: server said no"))
	p, _ := probeFixture("", raw)
	_, err := p.Probe(ctx, srv, probeConn())
	if !errors.Is(err, valkeyacl.ErrAuth) || err.Error() != "the server refused the operator's admin login" {
		t.Errorf("auth: %v", err)
	}
	noLeak("auth", err)

	// An unknown failure gets the default text.
	p, _ = probeFixture("", errors.New("server said boom at valkey.secret-host.test"))
	_, err = p.Probe(ctx, srv, probeConn())
	if err == nil || err.Error() != defaultText {
		t.Errorf("unknown: %v", err)
	}
	noLeak("unknown", err)

	// An unreachable server is marked as such.
	p, _ = probeFixture("", errors.Join(valkeyacl.ErrUnavailable, errors.New("dial valkey.secret-host.test")))
	_, err = p.Probe(ctx, srv, probeConn())
	if !errors.Is(err, valkeyacl.ErrUnavailable) || err.Error() != "the connection to the server failed" {
		t.Errorf("unavailable: %v", err)
	}
	noLeak("unavailable", err)

	// An error that is already fixed text passes through unchanged.
	p, _ = probeFixture("", lifecycle.ErrNameCollision)
	if _, err = p.Probe(ctx, srv, probeConn()); !errors.Is(err, lifecycle.ErrNameCollision) {
		t.Errorf("passthrough: %v", err)
	}

	// Missing credentials: no probe is made.
	conn := probeConn()
	delete(conn.Admin, AdminPasswordKey)
	p, cfgs := probeFixture("", nil)
	_, err = p.Probe(ctx, srv, conn)
	if !errors.Is(err, ErrAdminCredentials) || !errors.Is(err, server.ErrInvalidServer) || len(*cfgs) != 0 {
		t.Errorf("credentials: %v", err)
	}
	noLeak("credentials", err)

	// A proxy the egress policy cannot dial through.
	conn = probeConn()
	conn.Proxy = &url.URL{Scheme: "ftp", Host: "proxy.example.test"}
	_, err = p.Probe(ctx, srv, conn)
	if !errors.Is(err, server.ErrInvalidServer) || len(*cfgs) != 0 {
		t.Errorf("proxy: %v", err)
	}

	// A kind of another operator.
	_, err = p.Probe(ctx, otherServer{}, probeConn())
	if !errors.Is(err, server.ErrUnknownServerKind) {
		t.Errorf("kind: %v", err)
	}
}

// With no probe function the prober uses valkeyacl.Probe, which here is stopped
// by the egress policy at the dial (loopback), and returns a category.
func TestProberDefaultsToValkeyaclProbe(t *testing.T) {
	srv := &cachev1alpha1.CacheServer{}
	srv.Spec.Host = "127.0.0.1"
	p := Prober{Config: Config{Egress: egress.NewPolicy(egress.ModeStrict)}}
	conn := probeConn()
	conn.CABundle = nil // the system roots: a fake bundle is refused before any dial
	_, err := p.Probe(context.Background(), srv, conn)
	if !errors.Is(err, egress.ErrBlocked) || strings.Contains(err.Error(), "127.0.0.1") {
		t.Errorf("default probe: %v", err)
	}
}

// otherServer is a server.Server this operator does not own.
type otherServer struct{ server.Server }
