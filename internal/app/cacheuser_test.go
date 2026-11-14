package app

import (
	"testing"

	"github.com/craftainer-labs/lib-operator-go/pkg/apis/common"

	cachev1alpha1 "github.com/craftainer-labs/operator-cache/api/v1alpha1"
)

// A CacheUser is a credential kind and pins no deletion policy: lib-operator-go
// v0.2.0 no longer calls SetEffectiveDeletionPolicy for one, so this is the
// only caller of the no-op that the Claim interface asks for.
func TestCacheUserPinsNoDeletionPolicy(t *testing.T) {
	u := &cachev1alpha1.CacheUser{}
	u.SetEffectiveDeletionPolicy(common.DeletionDelete, common.SourceOperatorDefault)
	if got := u.GetEffectiveDeletionPolicy(); got != "" {
		t.Fatalf("effective policy = %q, want none", got)
	}
}
