//go:build integration

package controller

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/craftainer-labs/lib-operator-go/pkg/binding"

	cachev1alpha1 "github.com/craftainer-labs/operator-cache/api/v1alpha1"
)

// Integration is the External of one CacheUser wired to a given server and to an
// in-memory binding Secret instead of an API server, for test/integration, which
// runs the controller's paths (create, observe, update, rotate, delete) against a
// real Valkey. It is built only with the integration tag: nothing of it is in the
// operator or under the coverage gate.
type Integration struct {
	*userExternal
	// Binding is the data of the binding Secret as the lifecycle would keep it:
	// the test stores what Create, Update and RotateBegin return, as the
	// lifecycle writes it. Nil is no Secret.
	Binding binding.Data
}

// NewIntegration returns the External of the claim namespace/name on v, whose
// server is described by server and ca.
func NewIntegration(v Valkey, namespace, name string, server cachev1alpha1.ServerConfig, ca []byte) *Integration {
	i := &Integration{}
	be := &backend{cfg: server, server: func(context.Context) (Valkey, error) { return v, nil }}
	be.conn.CABundle = ca
	claim := &cachev1alpha1.CacheUser{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name, UID: "integration"}}
	e := newUserExternal(be, claim)
	e.persist = func(context.Context) error { return nil }
	e.binding = func(context.Context) (*corev1.Secret, error) {
		if i.Binding == nil {
			return nil, nil
		}
		return &corev1.Secret{Data: i.Binding}, nil
	}
	e.drop = func(context.Context, *corev1.Secret) error { i.Binding = nil; return nil }
	i.userExternal = e
	return i
}

// Claim is the claim the External acts for; its status holds externalName and
// externalId as the lifecycle would persist them.
func (i *Integration) Claim() *cachev1alpha1.CacheUser { return i.claim }
