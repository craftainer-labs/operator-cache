package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/craftainer-labs/lib-operator-go/pkg/apis/common"
)

// CacheServerSpec defines the desired state of CacheServer.
//
// +kubebuilder:validation:XValidation:rule="!has(self.adminCredentialsSecretRef.namespace)",message="adminCredentialsSecretRef.namespace must not be set: the Secret is in the CacheServer's namespace"
type CacheServerSpec struct {
	common.ServerCommon `json:",inline"`
	ServerConfig        `json:",inline"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:categories=craftainer
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Synced",type=string,JSONPath=`.status.conditions[?(@.type=="Synced")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// CacheServer is a Valkey or Redis server that only claims in its own namespace may use.
type CacheServer struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of CacheServer
	// +required
	Spec CacheServerSpec `json:"spec"`

	// status defines the observed state of CacheServer
	// +optional
	Status common.ServerStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// CacheServerList contains a list of CacheServer.
type CacheServerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []CacheServer `json:"items"`
}

// GetCommon returns the settings every server kind shares.
func (s *CacheServer) GetCommon() *common.ServerCommon { return &s.Spec.ServerCommon }

// GetEndpoint returns the server's URL, rediss://host:port.
func (s *CacheServer) GetEndpoint() string { return s.Spec.Endpoint() }

// GetAllowedNamespaces returns nil: a namespaced server serves only its own
// namespace.
func (s *CacheServer) GetAllowedNamespaces() *common.AllowedNamespaces { return nil }

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &CacheServer{}, &CacheServerList{})
		return nil
	})
}
