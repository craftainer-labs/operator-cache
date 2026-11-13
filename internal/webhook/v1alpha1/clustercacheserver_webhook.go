package v1alpha1

import (
	"context"

	"k8s.io/apimachinery/pkg/api/equality"
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	cachev1alpha1 "github.com/craftainer-labs/operator-cache/api/v1alpha1"
)

var clustercacheserverlog = logf.Log.WithName("clustercacheserver-resource")

// SetupClusterCacheServerWebhookWithManager registers the ClusterCacheServer webhooks with the manager.
func SetupClusterCacheServerWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &cachev1alpha1.ClusterCacheServer{}).
		WithValidator(&ClusterCacheServerValidator{}).
		Complete()
}

// +kubebuilder:webhook:path=/validate-cache-craftainer-dev-v1alpha1-clustercacheserver,mutating=false,failurePolicy=fail,sideEffects=None,groups=cache.craftainer.dev,resources=clustercacheservers,verbs=create;update,versions=v1alpha1,name=vclustercacheserver-v1alpha1.kb.io,admissionReviewVersions=v1

// ClusterCacheServerValidator validates ClusterCacheServers on create and update.
type ClusterCacheServerValidator struct{}

// ValidateCreate implements admission.Validator.
func (v *ClusterCacheServerValidator) ValidateCreate(_ context.Context, obj *cachev1alpha1.ClusterCacheServer) (admission.Warnings, error) {
	clustercacheserverlog.Info("validating create", "name", obj.GetName())
	return nil, v.validate(obj)
}

// ValidateUpdate implements admission.Validator. An object that is being
// deleted is admitted while its spec is unchanged, so finalizer and metadata
// updates are never blocked; a spec change is still validated.
func (v *ClusterCacheServerValidator) ValidateUpdate(_ context.Context, oldObj, newObj *cachev1alpha1.ClusterCacheServer) (admission.Warnings, error) {
	if !newObj.DeletionTimestamp.IsZero() && equality.Semantic.DeepEqual(oldObj.Spec, newObj.Spec) {
		return nil, nil
	}
	clustercacheserverlog.Info("validating update", "name", newObj.GetName())
	return nil, v.validate(newObj)
}

// ValidateDelete implements admission.Validator; deletes are always admitted
// (the verbs marker above does not even send them here).
func (v *ClusterCacheServerValidator) ValidateDelete(_ context.Context, _ *cachev1alpha1.ClusterCacheServer) (admission.Warnings, error) {
	return nil, nil
}

func (v *ClusterCacheServerValidator) validate(obj *cachev1alpha1.ClusterCacheServer) error {
	return invalid("ClusterCacheServer", obj.GetName(), validateServer(&obj.Spec.ServerConfig, obj.Spec.ProxyURL))
}
