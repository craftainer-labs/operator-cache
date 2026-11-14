package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/craftainer-labs/lib-operator-go/pkg/apis/common"
	"github.com/craftainer-labs/lib-operator-go/pkg/conditions"
	"github.com/craftainer-labs/lib-operator-go/pkg/lifecycle"
	"github.com/craftainer-labs/lib-operator-go/pkg/server"

	cachev1alpha1 "github.com/craftainer-labs/operator-cache/api/v1alpha1"
	"github.com/craftainer-labs/operator-cache/internal/valkeyacl"
)

// writes are the calls that change the server; a refused claim makes none.
var writes = []string{"SetUser", "DelUser"}

// tenantRules are the rules of a ReadWrite user without channels in namespace ns.
func tenantRules(t *testing.T, ns string) []string {
	t.Helper()
	rules, err := valkeyacl.Tenant{Prefix: ns, Write: true}.Rules()
	if err != nil {
		t.Fatal(err)
	}
	return rules
}

func TestUserLifecycleAndRotation(t *testing.T) {
	h := newHarness(t, nil)
	r := &UserReconciler{h.cfg}
	grace := metav1.Duration{Duration: time.Hour}
	u := h.user("app", func(u *cachev1alpha1.CacheUser) {
		u.Spec.RotationPolicy = &common.RotationPolicy{GracePeriod: &grace}
	})
	name := userName(h.ns, "app")

	// Create: GETUSER first, the intent, then SETUSER reset with the password
	// and the rules.
	h.reconcileOK(r, u)
	h.get(u)
	if u.Status.ExternalName != name || u.Status.ExternalID != name || u.Status.Binding == nil || u.Status.Binding.Name != "app" {
		t.Errorf("status = %+v", u.Status)
	}
	h.wantCondition(u.Status.Conditions, conditions.Ready, metav1.ConditionTrue, conditions.Available)
	h.fake.want(t, "GetUser "+name, "SetUser "+name+" reset >")
	data := h.bindingSecret(u)
	first := data["password"]
	if data["username"] != name || len(first) != 52 || data["host"] != "valkey.example.test" || data["port"] != "6379" ||
		data["uri"] != "rediss://valkey.example.test:6379" || data["keyPrefix"] != h.ns+":" ||
		data["type"] != "redis" || data["provider"] != "craftainer.dev" {
		t.Errorf("binding = %v", data)
	}
	if strings.Contains(data["uri"], first) || strings.Contains(data["uri"], "@") {
		t.Error("the URI carries credentials")
	}
	if got := h.fake.user(name); got == nil || !got.on || !got.hasPassword(first) || got.keys != "~"+h.ns+":*" || len(got.hashes) != 1 {
		t.Errorf("user on the server = %+v", got)
	}
	// The password is in the Secret only: not in an Event or a condition.
	h.rec.AssertNoSecret(t, first, "admin-pw")
	for _, c := range u.Status.Conditions {
		if strings.Contains(c.Message, first) {
			t.Errorf("the password is in condition %s", c.Type)
		}
	}
	// The connection is made with the server's address and the admin's login.
	if len(h.fake.configs) == 0 || h.fake.configs[0].Address != "valkey.example.test:6379" || h.fake.configs[0].Username != "admin" ||
		h.fake.configs[0].Password != "admin-pw" {
		t.Errorf("connection = %+v", h.fake.configs)
	}

	// Steady state: nothing is written and the Secret is kept.
	h.fake.reset()
	h.reconcileOK(r, u)
	h.fake.wantNot(t, writes...)
	if h.bindingSecret(u)["password"] != first {
		t.Error("the password changed without a reason")
	}

	// Rotation: the annotation adds a password beside the current one.
	h.patch(u, func() { u.Annotations = map[string]string{lifecycle.AnnotationRotate: "1"} })
	h.fake.reset()
	h.reconcileOK(r, u)
	h.fake.want(t, "SetUser "+name+" resetpass #"+valkeyacl.HashPassword(first)+" >")
	data = h.bindingSecret(u)
	second := data["password"]
	if data["username"] != name || second == first || len(second) != 52 {
		t.Errorf("rotated binding = %v", data)
	}
	if got := h.fake.user(name); !got.hasPassword(first) || !got.hasPassword(second) || len(got.hashes) != 2 {
		t.Errorf("passwords on the server after RotateBegin: %d", len(got.hashes))
	}
	h.get(u)
	if u.Status.Rotation == nil || u.Status.Rotation.Active != "1" || u.Status.Rotation.Sequence != 1 || u.Status.ExternalID != name {
		t.Fatalf("rotation = %+v, externalId = %q", u.Status.Rotation, u.Status.ExternalID)
	}

	// During the grace period the user is up to date and nothing is written.
	h.fake.reset()
	h.reconcileOK(r, u)
	h.fake.wantNot(t, writes...)

	// After the grace period only the password of the binding is kept.
	h.now = h.now.Add(2 * time.Hour)
	h.fake.reset()
	h.reconcileOK(r, u)
	h.fake.want(t, "SetUser "+name+" resetpass #"+valkeyacl.HashPassword(second))
	if got := h.fake.user(name); got.hasPassword(first) || !got.hasPassword(second) || len(got.hashes) != 1 {
		t.Errorf("passwords on the server after RotateFinish: %d", len(got.hashes))
	}
	h.rec.AssertEvent(t, "Normal", conditions.EventRotationFinished, "")

	// A change of the profile reapplies the rules without "reset", so the
	// password survives and the credentials in use stay valid.
	h.patch(u, func() { u.Spec.Profile = cachev1alpha1.ProfileReadOnly })
	h.fake.reset()
	h.reconcileOK(r, u)
	h.fake.want(t, "SetUser "+name+" on clearselectors resetkeys %R~"+h.ns+":*")
	h.fake.wantNot(t, "reset >", "resetpass", " >")
	if got := h.bindingSecret(u); got["password"] != second || !h.fake.user(name).hasPassword(second) || h.fake.user(name).keys != "%R~"+h.ns+":*" {
		t.Errorf("the credentials changed with the profile: %v", got)
	}

	// Channels are granted by the same update.
	h.patch(u, func() { u.Spec.Channels = true })
	h.reconcileOK(r, u)
	if got := h.fake.user(name); got.channels != "&"+h.ns+":*" || !got.hasPassword(second) {
		t.Errorf("user after channels = %+v", got)
	}

	// Rules someone widened by hand are repaired.
	h.fake.user(name).keys = "~*"
	h.fake.reset()
	h.reconcileOK(r, u)
	h.fake.want(t, "SetUser "+name+" on clearselectors")
	if h.fake.user(name).keys != "%R~"+h.ns+":*" {
		t.Errorf("keys = %q", h.fake.user(name).keys)
	}

	// Deleting removes the user, and the claim goes.
	h.must(env.Client.Delete(context.Background(), u))
	h.fake.reset()
	h.reconcileOK(r, u)
	h.fake.want(t, "DelUser "+name)
	if !h.gone(u) || h.fake.user(name) != nil {
		t.Errorf("the claim or its user is still there: %v", h.fake.users)
	}
}

