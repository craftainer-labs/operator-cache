package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/craftainer-labs/lib-operator-go/pkg/lifecycle"

	cachev1alpha1 "github.com/craftainer-labs/operator-cache/api/v1alpha1"
	"github.com/craftainer-labs/operator-cache/internal/valkeyacl"
)

// These tests drive the External of one user straight against the in-memory
// server, with no API server, for every branch of its calls.

var (
	extName = userName("tenant", "app")
	extSpec = cachev1alpha1.CacheUserSpec{Profile: cachev1alpha1.ProfileReadWrite}
)

func userExt(t *testing.T, v Valkey) *userExternal {
	t.Helper()
	be := &backend{
		cfg:    cachev1alpha1.ServerConfig{Host: "valkey.example.test"},
		server: func(context.Context) (Valkey, error) { return v, nil },
	}
	claim := &cachev1alpha1.CacheUser{ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "tenant", UID: "uid-1"}}
	e := newUserExternal(be, claim)
	e.persist = func(context.Context) error { return nil }
	e.binding = func(context.Context) (*corev1.Secret, error) { return nil, nil }
	e.drop = func(context.Context, *corev1.Secret) error { return nil }
	return e
}

// bindingWith makes the binding read return a Secret of the user with the
// password.
func bindingWith(e *userExternal, user, password string) {
	e.binding = func(context.Context) (*corev1.Secret, error) {
		return &corev1.Secret{Data: map[string][]byte{"username": []byte(user), "password": []byte(password)}}, nil
	}
}

// ownedBy is the controller reference of the claim of userExt.
func ownedBy(uid types.UID) metav1.OwnerReference {
	yes := true
	return metav1.OwnerReference{APIVersion: "cache.craftainer.dev/v1alpha1", Kind: "CacheUser", Name: "app", UID: uid, Controller: &yes}
}

// created is a user the External made, with its Observe.
func created(t *testing.T) (*userExternal, *memValkey, string) {
	t.Helper()
	v := newMemValkey()
	e := userExt(t, v)
	data, err := e.Create(context.Background(), extSpec)
	if err != nil {
		t.Fatal(err)
	}
	obs, err := e.Observe(context.Background(), extSpec)
	if err != nil || !obs.Exists || !obs.UpToDate {
		t.Fatalf("observe after create: %+v, %v", obs, err)
	}
	v.reset()
	return e, v, string(data["password"])
}

func TestNewUserExternalKeepsTheRecordedName(t *testing.T) {
	claim := &cachev1alpha1.CacheUser{ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "tenant"}}
	e := newUserExternal(&backend{}, claim)
	if e.name != extName || claim.GetExternalName() != extName {
		t.Errorf("external = %+v", e)
	}
	claim.SetExternalName("kept")
	if e = newUserExternal(&backend{}, claim); e.name != "kept" {
		t.Errorf("name = %q", e.name)
	}
}

func TestUserRules(t *testing.T) {
	e := userExt(t, newMemValkey())
	for _, tc := range []struct {
		profile  cachev1alpha1.Profile
		channels bool
		keys     string
	}{
		{"", false, "~tenant:*"},
		{cachev1alpha1.ProfileReadWrite, true, "~tenant:*"},
		{cachev1alpha1.ProfileReadOnly, false, "%R~tenant:*"},
	} {
		rules, err := e.rules(cachev1alpha1.CacheUserSpec{Profile: tc.profile, Channels: tc.channels})
		if err != nil || !slices.Contains(rules, tc.keys) || slices.Contains(rules, "&tenant:*") != tc.channels {
			t.Errorf("profile %q: %v, %v", tc.profile, rules, err)
		}
		if slices.Contains(rules, "+@write") != (tc.profile != cachev1alpha1.ProfileReadOnly) {
			t.Errorf("profile %q: write rule = %v", tc.profile, rules)
		}
	}
	// A profile or a namespace outside the rules is refused again here.
	if _, err := e.rules(cachev1alpha1.CacheUserSpec{Profile: "Admin"}); !errors.Is(err, ErrInvalidSpec) {
		t.Errorf("profile Admin: %v", err)
	}
	e.claim.Namespace = "Not_A-Label*"
	if _, err := e.rules(extSpec); !errors.Is(err, ErrInvalidSpec) || !errors.Is(err, valkeyacl.ErrInvalid) {
		t.Errorf("namespace: %v", err)
	}
}

