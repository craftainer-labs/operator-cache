package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/craftainer-labs/lib-operator-go/pkg/apis/common"
)

// Profile is the set of commands a CacheUser may run on its keys. The
// @dangerous and @admin command categories are never granted, and neither is
// any command that reaches a key outside the tenant's key prefix.
//
// +kubebuilder:validation:Enum=ReadOnly;ReadWrite
type Profile string

// Values of Profile.
const (
	// ProfileReadOnly grants the read commands (@read) on the tenant's keys.
	ProfileReadOnly Profile = "ReadOnly"
	// ProfileReadWrite grants the read and write commands (@read and @write)
	// on the tenant's keys.
	ProfileReadWrite Profile = "ReadWrite"
)

// AuthMode is how the user logs in (ADR 0002, section 16.4).
//
// +kubebuilder:validation:Enum=Password;IAM
type AuthMode string

// Values of AuthMode.
const (
	// AuthModePassword is a generated password in the binding Secret, with the
	// two-phase rotation of ADR 0002, section 8. It is the only mode of a
	// self-hosted server.
	AuthModePassword AuthMode = "Password"
	// AuthModeIAM is a cloud login with a short-lived token the consumer's own
	// identity makes: the operator creates no password, and the binding Secret
	// holds none. It needs a server with a provider.
	AuthModeIAM AuthMode = "IAM"
)

// IAMSpec is spec.iam of an IAM-mode claim: the consumer's cloud identity.
type IAMSpec struct {
	// principal is the consumer's cloud identity name. It is refused on AWS,
	// where the login is named by the operator and the consumer's role is
	// authorized by the ARN in the binding Secret (IAMPrincipalRequired).
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=320
	Principal string `json:"principal"`
}

// CacheUserSpec defines the desired state of CacheUser. It is a credential
// kind: it has no deletionPolicy (the ACL user is always deleted with the
// claim) and its login is written to a binding Secret.
//
// The key pattern is always the tenant's key prefix, "<namespace>:*", and the
// channel pattern "<namespace>:*" when channels is set. Neither is a field: a
// claim cannot name keys or channels outside its own namespace.
//
// +kubebuilder:validation:XValidation:rule="self.serverRef.kind in ['ClusterCacheServer', 'CacheServer']",message="serverRef.kind must be ClusterCacheServer or CacheServer"
// +kubebuilder:validation:XValidation:rule="!has(self.rotationPolicy) || self.managementPolicies == ['*'] || 'Update' in self.managementPolicies",message="rotationPolicy needs the Update management policy"
// +kubebuilder:validation:XValidation:rule="!has(self.iam) || (has(self.authMode) && self.authMode == 'IAM')",message="iam is allowed only with authMode IAM"
// +kubebuilder:validation:XValidation:rule="!has(self.rotationPolicy) || !has(self.authMode) || self.authMode == 'Password'",message="rotationPolicy is not allowed with authMode IAM: there is nothing to rotate"
type CacheUserSpec struct {
	common.ClaimCommon `json:",inline"`
	common.Credentials `json:",inline"`

	// authMode is how the user logs in: Password (default) or IAM. IAM needs a
	// server with a provider; on any other server the claim is refused
	// (AuthModeUnsupported). It is immutable: another mode is another external
	// object, so it is a new claim.
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="authMode is immutable"
	// +kubebuilder:default=Password
	// +optional
	AuthMode AuthMode `json:"authMode,omitempty"`

	// iam names the consumer's cloud identity. Only with authMode IAM.
	// +optional
	IAM *IAMSpec `json:"iam,omitempty"`

	// profile is which commands the user may run on the tenant's keys.
	// +kubebuilder:default=ReadWrite
	// +optional
	Profile Profile `json:"profile,omitempty"`

	// channels grants publish and subscribe on the tenant's channel pattern
	// (&<namespace>:*). It is off by default.
	// +kubebuilder:default=false
	// +optional
	Channels bool `json:"channels,omitempty"`
}

