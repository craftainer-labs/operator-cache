//go:build integration

package integration

import (
	"errors"
	"testing"

	"github.com/craftainer-labs/lib-operator-go/pkg/lifecycle"

	cachev1alpha1 "github.com/craftainer-labs/operator-cache/api/v1alpha1"
	"github.com/craftainer-labs/operator-cache/internal/controller"
	"github.com/craftainer-labs/operator-cache/internal/valkeyacl"
)

// The tests below drive the controller's External (internal/controller) against
// the real server: what a reconcile does on create, on a change of the spec, on a
// rotation and on delete, and what it refuses.

var (
	readWrite = cachev1alpha1.CacheUserSpec{Profile: cachev1alpha1.ProfileReadWrite}
	readOnly  = cachev1alpha1.CacheUserSpec{Profile: cachev1alpha1.ProfileReadOnly}
)

// newClaim is the External of a claim of a tenant namespace of its own, on the
// server as the administrator.
func newClaim(t *testing.T) (*controller.Integration, *valkeyacl.Admin, server, string) {
	t.Helper()
	a, s := admin(t)
	ns := "it-" + random(t)
	i := controller.NewIntegration(a, ns, "app", cachev1alpha1.ServerConfig{Host: s.host}, s.ca)
	t.Cleanup(func() { _, _ = a.DelUser(bg, i.Claim().GetExternalName()) })
	return i, a, s, ns
}

func TestControllerUserLifecycle(t *testing.T) {
	i, a, s, ns := newClaim(t)
	name := i.Claim().GetExternalName()
	if name == "" {
		t.Fatal("the claim has no derived name")
	}

	// Nothing there yet.
	if obs, err := i.Observe(bg, readWrite); err != nil || obs.Exists {
		t.Fatalf("Observe of nobody: %+v, %v", obs, err)
	}

	// Create: the user is the tenant's, with the password the binding holds.
	data, err := i.Create(bg, readWrite)
	if err != nil {
		t.Fatal(err)
	}
	i.Binding = data
	if i.Claim().GetExternalID() != name || string(data["username"]) != name || string(data["keyPrefix"]) != ns+":" {
		t.Fatalf("externalId = %q, binding = %v", i.Claim().GetExternalID(), data.Keys())
	}
	obs, err := i.Observe(bg, readWrite)
	if err != nil || !obs.Exists || !obs.UpToDate {
		t.Fatalf("Observe after Create: %+v, %v", obs, err)
	}
	c := loginOK(t, s, name, string(data["password"]))
	allowed(t, c, "SET", ns+":k", "v")
	allowed(t, c, "GET", ns+":k")
	denied(t, c, "SET", "other:k", "v")
	denied(t, c, "KEYS", "*")
	denied(t, c, "SCAN", "0")

	// A change of the profile is an update that keeps the password: the session
	// in use is not ended and the new rules apply.
	obs, err = i.Observe(bg, readOnly)
	if err != nil || !obs.Exists || obs.UpToDate {
		t.Fatalf("Observe after a change of profile: %+v, %v", obs, err)
	}
	if _, err := i.Update(bg, readOnly, obs.Current); err != nil {
		t.Fatal(err)
	}
	if obs, err = i.Observe(bg, readOnly); err != nil || !obs.UpToDate {
		t.Fatalf("Observe after Update: %+v, %v", obs, err)
	}
	allowed(t, c, "GET", ns+":k")
	denied(t, c, "SET", ns+":k", "w")
	loginOK(t, s, name, string(data["password"]))

	// Rules someone widened by hand are noticed and repaired.
	if err := a.SetUser(bg, name, "resetkeys", "~*", "allchannels", "+@all"); err != nil {
		t.Fatal(err)
	}
	obs, err = i.Observe(bg, readOnly)
	if err != nil || obs.UpToDate {
		t.Fatalf("Observe of a widened user: %+v, %v", obs, err)
	}
	if _, err := i.Update(bg, readOnly, obs.Current); err != nil {
		t.Fatal(err)
	}
	if obs, err = i.Observe(bg, readOnly); err != nil || !obs.UpToDate {
		t.Fatalf("Observe after the repair: %+v, %v", obs, err)
	}

	// An update whose binding Secret does not hold the user is refused with a
	// typed error, and the password is not touched.
	i.Binding = nil
	if _, err := i.Update(bg, readOnly, obs.Current); !errors.Is(err, controller.ErrBindingNotUsable) {
		t.Fatalf("Update without a Secret: %v", err)
	}
	loginOK(t, s, name, string(data["password"]))
	i.Binding = data

	// Delete ends the user and its sessions, and a second delete is a success.
	if err := i.Delete(bg, obs.Current); err != nil {
		t.Fatal(err)
	}
	if err := i.Delete(bg, obs.Current); err != nil {
		t.Fatalf("Delete of a gone user: %v", err)
	}
	if _, err := a.GetUser(bg, name); !errors.Is(err, valkeyacl.ErrNotFound) {
		t.Fatalf("GetUser after Delete: %v", err)
	}
	if _, err := s.login(name, string(data["password"])); err == nil {
		t.Fatal("the deleted user still logs in")
	}
}

