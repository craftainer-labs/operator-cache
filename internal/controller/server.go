// Package controller reconciles CacheUser claims with lib-operator-go's
// lifecycle.Reconcile (ADR 0002): it resolves the claim's server, logs in to it
// as the ACL administrator through the egress policy and hands the framework an
// External that drives the ACL users of package valkeyacl.
package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/craftainer-labs/lib-operator-go/pkg/apis/common"
	"github.com/craftainer-labs/lib-operator-go/pkg/server"

	cachev1alpha1 "github.com/craftainer-labs/operator-cache/api/v1alpha1"
	"github.com/craftainer-labs/operator-cache/internal/egress"
	"github.com/craftainer-labs/operator-cache/internal/valkeyacl"
)

// Keys of the admin Secret: the login of the ACL administrator.
const (
	AdminUsernameKey = "username"
	AdminPasswordKey = "password"
)

// Valkey is the ACL client the controller uses; *valkeyacl.Admin is the
// implementation, and tests replace it.
type Valkey interface {
	GetUser(ctx context.Context, name string) (valkeyacl.UserInfo, error)
	SetUser(ctx context.Context, name string, rules ...string) error
	DelUser(ctx context.Context, name string) (bool, error)
	// Alive reports whether the connection is still up; the cache connects
	// again when it is not.
	Alive() bool
	// Close ends the connection.
	Close()
}

var _ Valkey = (*valkeyacl.Admin)(nil)

// ConnectFunc logs in to a server; production uses ConnectAdmin.
type ConnectFunc func(ctx context.Context, cfg valkeyacl.Config) (Valkey, error)

// ConnectAdmin is the ConnectFunc of the operator: valkeyacl.Connect.
func ConnectAdmin(ctx context.Context, cfg valkeyacl.Config) (Valkey, error) {
	a, err := valkeyacl.Connect(ctx, cfg)
	return adminValkey(a), err
}

// adminValkey is the Valkey of an Admin, and nil for no Admin, so a failed
// connect does not hand back a Valkey that holds a nil pointer.
func adminValkey(a *valkeyacl.Admin) Valkey {
	if a == nil {
		return nil
	}
	return a
}

// Config is what the reconciler needs.
type Config struct {
	// Client reads and writes claims, servers and Secrets. Its Secret reads must
	// bypass the managed-by cache, which cannot see the admin Secret.
	Client client.Client
	// APIReader looks for binding Secrets (lifecycle.Options.APIReader).
	APIReader client.Reader
	Recorder  events.EventRecorder
	// ManagedBy is the managed-by label of binding Secrets and the audit
	// controller name.
	ManagedBy         string
	OperatorNamespace string
	// DefaultDeletionPolicy is the operator's default for data kinds; CacheUser
	// is a credential kind and has none, so it only goes to the framework.
	DefaultDeletionPolicy common.DeletionPolicy
	Egress                egress.Policy
	Connect               ConnectFunc
	// Cache keeps the connection of each server between reconciles; nil
	// connects for every use and closes the connection when the reconcile is
	// over, so the operator always sets one.
	Cache *ServerCache
	// MaxConcurrentReconciles is how many CacheUsers reconcile at once; below 1
	// means 1. A reconcile that waits on a dead server holds up the others until
	// its timeout, so a larger value bounds that.
	MaxConcurrentReconciles int
	// Now is the clock of the lifecycle (rotation and grace periods); nil is
	// time.Now. Tests advance it.
	Now func() time.Time
}

// backend is a resolved server.
type backend struct {
	cfg  cachev1alpha1.ServerConfig
	conn server.Connection
	// notReady is ErrServerNotReady while the server reports Ready=False.
	notReady error
	// server returns the logged-in connection to the server, connecting on first
	// use: a claim whose server is not Ready never connects.
	server func(ctx context.Context) (Valkey, error)
	// leases are the releases of the connections server handed out; release
	// calls them. The mutex guards the slice in case the lifecycle framework
	// calls server from more than one goroutine.
	leaseMu sync.Mutex
	leases  []func()
}

// release gives back every connection the reconcile used. The cache closes a
// connection it replaced meanwhile only now.
func (b *backend) release() {
	b.leaseMu.Lock()
	defer b.leaseMu.Unlock()
	for _, l := range b.leases {
		l()
	}
	b.leases = nil
}

// serverObject is the server a claim names, as much of it as the controller
// uses.
type serverObject struct {
	uid        types.UID
	generation int64
	cfg        cachev1alpha1.ServerConfig
	conditions []metav1.Condition
}

