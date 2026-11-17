package controller

import (
	"context"
	"fmt"
	"strconv"

	"github.com/craftainer-labs/lib-operator-go/pkg/cloud"
	"github.com/craftainer-labs/lib-operator-go/pkg/probe"
	"github.com/craftainer-labs/lib-operator-go/pkg/server"

	cachev1alpha1 "github.com/craftainer-labs/operator-cache/api/v1alpha1"
	"github.com/craftainer-labs/operator-cache/internal/valkeyacl"
)

// ProbeFunc makes one probe connection to a server and returns its version, or
// "" when it names none; valkeyacl.Probe is the implementation, and tests
// replace it.
type ProbeFunc func(ctx context.Context, cfg valkeyacl.Config) (string, error)

// Prober is the probe.Prober of both server kinds. It builds the connection
// settings through the egress policy and the server's proxy and TLS settings
// (the same adminConfig the claims use) and makes a fresh, short-lived admin
// connection of its own with valkeyacl.Probe. It never takes a connection from
// the claims' ServerCache and never puts one there, so a probe can neither pin
// nor evict a cached connection. A successful login (ACL WHOAMI) proves the
// server is reachable over verified TLS and accepts the admin credentials; the
// version is INFO server's, or HELLO's when the admin's ACL does not allow INFO,
// and empty when neither can be read.
//
// The caller (probe.Reconciler) bounds a probe by Options.Timeout and logs the
// error it returns, so the error is category-only: a fixed text per kind of
// failure, never the address, the login or the server's own words.
type Prober struct {
	Config Config
	// Check is the probe connection; nil means valkeyacl.Probe.
	Check ProbeFunc
}

var _ probe.CloudProber = Prober{}

// ProbeCloud returns what the probe reads of the server srv names, or an error
// that names only a category of failure. Both server kinds are read through the
// cloud resolvers of lib-operator-go (server.ConnectionForCloud), which return
// the self-hosted settings unchanged. A server with a provider has no admin
// Secret and no connection of its own: the probe asks the cloud's API whether
// the user group or ACL, and the replication group or cluster it is attached
// to, exist, and discovers the data-plane address.
func (p Prober) ProbeCloud(ctx context.Context, srv server.CloudServer, conn server.CloudConnection) (probe.CloudResult, error) {
	spec, aws, err := serverConfigOf(srv)
	if err != nil {
		return probe.CloudResult{}, err
	}
	if aws != nil {
		return p.probeAWS(ctx, *aws, conn)
	}
	dial, err := p.Config.Egress.HostDialer(conn.Proxy)
	if err != nil {
		return probe.CloudResult{}, categoryOnly(fmt.Errorf("%w: %w", server.ErrInvalidServer, err))
	}
	cfg, err := adminConfig(spec, conn.Connection, dial)
	if err != nil {
		return probe.CloudResult{}, categoryOnly(err)
	}
	run := p.Check
	if run == nil {
		run = valkeyacl.Probe
	}
	version, err := run(ctx, cfg)
	return probe.CloudResult{Version: version}, categoryOnly(err)
}

// probeAWS reads an AWS server: it builds the client the claims use (through
// the egress policy, with the credentials the library resolved) and asks the
// service for the user group or ACL and the cache they are attached to. Any
// failure leaves the server not Ready (the library reports it as
// ServerUnreachable); the drafted reasons TargetNotFound and
// EndpointOverrideForbidden cannot be expressed through the library yet, so the
// condition carries the library's coarse message and the category is in the log.
//
// What it discovers is the drafted set only: the data-plane endpoint and port,
// the replication group or cluster, the account (from the resource's ARN, when
// the service names one) and the region of the server. A key whose value is not
// known is left out, never empty. The engine version is MemoryDB's; ElastiCache
// reports none on a replication group.
func (p Prober) probeAWS(ctx context.Context, s AWSServer, conn server.CloudConnection) (probe.CloudResult, error) {
	s.Secret = conn.Credentials
	newAWS := p.Config.NewAWS
	if newAWS == nil {
		newAWS = NewAWSClient
	}
	api, err := newAWS(ctx, s, cloud.Egress{Mode: p.Config.EgressMode, Policy: p.Config.Egress, Proxy: conn.Proxy})
	if err != nil {
		return probe.CloudResult{}, categoryOnly(err)
	}
	t, err := api.Describe(ctx)
	if err != nil {
		return probe.CloudResult{}, categoryOnly(err)
	}
	d := map[string]string{cachev1alpha1.DiscoveredRegion: s.Block.Region}
	for k, v := range map[string]string{
		cachev1alpha1.DiscoveredEndpoint: t.Endpoint, cachev1alpha1.DiscoveredResourceID: t.ResourceID,
		cachev1alpha1.DiscoveredAccountID: t.AccountID,
	} {
		if v != "" {
			d[k] = v
		}
	}
	if t.Port > 0 {
		d[cachev1alpha1.DiscoveredPort] = strconv.Itoa(int(t.Port))
	}
	return probe.CloudResult{Version: t.Version, Discovered: d}, nil
}

// categoryOnly replaces the text of err with the fixed text of its kind. The
// raw error holds the server's address. Errors.Is still matches the kinds
// inside.
func categoryOnly(err error) error {
	if err == nil {
		return nil
	}
	for _, r := range rules {
		if r.match(err) {
			if r.text == "" {
				return err
			}
			return &coarseError{text: r.text, errs: []error{err}}
		}
	}
	return &coarseError{text: defaultText, errs: []error{err}}
}

// serverConfigOf returns the Valkey fields of a server kind of this operator
// and, for a server with a provider, its AWS settings (nil when it has none).
func serverConfigOf(srv server.CloudServer) (cachev1alpha1.ServerConfig, *AWSServer, error) {
	switch s := srv.(type) {
	case *cachev1alpha1.ClusterCacheServer:
		return s.Spec.ServerConfig, clusterAWS(s), nil
	case *cachev1alpha1.CacheServer:
		return s.Spec.ServerConfig, namespacedAWS(s), nil
	}
	return cachev1alpha1.ServerConfig{}, nil, fmt.Errorf("%w: %T", server.ErrUnknownServerKind, srv)
}

// clusterAWS is the AWS side of a cluster server, nil without a provider.
func clusterAWS(s *cachev1alpha1.ClusterCacheServer) *AWSServer {
	p := s.Spec.Provider
	if p == nil || p.AWS == nil {
		return nil
	}
	return &AWSServer{
		Scope: cloud.ScopeCluster, Block: p.AWS.AWSClusterBlock, Credentials: s.Spec.Credentials,
		ElastiCache: p.AWS.ElastiCache, MemoryDB: p.AWS.MemoryDB,
	}
}

// namespacedAWS is the AWS side of a namespaced server, nil without a provider.
func namespacedAWS(s *cachev1alpha1.CacheServer) *AWSServer {
	p := s.Spec.Provider
	if p == nil || p.AWS == nil {
		return nil
	}
	return &AWSServer{
		Scope: cloud.ScopeNamespaced, Block: cloud.AWSClusterBlock{AWSBlock: p.AWS.AWSBlock},
		Credentials: s.Spec.Credentials.AsCredentials(), ElastiCache: p.AWS.ElastiCache, MemoryDB: p.AWS.MemoryDB,
	}
}