func TestControllerRotation(t *testing.T) {
	i, a, s, ns := newClaim(t)
	name := i.Claim().GetExternalName()
	data, err := i.Create(bg, readWrite)
	if err != nil {
		t.Fatal(err)
	}
	i.Binding = data
	oldPW := string(data["password"])
	oldClient := loginOK(t, s, name, oldPW)
	obs, err := i.Observe(bg, readWrite)
	if err != nil || !obs.Exists {
		t.Fatalf("Observe: %+v, %v", obs, err)
	}

	// RotateBegin keeps the current password beside a new one: both log in, the
	// rules are the same and the session in use goes on.
	next, gen, err := i.RotateBegin(bg, readWrite, obs.Current, lifecycle.RotationState{Sequence: 0})
	if err != nil || gen != "1" {
		t.Fatalf("RotateBegin: %q, %v", gen, err)
	}
	newPW := string(next["password"])
	if newPW == oldPW {
		t.Fatal("RotateBegin kept the password")
	}
	u, err := a.GetUser(bg, name)
	if err != nil || len(u.PasswordHashes) != 2 || !(valkeyacl.Tenant{Prefix: ns, Write: true}).Matches(u) {
		t.Fatalf("after RotateBegin: %+v, %v", u, err)
	}
	loginOK(t, s, name, oldPW)
	newClient := loginOK(t, s, name, newPW)
	allowed(t, oldClient, "SET", ns+":k", "v")

	// A retry before the Secret was written (it still holds the old password)
	// keeps every password beside another new one: the first attempt's stays until
	// RotateFinish.
	again, _, err := i.RotateBegin(bg, readWrite, obs.Current, lifecycle.RotationState{Sequence: 0})
	if err != nil {
		t.Fatal(err)
	}
	loginOK(t, s, name, newPW)
	loginOK(t, s, name, oldPW)
	againPW := string(again["password"])
	loginOK(t, s, name, againPW)

	// A retry after the Secret was written but before it was marked: the Secret
	// holds the first attempt's password. The old password, which the consumers
	// may still hold, must still log in.
	i.Binding = next
	last, _, err := i.RotateBegin(bg, readWrite, obs.Current, lifecycle.RotationState{Sequence: 0})
	if err != nil {
		t.Fatal(err)
	}
	loginOK(t, s, name, oldPW)
	loginOK(t, s, name, newPW)
	newPW = string(last["password"])
	loginOK(t, s, name, newPW)
	again = last

	// RotateFinish with the new password in the Secret ends the old one.
	i.Binding = again
	if err := i.RotateFinish(bg, readWrite, obs.Current, lifecycle.RotationState{Active: "1", Sequence: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.login(name, oldPW); err == nil {
		t.Fatal("the old password still logs in")
	}
	loginOK(t, s, name, newPW)
	allowed(t, newClient, "GET", ns+":k")
	if u, err = a.GetUser(bg, name); err != nil || len(u.PasswordHashes) != 1 || u.PasswordHashes[0] != valkeyacl.HashPassword(newPW) {
		t.Fatalf("after RotateFinish: %+v, %v", u, err)
	}
}

// A retire whose Secret holds a password the server does not list changes
// nothing: "resetpass" would otherwise leave the user with no password.
func TestControllerRotateFinishRefusesAnUnknownPassword(t *testing.T) {
	i, a, s, _ := newClaim(t)
	name := i.Claim().GetExternalName()
	data, err := i.Create(bg, readWrite)
	if err != nil {
		t.Fatal(err)
	}
	pw := string(data["password"])
	obs, err := i.Observe(bg, readWrite)
	if err != nil {
		t.Fatal(err)
	}
	for label, secret := range map[string]map[string][]byte{
		"another password": {"username": []byte(name), "password": []byte("not-on-the-server-0123456789")},
		"no Secret":        nil,
	} {
		i.Binding = secret
		if err := i.RotateFinish(bg, readWrite, obs.Current, lifecycle.RotationState{Active: "1"}); !errors.Is(err, controller.ErrPasswordNotOnServer) {
			t.Errorf("%s: %v", label, err)
		}
		loginOK(t, s, name, pw)
		if u, err := a.GetUser(bg, name); err != nil || len(u.PasswordHashes) != 1 {
			t.Errorf("%s: the user's passwords changed: %+v, %v", label, u, err)
		}
	}
}

// A user that exists while the claim has no record of a create is someone else's:
// it is refused, and nothing of it changes.
func TestControllerNameCollision(t *testing.T) {
	i, a, s, _ := newClaim(t)
	name := i.Claim().GetExternalName()
	const foreignPW = "foreign-password-0123456789"
	if err := a.SetUser(bg, name, "on", valkeyacl.AddPassword(foreignPW), "~foreign:*", "+get", "+set"); err != nil {
		t.Fatal(err)
	}
	before, err := a.GetUser(bg, name)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := i.Observe(bg, readWrite); !errors.Is(err, lifecycle.ErrNameCollision) {
		t.Errorf("Observe: %v", err)
	}
	if _, err := i.Create(bg, readWrite); !errors.Is(err, lifecycle.ErrNameCollision) {
		t.Errorf("Create: %v", err)
	}
	if i.Claim().GetExternalID() != "" {
		t.Errorf("a create was recorded: %q", i.Claim().GetExternalID())
	}
	after, err := a.GetUser(bg, name)
	if err != nil || after.Keys != before.Keys || after.Commands != before.Commands ||
		len(after.PasswordHashes) != 1 || after.PasswordHashes[0] != valkeyacl.HashPassword(foreignPW) {
		t.Fatalf("the foreign user changed: %+v, %v", after, err)
	}
	c := loginOK(t, s, name, foreignPW)
	allowed(t, c, "SET", "foreign:k", "v")
}

// A create that crashed after the SETUSER, before the claim recorded the name, is
// finished: the intent was recorded, so the user is the claim's, and with no
// binding Secret the rotation adds a password that is then the only one.
func TestControllerFinishesACreateThatCrashed(t *testing.T) {
	i, a, s, ns := newClaim(t)
	name := i.Claim().GetExternalName()
	tenant := valkeyacl.Tenant{Prefix: ns, Write: true}
	rules, err := tenant.Rules()
	if err != nil {
		t.Fatal(err)
	}
	const crashedPW = "crashed-password-0123456789"
	if err := a.SetUser(bg, name, append([]string{"reset", valkeyacl.AddPassword(crashedPW)}, rules...)...); err != nil {
		t.Fatal(err)
	}
	i.Claim().SetExternalID("create-issued")

	obs, err := i.Observe(bg, readWrite)
	if err != nil || !obs.Exists || !obs.UpToDate {
		t.Fatalf("Observe: %+v, %v", obs, err)
	}
	data, _, err := i.RotateBegin(bg, readWrite, obs.Current, lifecycle.RotationState{})
	if err != nil {
		t.Fatal(err)
	}
	if i.Claim().GetExternalID() != name {
		t.Errorf("externalId = %q", i.Claim().GetExternalID())
	}
	i.Binding = data
	loginOK(t, s, name, crashedPW)
	if err := i.RotateFinish(bg, readWrite, obs.Current, lifecycle.RotationState{Active: "1", Sequence: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.login(name, crashedPW); err == nil {
		t.Fatal("the password of the crashed attempt still logs in")
	}
	loginOK(t, s, name, string(data["password"]))
}