// backend resolves ref for a claim in ns: the connection settings through
// server.Resolve (which enforces allowedNamespaces), the server's own fields,
// and a connection that dials through the egress policy and the server's proxy.
// Its errors are coarse: conditions show a fixed text, the log has the raw error.
func (c Config) backend(ctx context.Context, ns string, ref common.ServerRef) (*backend, error) {
	key := serverKey(ns, ref)
	conn, err := server.Resolve(ctx, c.Client, ns, ref, server.WithOperatorNamespace(c.OperatorNamespace))
	if err != nil {
		// A server that is gone or no longer usable keeps no connection. A
		// namespace the server does not serve says nothing about the server
		// (other namespaces share the entry), so it is not on this list.
		if errors.Is(err, server.ErrServerNotFound) || errors.Is(err, server.ErrInvalidServer) ||
			errors.Is(err, server.ErrAdminSecretNamespaceForbidden) {
			c.Cache.Forget(key)
		}
		return nil, coarse(ctx, "resolve the server", err)
	}
	obj, err := c.fetchServer(ctx, ns, ref)
	if err != nil {
		return nil, coarse(ctx, "read the server", err)
	}
	dial, err := c.Egress.HostDialer(conn.Proxy)
	if err != nil {
		return nil, coarse(ctx, "build the dialer", fmt.Errorf("%w: %w", server.ErrInvalidServer, err))
	}
	cfg, err := adminConfig(obj.cfg, conn, dial)
	if err != nil {
		return nil, coarse(ctx, "read the admin Secret", err)
	}
	version := stamp(obj, cfg, conn.Proxy)
	b := &backend{cfg: obj.cfg, conn: conn}
	if serverNotReady(obj.conditions) {
		b.notReady = ErrServerNotReady
	}
	b.server = func(ctx context.Context) (Valkey, error) {
		v, release, err := c.Cache.Get(ctx, key, version, func(ctx context.Context) (Valkey, error) { return c.Connect(ctx, cfg) })
		if err != nil {
			return nil, coarse(ctx, "connect to the server", err)
		}
		b.leaseMu.Lock()
		b.leases = append(b.leases, release)
		b.leaseMu.Unlock()
		return v, nil
	}
	return b, nil
}

// adminConfig is the connection to a server: its address, the admin login of
// conn and the TLS settings.
func adminConfig(spec cachev1alpha1.ServerConfig, conn server.Connection, dial egress.DialFunc) (valkeyacl.Config, error) {
	user, pass := conn.Admin[AdminUsernameKey], conn.Admin[AdminPasswordKey]
	if len(user) == 0 || len(pass) == 0 {
		return valkeyacl.Config{}, fmt.Errorf("%w: %w", server.ErrInvalidServer, ErrAdminCredentials)
	}
	return valkeyacl.Config{
		Address: spec.Address(), CA: conn.CABundle, ServerName: conn.ServerName, MinTLS: conn.MinTLSVersion,
		Username: string(user), Password: string(pass), Dial: dial,
	}, nil
}

// fetchServer reads the server's own fields, which server.Connection does not
// carry, its identity for the connection cache and its conditions.
func (c Config) fetchServer(ctx context.Context, ns string, ref common.ServerRef) (serverObject, error) {
	if ref.Kind == kindClusterCacheServer {
		var s cachev1alpha1.ClusterCacheServer
		err := c.Client.Get(ctx, client.ObjectKey{Name: ref.Name}, &s)
		return serverObject{s.UID, s.Generation, s.Spec.ServerConfig, s.Status.Conditions}, err
	}
	var s cachev1alpha1.CacheServer
	err := c.Client.Get(ctx, client.ObjectKey{Namespace: ns, Name: ref.Name}, &s)
	return serverObject{s.UID, s.Generation, s.Spec.ServerConfig, s.Status.Conditions}, err
}

// stamp identifies what a cached connection was made from: the server object
// (its UID and generation) and a hash of everything the connection takes from
// outside it, which the generation does not cover: the credentials, the CA
// bundle and the proxy. A changed or rotated admin password therefore replaces
// the connection. The credentials only go into the hash.
func stamp(o serverObject, cfg valkeyacl.Config, proxy *url.URL) string {
	h := sha256.New()
	for _, part := range [][]byte{
		[]byte(cfg.Address), cfg.CA, []byte(cfg.ServerName), []byte(strconv.FormatUint(uint64(cfg.MinTLS), 10)),
		[]byte(cfg.Username), []byte(cfg.Password), []byte(proxyText(proxy)),
	} {
		// Length-prefixed, so ("ab","c") and ("a","bc") differ.
		_, _ = h.Write(strconv.AppendInt(nil, int64(len(part)), 10))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write(part)
	}
	return fmt.Sprintf("%s/%d/%s", o.uid, o.generation, hex.EncodeToString(h.Sum(nil)))
}

func proxyText(u *url.URL) string {
	if u == nil {
		return ""
	}
	return u.String()
}
