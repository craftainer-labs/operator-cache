package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/craftainer-labs/lib-operator-go/pkg/binding"
	"github.com/craftainer-labs/lib-operator-go/pkg/conditions"
	"github.com/craftainer-labs/lib-operator-go/pkg/lifecycle"

	cachev1alpha1 "github.com/craftainer-labs/operator-cache/api/v1alpha1"
	"github.com/craftainer-labs/operator-cache/internal/awscache"
)

// pendingAfter is how soon a claim looks at an AWS user again after a write or
// while the user is not active (ADR 0002, section 16.6).
const pendingAfter = 10 * time.Second

// awsUser is the External of one CacheUser on an AWS server: one ElastiCache or
// MemoryDB user in the server's user group or ACL. status.externalName is its id
// and status.externalId its ARN. The ownership proof is the marker tag, which
// the user is made with (ADR 0002, section 10); status.externalId only records
// that this claim issued the create, so that a deletion knows whether to ask AWS.
//
// Writes are asynchronous: each reports lifecycle.Pending, and Observe reports it
// again until the user is active, so the claim looks again in ten seconds.
type awsUser struct {
	be    *backend
	claim *cachev1alpha1.CacheUser
	id    string
	iam   bool
	// pre is a precondition failure; Observe reports it, and Reconcile calls
	// nothing else when Observe fails.
	pre error
	// persist writes the claim's status now.
	persist func(context.Context) error
	// binding reads the binding Secret as it is now; nil when there is none.
	binding func(context.Context) (*corev1.Secret, error)
	// rec records the Events of a drift; nil records none.
	rec events.EventRecorder
}

// awsPassword is the External of a Password-mode claim: a Rotator.
type awsPassword struct{ *awsUser }

// awsIAM is the External of an IAM-mode claim: it has no credential to rotate
// and a binding Secret that holds no secret, so it is a NonRotator and a
// BindingRebuilder, and never a Rotator.
type awsIAM struct{ *awsUser }

var (
	_ lifecycle.Rotator[cachev1alpha1.CacheUserSpec, userState]          = awsPassword{}
	_ lifecycle.External[cachev1alpha1.CacheUserSpec, userState]         = awsIAM{}
	_ lifecycle.NonRotator                                               = awsIAM{}
	_ lifecycle.BindingRebuilder[cachev1alpha1.CacheUserSpec, userState] = awsIAM{}
)

// newAWSExternal returns the External of the claim for its authMode, and the
// part of it the caller sets up.
func newAWSExternal(be *backend, claim *cachev1alpha1.CacheUser) (lifecycle.External[cachev1alpha1.CacheUserSpec, userState], *awsUser) {
	// The id is derived once and kept in status.externalName (ADR 0002,
	// section 3).
	id := claim.GetExternalName()
	if id == "" {
		id = awscache.UserID(claim.Namespace, claim.Name)
		claim.SetExternalName(id)
	}
	u := &awsUser{be: be, claim: claim, id: id, iam: claim.Spec.AuthMode == cachev1alpha1.AuthModeIAM}
	if u.iam {
		return awsIAM{u}, u
	}
	return awsPassword{u}, u
}

// marker is the value of the marker tag of the claim.
func (e *awsUser) marker() string {
	return awscache.MarkerValue(e.claim.Namespace, e.claim.Name, string(e.claim.UID))
}

// access is the access string of the spec: the tenant's rules, checked again
// before they go to AWS.
func (e *awsUser) access(spec cachev1alpha1.CacheUserSpec) (string, error) {
	t, err := tenantOf(e.claim.Namespace, spec)
	if err != nil {
		return "", err
	}
	s, err := t.AccessString()
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrInvalidSpec, err)
	}
	return s, nil
}

// pendingOf reports a user or group that has not settled as a PendingError, and
// an active one as nil.
func pendingOf(u awscache.User) error {
	switch {
	case u.Status == awscache.StatusDeleting:
		return lifecycle.Pending(pendingAfter, conditions.Deleting, "waiting for the cloud to delete the user")
	case u.Status == awscache.StatusModifying:
		return lifecycle.Pending(pendingAfter, conditions.Updating, "waiting for the cloud to change the user")
	case u.Status != awscache.StatusActive:
		return lifecycle.Pending(pendingAfter, conditions.Creating, "waiting for the cloud to create the user")
	case u.GroupStatus != awscache.StatusActive:
		return lifecycle.Pending(pendingAfter, conditions.Updating, "waiting for the cloud to change the user group or ACL")
	}
	return nil
}

// sameAccess compares two access strings by their rules, ignoring order and
// spacing, because AWS may return them normalised.
func sameAccess(a, b string) bool {
	x, y := strings.Fields(a), strings.Fields(b)
	slices.Sort(x)
	slices.Sort(y)
	return slices.Equal(x, y)
}

// loginOf is the password the binding Secret holds for this user, "" when the
// Secret is missing, is of another user or holds none.
func (e *awsUser) loginOf(s *corev1.Secret) string {
	if s == nil || string(s.Data["username"]) != e.id {
		return ""
	}
	return string(s.Data["password"])
}

