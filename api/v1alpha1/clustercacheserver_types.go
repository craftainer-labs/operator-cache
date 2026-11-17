package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/craftainer-labs/lib-operator-go/pkg/apis/common"
	"github.com/craftainer-labs/lib-operator-go/pkg/cloud"
)

// +kubebuilder:validation:XValidation:rule="!has(self.adminCredentialsSecretRef) || (has(self.adminCredentialsSecretRef.namespace) && self.adminCredentialsSecretRef.namespace != '')",message="adminCredentialsSecretRef.namespace is required on a cluster-scoped server"
// +kubebuilder:validation:XValidation:rule="has(self.provider) ? !has(self.adminCredentialsSecretRef) : has(self.adminCredentialsSecretRef)",message="adminCredentialsSecretRef is required without provider and forbidden with one"
// +kubebuilder:validation:XValidation:rule="has(self.provider) || (has(self.host) && size(self.host) > 0)",message="host is required without provider"
// +kubebuilder:validation:XValidation:rule="has(self.provider) == has(oldSelf.provider)",message="provider cannot be added or removed: a server does not move between self-hosted and a cloud"
// +kubebuilder:validation:XValidation:rule="!has(self.provider) || self.provider.type == 'AWS'",message="only provider.type AWS is supported"
// +kubebuilder:validation:XValidation:rule="!has(self.credentials) || has(self.provider)",message="credentials is allowed only with provider"

// ClusterCacheServerSpec defines the desired state of ClusterCacheServer.
type ClusterCacheServerSpec struct {
	common.CloudServerCommon `json:",inline"`
	ServerConfig             `json:",inline"`

	// provider says the server is a managed cloud cache. Its type is immutable:
	// claims already point at objects in the cloud it names.
	// +optional
	Provider *ClusterProvider `json:"provider,omitempty"`

	// credentials are the operator's credentials to the cloud API. Absent with
	// a provider: WorkloadIdentity.
	// +optional
	Credentials *cloud.Credentials `json:"credentials,omitempty"`

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

// GetCloudCommon returns the settings every cloud-capable server kind shares
// (server.CloudServer).
func (s *ClusterCacheServer) GetCloudCommon() *common.CloudServerCommon {
	return &s.Spec.CloudServerCommon
}

// GetCredentialsSecret returns the name and namespace of
// spec.credentials.secretRef, and an empty name for workload identity.
func (s *ClusterCacheServer) GetCredentialsSecret() (name, namespace string) {
	if c := s.Spec.Credentials; c != nil && c.Source == cloud.SourceSecret && c.SecretRef != nil {
		return c.SecretRef.Name, c.SecretRef.Namespace
	}
	return "", ""
}

// GetServerStatus returns the status the probe writes (probe.Server).
func (s *ClusterCacheServer) GetServerStatus() *common.ServerStatus { return &s.Status }

// GetEndpoint returns the server's URL, rediss://host:port, or "" when the host
// is discovered.
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