func TestUserObserve(t *testing.T) {
	ctx := context.Background()

	// Nothing there: not found.
	v := newMemValkey()
	e := userExt(t, v)
	if obs, err := e.Observe(ctx, extSpec); err != nil || obs.Exists {
		t.Errorf("empty server: %+v, %v", obs, err)
	}

	// The precondition and the connection are reported before anything is read.
	e.pre = ErrServerNotReady
	if _, err := e.Observe(ctx, extSpec); !errors.Is(err, ErrServerNotReady) {
		t.Errorf("pre: %v", err)
	}
	e.pre = nil
	e.be.server = func(context.Context) (Valkey, error) { return nil, errServer }
	if _, err := e.Observe(ctx, extSpec); !errors.Is(err, errServer) {
		t.Errorf("connect: %v", err)
	}
	e.be.server = func(context.Context) (Valkey, error) { return v, nil }

	// A failing read is an error.
	v.fail["GetUser"] = errServer
	if _, err := e.Observe(ctx, extSpec); !errors.Is(err, errServer) {
		t.Errorf("GetUser: %v", err)
	}
	v.fail = map[string]error{}

	// A user that exists while the claim has no record is refused, and nothing is
	// written.
	v.put(extName, &memUser{on: true})
	if _, err := e.Observe(ctx, extSpec); !errors.Is(err, lifecycle.ErrNameCollision) {
		t.Errorf("a user without a record: %v", err)
	}
	v.wantNot(t, writes...)

	// With the record it is the claim's: up to date only when it matches.
	e.claim.SetExternalID(extName)
	obs, err := e.Observe(ctx, extSpec)
	if err != nil || !obs.Exists || obs.UpToDate {
		t.Errorf("a user with other rules: %+v, %v", obs, err)
	}
	// A profile that is not valid gives no wanted set: never up to date, no error.
	e, v, _ = created(t)
	obs, err = e.Observe(ctx, cachev1alpha1.CacheUserSpec{Profile: "Admin"})
	if err != nil || !obs.Exists || obs.UpToDate {
		t.Errorf("an invalid profile: %+v, %v", obs, err)
	}
	// What was widened by hand is not up to date.
	v.user(extName).keys = "~*"
	if obs, err := e.Observe(ctx, extSpec); err != nil || !obs.Exists || obs.UpToDate {
		t.Errorf("widened keys: %+v, %v", obs, err)
	}
}

