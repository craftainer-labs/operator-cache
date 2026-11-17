package controller

import (
	"cmp"
	"context"
	"net"
	"strconv"
	"sync"

	"github.com/craftainer-labs/lib-operator-go/pkg/cloud"
	"github.com/craftainer-labs/lib-operator-go/pkg/server"

	cachev1alpha1 "github.com/craftainer-labs/operator-cache/api/v1alpha1"
	"github.com/craftainer-labs/operator-cache/internal/awscache"
)

// AWSServer is the AWS side of a server object: what cloud.Egress.AWS and the
// choice between ElastiCache and MemoryDB need.
type AWSServer struct {
	// Scope is the scope of the server kind: only a cluster server may use an
	// endpoint override or workload identity.
	Scope cloud.Scope
	// Block is provider.aws without the target.
	Block cloud.AWSClusterBlock
	// Credentials is spec.credentials; nil is workload identity.
	Credentials *cloud.Credentials
	// Secret is the data of the credentials Secret, nil for workload identity.
	Secret map[string][]byte
	// ElastiCache or MemoryDB is the target; exactly one is set (the CRD checks).
	ElastiCache *cachev1alpha1.ElastiCacheTarget
	MemoryDB    *cachev1alpha1.MemoryDBTarget
}

// NewAWSFunc builds the client of an AWS server's user group or ACL; production
// uses NewAWSClient, and tests replace it.
type NewAWSFunc func(ctx context.Context, s AWSServer, eg cloud.Egress) (awscache.Client, error)

// NewAWSClient is the NewAWSFunc of the operator. It resolves the credentials
// (a missing identity fails here, not on the first call) and builds the SDK
// client over the egress policy's HTTP client.
func NewAWSClient(ctx context.Context, s AWSServer, eg cloud.Egress) (awscache.Client, error) {
	cfg, err := eg.AWS(ctx, s.Scope, s.Block, s.Credentials, s.Secret)
	if err != nil {
		return nil, err
	}
	if s.MemoryDB != nil {
		return awscache.NewMemoryDB(cfg, s.MemoryDB.ACLName), nil
	}
	return awscache.NewElastiCache(cfg, s.ElastiCache.UserGroupID), nil
}

// awsBackend is a resolved AWS server: its data-plane address, for the binding,
// and the client of its user group or ACL, built on first use so that a claim
// that never calls AWS never resolves credentials. It is built again at every
// reconcile; nothing is cached between them.
type awsBackend struct {
	region string
	// host and port are the data-plane address: the server's own host and port
	// where it sets them, else what the probe discovered, else the default port.
	host, port string
	build      func(context.Context) (awscache.Client, error)

	mu     sync.Mutex
	client awscache.Client
}

// newAWSBackend resolves what a claim needs of an AWS server.
func (c Config) newAWSBackend(obj serverObject, cconn server.CloudConnection) *awsBackend {
	s := *obj.aws
	s.Secret = cconn.Credentials
	newAWS := c.NewAWS
	if newAWS == nil {
		newAWS = NewAWSClient
	}
	eg := cloud.Egress{Mode: c.EgressMode, Policy: c.Egress, Proxy: cconn.Proxy}
	port := ""
	if obj.cfg.Port != nil {
		port = strconv.Itoa(int(*obj.cfg.Port))
	}
	return &awsBackend{
		region: s.Block.Region,
		host:   cmp.Or(obj.cfg.Host, obj.discovered[cachev1alpha1.DiscoveredEndpoint]),
		port:   cmp.Or(port, obj.discovered[cachev1alpha1.DiscoveredPort], strconv.Itoa(int(cachev1alpha1.DefaultPort))),
		build:  func(ctx context.Context) (awscache.Client, error) { return newAWS(ctx, s, eg) },
	}
}

// api returns the client of the user group or ACL.
func (a *awsBackend) api(ctx context.Context) (awscache.Client, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.client == nil {
		c, err := a.build(ctx)
		if err != nil {
			return nil, coarse(ctx, "build the AWS client", err)
		}
		a.client = c
	}
	return a.client, nil
}

// address is the data-plane address, or ErrAddressUnknown.
func (a *awsBackend) address() (host, port string, err error) {
	if a.host == "" {
		return "", "", ErrAddressUnknown
	}
	return a.host, a.port, nil
}

// uri is the endpoint of the server, rediss://host:port, with no credentials.
func (a *awsBackend) uri() string {
	return cachev1alpha1.EndpointScheme + "://" + net.JoinHostPort(a.host, a.port)
}
