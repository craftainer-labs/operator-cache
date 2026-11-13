package v1alpha1

import (
	"context"

	"k8s.io/apimachinery/pkg/api/equality"
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	cachev1alpha1 "github.com/craftainer-labs/operator-cache/api/v1alpha1"
)

var cacheserverlog = logf.Log.WithName("cacheserver-resource")

// SetupCacheServerWebhookWithManager registers the CacheServer webhooks with the manager.
func SetupCacheServerWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &cachev1alpha1.CacheServer{}).
		WithValidator(&CacheServerValidator{}).
		Complete()
}

// +kubebuilder:webhook:path=/validate-cache-craftainer-dev-v1alpha1-cacheserver,mutating=false,failurePolicy=fail,sideEffects=None,groups=cache.craftainer.dev,resources=cacheservers,verbs=create;update,versions=v1alpha1,name=vcacheserver-v1alpha1.kb.io,admissionReviewVersions=v1

// CacheServerValidator validates CacheServers on create and update.
type CacheServerValidator struct{}

// ValidateCreate implements admission.Validator.
func (v *CacheServerValidator) ValidateCreate(_ context.Context, obj *cachev1alpha1.CacheServer) (admission.Warnings, error) {
	cacheserverlog.Info("validating create", "name", obj.GetName())
	return nil, v.validate(obj)
}

// ValidateUpdate implements admission.Validator. An object that is being
// deleted is admitted while its spec is unchanged, so finalizer and metadata
// updates are never blocked; a spec change is still validated.
func (v *CacheServerValidator) ValidateUpdate(_ context.Context, oldObj, newObj *cachev1alpha1.CacheServer) (admission.Warnings, error) {
	if !newObj.DeletionTimestamp.IsZero() && equality.Semantic.DeepEqual(oldObj.Spec, newObj.Spec) {
		return nil, nil
	}
	cacheserverlog.Info("validating update", "name", newObj.GetName())
	return nil, v.validate(newObj)
}

// ValidateDelete implements admission.Validator; deletes are always admitted
// (the verbs marker above does not even send them here).
func (v *CacheServerValidator) ValidateDelete(_ context.Context, _ *cachev1alpha1.CacheServer) (admission.Warnings, error) {
	return nil, nil
}

func (v *CacheServerValidator) validate(obj *cachev1alpha1.CacheServer) error {
	return invalid("CacheServer", obj.GetName(), validateServer(&obj.Spec.ServerConfig, obj.Spec.ProxyURL))
}