func TestUserCreate(t *testing.T) {
	ctx := context.Background()

	// The user is made with "reset", a new password and the rules, after the
	// intent is recorded.
	v := newMemValkey()
	e := userExt(t, v)
	var recorded string
	e.persist = func(context.Context) error { recorded = e.claim.GetExternalID(); return nil }
	data, err := e.Create(ctx, extSpec)
	if err != nil || recorded != createIssued || e.claim.GetExternalID() != extName {
		t.Fatalf("create: %v, recorded %q, externalId %q", err, recorded, e.claim.GetExternalID())
	}
	pw := string(data["password"])
	if string(data["username"]) != extName || len(pw) != 52 || string(data["keyPrefix"]) != "tenant:" || !v.user(extName).hasPassword(pw) {
		t.Errorf("binding data = %v", data)
	}
	v.want(t, "GetUser "+extName, "SetUser "+extName+" reset >"+pw+" on clearselectors")

	// A user that exists with no record is not touched.
	v = newMemValkey()
	e = userExt(t, v)
	v.put(extName, &memUser{on: true, keys: "~x:*"})
	if _, err := e.Create(ctx, extSpec); !errors.Is(err, lifecycle.ErrNameCollision) || e.claim.GetExternalID() != "" {
		t.Errorf("a user without a record: %v, externalId %q", err, e.claim.GetExternalID())
	}
	v.wantNot(t, writes...)

	// A user the claim's own earlier attempt made is made again from "reset".
	e.claim.SetExternalID(createIssued)
	if _, err := e.Create(ctx, extSpec); err != nil || v.user(extName).keys != "~tenant:*" {
		t.Errorf("an earlier attempt's user: %v, %+v", err, v.user(extName))
	}

	// Every failing step is an error and writes nothing further.
	if _, err := e.Create(ctx, cachev1alpha1.CacheUserSpec{Profile: "Admin"}); !errors.Is(err, ErrInvalidSpec) {
		t.Errorf("invalid spec: %v", err)
	}
	e.be.server = func(context.Context) (Valkey, error) { return nil, errServer }
	if _, err := e.Create(ctx, extSpec); !errors.Is(err, errServer) {
		t.Errorf("connect: %v", err)
	}
	for _, method := range []string{"GetUser", "SetUser"} {
		v := newMemValkey()
		e := userExt(t, v)
		v.fail[method] = errServer
		if _, err := e.Create(ctx, extSpec); !errors.Is(err, errServer) {
			t.Errorf("%s: %v", method, err)
		}
	}
	refused := fmt.Errorf("%w: refused", valkeyacl.ErrInvalid)
	// A create that failed with no user made withdraws its record, so a user
	// made under the name later is a collision. Where the user exists, the record
	// stays.
	v = newMemValkey()
	e = userExt(t, v)
	v.fail["SetUser"] = refused
	persisted := 0
	e.persist = func(context.Context) error { persisted++; return nil }
	if _, err := e.Create(ctx, extSpec); !errors.Is(err, refused) || e.claim.GetExternalID() != "" || persisted != 2 {
		t.Errorf("failed create: %v, externalId %q, %d persists", err, e.claim.GetExternalID(), persisted)
	}
	// A persist that fails keeps the record in step with the stored one.
	v = newMemValkey()
	e = userExt(t, v)
	v.fail["SetUser"] = refused
	calls := 0
	e.persist = func(context.Context) error {
		if calls++; calls == 2 {
			return errServer
		}
		return nil
	}
	if _, err := e.Create(ctx, extSpec); !errors.Is(err, refused) || e.claim.GetExternalID() != createIssued {
		t.Errorf("withdraw whose write failed: %v, externalId %q", err, e.claim.GetExternalID())
	}
	// A denied command is a refusal too.
	v = newMemValkey()
	e = userExt(t, v)
	v.fail["SetUser"] = valkeyacl.ErrAuth
	e.persist = func(context.Context) error { return nil }
	if _, err := e.Create(ctx, extSpec); !errors.Is(err, valkeyacl.ErrAuth) || e.claim.GetExternalID() != "" {
		t.Errorf("denied create: %v, externalId %q", err, e.claim.GetExternalID())
	}
	// A server error that is neither (it may be a failover) keeps the record: the
	// server cannot say.
	v = newMemValkey()
	e = userExt(t, v)
	v.fail["SetUser"] = valkeyacl.ErrServer
	e.persist = func(context.Context) error { return nil }
	if _, err := e.Create(ctx, extSpec); !errors.Is(err, valkeyacl.ErrServer) || e.claim.GetExternalID() != createIssued {
		t.Errorf("server error: %v, externalId %q", err, e.claim.GetExternalID())
	}
	// A timeout or a lost connection may have been applied: the record stays.
	for _, cause := range []error{valkeyacl.ErrTimeout, valkeyacl.ErrUnavailable} {
		v = newMemValkey()
		e = userExt(t, v)
		v.fail["SetUser"] = cause
		e.persist = func(context.Context) error { return nil }
		if _, err := e.Create(ctx, extSpec); !errors.Is(err, cause) || e.claim.GetExternalID() != createIssued {
			t.Errorf("%v: %v, externalId %q", cause, err, e.claim.GetExternalID())
		}
	}
	v = newMemValkey()
	v.users[extName] = &memUser{commands: "-@all"}
	e = userExt(t, v)
	e.claim.SetExternalID(createIssued)
	v.fail["SetUser"] = refused
	e.persist = func(context.Context) error { return nil }
	if _, err := e.Create(ctx, extSpec); !errors.Is(err, refused) || e.claim.GetExternalID() != createIssued {
		t.Errorf("failed create of an existing user: %v, externalId %q", err, e.claim.GetExternalID())
	}
	v = newMemValkey()
	e = userExt(t, v)
	e.persist = func(context.Context) error { return errServer }
	if _, err := e.Create(ctx, extSpec); !errors.Is(err, errServer) {
		t.Errorf("persist: %v", err)
	}
	v.wantNot(t, writes...)
}