// A user that exists while the claim has no record of a create is not ours: it
// is refused with a NameCollision and never changed or deleted.
func TestUserNeverAdoptsWhatItDidNotMake(t *testing.T) {
	h := newHarness(t, nil)
	r := &UserReconciler{h.cfg}
	u := h.user("app", nil)
	name := userName(h.ns, "app")
	foreign := &memUser{on: true, keys: "~foreign:*", commands: "-@all +get"}
	foreign.addHash(valkeyacl.HashPassword("foreign-pw"))
	h.fake.put(name, foreign)

	h.reconcileOK(r, u)
	h.get(u)
	msg := h.wantCondition(u.Status.Conditions, conditions.Synced, metav1.ConditionFalse, conditions.NameCollision)
	if !strings.Contains(msg, "did not make it") || strings.Contains(msg, "foreign-pw") {
		t.Errorf("message = %q", msg)
	}
	h.wantCondition(u.Status.Conditions, conditions.Ready, metav1.ConditionFalse, conditions.NameCollision)
	if u.Status.ExternalID != "" {
		t.Errorf("a create was recorded for a name that is not ours: %q", u.Status.ExternalID)
	}
	h.fake.wantNot(t, writes...)

	// The claim made nothing, so deleting it leaves the user alone and goes
	// without contacting the server.
	h.fake.reset()
	h.must(env.Client.Delete(context.Background(), u))
	h.reconcileOK(r, u)
	if h.fake.joined() != "" || !h.gone(u) {
		t.Errorf("deleting a claim that made nothing: calls %q", h.fake.joined())
	}
	if got := h.fake.user(name); got != foreign || got.keys != "~foreign:*" || !got.hasPassword("foreign-pw") || len(got.hashes) != 1 {
		t.Errorf("the foreign user changed: %+v", got)
	}
}