// rotating reports whether a rotation is in flight, as userExternal does: the
// user may then hold two passwords.
func (e *awsUser) rotating(s *corev1.Secret) bool {
	return rotatingOf(e.claim, s)
}

// extraPasswords reports a user that holds more passwords than the binding
// Secret and a rotation explain. AWS shows only the count, so this is all the
// operator can tell; which password is extra it cannot.
func (e *awsUser) extraPasswords(u awscache.User, s *corev1.Secret) bool {
	return !e.iam && e.loginOf(s) != "" && u.Passwords > 1 && !e.rotating(s)
}

func (e *awsUser) collision() error {
	return fmt.Errorf("%w: a user of the id %q exists and belongs to another claim", lifecycle.ErrNameCollision, e.id)
}

// Observe implements lifecycle.External. A user whose marker is not this
// claim's is refused and never adopted, changed or deleted.
func (e *awsUser) Observe(ctx context.Context, spec cachev1alpha1.CacheUserSpec) (lifecycle.Observation[userState], error) {
	obs, err := e.observe(ctx, spec)
	return obs, coarse(ctx, "observe", err)
}

func (e *awsUser) observe(ctx context.Context, spec cachev1alpha1.CacheUserSpec) (lifecycle.Observation[userState], error) {
	none := lifecycle.Observation[userState]{}
	// Reconcile adds the finalizer with a patch that replaces the claim with the
	// server's copy, which loses a name set before it; set it again here.
	e.claim.SetExternalName(e.id)
	if e.pre != nil {
		return none, e.pre
	}
	api, err := e.be.aws.api(ctx)
	if err != nil {
		return none, err
	}
	u, err := api.Lookup(ctx, e.id)
	if errors.Is(err, awscache.ErrNotFound) {
		return none, nil
	}
	if err != nil {
		return none, err
	}
	if u.Marker != e.marker() {
		return none, e.collision()
	}
	if err := pendingOf(u); err != nil {
		return none, err
	}
	want, err := e.access(spec)
	upToDate := err == nil && u.Member && sameAccess(u.AccessString, want)
	if upToDate && !e.iam {
		s, err := e.binding(ctx)
		if err != nil {
			return none, err
		}
		upToDate = !e.extraPasswords(u, s)
	}
	return lifecycle.Observation[userState]{Exists: true, UpToDate: upToDate, Current: userState{aws: u}}, nil
}

// data is the binding of the user (ADR 0002, sections 7 and 16.5). A password
// binding holds the password; an IAM binding holds none, and names the ARN the
// consumer's role must be allowed to connect to. The uri carries no credentials.
func (e *awsUser) data(password, arn string) (binding.Data, error) {
	host, port, err := e.be.aws.address()
	if err != nil {
		return nil, err
	}
	d := binding.Data{
		"host": []byte(host), "port": []byte(port), "uri": []byte(e.be.aws.uri()), "username": []byte(e.id),
		"keyPrefix": []byte(e.claim.Namespace + ":"),
		"cloud":     []byte("aws"), "region": []byte(e.be.aws.region),
		"authMode": []byte(cachev1alpha1.AuthModePassword),
	}
	if len(e.be.conn.CABundle) > 0 {
		d["sslrootcert"] = e.be.conn.CABundle
	}
	if e.iam {
		d["authMode"] = []byte(cachev1alpha1.AuthModeIAM)
		d["iamResourceArn"] = []byte(arn)
	} else {
		d["password"] = []byte(password)
	}
	return d, nil
}

// Create implements lifecycle.External: the user, with the tenant's rules and,
// in Password mode, a new password, which the binding returns so that the
// lifecycle writes the Secret. The user joins the group in the same reconcile
// where AWS allows it; if not, Update joins it once the user is active.
func (e *awsUser) Create(ctx context.Context, spec cachev1alpha1.CacheUserSpec) (binding.Data, error) {
	data, err := e.create(ctx, spec)
	return data, coarse(ctx, "create", err)
}

func (e *awsUser) create(ctx context.Context, spec cachev1alpha1.CacheUserSpec) (binding.Data, error) {
	access, err := e.access(spec)
	if err != nil {
		return nil, err
	}
	if _, _, err := e.be.aws.address(); err != nil {
		return nil, err
	}
	api, err := e.be.aws.api(ctx)
	if err != nil {
		return nil, err
	}
	// Record the intent before the create, so a crash after it is recognised and
	// a deletion asks AWS.
	e.claim.SetExternalID(createIssued)
	if err := e.persist(ctx); err != nil {
		return nil, err
	}
	s := awscache.Spec{AccessString: access, IAM: e.iam, Marker: e.marker()}
	password := ""
	if !e.iam {
		password = newPassword()
		s.Passwords = []string{password}
	}
	arn, err := api.Create(ctx, e.id, s)
	if err != nil {
		return nil, err
	}
	e.claim.SetExternalID(arn)
	if err := api.Join(ctx, e.id); err != nil {
		log.FromContext(ctx).Info("the user could not join the user group yet; the next reconcile retries", "category", "join")
	}
	return e.data(password, arn)
}

