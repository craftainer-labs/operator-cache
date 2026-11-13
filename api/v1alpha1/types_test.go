package v1alpha1

import (
	"testing"
	"time"

	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/craftainer-labs/lib-operator-go/pkg/apis/common"
	"github.com/craftainer-labs/lib-operator-go/pkg/lifecycle"
	"github.com/craftainer-labs/lib-operator-go/pkg/server"
)

// The claim kind and the server kinds must satisfy the interfaces the
// lib-operator-go reconciler is written against.
var (
	_ lifecycle.Claim = (*CacheUser)(nil)
	_ server.Server   = (*CacheServer)(nil)
	_ server.Server   = (*ClusterCacheServer)(nil)
)

func TestAddToSchemeRegistersEveryKind(t *testing.T) {
	g := NewWithT(t)
	s := runtime.NewScheme()
	g.Expect(AddToScheme(s)).To(Succeed())
	for _, kind := range []string{
		"ClusterCacheServer", "ClusterCacheServerList", "CacheServer", "CacheServerList",
		"CacheUser", "CacheUserList",
	} {
		g.Expect(s.Recognizes(GroupVersion.WithKind(kind))).To(BeTrue(), kind)
	}
	g.Expect(GroupVersion.Group).To(Equal("cache.craftainer.dev"))
}

func TestEndpointUsesTheDefaults(t *testing.T) {
	g := NewWithT(t)
	port := int32(6380)
	for _, tc := range []struct {
		name   string
		config ServerConfig
		want   string
	}{
		{"defaults", ServerConfig{Host: "valkey.cache.svc"}, "rediss://valkey.cache.svc:6379"},
		{"on a port", ServerConfig{Host: "cache.example.com", Port: &port}, "rediss://cache.example.com:6380"},
		{"IPv6", ServerConfig{Host: "fd00::7"}, "rediss://[fd00::7]:6379"},
	} {
		g.Expect(tc.config.Endpoint()).To(Equal(tc.want), tc.name)
	}
	g.Expect((&ServerConfig{Host: "valkey", Port: &port}).Address()).To(Equal("valkey:6380"))
}

func TestCacheServerAccessors(t *testing.T) {
	g := NewWithT(t)
	b := &CacheServer{Spec: CacheServerSpec{
		ServerCommon: common.ServerCommon{ProxyURL: "http://proxy:3128"},
		ServerConfig: ServerConfig{Host: "valkey"},
	}}
	g.Expect(b.GetCommon().ProxyURL).To(Equal("http://proxy:3128"))
	g.Expect(b.GetEndpoint()).To(Equal("rediss://valkey:6379"))
	g.Expect(b.GetAllowedNamespaces()).To(BeNil())
}

func TestClusterCacheServerAccessors(t *testing.T) {
	g := NewWithT(t)
	b := &ClusterCacheServer{Spec: ClusterCacheServerSpec{
		ServerCommon:      common.ServerCommon{ProxyURL: "http://proxy:3128"},
		ServerConfig:      ServerConfig{Host: "valkey"},
		AllowedNamespaces: common.AllowedNamespaces{From: common.NamespacesAll},
	}}
	g.Expect(b.GetCommon().ProxyURL).To(Equal("http://proxy:3128"))
	g.Expect(b.GetEndpoint()).To(Equal("rediss://valkey:6379"))
	g.Expect(b.GetAllowedNamespaces().From).To(Equal(common.NamespacesAll))
}

func TestCacheUserAccessors(t *testing.T) {
	g := NewWithT(t)
	rotation := &common.RotationPolicy{}
	u := &CacheUser{
		ObjectMeta: metav1.ObjectMeta{Name: "session-store"},
		Spec: CacheUserSpec{
			ClaimCommon: common.ClaimCommon{
				ServerRef:          common.ServerRef{Kind: "ClusterCacheServer", Name: "valkey"},
				ManagementPolicies: []common.ManagementPolicy{common.ManagementAll},
			},
			Credentials: common.Credentials{RotationPolicy: rotation},
		},
	}
	g.Expect(u.GetServerRef()).To(Equal(common.ServerRef{Kind: "ClusterCacheServer", Name: "valkey"}))
	g.Expect(u.GetDeletionPolicy()).To(BeEmpty())
	g.Expect(u.GetManagementPolicies()).To(Equal([]common.ManagementPolicy{common.ManagementAll}))

	*u.GetConditions() = append(*u.GetConditions(), metav1.Condition{Type: "Synced"})
	g.Expect(u.Status.Conditions).To(HaveLen(1))

	// The binding Secret is named after the claim unless the spec names it.
	g.Expect(u.GetBindingSecretName()).To(Equal("session-store"))
	u.Spec.Binding.SecretName = "session-cache"
	g.Expect(u.GetBindingSecretName()).To(Equal("session-cache"))
	u.SetBindingName("session-cache")
	g.Expect(u.Status.Binding.Name).To(Equal("session-cache"))

	u.SetExternalName("team-a:session-store")
	g.Expect(u.GetExternalName()).To(Equal("team-a:session-store"))
	u.SetExternalID("id-1")
	g.Expect(u.GetExternalID()).To(Equal("id-1"))
	u.SetObservedGeneration(4)
	g.Expect(u.Status.ObservedGeneration).To(Equal(int64(4)))
	now := metav1.NewTime(time.Unix(1700000000, 0))
	u.SetLastReconcileTime(now)
	g.Expect(u.Status.LastReconcileTime.Equal(&now)).To(BeTrue())

	u.SetEffectiveDeletionPolicy(common.DeletionRetain, common.SourceFallback)
	g.Expect(u.GetEffectiveDeletionPolicy()).To(BeEmpty())

	u.SetLateInitialized(map[string]string{"k": "v"})
	g.Expect(u.Status.LateInitialized).To(HaveKeyWithValue("k", "v"))

	g.Expect(u.GetRotationPolicy()).To(BeIdenticalTo(rotation))
	g.Expect(u.GetRotationStatus()).To(Equal(common.RotationStatus{}))
	u.SetRotationStatus(common.RotationStatus{Active: "g1", Sequence: 2})
	g.Expect(u.GetRotationStatus()).To(Equal(common.RotationStatus{Active: "g1", Sequence: 2}))
}
