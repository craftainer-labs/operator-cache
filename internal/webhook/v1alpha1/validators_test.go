package v1alpha1

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/craftainer-labs/lib-operator-go/pkg/apis/common"
	cachev1alpha1 "github.com/craftainer-labs/operator-cache/api/v1alpha1"
)

func cacheServer(host, proxy string) *cachev1alpha1.CacheServer {
	return &cachev1alpha1.CacheServer{
		ObjectMeta: metav1.ObjectMeta{Name: "valkey", Namespace: "default"},
		Spec: cachev1alpha1.CacheServerSpec{
			ServerCommon: common.ServerCommon{ProxyURL: proxy},
			ServerConfig: cachev1alpha1.ServerConfig{Host: host},
		},
	}
}

func clusterCacheServer(host, proxy string) *cachev1alpha1.ClusterCacheServer {
	return &cachev1alpha1.ClusterCacheServer{
		ObjectMeta: metav1.ObjectMeta{Name: "valkey"},
		Spec: cachev1alpha1.ClusterCacheServerSpec{
			ServerCommon: common.ServerCommon{ProxyURL: proxy},
			ServerConfig: cachev1alpha1.ServerConfig{Host: host},
		},
	}
}

// serverCases are the host and proxy rules, shared by both server kinds.
var serverCases = []struct {
	name, host, proxy string
	field             string // the field the error names; empty means valid
	message           string // a substring of the error
}{
	{name: "valid", host: "valkey.cache.svc.cluster.local"},
	{name: "valid upper case", host: "Valkey.Example.COM"},
	{name: "valid fully qualified", host: "valkey.example.com."},
	{name: "valid IPv4", host: "10.0.0.7"},
	{name: "valid IPv6", host: "fd00::7"},
	{name: "valid with a proxy", host: "valkey", proxy: "socks5://proxy:1080"},
	{name: "host with a scheme", host: "rediss://valkey", field: "spec.host"},
	{name: "host with a port", host: "valkey:6379", field: "spec.host"},
	{name: "host with userinfo", host: "user:pass@valkey", field: "spec.host"},
	{name: "empty host", host: "", field: "spec.host"},
	{name: "host with a leading hyphen", host: "-valkey", field: "spec.host"},
	{name: "proxy with another scheme", host: "valkey", proxy: "ftp://proxy", field: "spec.proxyURL", message: "http, https or socks5"},
	{name: "proxy without a host", host: "valkey", proxy: "http://", field: "spec.proxyURL", message: "http, https or socks5"},
	{name: "malformed proxy", host: "valkey", proxy: "http://[::1", field: "spec.proxyURL", message: "http, https or socks5"},
	{name: "proxy with a user", host: "valkey", proxy: "http://user@proxy:3128", field: "spec.proxyURL", message: "must not carry credentials"},
	{name: "proxy with a password", host: "valkey", proxy: "socks5://user:hunter2@proxy:1080", field: "spec.proxyURL", message: "must not carry credentials"},
	{name: "valid proxy port bounds", host: "valkey", proxy: "http://proxy:65535"},
	{name: "proxy port too high", host: "valkey", proxy: "http://proxy:65536", field: "spec.proxyURL", message: "port must be between 1 and 65535"},
	{name: "proxy port zero", host: "valkey", proxy: "http://proxy:0", field: "spec.proxyURL", message: "port must be between 1 and 65535"},
	{name: "proxy with an empty userinfo", host: "valkey", proxy: "https://:@proxy:3128", field: "spec.proxyURL", message: "must not carry credentials"},
}

func expectField(g *WithT, err error, field, message string) {
	if field == "" {
		g.Expect(err).NotTo(HaveOccurred())
		return
	}
	g.Expect(apierrors.IsInvalid(err)).To(BeTrue(), "%v", err)
	g.Expect(err.Error()).To(ContainSubstring(field))
	g.Expect(err.Error()).To(ContainSubstring(message))
}

func TestCacheServerValidator(t *testing.T) {
	v := &CacheServerValidator{}
	ctx := context.Background()
	for _, tc := range serverCases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			warnings, err := v.ValidateCreate(ctx, cacheServer(tc.host, tc.proxy))
			g.Expect(warnings).To(BeEmpty())
			expectField(g, err, tc.field, tc.message)

			_, err = v.ValidateUpdate(ctx, cacheServer("old", ""), cacheServer(tc.host, tc.proxy))
			expectField(g, err, tc.field, tc.message)
		})
	}

	g := NewWithT(t)
	// An object being deleted is admitted while its spec is unchanged, so
	// finalizer removal is never blocked; a spec change is still validated.
	deleting := cacheServer("not a host", "ftp://bad")
	now := metav1.Now()
	deleting.DeletionTimestamp = &now
	_, err := v.ValidateUpdate(ctx, cacheServer("not a host", "ftp://bad"), deleting)
	g.Expect(err).NotTo(HaveOccurred())
	_, err = v.ValidateUpdate(ctx, cacheServer("old", ""), deleting)
	g.Expect(err).To(HaveOccurred())
	_, err = v.ValidateDelete(ctx, cacheServer("not a host", ""))
	g.Expect(err).NotTo(HaveOccurred())
}

func TestClusterCacheServerValidator(t *testing.T) {
	v := &ClusterCacheServerValidator{}
	ctx := context.Background()
	for _, tc := range serverCases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			warnings, err := v.ValidateCreate(ctx, clusterCacheServer(tc.host, tc.proxy))
			g.Expect(warnings).To(BeEmpty())
			expectField(g, err, tc.field, tc.message)

			_, err = v.ValidateUpdate(ctx, clusterCacheServer("old", ""), clusterCacheServer(tc.host, tc.proxy))
			expectField(g, err, tc.field, tc.message)
		})
	}

	g := NewWithT(t)
	deleting := clusterCacheServer("not a host", "ftp://bad")
	now := metav1.Now()
	deleting.DeletionTimestamp = &now
	_, err := v.ValidateUpdate(ctx, clusterCacheServer("not a host", "ftp://bad"), deleting)
	g.Expect(err).NotTo(HaveOccurred())
	_, err = v.ValidateUpdate(ctx, clusterCacheServer("old", ""), deleting)
	g.Expect(err).To(HaveOccurred())
	_, err = v.ValidateDelete(ctx, clusterCacheServer("not a host", ""))
	g.Expect(err).NotTo(HaveOccurred())
}

// An admission error never echoes a host or a URL: either can hold a password.
func TestServerErrorsDoNotEchoTheValues(t *testing.T) {
	g := NewWithT(t)
	_, err := (&CacheServerValidator{}).ValidateCreate(context.Background(),
		cacheServer("user:hunter2@valkey", "http://user:hunter2@proxy:3128"))
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).NotTo(ContainSubstring("hunter2"))
	g.Expect(err.Error()).NotTo(ContainSubstring("proxy:3128"))
	g.Expect(err.Error()).To(And(ContainSubstring("spec.host"), ContainSubstring("spec.proxyURL")))
}