func TestUserUpdate(t *testing.T) {
	ctx := context.Background()
	e, v, pw := created(t)
	cur := userState{}

	// With the Secret holding the user, the rules are reapplied without "reset"
	// and the same password is returned.
	bindingWith(e, extName, pw)
	data, err := e.Update(ctx, cachev1alpha1.CacheUserSpec{Profile: cachev1alpha1.ProfileReadOnly}, cur)
	if err != nil || string(data["password"]) != pw || v.user(extName).keys != "%R~tenant:*" || !v.user(extName).hasPassword(pw) {
		t.Errorf("update: %v, %v", err, data)
	}
	v.wantNot(t, "reset >", "resetpass", " >")

	// A Secret that does not hold the user is no credential: rules applied, a typed
	// error, and a Secret of the claim that holds something else is dropped.
	for _, tc := range []struct {
		name    string
		secret  *corev1.Secret
		dropped bool
	}{
		{"missing", nil, false},
		{"another user", &corev1.Secret{Data: map[string][]byte{"username": []byte("x"), "password": []byte("p")}}, false},
		{"no password", &corev1.Secret{Data: map[string][]byte{"username": []byte(extName)}}, false},
		{"ours, of another user", &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{OwnerReferences: []metav1.OwnerReference{ownedBy("uid-1")}},
			Data:       map[string][]byte{"username": []byte("x")},
		}, true},
	} {
		e.binding = func(context.Context) (*corev1.Secret, error) { return tc.secret, nil }
		dropped := false
		e.drop = func(context.Context, *corev1.Secret) error { dropped = true; return nil }
		if _, err := e.Update(ctx, extSpec, cur); !errors.Is(err, ErrBindingNotUsable) {
			t.Errorf("%s: %v", tc.name, err)
		}
		if dropped != tc.dropped {
			t.Errorf("%s: dropped = %v", tc.name, dropped)
		}
	}

	// Failures.
	e.binding = func(context.Context) (*corev1.Secret, error) {
		return &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{OwnerReferences: []metav1.OwnerReference{ownedBy("uid-1")}},
			Data:       map[string][]byte{"username": []byte("x")},
		}, nil
	}
	e.drop = func(context.Context, *corev1.Secret) error { return errServer }
	if _, err := e.Update(ctx, extSpec, cur); !errors.Is(err, errServer) {
		t.Errorf("drop: %v", err)
	}
	e.binding = func(context.Context) (*corev1.Secret, error) { return nil, errServer }
	if _, err := e.Update(ctx, extSpec, cur); !errors.Is(err, errServer) {
		t.Errorf("binding: %v", err)
	}
	if _, err := e.Update(ctx, cachev1alpha1.CacheUserSpec{Profile: "Admin"}, cur); !errors.Is(err, ErrInvalidSpec) {
		t.Errorf("invalid spec: %v", err)
	}
	v.fail["SetUser"] = errServer
	if _, err := e.Update(ctx, extSpec, cur); !errors.Is(err, errServer) {
		t.Errorf("SetUser: %v", err)
	}
	e.be.server = func(context.Context) (Valkey, error) { return nil, errServer }
	if _, err := e.Update(ctx, extSpec, cur); !errors.Is(err, errServer) {
		t.Errorf("connect: %v", err)
	}
}

func TestUserDelete(t *testing.T) {
	ctx := context.Background()
	e, v, _ := created(t)
	if err := e.Delete(ctx, userState{}); err != nil || v.user(extName) != nil {
		t.Errorf("delete: %v, %v", err, v.users)
	}
	v.want(t, "DelUser "+extName)

	// A user that is already gone is a success, as DELUSER says.
	if err := e.Delete(ctx, userState{}); err != nil {
		t.Errorf("delete of a gone user: %v", err)
	}

	v.fail["DelUser"] = errServer
	if err := e.Delete(ctx, userState{}); !errors.Is(err, errServer) {
		t.Errorf("DelUser: %v", err)
	}
	e.be.server = func(context.Context) (Valkey, error) { return nil, errServer }
	if err := e.Delete(ctx, userState{}); !errors.Is(err, errServer) {
		t.Errorf("connect: %v", err)
	}
}