// The create is recorded in the status before anything is made on the server.
func TestUserRecordsTheIntentBeforeItCreates(t *testing.T) {
	h := newHarness(t, nil)
	r := &UserReconciler{h.cfg}
	u := h.user("app", nil)
	var seen []string
	h.fake.onCall = func(call string) {
		if strings.HasPrefix(call, "SetUser") {
			var now cachev1alpha1.CacheUser
			h.must(env.Client.Get(context.Background(), client.ObjectKeyFromObject(u), &now))
			seen = append(seen, now.Status.ExternalID)
		}
	}
	h.reconcileOK(r, u)
	if len(seen) != 1 || seen[0] != createIssued {
		t.Errorf("status.externalId at the SETUSER = %q", seen)
	}
}

// A name taken between Observe and the create is refused: GETUSER runs again at
// the create, nothing is written and no intent is recorded.
func TestUserCreateRacesWithAnotherCreator(t *testing.T) {
	h := newHarness(t, nil)
	r := &UserReconciler{h.cfg}
	u := h.user("app", nil)
	name := userName(h.ns, "app")
	gets := 0
	h.fake.onCall = func(call string) {
		if call == "GetUser "+name {
			gets++
			if gets == 2 {
				h.fake.put(name, &memUser{on: true, keys: "~foreign:*"})
			}
		}
	}

	h.reconcileOK(r, u)
	h.get(u)
	h.wantCondition(u.Status.Conditions, conditions.Synced, metav1.ConditionFalse, conditions.NameCollision)
	if u.Status.ExternalID != "" {
		t.Errorf("status.externalId = %q", u.Status.ExternalID)
	}
	h.fake.wantNot(t, writes...)

	// Once the name is free the claim goes on.
	h.fake.onCall = nil
	h.fake.mu.Lock()
	delete(h.fake.users, name)
	h.fake.mu.Unlock()
	h.reconcileOK(r, u)
	h.get(u)
	h.wantCondition(u.Status.Conditions, conditions.Ready, metav1.ConditionTrue, conditions.Available)
}

// A user that was made but not recorded (the crash fell between the SETUSER and
// the status write) is finished, not refused: the claim recorded its intent.
// There is no binding Secret, so the lifecycle rotates, which keeps the password
// nobody has and then retires it.
func TestUserFinishesACreateThatCrashed(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, nil)
	r := &UserReconciler{h.cfg}
	u := h.user("app", nil)
	name := userName(h.ns, "app")
	h.must(h.fake.SetUser(ctx, name, append([]string{"reset", valkeyacl.AddPassword("crashed-pw")}, tenantRules(t, h.ns)...)...))
	h.get(u)
	u.Status.ExternalID = createIssued
	h.must(env.Client.Status().Update(ctx, u))
	h.fake.reset()

	h.reconcileOK(r, u)
	h.get(u)
	h.wantCondition(u.Status.Conditions, conditions.Ready, metav1.ConditionTrue, conditions.Available)
	if u.Status.ExternalID != name {
		t.Errorf("status.externalId = %q", u.Status.ExternalID)
	}
	// With no binding Secret there is no current password to keep.
	h.fake.want(t, "SetUser "+name+" >")
	h.fake.wantNot(t, "reset >", "resetpass")
	secret := h.bindingSecret(u)["password"]
	if got := h.fake.user(name); !got.hasPassword(secret) || !got.hasPassword("crashed-pw") {
		t.Errorf("passwords on the server: %d", len(got.hashes))
	}

	h.now = h.now.Add(2 * time.Hour)
	h.reconcileOK(r, u)
	if got := h.fake.user(name); got.hasPassword("crashed-pw") || !got.hasPassword(secret) || len(got.hashes) != 1 {
		t.Errorf("passwords on the server after the grace period: %d", len(got.hashes))
	}
}

