package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/craftainer-labs/lib-operator-go/pkg/apis/common"
	"github.com/craftainer-labs/lib-operator-go/pkg/cloud"
	"github.com/craftainer-labs/lib-operator-go/pkg/conditions"
	"github.com/craftainer-labs/lib-operator-go/pkg/lifecycle"
	"github.com/craftainer-labs/lib-operator-go/pkg/server"
	"github.com/craftainer-labs/lib-operator-go/pkg/testenv"

	cachev1alpha1 "github.com/craftainer-labs/operator-cache/api/v1alpha1"
	"github.com/craftainer-labs/operator-cache/internal/awscache"
	"github.com/craftainer-labs/operator-cache/internal/valkeyacl"
)

var awsID = awscache.UserID("tenant", "app")

// awsExt is the External of a claim of namespace "tenant" on an AWS server
// reached through api, with no API server behind it.
func awsExt(t *testing.T, api *memAWS, mutate func(*cachev1alpha1.CacheUser)) (lifecycle.External[cachev1alpha1.CacheUserSpec, userState], *awsUser) {
	t.Helper()
	claim := &cachev1alpha1.CacheUser{ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "tenant", UID: "uid-1"}}
	if mutate != nil {
		mutate(claim)
	}
	be := &backend{aws: &awsBackend{
		region: "eu-west-1", host: "cache.example.test", port: "6379",
		build: func(context.Context) (awscache.Client, error) { return api, nil },
	}}
	ext, x := newAWSExternal(be, claim)
	x.persist = func(context.Context) error { return nil }
	x.binding = func(context.Context) (*corev1.Secret, error) { return nil, nil }
	return ext, x
}

func asIAM(c *cachev1alpha1.CacheUser) { c.Spec.AuthMode = cachev1alpha1.AuthModeIAM }

// bindingOf makes the binding read return a Secret of the user with the password.
func bindingOf(x *awsUser, password string) {
	x.binding = func(context.Context) (*corev1.Secret, error) {
		return &corev1.Secret{Data: map[string][]byte{"username": []byte(awsID), "password": []byte(password)}}, nil
	}
}

// activeUser puts a settled user of the claim, in the group, on the server.
func activeUser(t *testing.T, api *memAWS, x *awsUser, passwords ...string) *memAWSUser {
	t.Helper()
	want, err := x.access(extSpec)
	if err != nil {
		t.Fatal(err)
	}
	u := &memAWSUser{arn: "arn:user", status: awscache.StatusActive, access: want, marker: x.marker(), passwords: passwords}
	api.users[awsID] = u
	api.member[awsID] = true
	return u
}

func isPending(err error, reason string) bool {
	var p *lifecycle.PendingError
	return errors.As(err, &p) && p.Reason == reason && p.After == pendingAfter
}

func TestNewAWSExternal(t *testing.T) {
	api := newMemAWS()
	ext, x := awsExt(t, api, nil)
	if _, ok := ext.(awsPassword); !ok || x.iam || x.id != awsID || x.claim.GetExternalName() != awsID {
		t.Errorf("password external = %T %+v", ext, x)
	}
	if _, ok := ext.(lifecycle.Rotator[cachev1alpha1.CacheUserSpec, userState]); !ok {
		t.Error("a password claim is not a Rotator")
	}
	// The recorded name is kept, whatever the claim is called now.
	_, x = awsExt(t, api, func(c *cachev1alpha1.CacheUser) { c.SetExternalName("kept") })
	if x.id != "kept" {
		t.Errorf("id = %q", x.id)
	}
	ext, x = awsExt(t, api, asIAM)
	if _, ok := ext.(awsIAM); !ok || !x.iam {
		t.Errorf("IAM external = %T", ext)
	}
	if _, ok := ext.(lifecycle.Rotator[cachev1alpha1.CacheUserSpec, userState]); ok {
		t.Error("an IAM claim is a Rotator")
	}
	if !ext.(awsIAM).RotationNotApplicable() {
		t.Error("an IAM login has something to rotate")
	}
}

func TestAWSAccess(t *testing.T) {
	_, x := awsExt(t, newMemAWS(), nil)
	got, err := x.access(cachev1alpha1.CacheUserSpec{Profile: cachev1alpha1.ProfileReadOnly, Channels: true})
	if err != nil || !strings.HasPrefix(got, "on %R~tenant:* &tenant:* -@all +@connection +@read +@pubsub -@dangerous") ||
		strings.Contains(got, "reset") || strings.Contains(got, "clearselectors") {
		t.Errorf("access = %q, %v", got, err)
	}
	if _, err = x.access(cachev1alpha1.CacheUserSpec{Profile: "Admin"}); !errors.Is(err, ErrInvalidSpec) {
		t.Errorf("profile: %v", err)
	}
	x.claim.Namespace = "Not_A-Label*"
	if _, err = x.access(extSpec); !errors.Is(err, ErrInvalidSpec) || !errors.Is(err, valkeyacl.ErrInvalid) {
		t.Errorf("namespace: %v", err)
	}
}

func TestPendingOf(t *testing.T) {
	active := awscache.User{Status: awscache.StatusActive, GroupStatus: awscache.StatusActive}
	if pendingOf(active) != nil {
		t.Error("an active user is pending")
	}
	for _, tc := range []struct {
		mutate func(*awscache.User)
		reason string
	}{
		{func(u *awscache.User) { u.Status = awscache.StatusDeleting }, conditions.Deleting},
		{func(u *awscache.User) { u.Status = awscache.StatusModifying }, conditions.Updating},
		{func(u *awscache.User) { u.Status = "creating" }, conditions.Creating},
		{func(u *awscache.User) { u.GroupStatus = awscache.StatusModifying }, conditions.Updating},
	} {
		u := active
		tc.mutate(&u)
		if err := pendingOf(u); !isPending(err, tc.reason) {
			t.Errorf("%+v: %v", u, err)
		}
	}
}

func TestSameAccess(t *testing.T) {
	if !sameAccess("on ~a:* +@read", "+@read   on ~a:*") || sameAccess("on ~a:*", "on ~b:*") || sameAccess("on ~a:*", "on ~a:* +@read") {
		t.Error("sameAccess")
	}
}