func TestUserRotateBegin(t *testing.T) {
	ctx := context.Background()
	st := lifecycle.RotationState{Sequence: 4}

	// With a current password it is kept beside the new one.
	e, v, pw := created(t)
	bindingWith(e, extName, pw)
	data, gen, err := e.RotateBegin(ctx, extSpec, userState{}, st)
	next := string(data["password"])
	if err != nil || gen != "5" || next == pw || len(next) != 52 {
		t.Fatalf("RotateBegin: %v, generation %q", err, gen)
	}
	if u := v.user(extName); len(u.hashes) != 2 || !u.hasPassword(pw) || !u.hasPassword(next) {
		t.Errorf("passwords: %d", len(u.hashes))
	}
	v.want(t, "SetUser "+extName+" resetpass #"+valkeyacl.HashPassword(pw)+" >"+next)

	// A retry before the Secret was written (it still holds the old password)
	// keeps every password the user has beside another new one: the password of
	// the failed attempt stays until RotateFinish, which keeps only the Secret's.
	bindingWith(e, extName, pw)
	data, _, err = e.RotateBegin(ctx, extSpec, userState{}, st)
	again := string(data["password"])
	if u := v.user(extName); err != nil || again == next || len(u.hashes) != 3 || !u.hasPassword(next) || !u.hasPassword(again) || !u.hasPassword(pw) {
		t.Errorf("retry: %v, passwords %d", err, len(v.user(extName).hashes))
	}
	v.want(t, "SetUser "+extName+" resetpass #")

	// A retry after the Secret was written but before it was marked: the Secret
	// holds the password of the first attempt, and the one the consumers may still
	// hold is only on the server. Nothing is dropped.
	e, v, pw = created(t)
	bindingWith(e, extName, pw)
	data, _, err = e.RotateBegin(ctx, extSpec, userState{}, st)
	if err != nil {
		t.Fatal(err)
	}
	first := string(data["password"])
	bindingWith(e, extName, first)
	data, _, err = e.RotateBegin(ctx, extSpec, userState{}, st)
	second := string(data["password"])
	if u := v.user(extName); err != nil || len(u.hashes) != 3 || !u.hasPassword(pw) || !u.hasPassword(first) || !u.hasPassword(second) {
		t.Fatalf("retry after the write: %v, passwords %d", err, len(v.user(extName).hashes))
	}
	// The rotation then finishes with the Secret holding the last one.
	bindingWith(e, extName, second)
	if err := e.RotateFinish(ctx, extSpec, userState{}, st); err != nil {
		t.Fatal(err)
	}
	if u := v.user(extName); len(u.hashes) != 1 || !u.hasPassword(second) {
		t.Errorf("after RotateFinish: %d passwords", len(u.hashes))
	}

	// With no binding Secret (or one of another user) there is no current password:
	// the new one is only added.
	for _, tc := range []struct {
		name string
		bind func(*userExternal)
	}{
		{"missing", func(*userExternal) {}},
		{"another user", func(e *userExternal) { bindingWith(e, "x", "p") }},
	} {
		e, v, pw := created(t)
		tc.bind(e)
		data, _, err := e.RotateBegin(ctx, extSpec, userState{}, st)
		if u := v.user(extName); err != nil || len(u.hashes) != 2 || !u.hasPassword(pw) || !u.hasPassword(string(data["password"])) {
			t.Errorf("%s: %v", tc.name, err)
		}
		v.want(t, "SetUser "+extName+" >")
		v.wantNot(t, "resetpass")
	}

	// Failures.
	e, v, _ = created(t)
	e.binding = func(context.Context) (*corev1.Secret, error) { return nil, errServer }
	if _, _, err := e.RotateBegin(ctx, extSpec, userState{}, st); !errors.Is(err, errServer) {
		t.Errorf("binding: %v", err)
	}
	bindingWith(e, extName, "p")
	v.fail["GetUser"] = errServer
	if _, _, err := e.RotateBegin(ctx, extSpec, userState{}, st); !errors.Is(err, errServer) {
		t.Errorf("GetUser: %v", err)
	}
	v.fail = map[string]error{}
	e.binding = func(context.Context) (*corev1.Secret, error) { return nil, nil }
	v.fail["SetUser"] = errServer
	if _, _, err := e.RotateBegin(ctx, extSpec, userState{}, st); !errors.Is(err, errServer) {
		t.Errorf("SetUser: %v", err)
	}
	e.be.server = func(context.Context) (Valkey, error) { return nil, errServer }
	if _, _, err := e.RotateBegin(ctx, extSpec, userState{}, st); !errors.Is(err, errServer) {
		t.Errorf("connect: %v", err)
	}
}

