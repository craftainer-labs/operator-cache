package controller

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"slices"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlcontroller "sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/craftainer-labs/lib-operator-go/pkg/binding"
	"github.com/craftainer-labs/lib-operator-go/pkg/lifecycle"

	cachev1alpha1 "github.com/craftainer-labs/operator-cache/api/v1alpha1"
	"github.com/craftainer-labs/operator-cache/internal/valkeyacl"
)

// createIssued is written to status.externalId before the user is made on the
// server. It records that this claim issued the create, so a user a crash left
// half made is recognised as this claim's and finished. status.externalId is the
// only proof of ownership: an ACL user carries no marker, so a user that exists
// while the claim has no record is someone else's and is never touched.
const createIssued = "create-issued"

// UserReconciler reconciles CacheUser claims (a credential kind).
type UserReconciler struct{ Config }

// Reconcile implements reconcile.Reconciler.
func (r *UserReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var u cachev1alpha1.CacheUser
	if err := r.Client.Get(ctx, req.NamespacedName, &u); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	// A claim that is being deleted and never issued a create has nothing on the
	// server: it is released without contacting it, which may be gone. A user
	// with its name, if there is one, is not ours and is not deleted.
	if !u.DeletionTimestamp.IsZero() && u.GetExternalID() == "" {
		return lifecycle.Reconcile[cachev1alpha1.CacheUserSpec, userState](ctx, r.Client, &u, u.Spec, nothingMade{}, r.options(nil))
	}
	be, err := r.backend(ctx, u.Namespace, u.Spec.ServerRef)
	opts := r.options(err)
	if be != nil {
		defer be.release()
	}
	var ext lifecycle.External[cachev1alpha1.CacheUserSpec, userState]
	if err == nil {
		opts.ServerHost = be.cfg.Host
		x := newUserExternal(be, &u)
		// Deleting a user does not need a Ready server.
		if u.DeletionTimestamp.IsZero() {
			x.pre = be.notReady
		}
		x.persist = func(ctx context.Context) error { return r.Client.Status().Update(ctx, &u) }
		x.binding = func(ctx context.Context) (*corev1.Secret, error) { return r.bindingSecret(ctx, &u) }
		x.drop = func(ctx context.Context, s *corev1.Secret) error {
			return client.IgnoreNotFound(r.Client.Delete(ctx, s, client.Preconditions{UID: &s.UID}))
		}
		ext = x
	}
	return lifecycle.Reconcile(ctx, r.Client, &u, u.Spec, ext, opts)
}

// options are the lifecycle options of the controller; resolveErr is the error
// of resolving the server, or nil.
func (r *UserReconciler) options(resolveErr error) lifecycle.Options {
	return lifecycle.Options{
		Class: lifecycle.ClassCredential, BindingType: binding.TypeRedis, Provider: binding.DefaultProvider,
		ManagedBy: r.ManagedBy, Recorder: r.Recorder, APIReader: r.APIReader,
		DefaultDeletionPolicy: r.DefaultDeletionPolicy, ResolveErr: resolveErr, Now: r.Now,
	}
}

// nothingMade is the External of a claim that never issued a create: nothing of
// it is on the server, so there is nothing to observe or delete.
type nothingMade struct{}

// Observe implements lifecycle.External: nothing exists.
func (nothingMade) Observe(context.Context, cachev1alpha1.CacheUserSpec) (lifecycle.Observation[userState], error) {
	return lifecycle.Observation[userState]{}, nil
}

// Create implements lifecycle.External; it is never called, because nothing exists.
func (nothingMade) Create(context.Context, cachev1alpha1.CacheUserSpec) (binding.Data, error) {
	return nil, nil
}

// Update implements lifecycle.External; it is never called, because nothing exists.
func (nothingMade) Update(context.Context, cachev1alpha1.CacheUserSpec, userState) (binding.Data, error) {
	return nil, nil
}

// Delete implements lifecycle.External: there is nothing to delete.
func (nothingMade) Delete(context.Context, userState) error { return nil }

