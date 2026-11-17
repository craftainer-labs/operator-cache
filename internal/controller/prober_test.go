package controller

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"strings"
	"testing"

	"github.com/craftainer-labs/lib-operator-go/pkg/cloud"
	"github.com/craftainer-labs/lib-operator-go/pkg/egress"
	"github.com/craftainer-labs/lib-operator-go/pkg/lifecycle"
	"github.com/craftainer-labs/lib-operator-go/pkg/server"

	cachev1alpha1 "github.com/craftainer-labs/operator-cache/api/v1alpha1"
	"github.com/craftainer-labs/operator-cache/internal/awscache"
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

	for name, srv := range map[string]server.CloudServer{"cluster": cluster, "namespaced": namespaced} {
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

// otherServer is a server.CloudServer this operator does not own.
type otherServer struct{ server.CloudServer }

// Probe is ProbeCloud of a plain connection, which is what the tests below build.
func (p Prober) Probe(ctx context.Context, srv server.CloudServer, conn server.Connection) (string, error) {
	res, err := p.ProbeCloud(ctx, srv, server.CloudConnection{Connection: conn})
	return res.Version, err
}

// awsServers are a cluster server on ElastiCache and a namespaced one on
// MemoryDB, both in eu-west-1.
func awsServers() map[string]server.CloudServer {
	cluster := &cachev1alpha1.ClusterCacheServer{}
	cluster.Spec.Credentials = &cloud.Credentials{Source: cloud.SourceSecret}
	cluster.Spec.Provider = &cachev1alpha1.ClusterProvider{Type: cloud.ProviderAWS, AWS: &cachev1alpha1.ClusterAWSCache{
		AWSClusterBlock: cloud.AWSClusterBlock{AWSBlock: cloud.AWSBlock{Region: "eu-west-1"}},
		AWSTarget:       cachev1alpha1.AWSTarget{ElastiCache: &cachev1alpha1.ElastiCacheTarget{UserGroupID: "g", ReplicationGroupID: "rg"}},
	}}
	namespaced := &cachev1alpha1.CacheServer{}
	namespaced.Spec.Provider = &cachev1alpha1.Provider{Type: cloud.ProviderAWS, AWS: &cachev1alpha1.AWSCache{
		AWSBlock:  cloud.AWSBlock{Region: "eu-west-1"},
		AWSTarget: cachev1alpha1.AWSTarget{MemoryDB: &cachev1alpha1.MemoryDBTarget{ACLName: "acl", ClusterName: "c"}},
	}}
	return map[string]server.CloudServer{"cluster": cluster, "namespaced": namespaced}
}

func awsProber(api *memAWS) Prober {
	return Prober{Config: Config{Egress: egress.NewPolicy(egress.ModeStrict), EgressMode: egress.ModeStrict, NewAWS: api.newClient}}
}

// A server with a provider is probed through the cloud's API, with the cloud
// credentials of the connection and no admin login, and discovers the drafted
// keys only.
func TestProberDiscoversAnAWSServer(t *testing.T) {
	for name, srv := range awsServers() {
		api := newMemAWS()
		api.target = awscache.Target{Endpoint: "cache.example.test", Port: 6380, ResourceID: "rg", AccountID: "111122223333", Version: "7.3"}
		creds := map[string][]byte{"k": []byte("v")}
		got, err := awsProber(api).ProbeCloud(context.Background(), srv, server.CloudConnection{Credentials: creds})
		want := map[string]string{
			cachev1alpha1.DiscoveredRegion: "eu-west-1", cachev1alpha1.DiscoveredEndpoint: "cache.example.test",
			cachev1alpha1.DiscoveredPort: "6380", cachev1alpha1.DiscoveredResourceID: "rg", cachev1alpha1.DiscoveredAccountID: "111122223333",
		}
		if err != nil || got.Version != "7.3" || !maps.Equal(got.Discovered, want) {
			t.Errorf("%s: %+v, %v", name, got, err)
		}
		if len(api.servers) != 1 || string(api.servers[0].Secret["k"]) != "v" || api.servers[0].Block.Region != "eu-west-1" {
			t.Errorf("%s: client built for %+v", name, api.servers)
		}
	}
	// What the service does not report is left out, never empty.
	api := newMemAWS()
	got, err := awsProber(api).ProbeCloud(context.Background(), awsServers()["cluster"], server.CloudConnection{})
	if err != nil || got.Version != "" || !maps.Equal(got.Discovered, map[string]string{cachev1alpha1.DiscoveredRegion: "eu-west-1"}) {
		t.Errorf("empty target: %+v, %v", got, err)
	}
}

// A server whose cloud cannot be reached fails with a fixed text: no ARN, no
// request id, no word of the service's own.
func TestProberAWSFailuresAreCategoryOnly(t *testing.T) {
	leak := "arn:aws:elasticache:eu-west-1:111122223333:user:secret req-1234"
	for name, tc := range map[string]struct {
		mutate func(*memAWS)
		kind   error
		text   string
	}{
		"build":    {func(m *memAWS) { m.buildErr = fmt.Errorf("%w: %s", cloud.ErrCredentialsUnavailable, leak) }, cloud.ErrCredentialsUnavailable, "the server's cloud credentials are not usable"},
		"target":   {func(m *memAWS) { m.fail["Describe"] = fmt.Errorf("%w: %s", awscache.ErrTargetNotFound, leak) }, awscache.ErrTargetNotFound, "the user group or ACL the server names does not exist"},
		"resource": {func(m *memAWS) { m.fail["Describe"] = fmt.Errorf("%w: %s", awscache.ErrResourceNotFound, leak) }, awscache.ErrResourceNotFound, "the replication group or cluster the server names does not exist"},
		"service":  {func(m *memAWS) { m.fail["Describe"] = errors.New(leak) }, nil, defaultText},
	} {
		api := newMemAWS()
		tc.mutate(api)
		_, err := awsProber(api).ProbeCloud(context.Background(), awsServers()["cluster"], server.CloudConnection{})
		if err == nil || err.Error() != tc.text || strings.Contains(err.Error(), "secret") || (tc.kind != nil && !errors.Is(err, tc.kind)) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// With no NewAWS the prober builds the SDK client of the operator, which checks
// the credentials before any call: a Secret that is missing is refused.
func TestProberAWSDefaultsToTheSDKClient(t *testing.T) {
	p := Prober{Config: Config{Egress: egress.NewPolicy(egress.ModeStrict), EgressMode: egress.ModeStrict}}
	_, err := p.ProbeCloud(context.Background(), awsServers()["cluster"], server.CloudConnection{})
	if !errors.Is(err, cloud.ErrCredentialsUnavailable) {
		t.Errorf("default client: %v", err)
	}
}