func TestAWSObserve(t *testing.T) {
	ctx := context.Background()

	t.Run("nothing there", func(t *testing.T) {
		api := newMemAWS()
		ext, _ := awsExt(t, api, nil)
		if obs, err := ext.Observe(ctx, extSpec); err != nil || obs.Exists {
			t.Errorf("observe: %+v, %v", obs, err)
		}
	})

	t.Run("failures", func(t *testing.T) {
		api := newMemAWS()
		ext, x := awsExt(t, api, nil)
		x.pre = ErrServerNotReady
		if _, err := ext.Observe(ctx, extSpec); !errors.Is(err, ErrServerNotReady) || api.joined() != "" {
			t.Errorf("pre: %v, calls %q", err, api.joined())
		}
		x.pre = nil
		errBuild := errors.New("build failed")
		x.be.aws.build = func(context.Context) (awscache.Client, error) { return nil, errBuild }
		if _, err := ext.Observe(ctx, extSpec); !errors.Is(err, errBuild) {
			t.Errorf("build: %v", err)
		}
		x.be.aws.build = func(context.Context) (awscache.Client, error) { return api, nil }
		api.fail["Lookup"] = errServer
		if _, err := ext.Observe(ctx, extSpec); !errors.Is(err, errServer) {
			t.Errorf("lookup: %v", err)
		}
	})

	t.Run("another claim's user", func(t *testing.T) {
		api := newMemAWS()
		ext, x := awsExt(t, api, nil)
		activeUser(t, api, x).marker = "tenant/other/uid-2"
		if _, err := ext.Observe(ctx, extSpec); !errors.Is(err, lifecycle.ErrNameCollision) {
			t.Errorf("a user with another marker: %v", err)
		}
		api.users[awsID].marker = ""
		if _, err := ext.Observe(ctx, extSpec); !errors.Is(err, lifecycle.ErrNameCollision) {
			t.Errorf("a user with no marker: %v", err)
		}
	})

	t.Run("a user that has not settled", func(t *testing.T) {
		api := newMemAWS()
		ext, x := awsExt(t, api, nil)
		u := activeUser(t, api, x, "p")
		u.status = awscache.StatusModifying
		if _, err := ext.Observe(ctx, extSpec); !isPending(err, conditions.Updating) {
			t.Errorf("modifying: %v", err)
		}
	})

	t.Run("up to date", func(t *testing.T) {
		api := newMemAWS()
		ext, x := awsExt(t, api, nil)
		bindingOf(x, "p")
		u := activeUser(t, api, x, "p")
		obs, err := ext.Observe(ctx, extSpec)
		if err != nil || !obs.Exists || !obs.UpToDate || obs.Current.aws.ARN != "arn:user" {
			t.Errorf("observe: %+v, %v", obs, err)
		}
		// AWS may return the rules in another order.
		rules := strings.Fields(u.access)
		slices.Reverse(rules)
		u.access = strings.Join(rules, " ")
		if obs, err = ext.Observe(ctx, extSpec); err != nil || !obs.UpToDate {
			t.Errorf("a reordered access string: %+v, %v", obs, err)
		}
	})

	t.Run("not up to date", func(t *testing.T) {
		api := newMemAWS()
		ext, x := awsExt(t, api, nil)
		bindingOf(x, "p")
		u := activeUser(t, api, x, "p")
		for name, mutate := range map[string]func(){
			"another access string": func() { u.access = "on ~other:*" },
			"not in the group":      func() { api.member[awsID] = false },
		} {
			obs, err := ext.Observe(ctx, extSpec)
			if err != nil || !obs.Exists || !obs.UpToDate {
				t.Fatalf("before %s: %+v, %v", name, obs, err)
			}
			old := *u
			mutate()
			if obs, err = ext.Observe(ctx, extSpec); err != nil || !obs.Exists || obs.UpToDate {
				t.Errorf("%s: %+v, %v", name, obs, err)
			}
			*u = old
			api.member[awsID] = true
		}
		// A spec the rules refuse is never up to date, and Observe still answers.
		if obs, err := ext.Observe(ctx, cachev1alpha1.CacheUserSpec{Profile: "Admin"}); err != nil || !obs.Exists || obs.UpToDate {
			t.Errorf("an invalid spec: %+v, %v", obs, err)
		}
	})

	t.Run("passwords", func(t *testing.T) {
		api := newMemAWS()
		ext, x := awsExt(t, api, nil)
		bindingOf(x, "p")
		activeUser(t, api, x, "p", "rogue")
		// Two passwords and no rotation: one is not the operator's.
		if obs, err := ext.Observe(ctx, extSpec); err != nil || obs.UpToDate {
			t.Errorf("extra password: %+v, %v", obs, err)
		}
		// In a rotation's grace period two are expected.
		x.claim.Status.Rotation = &common.RotationStatus{Previous: "1"}
		if obs, err := ext.Observe(ctx, extSpec); err != nil || !obs.UpToDate {
			t.Errorf("a rotation in flight: %+v, %v", obs, err)
		}
		// With no usable Secret nothing says which is extra.
		x.claim.Status.Rotation = nil
		x.binding = func(context.Context) (*corev1.Secret, error) { return nil, nil }
		if obs, err := ext.Observe(ctx, extSpec); err != nil || !obs.UpToDate {
			t.Errorf("no Secret: %+v, %v", obs, err)
		}
		x.binding = func(context.Context) (*corev1.Secret, error) { return nil, errServer }
		if _, err := ext.Observe(ctx, extSpec); !errors.Is(err, errServer) {
			t.Errorf("binding read: %v", err)
		}
	})

	t.Run("an IAM user holds no passwords to compare", func(t *testing.T) {
		api := newMemAWS()
		ext, x := awsExt(t, api, asIAM)
		x.binding = func(context.Context) (*corev1.Secret, error) { return nil, errServer }
		activeUser(t, api, x)
		if obs, err := ext.Observe(ctx, extSpec); err != nil || !obs.UpToDate {
			t.Errorf("observe: %+v, %v", obs, err)
		}
	})
}