// bindingSecret is the claim's binding Secret, nil when there is none. It reads
// through the API reader, because the cache only holds Secrets with the
// operator's label.
func (r *UserReconciler) bindingSecret(ctx context.Context, u *cachev1alpha1.CacheUser) (*corev1.Secret, error) {
	var s corev1.Secret
	err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: u.Namespace, Name: u.GetBindingSecretName()}, &s)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	return &s, err
}

// serverRefIndex indexes CacheUsers by the server they name, as kind/name.
const serverRefIndex = ".spec.serverRef"

func indexServerRef(o client.Object) []string {
	ref := o.(*cachev1alpha1.CacheUser).Spec.ServerRef
	return []string{ref.Kind + "/" + ref.Name}
}

// usersOfServer maps a server of a kind to the CacheUsers that name it, so a
// user that waits for its server, or whose server changed (its address), is
// reconciled when the server does. A namespaced server serves its own namespace
// only.
func usersOfServer(c client.Reader, kind string) handler.MapFunc {
	return func(ctx context.Context, o client.Object) []reconcile.Request {
		opts := []client.ListOption{client.MatchingFields{serverRefIndex: kind + "/" + o.GetName()}}
		if o.GetNamespace() != "" {
			opts = append(opts, client.InNamespace(o.GetNamespace()))
		}
		var list cachev1alpha1.CacheUserList
		if err := c.List(ctx, &list, opts...); err != nil {
			log.FromContext(ctx).Error(err, "cannot list the users of a server", "kind", kind, "server", o.GetName())
			return nil
		}
		reqs := make([]reconcile.Request, len(list.Items))
		for i := range list.Items {
			reqs[i] = reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&list.Items[i])}
		}
		return reqs
	}
}

// conditionsOf returns the status conditions of a server of either kind.
func conditionsOf(o client.Object) []metav1.Condition {
	switch s := o.(type) {
	case *cachev1alpha1.ClusterCacheServer:
		return s.Status.Conditions
	case *cachev1alpha1.CacheServer:
		return s.Status.Conditions
	}
	return nil
}

// serverChanged lets a server's creation and deletion through, and an update
// only when its spec changed or it went from Ready to not Ready or back: the
// status writes of a health probe would otherwise reconcile every user of the
// server at every probe.
var serverChanged = predicate.Funcs{UpdateFunc: func(e event.UpdateEvent) bool {
	return e.ObjectOld.GetGeneration() != e.ObjectNew.GetGeneration() ||
		serverNotReady(conditionsOf(e.ObjectOld)) != serverNotReady(conditionsOf(e.ObjectNew))
}}

// SetupWithManager registers the controller. It also watches the binding
// Secrets it owns (every change, so an edited Secret is repaired) and the
// servers its claims name (so a user that waits for its server goes on when the
// server exists or is Ready). Only the claims themselves pass the lifecycle
// filter.
func (r *UserReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &cachev1alpha1.CacheUser{}, serverRefIndex, indexServerRef); err != nil {
		return err
	}
	reader := mgr.GetClient()
	return ctrl.NewControllerManagedBy(mgr).
		For(&cachev1alpha1.CacheUser{}, builder.WithPredicates(lifecycle.Predicates())).
		Owns(&corev1.Secret{}).
		Watches(&cachev1alpha1.ClusterCacheServer{},
			handler.EnqueueRequestsFromMapFunc(usersOfServer(reader, kindClusterCacheServer)),
			builder.WithPredicates(serverChanged)).
		Watches(&cachev1alpha1.CacheServer{},
			handler.EnqueueRequestsFromMapFunc(usersOfServer(reader, kindCacheServer)),
			builder.WithPredicates(serverChanged)).
		WithOptions(ctrlcontroller.Options{MaxConcurrentReconciles: max(1, r.MaxConcurrentReconciles)}).
		Named("cacheuser").
		Complete(r)
}

// userState is what Observe read of a user on the server.
type userState struct {
	info valkeyacl.UserInfo
}

