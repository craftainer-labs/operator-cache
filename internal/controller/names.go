package controller

import (
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/craftainer-labs/lib-operator-go/pkg/apis/common"
	"github.com/craftainer-labs/lib-operator-go/pkg/conditions"
	"github.com/craftainer-labs/lib-operator-go/pkg/naming"
)

// The two server kinds a CacheUser may name.
const (
	kindClusterCacheServer = "ClusterCacheServer"
	kindCacheServer        = "CacheServer"
)

// maxUserNameLen is the longest ACL user name the operator derives. Valkey has no
// limit of its own; the cap keeps names readable in logs and in ACL LIST.
const maxUserNameLen = 128

// userName is the name of the ACL user of a claim. An ACL user carries no
// ownership marker, so the name alone has to make a collision between two claims
// improbable: the hash is the long one (80 bits).
func userName(namespace, name string) string {
	return naming.Tenant(namespace, name, maxUserNameLen, "-", naming.HashLong)
}

// serverKey names a server in the connection cache and in the serverRef index.
func serverKey(namespace string, ref common.ServerRef) string {
	if ref.Kind == kindClusterCacheServer {
		namespace = ""
	}
	return ref.Kind + "/" + namespace + "/" + ref.Name
}

// serverNotReady reports a Ready condition that says False. A server that says
// nothing is used: an absent condition is not a failed one.
func serverNotReady(conds []metav1.Condition) bool {
	c := meta.FindStatusCondition(conds, conditions.Ready)
	return c != nil && c.Status == metav1.ConditionFalse
}
