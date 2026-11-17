package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/craftainer-labs/lib-operator-go/pkg/apis/common"
	"github.com/craftainer-labs/lib-operator-go/pkg/cloud"
)

// CacheServerSpec defines the desired state of CacheServer.
//
// +kubebuilder:validation:XValidation:rule="!has(self.adminCredentialsSecretRef) || !has(self.adminCredentialsSecretRef.namespace)",message="adminCredentialsSecretRef.namespace must not be set: the Secret is in the CacheServer's namespace"
// +kubebuilder:validation:XValidation:rule="has(self.provider) ? !has(self.adminCredentialsSecretRef) : has(self.adminCredentialsSecretRef)",message="adminCredentialsSecretRef is required without provider and forbidden with one"
// +kubebuilder:validation:XValidation:rule="has(self.provider) || (has(self.host) && size(self.host) > 0)",message="host is required without provider"
// +kubebuilder:validation:XValidation:rule="!has(self.provider) || self.provider.type == 'AWS'",message="only provider.type AWS is supported"
// +kubebuilder:validation:XValidation:rule="!has(self.credentials) || has(self.provider)",message="credentials is allowed only with provider"
// +kubebuilder:validation:XValidation:rule="!has(self.provider) || has(self.credentials)",message="a namespaced server with a provider must set credentials with source Secret: it cannot use WorkloadIdentity"
type CacheServerSpec struct {
	common.CloudServerCommon `json:",inline"`
	ServerConfig             `json:",inline"`

	// provider says the server is a managed cloud cache. Its type is immutable.
	// +optional
	Provider *Provider `json:"provider,omitempty"`

	// credentials are the credentials to the cloud API: a Secret in the
	// server's namespace (a namespaced server cannot use WorkloadIdentity).
	// +optional
	Credentials *cloud.NamespacedCredentials `json:"credentials,omitempty"`
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

// GetCloudCommon returns the settings every cloud-capable server kind shares
// (server.CloudServer).
func (s *CacheServer) GetCloudCommon() *common.CloudServerCommon { return &s.Spec.CloudServerCommon }

// GetCredentialsSecret returns the name of spec.credentials.secretRef and an
// empty namespace (a namespaced server's Secret is in its own namespace), and
// an empty name when there are no credentials.
func (s *CacheServer) GetCredentialsSecret() (name, namespace string) {
	if c := s.Spec.Credentials; c != nil && c.SecretRef != nil {
		return c.SecretRef.Name, ""
	}
	return "", ""
}

// GetServerStatus returns the status the probe writes (probe.Server).
func (s *CacheServer) GetServerStatus() *common.ServerStatus { return &s.Status }

// GetEndpoint returns the server's URL, rediss://host:port, or "" when the host
// is discovered.
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
