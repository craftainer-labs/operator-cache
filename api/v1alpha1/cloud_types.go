package v1alpha1

import (
	"github.com/craftainer-labs/lib-operator-go/pkg/cloud"
)

// Keys of status.discovered: what the probe reads from the cloud (ADR 0002,
// section 16.1).
const (
	// DiscoveredEndpoint is the data-plane host name.
	DiscoveredEndpoint = "endpoint"
	// DiscoveredPort is the data-plane TCP port.
	DiscoveredPort = "port"
	// DiscoveredResourceID is the id of the cluster or replication group.
	DiscoveredResourceID = "resourceId"
	// DiscoveredAccountID is the AWS account that owns the target.
	DiscoveredAccountID = "accountId"
	// DiscoveredRegion is the AWS region of the target.
	DiscoveredRegion = "region"
)

// ElastiCacheTarget names an ElastiCache replication group and the user group
// attached to it. The admin creates both and attaches the user group; the
// operator only adds and removes its users in it.
type ElastiCacheTarget struct {
	// userGroupID is the ElastiCache user group the operator's users join.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=255
	UserGroupID string `json:"userGroupID"`

	// replicationGroupID is the replication group the user group is attached to.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=255
	ReplicationGroupID string `json:"replicationGroupID"`
}

// MemoryDBTarget names a MemoryDB cluster and the ACL attached to it. The admin
// creates both and attaches the ACL; the operator only adds and removes its
// users in it.
type MemoryDBTarget struct {
	// aclName is the MemoryDB ACL the operator's users join.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=255
	ACLName string `json:"aclName"`

	// clusterName is the cluster the ACL is attached to.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=255
	ClusterName string `json:"clusterName"`
}

// AWSTarget selects the managed service of an AWS cache server: exactly one of
// elasticache and memorydb (checked by the block that embeds it).
type AWSTarget struct {
	// elasticache selects Amazon ElastiCache.
	// +optional
	ElastiCache *ElastiCacheTarget `json:"elasticache,omitempty"`

	// memorydb selects Amazon MemoryDB.
	// +optional
	MemoryDB *MemoryDBTarget `json:"memorydb,omitempty"`
}

// AWSCache is provider.aws of a namespaced server (no endpointOverride).
// +kubebuilder:validation:XValidation:rule="has(self.elasticache) != has(self.memorydb)",message="set exactly one of elasticache and memorydb"
type AWSCache struct {
	cloud.AWSBlock `json:",inline"`
	AWSTarget      `json:",inline"`
}

// ClusterAWSCache is provider.aws of a cluster server.
// +kubebuilder:validation:XValidation:rule="has(self.elasticache) != has(self.memorydb)",message="set exactly one of elasticache and memorydb"
type ClusterAWSCache struct {
	cloud.AWSClusterBlock `json:",inline"`
	AWSTarget             `json:",inline"`
}

// Provider says the server is a managed cloud cache. Absent: self-hosted.
// Only AWS is offered in this version.
// +kubebuilder:validation:XValidation:rule="self.type == 'AWS' ? has(self.aws) : !has(self.aws)",message="aws is required with type AWS and forbidden otherwise"
type Provider struct {
	// type is the cloud. Only AWS is supported; it is immutable.
	Type cloud.ProviderType `json:"type"`
	// aws holds the AWS settings.
	// +optional
	AWS *AWSCache `json:"aws,omitempty"`
}

// ClusterProvider is the provider of a cluster server.
// +kubebuilder:validation:XValidation:rule="self.type == 'AWS' ? has(self.aws) : !has(self.aws)",message="aws is required with type AWS and forbidden otherwise"
type ClusterProvider struct {
	// type is the cloud. Only AWS is supported; it is immutable.
	Type cloud.ProviderType `json:"type"`
	// aws holds the AWS settings.
	// +optional
	AWS *ClusterAWSCache `json:"aws,omitempty"`
}