func TestAWSData(t *testing.T) {
	api := newMemAWS()
	_, x := awsExt(t, api, nil)
	x.be.conn.CABundle = []byte("pem")
	d, err := x.data("pw", "")
	got := map[string]string{}
	for k, v := range d {
		got[k] = string(v)
	}
	want := map[string]string{
		"host": "cache.example.test", "port": "6379", "uri": "rediss://cache.example.test:6379", "username": awsID,
		"keyPrefix": "tenant:", "cloud": "aws", "region": "eu-west-1", "authMode": "Password", "sslrootcert": "pem", "password": "pw",
	}
	if err != nil || fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("data = %v, %v", got, err)
	}
	_, x = awsExt(t, api, asIAM)
	d, err = x.data("", "arn:user")
	if err != nil || string(d["iamResourceArn"]) != "arn:user" || string(d["authMode"]) != "IAM" || d["password"] != nil || d["sslrootcert"] != nil {
		t.Errorf("IAM data = %v, %v", d, err)
	}
	x.be.aws.host = ""
	if _, err = x.data("", ""); !errors.Is(err, ErrAddressUnknown) {
		t.Errorf("no address: %v", err)
	}
}

func TestAWSCreate(t *testing.T) {
	ctx := context.Background()

	t.Run("a password user", func(t *testing.T) {
		api := newMemAWS()
		ext, x := awsExt(t, api, nil)
		data, err := ext.Create(ctx, extSpec)
		if err != nil || len(data["password"]) != 52 || string(data["username"]) != awsID {
			t.Fatalf("create: %v, %v", data, err)
		}
		if x.claim.GetExternalID() != "arn:aws:elasticache:eu-west-1:111122223333:user:"+awsID || !api.member[awsID] {
			t.Errorf("externalId = %q, member = %v", x.claim.GetExternalID(), api.member[awsID])
		}
		s := api.created[0]
		if s.Marker != "tenant/app/uid-1" || s.IAM || len(s.Passwords) != 1 || s.Passwords[0] != string(data["password"]) ||
			!strings.Contains(s.AccessString, "~tenant:*") {
			t.Errorf("spec = %+v", s)
		}
	})

	t.Run("an IAM user", func(t *testing.T) {
		api := newMemAWS()
		ext, x := awsExt(t, api, asIAM)
		data, err := ext.Create(ctx, extSpec)
		if err != nil || data["password"] != nil || string(data["iamResourceArn"]) != x.claim.GetExternalID() || !api.created[0].IAM || api.created[0].Passwords != nil {
			t.Errorf("create: %v, %v, %+v", data, err, api.created)
		}
	})

	t.Run("a group that is not ready for the user", func(t *testing.T) {
		api := newMemAWS()
		api.fail["Join"] = errServer
		ext, _ := awsExt(t, api, nil)
		if data, err := ext.Create(ctx, extSpec); err != nil || data["password"] == nil {
			t.Errorf("the password is lost with the join: %v, %v", data, err)
		}
	})

	t.Run("failures", func(t *testing.T) {
		api := newMemAWS()
		ext, x := awsExt(t, api, nil)
		if _, err := ext.Create(ctx, cachev1alpha1.CacheUserSpec{Profile: "Admin"}); !errors.Is(err, ErrInvalidSpec) || api.joined() != "" {
			t.Errorf("invalid spec: %v", err)
		}
		x.be.aws.host = ""
		if _, err := ext.Create(ctx, extSpec); !errors.Is(err, ErrAddressUnknown) || api.joined() != "" {
			t.Errorf("no address: %v", err)
		}
		x.be.aws.host = "cache.example.test"
		errBuild := errors.New("build failed")
		x.be.aws.build = func(context.Context) (awscache.Client, error) { return nil, errBuild }
		if _, err := ext.Create(ctx, extSpec); !errors.Is(err, errBuild) {
			t.Errorf("build: %v", err)
		}
		x.be.aws.build = func(context.Context) (awscache.Client, error) { return api, nil }
		x.persist = func(context.Context) error { return errServer }
		if _, err := ext.Create(ctx, extSpec); !errors.Is(err, errServer) || api.joined() != "" {
			t.Errorf("persist: %v", err)
		}
		x.persist = func(context.Context) error { return nil }
		api.fail["Create"] = errServer
		if _, err := ext.Create(ctx, extSpec); !errors.Is(err, errServer) || x.claim.GetExternalID() != createIssued {
			t.Errorf("create: %v, externalId %q", err, x.claim.GetExternalID())
		}
	})
}