// userExternal is the lifecycle.Rotator of one CacheUser (ADR 0002, section 8):
// one ACL user whose passwords overlap during a rotation. status.externalName is
// its name (derived once, with the long hash); status.externalId is the proof
// that it is this claim's.
type userExternal struct {
	be    *backend
	claim *cachev1alpha1.CacheUser
	name  string
	// pre is a precondition failure (the server is not Ready). Observe reports
	// it, and Reconcile calls nothing else when Observe fails.
	pre error
	// persist writes the claim's status now, instead of at the end of the
	// reconcile.
	persist func(context.Context) error
	// binding reads the binding Secret as it is now (through the API reader); nil
	// when there is none.
	binding func(context.Context) (*corev1.Secret, error)
	// drop deletes a binding Secret.
	drop func(context.Context, *corev1.Secret) error
}

func newUserExternal(be *backend, claim *cachev1alpha1.CacheUser) *userExternal {
	// Observe goes by status.externalName, never by re-deriving (ADR 0002,
	// section 3); the name is derived once, at the first reconcile.
	name := claim.GetExternalName()
	if name == "" {
		name = userName(claim.Namespace, claim.Name)
		claim.SetExternalName(name)
	}
	return &userExternal{be: be, claim: claim, name: name}
}

// tenant is what the spec lets the user do, on the keys of the claim's
// namespace. The profile is checked again here (the CRD and the webhook have
// checked it), and Rules checks the namespace.
func (e *userExternal) tenant(spec cachev1alpha1.CacheUserSpec) (valkeyacl.Tenant, error) {
	t := valkeyacl.Tenant{Prefix: e.claim.Namespace, Channels: spec.Channels}
	switch spec.Profile {
	case cachev1alpha1.ProfileReadOnly:
	case cachev1alpha1.ProfileReadWrite, "":
		t.Write = true
	default:
		return t, fmt.Errorf("%w: profile %q", ErrInvalidSpec, spec.Profile)
	}
	return t, nil
}

// rules rebuilds the ACL rules of the spec, which a write sends.
func (e *userExternal) rules(spec cachev1alpha1.CacheUserSpec) ([]string, error) {
	t, err := e.tenant(spec)
	if err != nil {
		return nil, err
	}
	rules, err := t.Rules()
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidSpec, err)
	}
	return rules, nil
}

func (e *userExternal) collision() error {
	return fmt.Errorf("%w: a user of the name %q exists and this claim did not make it", lifecycle.ErrNameCollision, e.name)
}

// Observe implements lifecycle.External. A user that exists while the claim has
// no record of a create is not ours: it is refused, never adopted, changed or
// deleted.
func (e *userExternal) Observe(ctx context.Context, spec cachev1alpha1.CacheUserSpec) (lifecycle.Observation[userState], error) {
	obs, err := e.observe(ctx, spec)
	return obs, coarse(ctx, "observe", err)
}

func (e *userExternal) observe(ctx context.Context, spec cachev1alpha1.CacheUserSpec) (lifecycle.Observation[userState], error) {
	none := lifecycle.Observation[userState]{}
	// Reconcile adds the finalizer with a patch that replaces the claim with the
	// server's copy, which loses a name set before it; set it again here.
	e.claim.SetExternalName(e.name)
	if e.pre != nil {
		return none, e.pre
	}
	v, err := e.be.server(ctx)
	if err != nil {
		return none, err
	}
	info, err := v.GetUser(ctx, e.name)
	if errors.Is(err, valkeyacl.ErrNotFound) {
		return none, nil
	}
	if err != nil {
		return none, err
	}
	if e.claim.GetExternalID() == "" {
		return none, e.collision()
	}
	// A rule the controller refuses gives no wanted set, so the user is never up
	// to date and Update reports it; deleting does not need a valid spec.
	t, err := e.tenant(spec)
	return lifecycle.Observation[userState]{Exists: true, UpToDate: err == nil && t.Matches(info), Current: userState{info}}, nil
}

// newPassword is a password for the user: 52 characters of 5 bits each.
func newPassword() string { return rand.Text() + rand.Text() }