// CacheUserStatus defines the observed state of CacheUser.
type CacheUserStatus struct {
	common.ClaimStatus       `json:",inline"`
	common.CredentialsStatus `json:",inline"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:categories=craftainer
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Synced",type=string,JSONPath=`.status.conditions[?(@.type=="Synced")].status`
// +kubebuilder:printcolumn:name="Server",type=string,JSONPath=`.spec.serverRef.name`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// CacheUser is an ACL user of a Valkey or Redis server, limited to the tenant's key prefix, written to a binding Secret.
type CacheUser struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of CacheUser
	// +required
	Spec CacheUserSpec `json:"spec"`

	// status defines the observed state of CacheUser
	// +optional
	Status CacheUserStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// CacheUserList contains a list of CacheUser.
type CacheUserList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []CacheUser `json:"items"`
}

// The methods below implement lib-operator-go's lifecycle.Claim.

// GetServerRef returns spec.serverRef.
func (u *CacheUser) GetServerRef() common.ServerRef { return u.Spec.ServerRef }

// GetDeletionPolicy returns "": a user is always deleted with its claim.
func (u *CacheUser) GetDeletionPolicy() common.DeletionPolicy { return "" }

// GetManagementPolicies returns spec.managementPolicies.
func (u *CacheUser) GetManagementPolicies() []common.ManagementPolicy {
	return u.Spec.ManagementPolicies
}

// GetConditions returns status.conditions.
func (u *CacheUser) GetConditions() *[]metav1.Condition { return &u.Status.Conditions }

// GetBindingSecretName returns spec.binding.secretName, or the claim's name.
func (u *CacheUser) GetBindingSecretName() string {
	if u.Spec.Binding.SecretName != "" {
		return u.Spec.Binding.SecretName
	}
	return u.Name
}

// SetBindingName sets status.binding.name.
func (u *CacheUser) SetBindingName(v string) { u.Status.Binding = &common.BindingStatus{Name: v} }

// GetExternalName returns status.externalName.
func (u *CacheUser) GetExternalName() string { return u.Status.ExternalName }

// SetExternalName sets status.externalName.
func (u *CacheUser) SetExternalName(v string) { u.Status.ExternalName = v }

// GetExternalID returns status.externalId.
func (u *CacheUser) GetExternalID() string { return u.Status.ExternalID }

// SetExternalID sets status.externalId.
func (u *CacheUser) SetExternalID(v string) { u.Status.ExternalID = v }

// SetObservedGeneration sets status.observedGeneration.
func (u *CacheUser) SetObservedGeneration(v int64) { u.Status.ObservedGeneration = v }

// SetLastReconcileTime sets status.lastReconcileTime.
func (u *CacheUser) SetLastReconcileTime(v metav1.Time) { u.Status.LastReconcileTime = &v }

// GetEffectiveDeletionPolicy returns "": a user has no pinned policy.
func (u *CacheUser) GetEffectiveDeletionPolicy() common.DeletionPolicy { return "" }

// SetEffectiveDeletionPolicy does nothing: a user has no pinned policy.
func (u *CacheUser) SetEffectiveDeletionPolicy(common.DeletionPolicy, common.DeletionPolicySource) {
}

// SetLateInitialized sets status.lateInitialized.
func (u *CacheUser) SetLateInitialized(v map[string]string) { u.Status.LateInitialized = v }

// GetRotationPolicy returns spec.rotationPolicy.
func (u *CacheUser) GetRotationPolicy() *common.RotationPolicy { return u.Spec.RotationPolicy }

// GetRotationStatus returns status.rotation, or the zero status.
func (u *CacheUser) GetRotationStatus() common.RotationStatus {
	if u.Status.Rotation == nil {
		return common.RotationStatus{}
	}
	return *u.Status.Rotation
}

// SetRotationStatus sets status.rotation.
func (u *CacheUser) SetRotationStatus(v common.RotationStatus) { u.Status.Rotation = &v }

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &CacheUser{}, &CacheUserList{})
		return nil
	})
}