func TestAWSUpdate(t *testing.T) {
	ctx := context.Background()

	t.Run("joins the group first", func(t *testing.T) {
		api := newMemAWS()
		ext, x := awsExt(t, api, nil)
		activeUser(t, api, x, "p")
		_, err := ext.Update(ctx, extSpec, userState{aws: awscache.User{Member: false}})
		if !isPending(err, conditions.Updating) || !api.member[awsID] || len(api.modified) != 0 {
			t.Errorf("update: %v, member %v, modified %v", err, api.member[awsID], api.modified)
		}
		api.fail["Join"] = errServer
		if _, err = ext.Update(ctx, extSpec, userState{}); !errors.Is(err, errServer) {
			t.Errorf("join failure: %v", err)
		}
	})

	t.Run("sets the access string", func(t *testing.T) {
		api := newMemAWS()
		ext, x := awsExt(t, api, nil)
		bindingOf(x, "p")
		activeUser(t, api, x, "p")
		data, err := ext.Update(ctx, cachev1alpha1.CacheUserSpec{Profile: cachev1alpha1.ProfileReadOnly}, userState{aws: awscache.User{Member: true, Passwords: 1}})
		if data != nil || !isPending(err, conditions.Updating) || len(api.modified) != 1 || api.modified[0].Passwords != nil ||
			!strings.Contains(api.modified[0].AccessString, "%R~tenant:*") {
			t.Errorf("update: %v, %v, %+v", data, err, api.modified)
		}
	})

	t.Run("removes passwords the operator did not issue", func(t *testing.T) {
		api := newMemAWS()
		ext, x := awsExt(t, api, nil)
		rec := &testenv.Recorder{}
		x.rec = rec
		bindingOf(x, "p")
		activeUser(t, api, x, "p", "rogue")
		_, err := ext.Update(ctx, extSpec, userState{aws: awscache.User{Member: true, Passwords: 2}})
		if !isPending(err, conditions.Updating) || len(api.modified) != 1 || !slices.Equal(api.modified[0].Passwords, []string{"p"}) {
			t.Errorf("update: %v, %+v", err, api.modified)
		}
		rec.AssertEvent(t, corev1.EventTypeWarning, "PasswordDrift", "removed")
		rec.AssertNoSecret(t, "p", "rogue")
	})

	t.Run("an IAM user has no passwords", func(t *testing.T) {
		api := newMemAWS()
		ext, x := awsExt(t, api, asIAM)
		x.binding = func(context.Context) (*corev1.Secret, error) { return nil, errServer }
		activeUser(t, api, x)
		if _, err := ext.Update(ctx, extSpec, userState{aws: awscache.User{Member: true}}); !isPending(err, conditions.Updating) || api.modified[0].Passwords != nil {
			t.Errorf("update: %v, %+v", err, api.modified)
		}
	})

	t.Run("failures", func(t *testing.T) {
		api := newMemAWS()
		ext, x := awsExt(t, api, nil)
		member := userState{aws: awscache.User{Member: true}}
		if _, err := ext.Update(ctx, cachev1alpha1.CacheUserSpec{Profile: "Admin"}, member); !errors.Is(err, ErrInvalidSpec) {
			t.Errorf("invalid spec: %v", err)
		}
		errBuild := errors.New("build failed")
		x.be.aws.build = func(context.Context) (awscache.Client, error) { return nil, errBuild }
		if _, err := ext.Update(ctx, extSpec, member); !errors.Is(err, errBuild) {
			t.Errorf("build: %v", err)
		}
		x.be.aws.build = func(context.Context) (awscache.Client, error) { return api, nil }
		x.binding = func(context.Context) (*corev1.Secret, error) { return nil, errServer }
		if _, err := ext.Update(ctx, extSpec, member); !errors.Is(err, errServer) {
			t.Errorf("binding: %v", err)
		}
		x.binding = func(context.Context) (*corev1.Secret, error) { return nil, nil }
		api.fail["Modify"] = errServer
		if _, err := ext.Update(ctx, extSpec, member); !errors.Is(err, errServer) {
			t.Errorf("modify: %v", err)
		}
	})
}

func TestAWSDelete(t *testing.T) {
	ctx := context.Background()
	api := newMemAWS()
	ext, x := awsExt(t, api, nil)
	activeUser(t, api, x, "p")
	if err := ext.Delete(ctx, userState{}); !isPending(err, conditions.Deleting) || api.users[awsID].status != awscache.StatusDeleting {
		t.Errorf("delete: %v", err)
	}
	api.fail["Delete"] = errServer
	if err := ext.Delete(ctx, userState{}); !errors.Is(err, errServer) {
		t.Errorf("delete failure: %v", err)
	}
	errBuild := errors.New("build failed")
	x.be.aws.client, x.be.aws.build = nil, func(context.Context) (awscache.Client, error) { return nil, errBuild }
	if err := ext.Delete(ctx, userState{}); !errors.Is(err, errBuild) {
		t.Errorf("build: %v", err)
	}
}

func TestAWSRotation(t *testing.T) {
	ctx := context.Background()
	rotate := func(ext lifecycle.External[cachev1alpha1.CacheUserSpec, userState]) (map[string][]byte, lifecycle.Generation, error) {
		return ext.(lifecycle.Rotator[cachev1alpha1.CacheUserSpec, userState]).RotateBegin(ctx, extSpec, userState{}, lifecycle.RotationState{Sequence: 3})
	}
	finish := func(ext lifecycle.External[cachev1alpha1.CacheUserSpec, userState]) error {
		return ext.(lifecycle.Rotator[cachev1alpha1.CacheUserSpec, userState]).RotateFinish(ctx, extSpec, userState{}, lifecycle.RotationState{})
	}

	t.Run("begin keeps the current password beside a new one", func(t *testing.T) {
		api := newMemAWS()
		ext, x := awsExt(t, api, nil)
		bindingOf(x, "current")
		activeUser(t, api, x, "current")
		data, gen, err := rotate(ext)
		if err != nil || gen != "4" || len(api.modified) != 1 || len(api.modified[0].Passwords) != 2 ||
			api.modified[0].Passwords[0] != "current" || api.modified[0].Passwords[1] != string(data["password"]) {
			t.Errorf("begin: %v, %q, %v, %+v", data, gen, err, api.modified)
		}
	})

	t.Run("begin without a usable Secret keeps nothing", func(t *testing.T) {
		api := newMemAWS()
		ext, x := awsExt(t, api, nil)
		activeUser(t, api, x, "lost")
		data, _, err := rotate(ext)
		if err != nil || len(api.modified[0].Passwords) != 1 || api.modified[0].Passwords[0] != string(data["password"]) {
			t.Errorf("begin: %v, %v, %+v", data, err, api.modified)
		}
	})

	t.Run("begin failures", func(t *testing.T) {
		api := newMemAWS()
		ext, x := awsExt(t, api, nil)
		x.be.aws.host = ""
		if _, _, err := rotate(ext); !errors.Is(err, ErrAddressUnknown) {
			t.Errorf("no address: %v", err)
		}
		x.be.aws.host = "cache.example.test"
		errBuild := errors.New("build failed")
		x.be.aws.build = func(context.Context) (awscache.Client, error) { return nil, errBuild }
		if _, _, err := rotate(ext); !errors.Is(err, errBuild) {
			t.Errorf("build: %v", err)
		}
		x.be.aws.build = func(context.Context) (awscache.Client, error) { return api, nil }
		x.binding = func(context.Context) (*corev1.Secret, error) { return nil, errServer }
		if _, _, err := rotate(ext); !errors.Is(err, errServer) {
			t.Errorf("binding: %v", err)
		}
		x.binding = func(context.Context) (*corev1.Secret, error) { return nil, nil }
		activeUser(t, api, x, "p")
		api.fail["Modify"] = errServer
		if _, _, err := rotate(ext); !errors.Is(err, errServer) {
			t.Errorf("modify: %v", err)
		}
	})

	t.Run("finish keeps only the Secret's password", func(t *testing.T) {
		api := newMemAWS()
		ext, x := awsExt(t, api, nil)
		bindingOf(x, "current")
		activeUser(t, api, x, "old", "current")
		if err := finish(ext); err != nil || !slices.Equal(api.users[awsID].passwords, []string{"current"}) {
			t.Errorf("finish: %v, %v", err, api.users[awsID].passwords)
		}
	})

	t.Run("finish failures", func(t *testing.T) {
		api := newMemAWS()
		ext, x := awsExt(t, api, nil)
		activeUser(t, api, x, "old", "current")
		// No password in the Secret: the user would be left with none.
		if err := finish(ext); !errors.Is(err, ErrPasswordNotOnServer) || len(api.modified) != 0 {
			t.Errorf("no password: %v", err)
		}
		x.binding = func(context.Context) (*corev1.Secret, error) { return nil, errServer }
		if err := finish(ext); !errors.Is(err, errServer) {
			t.Errorf("binding: %v", err)
		}
		errBuild := errors.New("build failed")
		x.be.aws.build = func(context.Context) (awscache.Client, error) { return nil, errBuild }
		if err := finish(ext); !errors.Is(err, errBuild) {
			t.Errorf("build: %v", err)
		}
	})
}

