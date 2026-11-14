package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/craftainer-labs/lib-operator-go/pkg/apis/common"
)

// +kubebuilder:validation:XValidation:rule="has(self.adminCredentialsSecretRef.namespace) && self.adminCredentialsSecretRef.namespace != ''",message="adminCredentialsSecretRef.namespace is required on a cluster-scoped server"

// ClusterCacheServerSpec defines the desired state of ClusterCacheServer.
type ClusterCacheServerSpec struct {
	common.ServerCommon `json:",inline"`
	ServerConfig        `json:",inline"`

	// allowedNamespaces says which namespaces may claim from this server. It is
	// required, so a server without it is rejected: it fails closed.
	AllowedNamespaces common.AllowedNamespaces `json:"allowedNamespaces"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,categories=craftainer
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Synced",type=string,JSONPath=`.status.conditions[?(@.type=="Synced")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ClusterCacheServer is a cluster-scoped Valkey or Redis server that the namespaces in
// allowedNamespaces may claim from.
type ClusterCacheServer struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of ClusterCacheServer
	// +required
	Spec ClusterCacheServerSpec `json:"spec"`

	// status defines the observed state of ClusterCacheServer
	// +optional
	Status common.ServerStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// ClusterCacheServerList contains a list of ClusterCacheServer.
type ClusterCacheServerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []ClusterCacheServer `json:"items"`
}

// GetCommon returns the settings every server kind shares.
func (s *ClusterCacheServer) GetCommon() *common.ServerCommon { return &s.Spec.ServerCommon }

// GetServerStatus returns the status the probe writes (probe.Server).
func (s *ClusterCacheServer) GetServerStatus() *common.ServerStatus { return &s.Status }

// GetEndpoint returns the server's URL, rediss://host:port.
func (s *ClusterCacheServer) GetEndpoint() string { return s.Spec.Endpoint() }

// GetAllowedNamespaces returns spec.allowedNamespaces.
func (s *ClusterCacheServer) GetAllowedNamespaces() *common.AllowedNamespaces {
	return &s.Spec.AllowedNamespaces
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &ClusterCacheServer{}, &ClusterCacheServerList{})
		return nil
	})
}
