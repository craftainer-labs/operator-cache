//go:build integration

package controller

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/craftainer-labs/lib-operator-go/pkg/binding"

	cachev1alpha1 "github.com/craftainer-labs/operator-cache/api/v1alpha1"
	"github.com/craftainer-labs/operator-cache/internal/awscache"
)

// IntegrationAWS is the External of one Password-mode CacheUser on an AWS server,
// wired to a given awscache.Client and to an in-memory binding Secret instead of
// an API server, for test/integration/awscloud, which runs the controller's paths
// (create, observe, update, rotate, delete) against moto and the fake in front of
// it. It exists only with the integration tag: nothing of it is in the operator
// or under the coverage gate.
type IntegrationAWS struct {
	awsPassword
	// Binding is the data of the binding Secret as the lifecycle would keep it:
	// the test stores what Create and RotateBegin return. Nil is no Secret.
	Binding binding.Data
}

// IntegrationAWSIAM is the External of one IAM-mode CacheUser, wired like
// IntegrationAWS.
type IntegrationAWSIAM struct {
	awsIAM
	// Binding is as for IntegrationAWS.
	Binding binding.Data
}

// NewIntegrationAWS returns the External of the Password-mode claim
// namespace/name, whose server is in region at host:port and whose user group or
// ACL is reached through api.
func NewIntegrationAWS(api awscache.Client, namespace, name, region, host, port string) *IntegrationAWS {
	i := &IntegrationAWS{}
	i.awsPassword = awsPassword{integrationUser(api, namespace, name, region, host, port, false, func() binding.Data { return i.Binding })}
	return i
}

// NewIntegrationAWSIAM is NewIntegrationAWS for a claim of authMode IAM.
func NewIntegrationAWSIAM(api awscache.Client, namespace, name, region, host, port string) *IntegrationAWSIAM {
	i := &IntegrationAWSIAM{}
	i.awsIAM = awsIAM{integrationUser(api, namespace, name, region, host, port, true, func() binding.Data { return i.Binding })}
	return i
}

// Claim is the claim the External acts for; its status holds externalName and
// externalId as the lifecycle would persist them.
func (i *IntegrationAWS) Claim() *cachev1alpha1.CacheUser { return i.claim }

// Claim is the claim the External acts for.
func (i *IntegrationAWSIAM) Claim() *cachev1alpha1.CacheUser { return i.claim }

func integrationUser(api awscache.Client, namespace, name, region, host, port string, iam bool, get func() binding.Data) *awsUser {
	be := &backend{aws: &awsBackend{
		region: region, host: host, port: port,
		build: func(context.Context) (awscache.Client, error) { return api, nil },
	}}
	claim := &cachev1alpha1.CacheUser{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name, UID: "integration-aws"}}
	if iam {
		claim.Spec.AuthMode = cachev1alpha1.AuthModeIAM
	}
	_, x := newAWSExternal(be, claim)
	x.persist = func(context.Context) error { return nil }
	x.binding = func(context.Context) (*corev1.Secret, error) {
		d := get()
		if d == nil {
			return nil, nil
		}
		return &corev1.Secret{Data: d}, nil
	}
	return x
}