func TestAWSRebuildBinding(t *testing.T) {
	api := newMemAWS()
	ext, _ := awsExt(t, api, asIAM)
	rebuilder := ext.(lifecycle.BindingRebuilder[cachev1alpha1.CacheUserSpec, userState])
	data, err := rebuilder.RebuildBinding(context.Background(), extSpec, userState{aws: awscache.User{ARN: "arn:user"}})
	if err != nil || string(data["iamResourceArn"]) != "arn:user" || data["password"] != nil {
		t.Errorf("rebuild: %v, %v", data, err)
	}
	if api.joined() != "" {
		t.Errorf("a rebuild called the cloud: %q", api.joined())
	}
}

// The password flow end to end: create, wait for AWS, rotate, change, delete.
func TestAWSUserLifecycleAndRotation(t *testing.T) {
	ctx := context.Background()
	h, api := newAWSHarness(t, nil)
	r := &UserReconciler{h.cfg}
	grace := metav1.Duration{Duration: time.Hour}
	u := h.user("app", func(u *cachev1alpha1.CacheUser) {
		u.Spec.RotationPolicy = &common.RotationPolicy{GracePeriod: &grace}
	})
	id := awscache.UserID(h.ns, "app")
	arn := "arn:aws:elasticache:eu-west-1:111122223333:user:" + id
	step := func() {
		t.Helper()
		api.settle()
		api.reset()
		h.reconcileOK(r, u)
		h.get(u)
	}

	// Create: the user, then the group, and the binding with the password.
	h.reconcileOK(r, u)
	h.get(u)
	if u.Status.ExternalName != id || u.Status.ExternalID != arn || u.Status.Binding == nil {
		t.Errorf("status = %+v", u.Status)
	}
	api.want(t, "Lookup "+id, "Create "+id, "Join "+id)
	data := h.bindingSecret(u)
	first := data["password"]
	if data["username"] != id || len(first) != 52 || data["host"] != "cache.example.test" || data["port"] != "6379" ||
		data["uri"] != "rediss://cache.example.test:6379" || data["keyPrefix"] != h.ns+":" || data["cloud"] != "aws" ||
		data["region"] != "eu-west-1" || data["authMode"] != "Password" || data["iamResourceArn"] != "" ||
		data["type"] != "redis" || data["provider"] != "craftainer.dev" {
		t.Errorf("binding = %v", data)
	}
	if s := api.created[0]; s.Marker != awscache.MarkerValue(h.ns, "app", string(u.UID)) || len(s.Passwords) != 1 || s.Passwords[0] != first {
		t.Errorf("created = %+v", s)
	}
	h.rec.AssertNoSecret(t, first)

	// AWS has not finished: the claim is pending, not failed.
	api.reset()
	h.reconcileOK(r, u)
	h.get(u)
	h.wantCondition(u.Status.Conditions, conditions.Ready, metav1.ConditionFalse, conditions.Creating)
	h.wantCondition(u.Status.Conditions, conditions.Synced, metav1.ConditionTrue, conditions.Creating)
	api.noWrites(t)

	// Settled: nothing is written, and the Secret is kept.
	step()
	h.wantCondition(u.Status.Conditions, conditions.Ready, metav1.ConditionTrue, conditions.Available)
	api.noWrites(t)
	if h.bindingSecret(u)["password"] != first {
		t.Error("the password changed without a reason")
	}

	// Rotation: the user holds both passwords.
	h.patch(u, func() { u.Annotations = map[string]string{lifecycle.AnnotationRotate: "1"} })
	step()
	second := h.bindingSecret(u)["password"]
	if got := api.modified[len(api.modified)-1].Passwords; second == first || !slices.Equal(got, []string{first, second}) {
		t.Errorf("passwords after RotateBegin: %v", got)
	}
	if u.Status.Rotation == nil || u.Status.Rotation.Active != "1" || u.Status.ExternalID != arn {
		t.Fatalf("rotation = %+v, externalId = %q", u.Status.Rotation, u.Status.ExternalID)
	}

	// During the grace period two passwords are expected: nothing is written.
	step()
	api.noWrites(t)
	h.wantCondition(u.Status.Conditions, conditions.Ready, metav1.ConditionTrue, conditions.Available)

	// After it only the binding's password is kept.
	h.now = h.now.Add(2 * time.Hour)
	step()
	if got := api.modified[len(api.modified)-1].Passwords; !slices.Equal(got, []string{second}) {
		t.Errorf("passwords after RotateFinish: %v", got)
	}
	h.rec.AssertEvent(t, "Normal", conditions.EventRotationFinished, "")

	// A change of the profile is one step, then pending, and keeps the password.
	h.patch(u, func() { u.Spec.Profile = cachev1alpha1.ProfileReadOnly })
	step()
	h.wantCondition(u.Status.Conditions, conditions.Ready, metav1.ConditionFalse, conditions.Updating)
	h.wantCondition(u.Status.Conditions, conditions.Synced, metav1.ConditionTrue, conditions.Updating)
	if got := api.user(id); !strings.Contains(got.access, "%R~"+h.ns+":*") || h.bindingSecret(u)["password"] != second {
		t.Errorf("user after the profile change = %+v", got)
	}
	step()
	h.wantCondition(u.Status.Conditions, conditions.Ready, metav1.ConditionTrue, conditions.Available)
	api.noWrites(t)

	// A user that left its group joins it again.
	api.member[id] = false
	step()
	api.want(t, "Join "+id)
	h.wantCondition(u.Status.Conditions, conditions.Ready, metav1.ConditionFalse, conditions.Updating)
	step()
	h.wantCondition(u.Status.Conditions, conditions.Ready, metav1.ConditionTrue, conditions.Available)

	// Deleting asks AWS, and the claim stays until the user is gone.
	h.must(env.Client.Delete(ctx, u))
	api.settle()
	api.reset()
	h.reconcileOK(r, u)
	api.want(t, "Delete "+id)
	h.get(u)
	h.wantCondition(u.Status.Conditions, conditions.Ready, metav1.ConditionFalse, conditions.Deleting)
	h.reconcileOK(r, u)
	if h.gone(u) {
		t.Fatal("the claim went while AWS was still deleting the user")
	}
	api.settle()
	h.reconcileOK(r, u)
	if !h.gone(u) || api.user(id) != nil {
		t.Errorf("the claim or its user is still there: %v", api.users)
	}
}

