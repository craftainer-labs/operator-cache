package v1alpha1

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/craftainer-labs/lib-operator-go/pkg/apis/common"
	cachev1alpha1 "github.com/craftainer-labs/operator-cache/api/v1alpha1"
)

// These specs send objects through the API server with the generated CRDs
// installed, so they check the schema defaults and the CEL rules next to the
// webhooks: the CRDs are the API's contract and the webhooks only add to it.

var _ = Describe("The cache.craftainer.dev CRDs", func() {
	const ns = "default"

	// create creates obj and deletes it when the spec ends.
	create := func(obj client.Object) error {
		err := k8sClient.Create(ctx, obj)
		if err == nil {
			DeferCleanup(func() { Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, obj))).To(Succeed()) })
		}
		return err
	}
	rejected := func(err error, substring string) {
		GinkgoHelper()
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring(substring))
	}

	clusterServer := func(name string) *cachev1alpha1.ClusterCacheServer {
		return &cachev1alpha1.ClusterCacheServer{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: cachev1alpha1.ClusterCacheServerSpec{
				ServerCommon: common.ServerCommon{
					AdminCredentialsSecretRef: common.AdminSecretRef{Name: "admin", Namespace: "craftainer-system"},
				},
				ServerConfig:      cachev1alpha1.ServerConfig{Host: "valkey.cache.svc"},
				AllowedNamespaces: common.AllowedNamespaces{From: common.NamespacesAll},
			},
		}
	}
	server := func(name string) *cachev1alpha1.CacheServer {
		return &cachev1alpha1.CacheServer{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: cachev1alpha1.CacheServerSpec{
				ServerCommon: common.ServerCommon{AdminCredentialsSecretRef: common.AdminSecretRef{Name: "admin"}},
				ServerConfig: cachev1alpha1.ServerConfig{Host: "valkey.team-a.svc"},
			},
		}
	}
	user := func(name string) *cachev1alpha1.CacheUser {
		return &cachev1alpha1.CacheUser{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: cachev1alpha1.CacheUserSpec{
				ClaimCommon: common.ClaimCommon{ServerRef: common.ServerRef{Kind: "CacheServer", Name: "valkey"}},
			},
		}
	}

	Describe("ClusterCacheServer", func() {
		It("applies the schema defaults", func() {
			Expect(create(clusterServer("defaults"))).To(Succeed())
			got := &cachev1alpha1.ClusterCacheServer{}
			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: "defaults"}, got)).To(Succeed())
			Expect(got.Spec.Port).To(HaveValue(Equal(int32(6379))))
			Expect(got.Spec.Mode).To(Equal(cachev1alpha1.ModeStandalone))
			Expect(got.GetEndpoint()).To(Equal("rediss://valkey.cache.svc:6379"))
		})

		It("requires the admin Secret's namespace", func() {
			b := clusterServer("no-ns")
			b.Spec.AdminCredentialsSecretRef.Namespace = ""
			rejected(create(b), "adminCredentialsSecretRef.namespace is required")
		})

		It("fails closed without a valid allowedNamespaces", func() {
			b := clusterServer("no-from")
			b.Spec.AllowedNamespaces = common.AllowedNamespaces{}
			rejected(create(b), "allowedNamespaces")

			b = clusterServer("selector-missing")
			b.Spec.AllowedNamespaces = common.AllowedNamespaces{From: common.NamespacesSelector}
			rejected(create(b), "selector is required when from is Selector")

			b = clusterServer("selector-empty")
			b.Spec.AllowedNamespaces = common.AllowedNamespaces{From: common.NamespacesSelector, Selector: &common.LabelSelector{}}
			rejected(create(b), "selector must have matchLabels or matchExpressions")

			b = clusterServer("selector-with-all")
			b.Spec.AllowedNamespaces.Selector = &common.LabelSelector{MatchLabels: map[string]string{"a": "b"}}
			rejected(create(b), "selector must not be set when from is All")
		})

		It("admits a selector that matches labels", func() {
			b := clusterServer("selector")
			b.Spec.AllowedNamespaces = common.AllowedNamespaces{
				From:     common.NamespacesSelector,
				Selector: &common.LabelSelector{MatchLabels: map[string]string{"craftainer.dev/cache": "allowed"}},
			}
			Expect(create(b)).To(Succeed())
		})

		It("rejects a host that is not a bare host name", func() {
			for _, host := range []string{"rediss://valkey", "valkey:6379", "user:pw@valkey", "valkey/path", "bad host"} {
				b := clusterServer("bad-host")
				b.Spec.Host = host
				rejected(create(b), "spec.host")
			}
		})

		It("rejects a port out of range and an unsupported mode", func() {
			b := clusterServer("bad-port")
			zero := int32(0)
			b.Spec.Port = &zero
			rejected(create(b), "port")

			b = clusterServer("cluster-mode")
			b.Spec.Mode = "Cluster"
			rejected(create(b), "mode")
		})

		It("rejects a proxy URL with credentials in the webhook, without echoing it", func() {
			b := clusterServer("proxy-userinfo")
			b.Spec.ProxyURL = "http://user:hunter2@proxy:3128"
			err := create(b)
			rejected(err, "must not carry credentials")
			Expect(err.Error()).NotTo(ContainSubstring("hunter2"))
		})

		It("rejects a proxy URL with an unsupported scheme in the webhook", func() {
			b := clusterServer("bad-proxy")
			b.Spec.ProxyURL = "ftp://proxy"
			rejected(create(b), "must be an http, https or socks5 URL with a host")
		})
	})

	Describe("CacheServer", func() {
		It("is admitted with the Secret in its own namespace", func() {
			Expect(create(server("plain"))).To(Succeed())
		})

		It("forbids a namespace on the admin Secret", func() {
			b := server("with-ns")
			b.Spec.AdminCredentialsSecretRef.Namespace = "elsewhere"
			rejected(create(b), "adminCredentialsSecretRef.namespace must not be set")
		})

		It("rejects a host that is not a host name in the webhook", func() {
			b := server("leading-hyphen")
			b.Spec.Host = "-valkey"
			rejected(create(b), "must be a host name or an IP address")
		})
	})

	Describe("CacheUser", func() {
		It("applies the schema defaults", func() {
			Expect(create(user("defaults"))).To(Succeed())
			got := &cachev1alpha1.CacheUser{}
			Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "defaults"}, got)).To(Succeed())
			Expect(got.Spec.ManagementPolicies).To(Equal([]common.ManagementPolicy{common.ManagementAll}))
			Expect(got.Spec.Profile).To(Equal(cachev1alpha1.ProfileReadWrite))
			Expect(got.Spec.Channels).To(BeFalse())
			Expect(got.Spec.RotationPolicy).To(BeNil())
		})

		It("admits both profiles and channels", func() {
			u := user("read-only")
			u.Spec.Profile = cachev1alpha1.ProfileReadOnly
			u.Spec.Channels = true
			Expect(create(u)).To(Succeed())
		})

		It("rejects an unknown profile", func() {
			u := user("admin")
			u.Spec.Profile = "Admin"
			rejected(create(u), "profile")
		})

		It("has no deletionPolicy, key pattern or command list", func() {
			u := &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": "cache.craftainer.dev/v1alpha1",
				"kind":       "CacheUser",
				"metadata":   map[string]any{"name": "pruned", "namespace": ns},
				"spec": map[string]any{
					"serverRef":      map[string]any{"kind": "CacheServer", "name": "valkey"},
					"deletionPolicy": "Retain",
					"keys":           "~*",
					"commands":       "+@all",
				},
			}}
			Expect(create(u)).To(Succeed())
			stored := &unstructured.Unstructured{}
			stored.SetGroupVersionKind(cachev1alpha1.GroupVersion.WithKind("CacheUser"))
			Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "pruned"}, stored)).To(Succeed())
			spec, _, _ := unstructured.NestedMap(stored.Object, "spec")
			Expect(spec).NotTo(HaveKey("deletionPolicy"))
			Expect(spec).NotTo(HaveKey("keys"))
			Expect(spec).NotTo(HaveKey("commands"))
		})

		It("rejects a server kind of another group", func() {
			u := user("other-kind")
			u.Spec.ServerRef.Kind = "DatabaseServer"
			rejected(create(u), "serverRef.kind must be ClusterCacheServer or CacheServer")
		})

		It("keeps serverRef immutable", func() {
			u := user("pinned")
			Expect(create(u)).To(Succeed())
			u.Spec.ServerRef.Name = "other"
			rejected(k8sClient.Update(ctx, u), "serverRef is immutable")
		})

		It("needs the Update management policy for a rotationPolicy", func() {
			u := user("rotating")
			u.Spec.ManagementPolicies = []common.ManagementPolicy{common.ManagementObserve, common.ManagementCreate}
			u.Spec.RotationPolicy = &common.RotationPolicy{Interval: &metav1.Duration{Duration: 2160 * time.Hour}}
			rejected(create(u), "rotationPolicy needs the Update management policy")

			u = user("rotating")
			u.Spec.RotationPolicy = &common.RotationPolicy{Interval: &metav1.Duration{Duration: 2160 * time.Hour}}
			Expect(create(u)).To(Succeed())
		})
	})
})