func TestUserRotateFinish(t *testing.T) {
	ctx := context.Background()
	st := lifecycle.RotationState{Active: "1"}

	// The previous password goes, the one of the binding stays.
	e, v, old := created(t)
	bindingWith(e, extName, old)
	data, _, err := e.RotateBegin(ctx, extSpec, userState{}, st)
	if err != nil {
		t.Fatal(err)
	}
	next := string(data["password"])
	bindingWith(e, extName, next)
	v.reset()
	if err := e.RotateFinish(ctx, extSpec, userState{}, st); err != nil {
		t.Fatal(err)
	}
	if u := v.user(extName); len(u.hashes) != 1 || !u.hasPassword(next) || u.hasPassword(old) {
		t.Errorf("passwords: %d", len(u.hashes))
	}
	v.want(t, "SetUser "+extName+" resetpass #"+valkeyacl.HashPassword(next))

	// The binding's password is not one of the user's (or there is none): the
	// user keeps what it has, because "resetpass" would leave it with nothing.
	for _, tc := range []struct {
		name string
		bind func(*userExternal)
	}{
		{"another password", func(e *userExternal) { bindingWith(e, extName, "not-on-the-server") }},
		{"another user", func(e *userExternal) { bindingWith(e, "x", next) }},
		{"no Secret", func(e *userExternal) { e.binding = func(context.Context) (*corev1.Secret, error) { return nil, nil } }},
	} {
		tc.bind(e)
		v.reset()
		if err := e.RotateFinish(ctx, extSpec, userState{}, st); !errors.Is(err, ErrPasswordNotOnServer) {
			t.Errorf("%s: %v", tc.name, err)
		}
		v.wantNot(t, "SetUser")
		if u := v.user(extName); len(u.hashes) != 1 || !u.hasPassword(next) {
			t.Errorf("%s: the user's passwords changed", tc.name)
		}
	}

	// Failures.
	e.binding = func(context.Context) (*corev1.Secret, error) { return nil, errServer }
	if err := e.RotateFinish(ctx, extSpec, userState{}, st); !errors.Is(err, errServer) {
		t.Errorf("binding: %v", err)
	}
	bindingWith(e, extName, next)
	v.fail["GetUser"] = errServer
	if err := e.RotateFinish(ctx, extSpec, userState{}, st); !errors.Is(err, errServer) {
		t.Errorf("GetUser: %v", err)
	}
	v.fail = map[string]error{"SetUser": errServer}
	if err := e.RotateFinish(ctx, extSpec, userState{}, st); !errors.Is(err, errServer) {
		t.Errorf("SetUser: %v", err)
	}
	e.be.server = func(context.Context) (Valkey, error) { return nil, errServer }
	if err := e.RotateFinish(ctx, extSpec, userState{}, st); !errors.Is(err, errServer) {
		t.Errorf("connect: %v", err)
	}
}

func TestUserDataCarriesTheServersFields(t *testing.T) {
	e := userExt(t, newMemValkey())
	e.be.cfg.Port = new(int32)
	*e.be.cfg.Port = 6380
	e.be.conn.CABundle = []byte("pem")
	data := e.data("pw")
	for k, want := range map[string]string{
		"host": "valkey.example.test", "port": "6380", "uri": "rediss://valkey.example.test:6380", "username": extName,
		"password": "pw", "keyPrefix": "tenant:", "sslrootcert": "pem",
	} {
		if string(data[k]) != want {
			t.Errorf("%s = %q, want %q", k, data[k], want)
		}
	}
	if strings.Contains(string(data["uri"]), "pw") {
		t.Error("the URI carries the password")
	}
}