// IAM mode: no password, no rotation, a binding that can be rebuilt.
func TestAWSUserIAMMode(t *testing.T) {
	ctx := context.Background()
	h, api := newAWSHarness(t, nil)
	r := &UserReconciler{h.cfg}
	u := h.user("iam", func(u *cachev1alpha1.CacheUser) { u.Spec.AuthMode = cachev1alpha1.AuthModeIAM })
	id := awscache.UserID(h.ns, "iam")
	arn := "arn:aws:elasticache:eu-west-1:111122223333:user:" + id
	step := func() {
		t.Helper()
		api.settle()
		api.reset()
		h.reconcileOK(r, u)
		h.get(u)
	}

	h.reconcileOK(r, u)
	h.get(u)
	data := h.bindingSecret(u)
	if _, ok := data["password"]; ok || data["authMode"] != "IAM" || data["iamResourceArn"] != arn || data["username"] != id ||
		data["uri"] != "rediss://cache.example.test:6379" || data["cloud"] != "aws" || u.Status.ExternalID != arn {
		t.Errorf("binding = %v", data)
	}
	if s := api.created[0]; !s.IAM || s.Passwords != nil {
		t.Errorf("created = %+v", s)
	}
	step()
	api.noWrites(t)
	h.wantCondition(u.Status.Conditions, conditions.Ready, metav1.ConditionTrue, conditions.Available)

	// The rotate annotation is acknowledged and changes nothing.
	h.patch(u, func() { u.Annotations = map[string]string{lifecycle.AnnotationRotate: "1"} })
	step()
	api.noWrites(t)
	h.rec.AssertEvent(t, corev1.EventTypeWarning, conditions.EventRotationNotApplicable, "")
	if u.Status.Rotation == nil || u.Status.Rotation.LastTrigger != "1" || u.Status.Rotation.Active != "" {
		t.Errorf("rotation = %+v", u.Status.Rotation)
	}

	// A change of the profile touches the access string only.
	h.patch(u, func() { u.Spec.Profile = cachev1alpha1.ProfileReadOnly })
	step()
	if last := api.modified[len(api.modified)-1]; last.Passwords != nil || !strings.Contains(last.AccessString, "%R~") {
		t.Errorf("modified = %+v", last)
	}

	// A deleted binding Secret is rebuilt from the user, with no new credential.
	step()
	var s corev1.Secret
	h.must(env.Client.Get(ctx, client.ObjectKey{Namespace: h.ns, Name: u.GetBindingSecretName()}, &s))
	h.must(env.Client.Delete(ctx, &s))
	step()
	if got := h.bindingSecret(u); got["iamResourceArn"] != arn || got["authMode"] != "IAM" {
		t.Errorf("rebuilt binding = %v", got)
	}
	api.noWrites(t)
}

// IAM mode is refused where it does not fit, with the claim reasons of ADR 0002,
// section 16.1, and nothing is made.
func TestAWSUserIAMRefusals(t *testing.T) {
	t.Run("a self-hosted server", func(t *testing.T) {
		h := newHarness(t, nil)
		r := &UserReconciler{h.cfg}
		u := h.user("app", func(u *cachev1alpha1.CacheUser) { u.Spec.AuthMode = cachev1alpha1.AuthModeIAM })
		if err := h.reconcile(r, u); !errors.Is(err, ErrAuthModeUnsupported) {
			t.Fatalf("reconcile: %v", err)
		}
		h.get(u)
		msg := h.wantCondition(u.Status.Conditions, conditions.Synced, metav1.ConditionFalse, conditions.ReconcileError)
		if !strings.Contains(msg, "AuthModeUnsupported") {
			t.Errorf("message = %q", msg)
		}
		h.rec.AssertEvent(t, corev1.EventTypeWarning, "AuthModeUnsupported", "needs a server with a provider")
		h.fake.wantNot(t, writes...)
		if h.fake.joined() != "" || len(h.fake.configs) != 0 {
			t.Errorf("the claim reached the server: %q", h.fake.joined())
		}
	})

	t.Run("a principal on AWS", func(t *testing.T) {
		h, api := newAWSHarness(t, nil)
		r := &UserReconciler{h.cfg}
		u := h.user("app", func(u *cachev1alpha1.CacheUser) {
			u.Spec.AuthMode = cachev1alpha1.AuthModeIAM
			u.Spec.IAM = &cachev1alpha1.IAMSpec{Principal: "app@example.test"}
		})
		if err := h.reconcile(r, u); !errors.Is(err, ErrIAMPrincipalRefused) {
			t.Fatalf("reconcile: %v", err)
		}
		h.rec.AssertEvent(t, corev1.EventTypeWarning, "IAMPrincipalRequired", "not used on AWS")
		api.noWrites(t)
		if api.joined() != "" {
			t.Errorf("the claim reached AWS: %q", api.joined())
		}
		// Deleting needs no valid mode: nothing was made, and the claim goes.
		h.must(env.Client.Delete(context.Background(), u))
		h.reconcileOK(r, u)
		if !h.gone(u) {
			t.Error("the claim was not released")
		}
	})
}

