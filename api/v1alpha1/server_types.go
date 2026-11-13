package v1alpha1

import (
	"net"
	"strconv"
)

// Mode is how the Valkey or Redis deployment is laid out.
//
// ACLs are per node, so a Cluster or Sentinel deployment would need every ACL
// command sent to every node. That is out of v1alpha1.
//
// +kubebuilder:validation:Enum=Standalone
type Mode string

// Values of Mode.
const (
	// ModeStandalone is a single server (a primary, with or without replicas
	// that the operator does not manage).
	ModeStandalone Mode = "Standalone"
)

const (
	// DefaultPort is the server port when spec.port is unset.
	DefaultPort int32 = 6379
	// EndpointScheme is the scheme of a server's endpoint: the operator reaches
	// Valkey and Redis over TLS only, so the plain "redis" scheme is not used.
	EndpointScheme = "rediss"
)

// ServerConfig is what the two server kinds share besides the server blocks of
// lib-operator-go. It has no type-level rules: each kind's spec carries the
// rules that differ between the cluster and the namespaced kind.
type ServerConfig struct {
	// host is the server's host name or IP address. It never carries a scheme,
	// a port, a path or credentials: only letters, digits, dots, hyphens and
	// colons (for an IPv6 address) are allowed.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9.:-]+$`
	Host string `json:"host"`

	// port is the server's TLS port.
	// +kubebuilder:default=6379
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +optional
	Port *int32 `json:"port,omitempty"`

	// mode is the deployment layout. Only Standalone is offered.
	// +kubebuilder:default=Standalone
	// +optional
	Mode Mode `json:"mode,omitempty"`
}

// Address returns host:port, from the default where the port is unset. An IPv6
// host is bracketed.
func (c *ServerConfig) Address() string {
	port := DefaultPort
	if c.Port != nil {
		port = *c.Port
	}
	return net.JoinHostPort(c.Host, strconv.Itoa(int(port)))
}

// Endpoint returns the server's URL, rediss://host:port. It is what a server
// kind's GetEndpoint returns.
func (c *ServerConfig) Endpoint() string {
	return EndpointScheme + "://" + c.Address()
}