// A create that failed after the intent was recorded is repeated: the user is not
// on the server, and the record is ours.
func TestUserRepeatsACreateThatFailed(t *testing.T) {
	h := newHarness(t, nil)
	r := &UserReconciler{h.cfg}
	u := h.user("app", nil)
	name := userName(h.ns, "app")
	h.fake.fail["SetUser"] = fmt.Errorf("%w: connection reset", valkeyacl.ErrUnavailable)

	if err := h.reconcile(r, u); !errors.Is(err, valkeyacl.ErrUnavailable) {
		t.Fatalf("reconcile: %v", err)
	}
	h.get(u)
	if u.Status.ExternalID != createIssued {
		t.Fatalf("status.externalId = %q", u.Status.ExternalID)
	}

	h.fake.fail = map[string]error{}
	h.reconcileOK(r, u)
	h.get(u)
	h.wantCondition(u.Status.Conditions, conditions.Ready, metav1.ConditionTrue, conditions.Available)
	if u.Status.ExternalID != name || h.fake.user(name) == nil {
		t.Errorf("externalId = %q, user = %v", u.Status.ExternalID, h.fake.user(name))
	}

	// Deleting a claim whose create was issued deletes the user.
	h.must(env.Client.Delete(context.Background(), u))
	h.reconcileOK(r, u)
	if !h.gone(u) || h.fake.user(name) != nil {
		t.Error("the user was not deleted")
	}
}

// The password in the binding Secret is never reset in place. When the Secret
// does not hold the user, the rules are applied, Update reports it, and the next
// reconcile rotates.
func TestUserUpdateWithoutAValidSecretRotates(t *testing.T) {
	ctx := context.Background()
	tamper := map[string]func(h *harness, u *cachev1alpha1.CacheUser){
		"missing Secret": func(h *harness, u *cachev1alpha1.CacheUser) {
			var s corev1.Secret
			h.must(env.Client.Get(ctx, client.ObjectKey{Namespace: h.ns, Name: u.GetBindingSecretName()}, &s))
			h.must(env.Client.Delete(ctx, &s))
		},
		"another user in the Secret": func(h *harness, u *cachev1alpha1.CacheUser) {
			var s corev1.Secret
			h.must(env.Client.Get(ctx, client.ObjectKey{Namespace: h.ns, Name: u.GetBindingSecretName()}, &s))
			s.Data["username"] = []byte("tampered")
			h.must(env.Client.Update(ctx, &s))
		},
	}
	for name, mutate := range tamper {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, nil)
			r := &UserReconciler{h.cfg}
			u := h.user("app", nil)
			h.reconcileOK(r, u)
			h.get(u)
			user := userName(h.ns, "app")
			password := h.bindingSecret(u)["password"]

			mutate(h, u)
			h.patch(u, func() { u.Spec.Profile = cachev1alpha1.ProfileReadOnly })
			h.fake.reset()
			if err := h.reconcile(r, u); !errors.Is(err, ErrBindingNotUsable) {
				t.Fatalf("update: %v", err)
			}
			h.fake.want(t, "SetUser "+user+" on clearselectors")
			h.fake.wantNot(t, "reset >", "resetpass")
			if !h.fake.user(user).hasPassword(password) {
				t.Error("the password was reset")
			}

			h.fake.reset()
			h.reconcileOK(r, u)
			h.fake.want(t, "SetUser "+user+" >")
			got := h.bindingSecret(u)
			if got["username"] != user || got["password"] == "" || got["password"] == password {
				t.Errorf("binding after the rotation = %v", got)
			}
			// The old password stays valid through the grace period.
			if !h.fake.user(user).hasPassword(password) || !h.fake.user(user).hasPassword(got["password"]) {
				t.Error("the passwords on the server after the rotation")
			}
			h.get(u)
			if u.Status.Rotation == nil || u.Status.Rotation.Sequence != 1 {
				t.Errorf("rotation = %+v", u.Status.Rotation)
			}
		})
	}
}