// A user of the claim's id that another claim made is refused and left alone.
func TestAWSUserNeverAdoptsAnotherClaimsUser(t *testing.T) {
	h, api := newAWSHarness(t, nil)
	r := &UserReconciler{h.cfg}
	u := h.user("app", nil)
	id := awscache.UserID(h.ns, "app")
	api.users[id] = &memAWSUser{arn: "arn:foreign", status: awscache.StatusActive, marker: "other/claim/uid", access: "on ~foreign:*", passwords: []string{"x"}}

	h.reconcileOK(r, u)
	h.get(u)
	h.wantCondition(u.Status.Conditions, conditions.Synced, metav1.ConditionFalse, conditions.NameCollision)
	if u.Status.ExternalID != "" {
		t.Errorf("a create was recorded: %q", u.Status.ExternalID)
	}
	api.noWrites(t)

	// The claim made nothing: deleting it does not ask AWS.
	api.reset()
	h.must(env.Client.Delete(context.Background(), u))
	h.reconcileOK(r, u)
	if !h.gone(u) || api.joined() != "" || api.user(id) == nil {
		t.Errorf("deleting a claim that made nothing: calls %q", api.joined())
	}
}

// A user that holds more passwords than the Secret and a rotation explain is
// repaired with the Secret's password alone.
func TestAWSUserPasswordDrift(t *testing.T) {
	h, api := newAWSHarness(t, nil)
	r := &UserReconciler{h.cfg}
	u := h.user("app", nil)
	id := awscache.UserID(h.ns, "app")
	h.reconcileOK(r, u)
	api.settle()
	first := h.bindingSecret(u)["password"]

	api.users[id].passwords = []string{first, "rogue"}
	api.reset()
	h.reconcileOK(r, u)
	if got := api.modified[len(api.modified)-1].Passwords; !slices.Equal(got, []string{first}) {
		t.Errorf("passwords after the repair: %v", got)
	}
	h.rec.AssertEvent(t, corev1.EventTypeWarning, "PasswordDrift", "removed")
	h.rec.AssertNoSecret(t, first, "rogue")
}

// The data-plane address is the server's host and port, else what the probe
// discovered.
func TestAWSUserAddress(t *testing.T) {
	ctx := context.Background()
	h, api := newAWSHarness(t, func(s *cachev1alpha1.ClusterCacheServer) { s.Spec.Host = "" })
	r := &UserReconciler{h.cfg}
	u := h.user("app", nil)

	// Nothing says where the server is: no user is made.
	if err := h.reconcile(r, u); !errors.Is(err, ErrAddressUnknown) {
		t.Fatalf("reconcile: %v", err)
	}
	api.noWrites(t)

	h.get(h.server)
	h.server.Status.Discovered = map[string]string{
		cachev1alpha1.DiscoveredEndpoint: "sessions.abc.use1.cache.amazonaws.com", cachev1alpha1.DiscoveredPort: "6380",
	}
	h.must(env.Client.Status().Update(ctx, h.server))
	h.reconcileOK(r, u)
	got := h.bindingSecret(u)
	if got["host"] != "sessions.abc.use1.cache.amazonaws.com" || got["port"] != "6380" ||
		got["uri"] != "rediss://sessions.abc.use1.cache.amazonaws.com:6380" {
		t.Errorf("binding = %v", got)
	}
}

// A server that is not Ready is not used, and a deletion does not wait for it.
func TestAWSUserWaitsForAReadyServer(t *testing.T) {
	ctx := context.Background()
	h, api := newAWSHarness(t, nil)
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
	if err := h.reconcile(r, u); !errors.Is(err, ErrServerNotReady) || api.joined() != "" || len(api.servers) != 0 {
		t.Fatalf("reconcile: %v, calls %q, clients %d", err, api.joined(), len(api.servers))
	}
	setReady(metav1.ConditionTrue)
	h.reconcileOK(r, u)
	api.settle()
	setReady(metav1.ConditionFalse)
	h.must(env.Client.Delete(ctx, u))
	h.reconcileOK(r, u)
	api.settle()
	h.reconcileOK(r, u)
	if !h.gone(u) {
		t.Error("the deletion waited for the server")
	}
}

// What AWS says and what a credential is stay out of conditions and Events.
func TestAWSUserFailuresAreCoarse(t *testing.T) {
	leak := "arn:aws:iam::111122223333:user/hunter2 AccessDenied"
	for name, tc := range map[string]struct {
		inject func(api *memAWS)
		text   string
	}{
		"an API error": {func(api *memAWS) { api.fail["Create"] = errors.New(leak) }, defaultText},
		"credentials": {func(api *memAWS) { api.buildErr = fmt.Errorf("%w: %s", cloud.ErrCredentialsUnavailable, leak) },
			"the server's cloud credentials are not usable"},
		"an endpoint override": {func(api *memAWS) { api.buildErr = fmt.Errorf("%w: %s", cloud.ErrEndpointOverrideForbidden, leak) },
			"the server's endpoint override is not allowed"},
		"a missing group": {func(api *memAWS) { api.fail["Lookup"] = fmt.Errorf("%w: %s", awscache.ErrTargetNotFound, leak) },
			"the user group or ACL the server names does not exist"},
	} {
		t.Run(name, func(t *testing.T) {
			h, api := newAWSHarness(t, nil)
			r := &UserReconciler{h.cfg}
			u := h.user("app", nil)
			tc.inject(api)
			if err := h.reconcile(r, u); err == nil || strings.Contains(err.Error(), "hunter2") {
				t.Fatalf("reconcile: %v", err)
			}
			h.get(u)
			msg := h.wantCondition(u.Status.Conditions, conditions.Synced, metav1.ConditionFalse, conditions.ReconcileError)
			if !strings.HasSuffix(msg, tc.text) {
				t.Errorf("message = %q, want it to end with %q", msg, tc.text)
			}
			h.rec.AssertNoSecret(t, "hunter2", "111122223333")
		})
	}
}