// Update implements lifecycle.External: one step toward the spec, then pending.
// The step is joining the group, else setting the access string, with the
// binding Secret's password alone when the user holds passwords the operator did
// not issue. The binding Secret is not rewritten here; Observe finds the next
// step once the user is active.
func (e *awsUser) Update(ctx context.Context, spec cachev1alpha1.CacheUserSpec, st userState) (binding.Data, error) {
	return nil, coarse(ctx, "update", e.update(ctx, spec, st))
}

func (e *awsUser) update(ctx context.Context, spec cachev1alpha1.CacheUserSpec, st userState) error {
	access, err := e.access(spec)
	if err != nil {
		return err
	}
	api, err := e.be.aws.api(ctx)
	if err != nil {
		return err
	}
	if !st.aws.Member {
		if err := api.Join(ctx, e.id); err != nil {
			return err
		}
		return lifecycle.Pending(pendingAfter, conditions.Updating, "waiting for the cloud to add the user to its group")
	}
	s := awscache.Spec{AccessString: access}
	drift := false
	if !e.iam {
		b, err := e.binding(ctx)
		if err != nil {
			return err
		}
		if e.extraPasswords(st.aws, b) {
			s.Passwords = []string{e.loginOf(b)}
			drift = true
		}
	}
	if err := api.Modify(ctx, e.id, s); err != nil {
		return err
	}
	// The Event comes after the write, so that it never says passwords were
	// removed when AWS refused to remove them.
	if drift {
		reportDrift(ctx, e.rec, e.claim, "unknown-passwords", "the user held passwords the operator did not issue; they were removed")
	}
	return lifecycle.Pending(pendingAfter, conditions.Updating, "waiting for the cloud to change the user")
}

// Delete implements lifecycle.External: the user goes with the claim, which takes
// it out of its group. Pending says the cloud is still deleting it; Observe then
// reports it until it is gone. Reconcile never gets here for a claim with no
// record of a create.
func (e *awsUser) Delete(ctx context.Context, _ userState) error {
	return coarse(ctx, "delete", e.remove(ctx))
}

func (e *awsUser) remove(ctx context.Context) error {
	api, err := e.be.aws.api(ctx)
	if err != nil {
		return err
	}
	if err := api.Delete(ctx, e.id); err != nil {
		return err
	}
	return lifecycle.Pending(pendingAfter, conditions.Deleting, "waiting for the cloud to delete the user")
}

// RotateBegin implements lifecycle.Rotator: the user holds the binding Secret's
// password beside a new one (AWS allows two). With no usable Secret, nothing is
// worth keeping and the new password stands alone. AWS shows no hashes, so a
// retry after the Secret was written but before the rotation was recorded keeps
// the first attempt's password and drops the one consumers still use.
func (e awsPassword) RotateBegin(ctx context.Context, _ cachev1alpha1.CacheUserSpec, _ userState, st lifecycle.RotationState) (binding.Data, lifecycle.Generation, error) {
	data, err := e.begin(ctx)
	if err != nil {
		return nil, "", coarse(ctx, "rotate", err)
	}
	return data, lifecycle.Generation(strconv.FormatInt(st.Sequence+1, 10)), nil
}

func (e awsPassword) begin(ctx context.Context) (binding.Data, error) {
	if _, _, err := e.be.aws.address(); err != nil {
		return nil, err
	}
	api, err := e.be.aws.api(ctx)
	if err != nil {
		return nil, err
	}
	b, err := e.binding(ctx)
	if err != nil {
		return nil, err
	}
	password := newPassword()
	passwords := []string{password}
	if current := e.loginOf(b); current != "" {
		passwords = []string{current, password}
	}
	if err := api.Modify(ctx, e.id, awscache.Spec{Passwords: passwords}); err != nil {
		return nil, err
	}
	return e.data(password, "")
}

// RotateFinish implements lifecycle.Rotator: the user keeps only the binding
// Secret's password. Without one, nothing is changed, because the user would be
// left with none. It returns nil after the write: the next Observe reports the
// user pending until AWS is done.
func (e awsPassword) RotateFinish(ctx context.Context, _ cachev1alpha1.CacheUserSpec, _ userState, _ lifecycle.RotationState) error {
	return coarse(ctx, "rotate", e.retire(ctx))
}

func (e awsPassword) retire(ctx context.Context) error {
	api, err := e.be.aws.api(ctx)
	if err != nil {
		return err
	}
	b, err := e.binding(ctx)
	if err != nil {
		return err
	}
	current := e.loginOf(b)
	if current == "" {
		return ErrPasswordNotOnServer
	}
	return api.Modify(ctx, e.id, awscache.Spec{Passwords: []string{current}})
}

// RotationNotApplicable implements lifecycle.NonRotator: an IAM login has no
// credential to rotate.
func (awsIAM) RotationNotApplicable() bool { return true }

// RebuildBinding implements lifecycle.BindingRebuilder: the IAM binding holds no
// secret, so a deleted Secret is rebuilt from the observed user.
func (e awsIAM) RebuildBinding(_ context.Context, _ cachev1alpha1.CacheUserSpec, cur userState) (binding.Data, error) {
	return e.data("", cur.aws.ARN)
}