// A claim whose server reports Ready=False waits and never connects, unless it
// is being deleted; a server with no Ready condition at all is used.
func TestUserWaitsForAReadyServer(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, nil)
	r := &UserReconciler{h.cfg}
	setReady := func(status metav1.ConditionStatus) {
		t.Helper()
		h.get(h.server)
		h.server.Status.Conditions = []metav1.Condition{{
			Type: conditions.Ready, Status: status, Reason: "Probe", Message: "probed", LastTransitionTime: metav1.Now(),
		}}
		h.must(env.Client.Status().Update(ctx, h.server))
	}
	setReady(metav1.ConditionFalse)
	u := h.user("app", nil)

	if err := h.reconcile(r, u); !errors.Is(err, ErrServerNotReady) {
		t.Fatalf("reconcile: %v", err)
	}
	if len(h.fake.configs) != 0 || h.fake.joined() != "" {
		t.Errorf("a claim of a server that is not Ready connected: %v, %q", h.fake.configs, h.fake.joined())
	}
	h.get(u)
	h.wantCondition(u.Status.Conditions, conditions.Ready, metav1.ConditionFalse, conditions.ReconcileError)

	setReady(metav1.ConditionTrue)
	h.reconcileOK(r, u)
	h.get(u)
	h.wantCondition(u.Status.Conditions, conditions.Ready, metav1.ConditionTrue, conditions.Available)

	// Deleting does not wait for the probe.
	setReady(metav1.ConditionFalse)
	h.must(env.Client.Delete(ctx, u))
	h.reconcileOK(r, u)
	if !h.gone(u) || len(h.fake.users) != 0 {
		t.Errorf("the user was not deleted: %v", h.fake.users)
	}
}

// Conditions and Events carry one fixed text per kind of failure: the raw error,
// with an address or a server's own words, goes to the log.
func TestUserFailuresAreCoarse(t *testing.T) {
	leak := "10.9.9.9:6379 hunter2 server-said"
	cases := []struct {
		name   string
		inject func(h *harness)
		reason string
		text   string
	}{
		{"unavailable", func(h *harness) { h.fake.fail["GetUser"] = fmt.Errorf("%w: dial %s", valkeyacl.ErrUnavailable, leak) },
			conditions.ServerUnreachable, "the connection to the server failed"},
		{"timeout", func(h *harness) { h.fake.fail["GetUser"] = fmt.Errorf("%w: %s", valkeyacl.ErrTimeout, leak) },
			conditions.ServerUnreachable, "the server did not answer in time"},
		{"authentication", func(h *harness) { h.fake.fail["GetUser"] = fmt.Errorf("%w: %s", valkeyacl.ErrAuth, leak) },
			conditions.ReconcileError, "the server refused the operator's admin login"},
		{"a server error", func(h *harness) { h.fake.fail["GetUser"] = fmt.Errorf("%w: %s", valkeyacl.ErrServer, leak) },
			conditions.ReconcileError, "the server reported an error"},
		{"an unknown error", func(h *harness) { h.fake.fail["GetUser"] = errors.New(leak) },
			conditions.ReconcileError, defaultText},
		{"a failed connection", func(h *harness) { h.connectErr = fmt.Errorf("%w: dial %s", valkeyacl.ErrUnavailable, leak) },
			conditions.ServerUnreachable, "the connection to the server failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, nil)
			r := &UserReconciler{h.cfg}
			u := h.user("app", nil)
			tc.inject(h)

			if err := h.reconcile(r, u); err == nil || strings.Contains(err.Error(), "hunter2") {
				t.Fatalf("reconcile: %v", err)
			}
			h.get(u)
			msg := h.wantCondition(u.Status.Conditions, conditions.Synced, metav1.ConditionFalse, tc.reason)
			if !strings.HasSuffix(msg, tc.text) {
				t.Errorf("message = %q, want it to end with %q", msg, tc.text)
			}
			for _, c := range u.Status.Conditions {
				if strings.Contains(c.Message, "hunter2") || strings.Contains(c.Message, "10.9.9.9") {
					t.Errorf("condition %s carries the raw error: %s", c.Type, c.Message)
				}
			}
			h.rec.AssertNoSecret(t, "hunter2", "10.9.9.9", "server-said")
		})
	}
}