// A server of a tenant names its credentials in the tenant's namespace, and a
// MemoryDB target selects the ACL.
func TestBackendOfANamespacedAWSServer(t *testing.T) {
	ctx := context.Background()
	h, api := newAWSHarness(t, nil)
	h.must(env.Client.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "aws-creds", Namespace: h.ns},
		Data:       map[string][]byte{cloud.KeyAWSAccessKeyID: []byte("test-key-id"), cloud.KeyAWSSecretAccessKey: []byte("test-secret")},
	}))
	team := &cachev1alpha1.CacheServer{ObjectMeta: metav1.ObjectMeta{Name: "team", Namespace: h.ns}}
	team.Spec.Host = "team.example.test"
	team.Spec.Port = new(int32)
	*team.Spec.Port = 6380
	team.Spec.Provider = &cachev1alpha1.Provider{Type: cloud.ProviderAWS, AWS: &cachev1alpha1.AWSCache{
		AWSBlock:  cloud.AWSBlock{Region: "eu-west-1"},
		AWSTarget: cachev1alpha1.AWSTarget{MemoryDB: &cachev1alpha1.MemoryDBTarget{ACLName: "acl", ClusterName: "sessions"}},
	}}
	team.Spec.Credentials = &cloud.NamespacedCredentials{Source: cloud.SourceSecret, SecretRef: &cloud.SecretRef{Name: "aws-creds"}}
	h.must(env.Client.Create(ctx, team))

	be, err := h.cfg.backend(ctx, h.ns, common.ServerRef{Kind: kindCacheServer, Name: "team"})
	if err != nil || be.aws == nil {
		t.Fatalf("backend: %v", err)
	}
	if _, err := be.aws.api(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := be.aws.api(ctx); err != nil || len(api.servers) != 1 {
		t.Fatalf("the client is built once per reconcile: %v, %d", err, len(api.servers))
	}
	s := api.servers[0]
	if s.Scope != cloud.ScopeNamespaced || s.Block.Region != "eu-west-1" || s.MemoryDB == nil || s.MemoryDB.ACLName != "acl" || s.ElastiCache != nil ||
		s.Credentials == nil || s.Credentials.Source != cloud.SourceSecret || string(s.Secret[cloud.KeyAWSAccessKeyID]) != "test-key-id" {
		t.Errorf("server = %+v", s)
	}
	if host, port, err := be.aws.address(); err != nil || host != "team.example.test" || port != "6380" {
		t.Errorf("address = %q, %q, %v", host, port, err)
	}
}

func TestNewAWSBackend(t *testing.T) {
	port := int32(7000)
	for name, tc := range map[string]struct {
		obj        serverObject
		host, port string
	}{
		"the server's own":  {serverObject{cfg: cachev1alpha1.ServerConfig{Host: "h", Port: &port}, discovered: map[string]string{"endpoint": "d", "port": "1"}}, "h", "7000"},
		"discovered":        {serverObject{discovered: map[string]string{"endpoint": "d", "port": "1"}}, "d", "1"},
		"the default port":  {serverObject{discovered: map[string]string{"endpoint": "d"}}, "d", "6379"},
		"nothing is known":  {serverObject{}, "", "6379"},
		"a host and a port": {serverObject{cfg: cachev1alpha1.ServerConfig{Host: "h"}, discovered: map[string]string{"port": "1"}}, "h", "1"},
	} {
		tc.obj.aws = &AWSServer{Block: cloud.AWSClusterBlock{AWSBlock: cloud.AWSBlock{Region: "eu-west-1"}}}
		// No NewAWS: the production builder is used, which is not called here.
		b := Config{}.newAWSBackend(tc.obj, server.CloudConnection{})
		if b.host != tc.host || b.port != tc.port || b.region != "eu-west-1" || b.build == nil {
			t.Errorf("%s: %+v", name, b)
		}
	}
	if (&awsBackend{host: "h", port: "1"}).uri() != "rediss://h:1" {
		t.Error("uri")
	}
	if (&awsBackend{host: "::1", port: "1"}).uri() != "rediss://[::1]:1" {
		t.Error("uri of an IPv6 address")
	}
}

func TestNewAWSClient(t *testing.T) {
	ctx := context.Background()
	s := AWSServer{
		Scope:       cloud.ScopeCluster,
		Block:       cloud.AWSClusterBlock{AWSBlock: cloud.AWSBlock{Region: "eu-west-1"}},
		Credentials: &cloud.Credentials{Source: cloud.SourceSecret, SecretRef: &cloud.SecretRef{Name: "creds", Namespace: "ops"}},
		Secret:      map[string][]byte{cloud.KeyAWSAccessKeyID: []byte("test-key-id"), cloud.KeyAWSSecretAccessKey: []byte("test-secret")},
		ElastiCache: &cachev1alpha1.ElastiCacheTarget{UserGroupID: "group", ReplicationGroupID: "rg"},
	}
	c, err := NewAWSClient(ctx, s, cloud.Egress{})
	if ec, ok := c.(awscache.ElastiCache); err != nil || !ok || ec.UserGroupID != "group" {
		t.Errorf("ElastiCache: %v, %v", c, err)
	}
	s.ElastiCache, s.MemoryDB = nil, &cachev1alpha1.MemoryDBTarget{ACLName: "acl", ClusterName: "c"}
	if c, err = NewAWSClient(ctx, s, cloud.Egress{}); err != nil {
		t.Fatal(err)
	} else if mdb, ok := c.(awscache.MemoryDB); !ok || mdb.ACLName != "acl" {
		t.Errorf("MemoryDB: %v", c)
	}
	// An endpoint override needs the allow-private mode.
	s.Block.EndpointOverride = "http://10.0.0.5:4566"
	if _, err = NewAWSClient(ctx, s, cloud.Egress{}); !errors.Is(err, cloud.ErrEndpointOverrideForbidden) {
		t.Errorf("endpoint override: %v", err)
	}
}