// Create implements lifecycle.External: the user, with a new password and the
// rules of the spec. It asks the server first: a user that exists while the claim
// has no record of a create is someone else's, and nothing is written. Between
// that check and the SETUSER another client could make a user of the same name,
// and the command would then reset it; the window is as long as one round trip
// and the name carries an 80-bit hash, so it is a known limit rather than
// something the protocol lets us close (ACL SETUSER cannot fail on an existing
// user).
func (e *userExternal) Create(ctx context.Context, spec cachev1alpha1.CacheUserSpec) (binding.Data, error) {
	data, err := e.create(ctx, spec)
	return data, coarse(ctx, "create", err)
}

func (e *userExternal) create(ctx context.Context, spec cachev1alpha1.CacheUserSpec) (binding.Data, error) {
	rules, err := e.rules(spec)
	if err != nil {
		return nil, err
	}
	v, err := e.be.server(ctx)
	if err != nil {
		return nil, err
	}
	_, err = v.GetUser(ctx, e.name)
	switch {
	case err == nil && e.claim.GetExternalID() == "":
		return nil, e.collision()
	case err != nil && !errors.Is(err, valkeyacl.ErrNotFound):
		return nil, err
	}
	// Record the intent before the create, so a crash after it is recognised.
	e.claim.SetExternalID(createIssued)
	if err := e.persist(ctx); err != nil {
		return nil, err
	}
	password := newPassword()
	// "reset" first: whatever the user was (a crashed attempt of this claim's
	// own), it is off, without passwords, keys, channels and commands before the
	// rules apply.
	if err := v.SetUser(ctx, e.name, append([]string{"reset", valkeyacl.AddPassword(password)}, rules...)...); err != nil {
		return nil, err
	}
	e.claim.SetExternalID(e.name)
	return e.data(password), nil
}

// Update implements lifecycle.External: the rules follow the spec, without
// "reset", so the password survives and the credentials in use stay valid. The
// binding Secret is returned as it is, with the fields that follow the spec.
//
// A password is never reset in place. When the binding Secret does not hold the
// user (it is missing, or holds another user or no password), Update has applied
// the rules and returns ErrBindingNotUsable, after deleting a Secret of this
// claim that holds something else: the next reconcile finds the Secret missing
// and the lifecycle rotates.
func (e *userExternal) Update(ctx context.Context, spec cachev1alpha1.CacheUserSpec, _ userState) (binding.Data, error) {
	data, err := e.update(ctx, spec)
	return data, coarse(ctx, "update", err)
}

func (e *userExternal) update(ctx context.Context, spec cachev1alpha1.CacheUserSpec) (binding.Data, error) {
	rules, err := e.rules(spec)
	if err != nil {
		return nil, err
	}
	v, err := e.be.server(ctx)
	if err != nil {
		return nil, err
	}
	if err := v.SetUser(ctx, e.name, rules...); err != nil {
		return nil, err
	}
	e.claim.SetExternalID(e.name)
	s, err := e.binding(ctx)
	if err != nil {
		return nil, err
	}
	if s != nil && string(s.Data["username"]) == e.name && len(s.Data["password"]) > 0 {
		return e.data(string(s.Data["password"])), nil
	}
	if s != nil && metav1.IsControlledBy(s, e.claim) {
		if err := e.drop(ctx, s); err != nil {
			return nil, err
		}
	}
	return nil, ErrBindingNotUsable
}

// Delete implements lifecycle.External: the user goes with the claim, which ends
// its sessions. Reconcile never gets here for a claim with no record of a
// create, so only a user this claim made is deleted; one that is already gone is
// a success, as DELUSER says.
func (e *userExternal) Delete(ctx context.Context, _ userState) error {
	return coarse(ctx, "delete", e.remove(ctx))
}

func (e *userExternal) remove(ctx context.Context) error {
	v, err := e.be.server(ctx)
	if err != nil {
		return err
	}
	_, err = v.DelUser(ctx, e.name)
	return err
}

// currentPassword is the password the binding Secret holds for this user, or ""
// when there is none.
func (e *userExternal) currentPassword(ctx context.Context) (string, error) {
	s, err := e.binding(ctx)
	if err != nil || s == nil || string(s.Data["username"]) != e.name {
		return "", err
	}
	return string(s.Data["password"]), nil
}

