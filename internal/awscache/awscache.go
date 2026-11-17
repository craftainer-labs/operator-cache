// Package awscache drives the RBAC users of Amazon ElastiCache and MemoryDB: one
// user per claim, in the user group or ACL the administrator attached to the
// cluster. Both services are API-only (ACL SETUSER is refused) and
// asynchronous: a write returns at once and the user's status says when it is
// done, so the package never waits; the controller asks again (lifecycle.Pending).
//
// Only a password count is visible on an AWS user, never a hash, so the package
// can tell that a user holds more passwords than the operator issued but not
// which ones.
package awscache

import (
	"context"
	"errors"
	"strings"

	"github.com/craftainer-labs/lib-operator-go/pkg/naming"
)

// MarkerKey is the tag that records the claim a user belongs to (ADR 0002,
// section 10). Its value is MarkerValue.
const MarkerKey = "craftainer.dev/claim"

// MaxUserIDLen is the longest user id ElastiCache accepts.
const MaxUserIDLen = 40

// Status values of a user or group the controller treats specially; every other
// value is a state that has not settled.
const (
	StatusActive    = "active"
	StatusModifying = "modifying"
	StatusDeleting  = "deleting"
)

var (
	// ErrNotFound means the user does not exist.
	ErrNotFound = errors.New("the user does not exist")
	// ErrTargetNotFound means the user group or ACL the server names does not
	// exist, so no user can join it.
	ErrTargetNotFound = errors.New("the user group or ACL of the server does not exist")
)

// MarkerValue is the value of the marker tag for a claim.
func MarkerValue(namespace, name, uid string) string {
	return namespace + "/" + name + "/" + uid
}

// UserID derives the id of the user of a claim: the namespace and name cut to
// MaxUserIDLen with the short hash of naming.Tenant, then made a valid id.
// ElastiCache wants letters, digits and single hyphens, starting with a letter:
// a dot of the claim's name becomes a hyphen, doubled hyphens collapse, and a
// namespace that starts with a digit gets a "u" in front, inside the limit.
func UserID(namespace, name string) string {
	limit, prefix := MaxUserIDLen, ""
	if namespace[0] >= '0' && namespace[0] <= '9' {
		limit, prefix = limit-1, "u"
	}
	id := strings.ReplaceAll(naming.Tenant(namespace, name, limit, "-", naming.HashShort), ".", "-")
	for strings.Contains(id, "--") {
		id = strings.ReplaceAll(id, "--", "-")
	}
	return prefix + id
}

// User is a user as the service reports it.
type User struct {
	// ARN is the user's ARN.
	ARN string
	// Status is the user's status.
	Status string
	// AccessString is the user's permissions.
	AccessString string
	// Passwords is how many passwords the user holds.
	Passwords int
	// Member is true when the user is in the server's user group or ACL.
	Member bool
	// GroupStatus is the status of the user group or ACL.
	GroupStatus string
	// Marker is the value of the marker tag, "" when there is none.
	Marker string
}

// Spec is what a write sets.
type Spec struct {
	// AccessString replaces the user's permissions; "" leaves them.
	AccessString string
	// IAM makes the user authenticate with an IAM token (create only).
	IAM bool
	// Passwords replaces the user's passwords; nil leaves them. A password user
	// holds one or two.
	Passwords []string
	// Marker is the marker tag's value (create only).
	Marker string
}

// Client drives the users of one server (one user group or ACL). Every write
// returns once the service has accepted it; the user's Status says when it is
// done.
type Client interface {
	// Lookup returns the user, or ErrNotFound. ErrTargetNotFound means the
	// user group or ACL is missing.
	Lookup(ctx context.Context, id string) (User, error)
	// Create makes the user and returns its ARN. It does not add it to the group.
	Create(ctx context.Context, id string, s Spec) (string, error)
	// Modify changes the user's access string and passwords.
	Modify(ctx context.Context, id string, s Spec) error
	// Join adds the user to the group or ACL.
	Join(ctx context.Context, id string) error
	// Delete deletes the user, which takes it out of every group; a user that is
	// already gone is a success.
	Delete(ctx context.Context, id string) error
}
