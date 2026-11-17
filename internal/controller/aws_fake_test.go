package controller

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/craftainer-labs/lib-operator-go/pkg/cloud"

	cachev1alpha1 "github.com/craftainer-labs/operator-cache/api/v1alpha1"
	"github.com/craftainer-labs/operator-cache/internal/awscache"
)

// memAWSUser is a user of the in-memory AWS user group.
type memAWSUser struct {
	arn, status, access, marker string
	passwords                   []string
}

// memAWS is an awscache.Client in memory with the asynchronous behaviour of
// ElastiCache: a write leaves the user (or the group) in a transitional state
// until settle is called. It records every call, and fails the methods named in
// fail with the error given.
type memAWS struct {
	mu          sync.Mutex
	users       map[string]*memAWSUser
	member      map[string]bool
	groupStatus string
	calls       []string
	fail        map[string]error
	// created and modified are the Specs the writes got.
	created, modified []awscache.Spec
	// servers are the AWSServers the client was built for; buildErr fails the build.
	servers  []AWSServer
	buildErr error
}

func newMemAWS() *memAWS {
	return &memAWS{
		users: map[string]*memAWSUser{}, member: map[string]bool{}, groupStatus: awscache.StatusActive,
		fail: map[string]error{},
	}
}

// newClient is the NewAWSFunc of the tests.
func (m *memAWS) newClient(_ context.Context, s AWSServer, _ cloud.Egress) (awscache.Client, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.servers = append(m.servers, s)
	if m.buildErr != nil {
		return nil, m.buildErr
	}
	return m, nil
}

func (m *memAWS) enter(call string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, call)
	name, _, _ := strings.Cut(call, " ")
	return m.fail[name]
}

func (m *memAWS) reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = nil
}

func (m *memAWS) joined() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return strings.Join(m.calls, "\n")
}

// wrote reports whether a call changed the cloud.
func (m *memAWS) wrote() bool {
	for _, w := range []string{"Create ", "Modify ", "Join ", "Delete "} {
		if strings.Contains(m.joined(), w) {
			return true
		}
	}
	return false
}

func (m *memAWS) user(id string) *memAWSUser {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.users[id]
}

// settle ends every transitional state: the cloud has finished.
func (m *memAWS) settle() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.groupStatus = awscache.StatusActive
	for id, u := range m.users {
		if u.status == awscache.StatusDeleting {
			delete(m.users, id)
			delete(m.member, id)
			continue
		}
		u.status = awscache.StatusActive
	}
}

func (m *memAWS) Lookup(_ context.Context, id string) (awscache.User, error) {
	if err := m.enter("Lookup " + id); err != nil {
		return awscache.User{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	u, ok := m.users[id]
	if !ok {
		return awscache.User{}, awscache.ErrNotFound
	}
	return awscache.User{
		ARN: u.arn, Status: u.status, AccessString: u.access, Passwords: len(u.passwords), Member: m.member[id],
		GroupStatus: m.groupStatus, Marker: u.marker,
	}, nil
}

func (m *memAWS) Create(_ context.Context, id string, s awscache.Spec) (string, error) {
	if err := m.enter("Create " + id); err != nil {
		return "", err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.created = append(m.created, s)
	arn := "arn:aws:elasticache:eu-west-1:111122223333:user:" + id
	m.users[id] = &memAWSUser{arn: arn, status: "creating", access: s.AccessString, marker: s.Marker, passwords: slices.Clone(s.Passwords)}
	return arn, nil
}

func (m *memAWS) Modify(_ context.Context, id string, s awscache.Spec) error {
	if err := m.enter("Modify " + id); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.modified = append(m.modified, s)
	u := m.users[id]
	if u == nil {
		return nil
	}
	if s.AccessString != "" {
		u.access = s.AccessString
	}
	if s.Passwords != nil {
		u.passwords = slices.Clone(s.Passwords)
	}
	u.status = awscache.StatusModifying
	return nil
}

func (m *memAWS) Join(_ context.Context, id string) error {
	if err := m.enter("Join " + id); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.member[id] = true
	m.groupStatus = awscache.StatusModifying
	return nil
}

func (m *memAWS) Delete(_ context.Context, id string) error {
	if err := m.enter("Delete " + id); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if u := m.users[id]; u != nil {
		u.status = awscache.StatusDeleting
	}
	return nil
}

// newAWSHarness is a harness whose server is an ElastiCache user group in AWS,
// reached through a memAWS, with a host of its own.
func newAWSHarness(t *testing.T, mutate func(*cachev1alpha1.ClusterCacheServer)) (*harness, *memAWS) {
	t.Helper()
	api := newMemAWS()
	h := newHarness(t, func(s *cachev1alpha1.ClusterCacheServer) {
		s.Spec.Host, s.Spec.AdminCredentialsSecretRef = "cache.example.test", nil
		s.Spec.Provider = &cachev1alpha1.ClusterProvider{Type: cloud.ProviderAWS, AWS: &cachev1alpha1.ClusterAWSCache{
			AWSClusterBlock: cloud.AWSClusterBlock{AWSBlock: cloud.AWSBlock{Region: "eu-west-1"}},
			AWSTarget: cachev1alpha1.AWSTarget{
				ElastiCache: &cachev1alpha1.ElastiCacheTarget{UserGroupID: "craftainer", ReplicationGroupID: "sessions"},
			},
		}}
		if mutate != nil {
			mutate(s)
		}
	})
	h.cfg.NewAWS = api.newClient
	return h, api
}

// want fails the test unless the calls so far contain every fragment.
func (m *memAWS) want(t *testing.T, fragments ...string) {
	t.Helper()
	for _, f := range fragments {
		if !strings.Contains(m.joined(), f) {
			t.Errorf("no call contains %q; calls:\n%s", f, m.joined())
		}
	}
}

// noWrites fails the test when a call changed the cloud.
func (m *memAWS) noWrites(t *testing.T) {
	t.Helper()
	if m.wrote() {
		t.Errorf("the cloud was changed; calls:\n%s", fmt.Sprint(m.joined()))
	}
}