// RotateBegin implements lifecycle.Rotator: the user keeps every password it has
// beside a new one, so every credential a consumer may still hold works through
// the grace period. What is "previous" comes from the server, not from the
// binding Secret: the lifecycle writes the Secret before it marks it and before
// it records the rotation, so a retry after a crash between those steps finds
// the Secret holding the new password of the first attempt. Keeping only the
// Secret's password would drop the one the consumers still use. The passwords
// of a failed attempt that nobody was given stay until RotateFinish, which keeps
// only the password of the Secret. With no usable binding Secret (missing, or of
// another user) there is no credential to protect: the user gets the new password
// alone.
func (e *userExternal) RotateBegin(ctx context.Context, _ cachev1alpha1.CacheUserSpec, _ userState, st lifecycle.RotationState) (binding.Data, lifecycle.Generation, error) {
	data, err := e.begin(ctx)
	if err != nil {
		return nil, "", coarse(ctx, "rotate", err)
	}
	return data, lifecycle.Generation(strconv.FormatInt(st.Sequence+1, 10)), nil
}

func (e *userExternal) begin(ctx context.Context) (binding.Data, error) {
	v, err := e.be.server(ctx)
	if err != nil {
		return nil, err
	}
	current, err := e.currentPassword(ctx)
	if err != nil {
		return nil, err
	}
	password := newPassword()
	var rules []string
	if current != "" {
		info, err := v.GetUser(ctx, e.name)
		if err != nil && !errors.Is(err, valkeyacl.ErrNotFound) {
			return nil, err
		}
		// "resetpass" then every hash the user has, so what stays does not depend
		// on the order of the commands. A password the server does not list is
		// not reinstated from the Secret: that would let whoever can write the
		// Secret choose a password for the user.
		hashes := info.PasswordHashes
		rules = valkeyacl.KeepOnly(hashes...)
	}
	if err := v.SetUser(ctx, e.name, append(rules, valkeyacl.AddPassword(password))...); err != nil {
		return nil, err
	}
	e.claim.SetExternalID(e.name)
	return e.data(password), nil
}

// RotateFinish implements lifecycle.Rotator: the user keeps only the password of
// the binding Secret, which RotateBegin wrote, so the previous one stops
// working. The hash is looked up first: if the server does not list it, nothing
// is changed, because "resetpass" would otherwise leave the user with no
// password at all.
func (e *userExternal) RotateFinish(ctx context.Context, _ cachev1alpha1.CacheUserSpec, _ userState, _ lifecycle.RotationState) error {
	return coarse(ctx, "rotate", e.retire(ctx))
}

func (e *userExternal) retire(ctx context.Context) error {
	v, err := e.be.server(ctx)
	if err != nil {
		return err
	}
	current, err := e.currentPassword(ctx)
	if err != nil {
		return err
	}
	hash := valkeyacl.HashPassword(current)
	info, err := v.GetUser(ctx, e.name)
	if err != nil {
		return err
	}
	if current == "" || !slices.Contains(info.PasswordHashes, hash) {
		return ErrPasswordNotOnServer
	}
	return v.SetUser(ctx, e.name, valkeyacl.KeepOnly(hash)...)
}

// data is the redis binding of the user (ADR 0002, section 7). The key prefix is
// the one the user is limited to; uri carries no credentials.
func (e *userExternal) data(password string) binding.Data {
	// Address is always host:port, with the default port filled in.
	_, port, _ := net.SplitHostPort(e.be.cfg.Address())
	return binding.Data{
		"host":        []byte(e.be.cfg.Host),
		"port":        []byte(port),
		"uri":         []byte(e.be.cfg.Endpoint()),
		"username":    []byte(e.name),
		"password":    []byte(password),
		"keyPrefix":   []byte(e.claim.Namespace + ":"),
		"sslrootcert": e.be.conn.CABundle,
	}
}

var _ lifecycle.Rotator[cachev1alpha1.CacheUserSpec, userState] = (*userExternal)(nil)
