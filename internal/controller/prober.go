package controller

import (
	"context"
	"fmt"

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

var _ probe.Prober = Prober{}

// Probe returns the version of the server srv names, or an error that names
// only a category of failure.
func (p Prober) Probe(ctx context.Context, srv server.Server, conn server.Connection) (string, error) {
	spec, err := serverConfigOf(srv)
	if err != nil {
		return "", err
	}
	dial, err := p.Config.Egress.HostDialer(conn.Proxy)
	if err != nil {
		return "", categoryOnly(fmt.Errorf("%w: %w", server.ErrInvalidServer, err))
	}
	cfg, err := adminConfig(spec, conn, dial)
	if err != nil {
		return "", categoryOnly(err)
	}
	run := p.Check
	if run == nil {
		run = valkeyacl.Probe
	}
	version, err := run(ctx, cfg)
	return version, categoryOnly(err)
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

// serverConfigOf returns the Valkey fields of a server kind of this operator.
func serverConfigOf(srv server.Server) (cachev1alpha1.ServerConfig, error) {
	switch s := srv.(type) {
	case *cachev1alpha1.ClusterCacheServer:
		return s.Spec.ServerConfig, nil
	case *cachev1alpha1.CacheServer:
		return s.Spec.ServerConfig, nil
	}
	return cachev1alpha1.ServerConfig{}, fmt.Errorf("%w: %T", server.ErrUnknownServerKind, srv)
}