// A server that cannot be resolved is a condition with a fixed text, and no
// connection is made.
func TestUserOnAnUnusableServer(t *testing.T) {
	ctx := context.Background()
	t.Run("missing server", func(t *testing.T) {
		h := newHarness(t, nil)
		r := &UserReconciler{h.cfg}
		u := h.user("app", func(u *cachev1alpha1.CacheUser) { u.Spec.ServerRef.Name = "no-such-server" })
		h.reconcileOK(r, u)
		h.get(u)
		msg := h.wantCondition(u.Status.Conditions, conditions.Synced, metav1.ConditionFalse, conditions.ServerNotFound)
		if !strings.HasSuffix(msg, "the referenced server was not found") || strings.Contains(msg, "no-such-server") {
			t.Errorf("message = %q", msg)
		}
	})
	t.Run("namespace not allowed", func(t *testing.T) {
		h := newHarness(t, func(s *cachev1alpha1.ClusterCacheServer) {
			s.Spec.AllowedNamespaces = common.AllowedNamespaces{
				From: common.NamespacesSelector, Selector: &common.LabelSelector{MatchLabels: map[string]string{"tenant": "yes"}},
			}
		})
		r := &UserReconciler{h.cfg}
		u := h.user("app", nil)
		h.reconcileOK(r, u)
		h.get(u)
		h.wantCondition(u.Status.Conditions, conditions.Synced, metav1.ConditionFalse, conditions.NamespaceNotAllowed)
		if len(h.fake.configs) != 0 {
			t.Error("a claim of a namespace the server does not serve connected")
		}
	})
	t.Run("admin Secret without the keys", func(t *testing.T) {
		h := newHarness(t, nil)
		var admin corev1.Secret
		h.must(env.Client.Get(ctx, client.ObjectKey{Namespace: h.ops, Name: h.server.Spec.AdminCredentialsSecretRef.Name}, &admin))
		delete(admin.Data, "password")
		h.must(env.Client.Update(ctx, &admin))
		r := &UserReconciler{h.cfg}
		u := h.user("app", nil)
		if err := h.reconcile(r, u); !errors.Is(err, server.ErrInvalidServer) {
			t.Errorf("reconcile: %v", err)
		}
		h.get(u)
		msg := h.wantCondition(u.Status.Conditions, conditions.Synced, metav1.ConditionFalse, conditions.ServerUnreachable)
		if strings.Contains(msg, admin.Name) {
			t.Errorf("message names the admin Secret: %q", msg)
		}
	})
	t.Run("proxy with credentials", func(t *testing.T) {
		h := newHarness(t, func(s *cachev1alpha1.ClusterCacheServer) {
			s.Spec.ProxyURL = "http://user:hunter2@proxy.example.test:3128"
		})
		r := &UserReconciler{h.cfg}
		u := h.user("app", nil)
		if err := h.reconcile(r, u); err == nil || strings.Contains(err.Error(), "hunter2") {
			t.Errorf("reconcile: %v", err)
		}
		h.get(u)
		_, _, msg := condition(u.Status.Conditions, conditions.Synced)
		if strings.Contains(msg, "hunter2") {
			t.Errorf("message = %q", msg)
		}
	})
	t.Run("the server is gone after it was resolved", func(t *testing.T) {
		h := newHarness(t, nil)
		boom := errors.New("api server down")
		calls := 0
		cfg := h.cfg
		cfg.Client = interceptedClient(t, func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*cachev1alpha1.ClusterCacheServer); ok {
				calls++
				if calls > 1 {
					return boom
				}
			}
			return c.Get(ctx, key, obj, opts...)
		})
		if _, err := cfg.backend(ctx, h.ns, h.ref()); !errors.Is(err, boom) {
			t.Errorf("backend: %v", err)
		}
	})
}

// interceptedClient is a client of the test API server whose Get is replaced.
func interceptedClient(t *testing.T, get func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error) client.Client {
	t.Helper()
	wc, err := client.NewWithWatch(env.Config, client.Options{Scheme: env.Scheme})
	if err != nil {
		t.Fatal(err)
	}
	return interceptor.NewClient(wc, interceptor.Funcs{Get: get})
}

