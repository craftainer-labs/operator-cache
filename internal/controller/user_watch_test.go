package controller

import (
	"context"
	"errors"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"

	"github.com/craftainer-labs/lib-operator-go/pkg/apis/common"
	"github.com/craftainer-labs/lib-operator-go/pkg/conditions"

	cachev1alpha1 "github.com/craftainer-labs/operator-cache/api/v1alpha1"
)

// listStub answers List with a fixed set of users and records the options.
type listStub struct {
	client.Reader
	items []cachev1alpha1.CacheUser
	err   error
	opts  *client.ListOptions
}

func (l *listStub) List(_ context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if l.err != nil {
		return l.err
	}
	l.opts = &client.ListOptions{}
	l.opts.ApplyOptions(opts)
	list.(*cachev1alpha1.CacheUserList).Items = l.items
	return nil
}

func TestIndexServerRef(t *testing.T) {
	u := &cachev1alpha1.CacheUser{}
	u.Spec.ServerRef = common.ServerRef{Kind: kindCacheServer, Name: "team-valkey"}
	if got := indexServerRef(u); len(got) != 1 || got[0] != "CacheServer/team-valkey" {
		t.Errorf("index = %v", got)
	}
}

// A server event reaches the users that name it: a cluster server's across
// every namespace, a namespaced server's in its own.
func TestUsersOfServer(t *testing.T) {
	ctx := context.Background()
	mk := func(name string) cachev1alpha1.CacheUser {
		return cachev1alpha1.CacheUser{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "tenant"}}
	}
	stub := &listStub{items: []cachev1alpha1.CacheUser{mk("one"), mk("two")}}

	cluster := &cachev1alpha1.ClusterCacheServer{ObjectMeta: metav1.ObjectMeta{Name: "shared"}}
	reqs := usersOfServer(stub, kindClusterCacheServer)(ctx, cluster)
	if len(reqs) != 2 || reqs[0].Name != "one" || reqs[1].Namespace != "tenant" {
		t.Errorf("requests = %v", reqs)
	}
	if stub.opts.Namespace != "" || stub.opts.FieldSelector.String() != serverRefIndex+"=ClusterCacheServer/shared" {
		t.Errorf("list options of a cluster server = %+v", stub.opts)
	}

	namespaced := &cachev1alpha1.CacheServer{ObjectMeta: metav1.ObjectMeta{Name: "team", Namespace: "tenant"}}
	if reqs := usersOfServer(stub, kindCacheServer)(ctx, namespaced); len(reqs) != 2 {
		t.Errorf("requests = %v", reqs)
	}
	if stub.opts.Namespace != "tenant" || stub.opts.FieldSelector.String() != serverRefIndex+"=CacheServer/team" {
		t.Errorf("list options of a namespaced server = %+v", stub.opts)
	}

	stub.err = errServer
	if reqs := usersOfServer(stub, kindCacheServer)(ctx, namespaced); len(reqs) != 0 {
		t.Errorf("a failed list gave %v", reqs)
	}
}

func TestConditionsOf(t *testing.T) {
	ready := []metav1.Condition{{Type: conditions.Ready, Status: metav1.ConditionFalse}}
	cluster := &cachev1alpha1.ClusterCacheServer{}
	cluster.Status.Conditions = ready
	namespaced := &cachev1alpha1.CacheServer{}
	namespaced.Status.Conditions = ready
	if len(conditionsOf(cluster)) != 1 || len(conditionsOf(namespaced)) != 1 || conditionsOf(&cachev1alpha1.CacheUser{}) != nil {
		t.Error("conditionsOf")
	}
}

// A server's update reconciles its users when its spec changed, or it went from
// Ready to not Ready or back; a status write that changes nothing else does not.
func TestServerChangedPredicate(t *testing.T) {
	mk := func(generation int64, status metav1.ConditionStatus) *cachev1alpha1.ClusterCacheServer {
		s := &cachev1alpha1.ClusterCacheServer{ObjectMeta: metav1.ObjectMeta{Generation: generation}}
		if status != "" {
			s.Status.Conditions = []metav1.Condition{{Type: conditions.Ready, Status: status}}
		}
		return s
	}
	update := func(o, n *cachev1alpha1.ClusterCacheServer) bool {
		return serverChanged.Update(event.UpdateEvent{ObjectOld: o, ObjectNew: n})
	}
	if update(mk(1, ""), mk(1, "")) || update(mk(1, metav1.ConditionTrue), mk(1, metav1.ConditionUnknown)) {
		t.Error("a status write with no change in readiness reconciles the users")
	}
	if !update(mk(1, ""), mk(2, "")) || !update(mk(1, metav1.ConditionTrue), mk(1, metav1.ConditionFalse)) ||
		!update(mk(1, metav1.ConditionFalse), mk(1, metav1.ConditionTrue)) {
		t.Error("a change that matters does not reconcile the users")
	}
	if !serverChanged.Create(event.CreateEvent{Object: mk(1, "")}) || !serverChanged.Delete(event.DeleteEvent{Object: mk(1, "")}) {
		t.Error("creation and deletion are filtered")
	}
}

// failingIndexer and indexFailure make the registration of the index fail.
type failingIndexer struct{}

func (failingIndexer) IndexField(context.Context, client.Object, string, client.IndexerFunc) error {
	return errServer
}

type indexFailure struct{ ctrl.Manager }

func (indexFailure) GetFieldIndexer() client.FieldIndexer { return failingIndexer{} }

func TestUserSetupReportsAnIndexError(t *testing.T) {
	if err := (&UserReconciler{}).SetupWithManager(indexFailure{}); !errors.Is(err, errServer) {
		t.Errorf("setup: %v", err)
	}
}