// A namespaced server keeps its admin Secret in the claim's namespace, and its
// port and CA bundle go to the binding.
func TestUserOnANamespacedServer(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, nil)
	r := &UserReconciler{h.cfg}
	h.must(env.Client.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "team-admin", Namespace: h.ns},
		Data:       map[string][]byte{"username": []byte("team-admin"), "password": []byte("team-pw")},
	}))
	h.must(env.Client.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "team-ca", Namespace: h.ns},
		Data:       map[string][]byte{"ca.crt": []byte("pem-bytes")},
	}))
	team := &cachev1alpha1.CacheServer{ObjectMeta: metav1.ObjectMeta{Name: "team", Namespace: h.ns}}
	team.Spec.Host = "team.example.test"
	team.Spec.Port = new(int32)
	*team.Spec.Port = 6380
	team.Spec.AdminCredentialsSecretRef = common.AdminSecretRef{Name: "team-admin"}
	team.Spec.TLS = &common.TLS{CABundle: &common.CABundle{SecretRef: &common.KeyReference{Name: "team-ca"}}}
	h.must(env.Client.Create(ctx, team))
	u := h.user("app", func(u *cachev1alpha1.CacheUser) {
		u.Spec.ServerRef = common.ServerRef{Kind: kindCacheServer, Name: "team"}
	})

	h.reconcileOK(r, u)
	h.get(u)
	h.wantCondition(u.Status.Conditions, conditions.Ready, metav1.ConditionTrue, conditions.Available)
	got := h.bindingSecret(u)
	if got["host"] != "team.example.test" || got["port"] != "6380" || got["uri"] != "rediss://team.example.test:6380" ||
		got["sslrootcert"] != "pem-bytes" {
		t.Errorf("binding = %v", got)
	}
	last := h.fake.configs[len(h.fake.configs)-1]
	if last.Address != "team.example.test:6380" || last.Username != "team-admin" || last.Password != "team-pw" || string(last.CA) != "pem-bytes" {
		t.Errorf("connection = %+v", last)
	}
}

// A claim that never issued a create is released without the server: it may be
// unreachable, unresolvable or gone.
func TestUserDeletionNeedsNoServerWhenNothingWasMade(t *testing.T) {
	for name, prepare := range map[string]func(h *harness) *cachev1alpha1.CacheUser{
		"an unreachable server": func(h *harness) *cachev1alpha1.CacheUser {
			h.connectErr = fmt.Errorf("%w: down", valkeyacl.ErrUnavailable)
			return h.user("app", nil)
		},
		"a missing server": func(h *harness) *cachev1alpha1.CacheUser {
			return h.user("app", func(u *cachev1alpha1.CacheUser) { u.Spec.ServerRef.Name = "no-such-server" })
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, nil)
			r := &UserReconciler{h.cfg}
			u := prepare(h)
			_ = h.reconcile(r, u) // the finalizer is added and the claim is not Ready
			h.get(u)
			if len(u.Finalizers) == 0 || u.Status.ExternalID != "" {
				t.Fatalf("finalizers = %v, externalId = %q", u.Finalizers, u.Status.ExternalID)
			}
			h.must(env.Client.Delete(context.Background(), u))
			h.reconcileOK(r, u)
			if !h.gone(u) || h.fake.joined() != "" {
				t.Errorf("the claim was not released: calls %q", h.fake.joined())
			}
		})
	}
}

func TestNothingMade(t *testing.T) {
	ctx := context.Background()
	var n nothingMade
	obs, err := n.Observe(ctx, cachev1alpha1.CacheUserSpec{})
	if err != nil || obs.Exists {
		t.Errorf("observe: %+v, %v", obs, err)
	}
	if data, err := n.Create(ctx, cachev1alpha1.CacheUserSpec{}); data != nil || err != nil {
		t.Errorf("create: %v, %v", data, err)
	}
	if data, err := n.Update(ctx, cachev1alpha1.CacheUserSpec{}, userState{}); data != nil || err != nil {
		t.Errorf("update: %v, %v", data, err)
	}
	if err := n.Delete(ctx, userState{}); err != nil {
		t.Errorf("delete: %v", err)
	}
}

func TestReconcileOfAMissingClaim(t *testing.T) {
	h := newHarness(t, nil)
	r := &UserReconciler{h.cfg}
	h.reconcileOK(r, &cachev1alpha1.CacheUser{ObjectMeta: metav1.ObjectMeta{Name: "never", Namespace: h.ns}})
}
