package awscache

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/elasticache"
	ectypes "github.com/aws/aws-sdk-go-v2/service/elasticache/types"
	"github.com/aws/aws-sdk-go-v2/service/memorydb"
	mdbtypes "github.com/aws/aws-sdk-go-v2/service/memorydb/types"
)

var errAPI = errors.New("the API failed")

func TestUserID(t *testing.T) {
	for _, tc := range []struct{ ns, name string }{
		{"tenant", "app"},
		{"team-a", "orders.v2"},
		{"a--b", "x"},
		{"1tenant", "app"},
		{strings.Repeat("n", 63), strings.Repeat("a", 253)},
		{"9", "x.y.z"},
	} {
		id := UserID(tc.ns, tc.name)
		if len(id) > MaxUserIDLen || strings.Contains(id, "--") || strings.Contains(id, ".") ||
			id[0] < 'a' || id[0] > 'z' || strings.HasSuffix(id, "-") {
			t.Errorf("UserID(%q, %q) = %q", tc.ns, tc.name, id)
		}
		if id != UserID(tc.ns, tc.name) {
			t.Errorf("UserID(%q, %q) is not stable", tc.ns, tc.name)
		}
	}
	if UserID("tenant", "app") == UserID("tenant", "app2") || UserID("tenant", "app") == UserID("tenant2", "app") {
		t.Error("UserID is not a function of the namespace and the name")
	}
	if got := UserID("1tenant", "app"); !strings.HasPrefix(got, "u1tenant-app-") {
		t.Errorf("a digit first gets a u in front: %q", got)
	}
	if got := MarkerValue("ns", "name", "uid"); got != "ns/name/uid" {
		t.Errorf("marker = %q", got)
	}
	long := MarkerValue(strings.Repeat("n", 63), strings.Repeat("a", 253), "0123456789abcdef0123456789abcdef0123")
	if len(long) > maxTagValueLen || !strings.HasPrefix(long, "sha256:") {
		t.Errorf("a long marker is hashed to fit a tag value: %q", long)
	}
	// 256 characters fit a tag value as they are; 257 are hashed.
	if got := MarkerValue("n", strings.Repeat("a", 256-len("n//uid")), "uid"); len(got) != 256 || strings.HasPrefix(got, "sha256:") {
		t.Errorf("a marker of exactly 256 characters stays plain: %d %q", len(got), got)
	}
	if got := MarkerValue("n", strings.Repeat("a", 257-len("n//uid")), "uid"); !strings.HasPrefix(got, "sha256:") {
		t.Errorf("a marker of 257 characters is hashed: %q", got)
	}
	if other := MarkerValue(strings.Repeat("n", 63), strings.Repeat("a", 253), "ffffffffffffffffffffffffffffffffffff"); other == long {
		t.Error("two claims share a marker")
	}
}

// fakeEC is an ElastiCache SDK client.
type fakeEC struct {
	users  []ectypes.User
	groups []ectypes.UserGroup
	tags   []ectypes.Tag
	rgs    []ectypes.ReplicationGroup
	errs   map[string]error

	created  *elasticache.CreateUserInput
	modified *elasticache.ModifyUserInput
	joined   *elasticache.ModifyUserGroupInput
	deleted  *elasticache.DeleteUserInput
}

func (f *fakeEC) DescribeUsers(context.Context, *elasticache.DescribeUsersInput, ...func(*elasticache.Options)) (*elasticache.DescribeUsersOutput, error) {
	return &elasticache.DescribeUsersOutput{Users: f.users}, f.errs["DescribeUsers"]
}

func (f *fakeEC) CreateUser(_ context.Context, in *elasticache.CreateUserInput, _ ...func(*elasticache.Options)) (*elasticache.CreateUserOutput, error) {
	f.created = in
	return &elasticache.CreateUserOutput{ARN: aws.String("arn:user")}, f.errs["CreateUser"]
}

func (f *fakeEC) ModifyUser(_ context.Context, in *elasticache.ModifyUserInput, _ ...func(*elasticache.Options)) (*elasticache.ModifyUserOutput, error) {
	f.modified = in
	return &elasticache.ModifyUserOutput{}, f.errs["ModifyUser"]
}

func (f *fakeEC) DeleteUser(_ context.Context, in *elasticache.DeleteUserInput, _ ...func(*elasticache.Options)) (*elasticache.DeleteUserOutput, error) {
	f.deleted = in
	return &elasticache.DeleteUserOutput{}, f.errs["DeleteUser"]
}

func (f *fakeEC) DescribeUserGroups(context.Context, *elasticache.DescribeUserGroupsInput, ...func(*elasticache.Options)) (*elasticache.DescribeUserGroupsOutput, error) {
	return &elasticache.DescribeUserGroupsOutput{UserGroups: f.groups}, f.errs["DescribeUserGroups"]
}

func (f *fakeEC) ModifyUserGroup(_ context.Context, in *elasticache.ModifyUserGroupInput, _ ...func(*elasticache.Options)) (*elasticache.ModifyUserGroupOutput, error) {
	f.joined = in
	return &elasticache.ModifyUserGroupOutput{}, f.errs["ModifyUserGroup"]
}

func (f *fakeEC) ListTagsForResource(context.Context, *elasticache.ListTagsForResourceInput, ...func(*elasticache.Options)) (*elasticache.ListTagsForResourceOutput, error) {
	return &elasticache.ListTagsForResourceOutput{TagList: f.tags}, f.errs["ListTagsForResource"]
}

func (f *fakeEC) DescribeReplicationGroups(context.Context, *elasticache.DescribeReplicationGroupsInput, ...func(*elasticache.Options)) (*elasticache.DescribeReplicationGroupsOutput, error) {
	return &elasticache.DescribeReplicationGroupsOutput{ReplicationGroups: f.rgs}, f.errs["DescribeReplicationGroups"]
}

func newFakeEC() *fakeEC {
	return &fakeEC{
		users: []ectypes.User{{
			UserId: aws.String("u1"), ARN: aws.String("arn:user"), Status: aws.String("active"), AccessString: aws.String("on ~a:*"),
			Authentication: &ectypes.Authentication{PasswordCount: aws.Int32(2), Type: ectypes.AuthenticationTypePassword},
		}},
		groups: []ectypes.UserGroup{{UserGroupId: aws.String("g"), Status: aws.String("active"), UserIds: []string{"default", "u1"}, Engine: aws.String("redis")}},
		tags:   []ectypes.Tag{{Key: aws.String("other"), Value: aws.String("x")}, {Key: aws.String(MarkerKey), Value: aws.String("ns/n/uid")}},
		rgs: []ectypes.ReplicationGroup{{
			ARN:        aws.String("arn:aws:elasticache:eu-west-1:111122223333:replicationgroup:rg"),
			NodeGroups: []ectypes.NodeGroup{{PrimaryEndpoint: &ectypes.Endpoint{Address: aws.String("master.rg.example.test"), Port: aws.Int32(6379)}}},
		}},
		errs: map[string]error{},
	}
}

func TestElastiCacheLookup(t *testing.T) {
	ctx := context.Background()
	f := newFakeEC()
	c := ElastiCache{API: f, UserGroupID: "g"}
	got, err := c.Lookup(ctx, "u1")
	want := User{ARN: "arn:user", Status: "active", AccessString: "on ~a:*", Passwords: 2, Member: true, GroupStatus: "active", Marker: "ns/n/uid"}
	if err != nil || got != want {
		t.Errorf("Lookup = %+v, %v; want %+v", got, err, want)
	}

	// A user with no authentication block, outside the group and with no marker.
	f.users[0].Authentication = nil
	f.groups[0].UserIds = []string{"default"}
	f.tags = nil
	if got, err = c.Lookup(ctx, "u1"); err != nil || got.Member || got.Marker != "" || got.Passwords != 0 {
		t.Errorf("Lookup = %+v, %v", got, err)
	}

	for name, tc := range map[string]struct {
		mutate func(f *fakeEC)
		want   error
	}{
		"user fault":    {func(f *fakeEC) { f.errs["DescribeUsers"] = &ectypes.UserNotFoundFault{} }, ErrNotFound},
		"no user":       {func(f *fakeEC) { f.users = nil }, ErrNotFound},
		"describe":      {func(f *fakeEC) { f.errs["DescribeUsers"] = errAPI }, errAPI},
		"group fault":   {func(f *fakeEC) { f.errs["DescribeUserGroups"] = &ectypes.UserGroupNotFoundFault{} }, ErrTargetNotFound},
		"no group":      {func(f *fakeEC) { f.groups = nil }, ErrTargetNotFound},
		"group failure": {func(f *fakeEC) { f.errs["DescribeUserGroups"] = errAPI }, errAPI},
		"tags":          {func(f *fakeEC) { f.errs["ListTagsForResource"] = errAPI }, errAPI},
	} {
		f := newFakeEC()
		tc.mutate(f)
		if _, err := (ElastiCache{API: f, UserGroupID: "g"}).Lookup(ctx, "u1"); !errors.Is(err, tc.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestElastiCacheWrites(t *testing.T) {
	ctx := context.Background()
	f := newFakeEC()
	c := ElastiCache{API: f, UserGroupID: "g"}

	arn, err := c.Create(ctx, "u1", Spec{AccessString: "on ~a:*", Passwords: []string{"p"}, Marker: "ns/n/uid"})
	in := f.created
	if err != nil || arn != "arn:user" || aws.ToString(in.UserId) != "u1" || aws.ToString(in.UserName) != "u1" ||
		aws.ToString(in.Engine) != "redis" || aws.ToString(in.AccessString) != "on ~a:*" ||
		in.AuthenticationMode.Type != ectypes.InputAuthenticationTypePassword || !slices.Equal(in.AuthenticationMode.Passwords, []string{"p"}) ||
		len(in.Tags) != 1 || aws.ToString(in.Tags[0].Key) != MarkerKey || aws.ToString(in.Tags[0].Value) != "ns/n/uid" {
		t.Errorf("Create: %q, %v, %+v", arn, err, in)
	}
	// An IAM user has no password, and an engine the group does not name is Valkey.
	f.groups[0].Engine = nil
	if _, err = c.Create(ctx, "u1", Spec{IAM: true}); err != nil || f.created.AuthenticationMode.Type != ectypes.InputAuthenticationTypeIam ||
		len(f.created.AuthenticationMode.Passwords) != 0 || aws.ToString(f.created.Engine) != "valkey" {
		t.Errorf("Create IAM: %v, %+v", err, f.created)
	}
	f.errs["CreateUser"] = errAPI
	if _, err = c.Create(ctx, "u1", Spec{}); !errors.Is(err, errAPI) {
		t.Errorf("Create failure: %v", err)
	}
	f.errs["DescribeUserGroups"] = &ectypes.UserGroupNotFoundFault{}
	if _, err = c.Create(ctx, "u1", Spec{}); !errors.Is(err, ErrTargetNotFound) {
		t.Errorf("Create without a group: %v", err)
	}

	// Modify sends what is set and nothing else.
	if err = c.Modify(ctx, "u1", Spec{AccessString: "on ~b:*"}); err != nil || aws.ToString(f.modified.AccessString) != "on ~b:*" || f.modified.AuthenticationMode != nil {
		t.Errorf("Modify access: %v, %+v", err, f.modified)
	}
	if err = c.Modify(ctx, "u1", Spec{Passwords: []string{"a", "b"}}); err != nil || f.modified.AccessString != nil ||
		!slices.Equal(f.modified.AuthenticationMode.Passwords, []string{"a", "b"}) {
		t.Errorf("Modify passwords: %v, %+v", err, f.modified)
	}
	f.errs["ModifyUser"] = errAPI
	if err = c.Modify(ctx, "u1", Spec{}); !errors.Is(err, errAPI) {
		t.Errorf("Modify failure: %v", err)
	}

	if err = c.Join(ctx, "u1"); err != nil || aws.ToString(f.joined.UserGroupId) != "g" || !slices.Equal(f.joined.UserIdsToAdd, []string{"u1"}) {
		t.Errorf("Join: %v, %+v", err, f.joined)
	}
	f.errs["ModifyUserGroup"] = errAPI
	if err = c.Join(ctx, "u1"); !errors.Is(err, errAPI) {
		t.Errorf("Join failure: %v", err)
	}

	if err = c.Delete(ctx, "u1"); err != nil || aws.ToString(f.deleted.UserId) != "u1" {
		t.Errorf("Delete: %v, %+v", err, f.deleted)
	}
	f.errs["DeleteUser"] = &ectypes.UserNotFoundFault{}
	if err = c.Delete(ctx, "u1"); err != nil {
		t.Errorf("Delete of a user that is gone: %v", err)
	}
	f.errs["DeleteUser"] = errAPI
	if err = c.Delete(ctx, "u1"); !errors.Is(err, errAPI) {
		t.Errorf("Delete failure: %v", err)
	}
}

// fakeMDB is a MemoryDB SDK client.
type fakeMDB struct {
	users []mdbtypes.User
	acls  []mdbtypes.ACL
	tags  []mdbtypes.Tag
	cls   []mdbtypes.Cluster
	errs  map[string]error

	created  *memorydb.CreateUserInput
	modified *memorydb.UpdateUserInput
	joined   *memorydb.UpdateACLInput
	deleted  *memorydb.DeleteUserInput
}

func (f *fakeMDB) DescribeUsers(context.Context, *memorydb.DescribeUsersInput, ...func(*memorydb.Options)) (*memorydb.DescribeUsersOutput, error) {
	return &memorydb.DescribeUsersOutput{Users: f.users}, f.errs["DescribeUsers"]
}

func (f *fakeMDB) CreateUser(_ context.Context, in *memorydb.CreateUserInput, _ ...func(*memorydb.Options)) (*memorydb.CreateUserOutput, error) {
	f.created = in
	return &memorydb.CreateUserOutput{User: &mdbtypes.User{ARN: aws.String("arn:user")}}, f.errs["CreateUser"]
}

func (f *fakeMDB) UpdateUser(_ context.Context, in *memorydb.UpdateUserInput, _ ...func(*memorydb.Options)) (*memorydb.UpdateUserOutput, error) {
	f.modified = in
	return &memorydb.UpdateUserOutput{}, f.errs["UpdateUser"]
}

func (f *fakeMDB) DeleteUser(_ context.Context, in *memorydb.DeleteUserInput, _ ...func(*memorydb.Options)) (*memorydb.DeleteUserOutput, error) {
	f.deleted = in
	return &memorydb.DeleteUserOutput{}, f.errs["DeleteUser"]
}

func (f *fakeMDB) DescribeACLs(context.Context, *memorydb.DescribeACLsInput, ...func(*memorydb.Options)) (*memorydb.DescribeACLsOutput, error) {
	return &memorydb.DescribeACLsOutput{ACLs: f.acls}, f.errs["DescribeACLs"]
}

func (f *fakeMDB) UpdateACL(_ context.Context, in *memorydb.UpdateACLInput, _ ...func(*memorydb.Options)) (*memorydb.UpdateACLOutput, error) {
	f.joined = in
	return &memorydb.UpdateACLOutput{}, f.errs["UpdateACL"]
}

func (f *fakeMDB) ListTags(context.Context, *memorydb.ListTagsInput, ...func(*memorydb.Options)) (*memorydb.ListTagsOutput, error) {
	return &memorydb.ListTagsOutput{TagList: f.tags}, f.errs["ListTags"]
}

func (f *fakeMDB) DescribeClusters(context.Context, *memorydb.DescribeClustersInput, ...func(*memorydb.Options)) (*memorydb.DescribeClustersOutput, error) {
	return &memorydb.DescribeClustersOutput{Clusters: f.cls}, f.errs["DescribeClusters"]
}

func newFakeMDB() *fakeMDB {
	return &fakeMDB{
		users: []mdbtypes.User{{
			Name: aws.String("u1"), ARN: aws.String("arn:user"), Status: aws.String("active"), AccessString: aws.String("on ~a:*"),
			Authentication: &mdbtypes.Authentication{PasswordCount: aws.Int32(1), Type: mdbtypes.AuthenticationTypePassword},
		}},
		acls: []mdbtypes.ACL{{Name: aws.String("acl"), Status: aws.String("modifying"), UserNames: []string{"u1"}}},
		tags: []mdbtypes.Tag{{Key: aws.String("other"), Value: aws.String("x")}, {Key: aws.String(MarkerKey), Value: aws.String("ns/n/uid")}},
		cls: []mdbtypes.Cluster{{
			ARN: aws.String("arn:aws:memorydb:eu-west-1:111122223333:cluster:c"), EngineVersion: aws.String("7.3"),
			ClusterEndpoint: &mdbtypes.Endpoint{Address: aws.String("clustercfg.c.example.test"), Port: 6379},
		}},
		errs: map[string]error{},
	}
}

func TestMemoryDBLookup(t *testing.T) {
	ctx := context.Background()
	f := newFakeMDB()
	c := MemoryDB{API: f, ACLName: "acl"}
	got, err := c.Lookup(ctx, "u1")
	want := User{ARN: "arn:user", Status: "active", AccessString: "on ~a:*", Passwords: 1, Member: true, GroupStatus: "modifying", Marker: "ns/n/uid"}
	if err != nil || got != want {
		t.Errorf("Lookup = %+v, %v; want %+v", got, err, want)
	}

	f.users[0].Authentication = nil
	f.acls[0].UserNames = nil
	f.tags = nil
	if got, err = c.Lookup(ctx, "u1"); err != nil || got.Member || got.Marker != "" || got.Passwords != 0 {
		t.Errorf("Lookup = %+v, %v", got, err)
	}

	for name, tc := range map[string]struct {
		mutate func(f *fakeMDB)
		want   error
	}{
		"user fault": {func(f *fakeMDB) { f.errs["DescribeUsers"] = &mdbtypes.UserNotFoundFault{} }, ErrNotFound},
		"no user":    {func(f *fakeMDB) { f.users = nil }, ErrNotFound},
		"describe":   {func(f *fakeMDB) { f.errs["DescribeUsers"] = errAPI }, errAPI},
		"acl fault":  {func(f *fakeMDB) { f.errs["DescribeACLs"] = &mdbtypes.ACLNotFoundFault{} }, ErrTargetNotFound},
		"no acl":     {func(f *fakeMDB) { f.acls = nil }, ErrTargetNotFound},
		"acl":        {func(f *fakeMDB) { f.errs["DescribeACLs"] = errAPI }, errAPI},
		"tags":       {func(f *fakeMDB) { f.errs["ListTags"] = errAPI }, errAPI},
	} {
		f := newFakeMDB()
		tc.mutate(f)
		if _, err := (MemoryDB{API: f, ACLName: "acl"}).Lookup(ctx, "u1"); !errors.Is(err, tc.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestMemoryDBWrites(t *testing.T) {
	ctx := context.Background()
	f := newFakeMDB()
	c := MemoryDB{API: f, ACLName: "acl"}

	arn, err := c.Create(ctx, "u1", Spec{AccessString: "on ~a:*", Passwords: []string{"p"}, Marker: "ns/n/uid"})
	in := f.created
	if err != nil || arn != "arn:user" || aws.ToString(in.UserName) != "u1" || aws.ToString(in.AccessString) != "on ~a:*" ||
		in.AuthenticationMode.Type != mdbtypes.InputAuthenticationTypePassword || !slices.Equal(in.AuthenticationMode.Passwords, []string{"p"}) ||
		len(in.Tags) != 1 || aws.ToString(in.Tags[0].Key) != MarkerKey || aws.ToString(in.Tags[0].Value) != "ns/n/uid" {
		t.Errorf("Create: %q, %v, %+v", arn, err, in)
	}
	if _, err = c.Create(ctx, "u1", Spec{IAM: true}); err != nil || f.created.AuthenticationMode.Type != mdbtypes.InputAuthenticationTypeIam ||
		len(f.created.AuthenticationMode.Passwords) != 0 {
		t.Errorf("Create IAM: %v, %+v", err, f.created)
	}
	f.errs["CreateUser"] = errAPI
	if _, err = c.Create(ctx, "u1", Spec{}); !errors.Is(err, errAPI) {
		t.Errorf("Create failure: %v", err)
	}

	if err = c.Modify(ctx, "u1", Spec{AccessString: "on ~b:*"}); err != nil || aws.ToString(f.modified.AccessString) != "on ~b:*" || f.modified.AuthenticationMode != nil {
		t.Errorf("Modify access: %v, %+v", err, f.modified)
	}
	if err = c.Modify(ctx, "u1", Spec{Passwords: []string{"a", "b"}}); err != nil || f.modified.AccessString != nil ||
		!slices.Equal(f.modified.AuthenticationMode.Passwords, []string{"a", "b"}) {
		t.Errorf("Modify passwords: %v, %+v", err, f.modified)
	}
	f.errs["UpdateUser"] = errAPI
	if err = c.Modify(ctx, "u1", Spec{}); !errors.Is(err, errAPI) {
		t.Errorf("Modify failure: %v", err)
	}

	if err = c.Join(ctx, "u1"); err != nil || aws.ToString(f.joined.ACLName) != "acl" || !slices.Equal(f.joined.UserNamesToAdd, []string{"u1"}) {
		t.Errorf("Join: %v, %+v", err, f.joined)
	}
	f.errs["UpdateACL"] = errAPI
	if err = c.Join(ctx, "u1"); !errors.Is(err, errAPI) {
		t.Errorf("Join failure: %v", err)
	}

	if err = c.Delete(ctx, "u1"); err != nil || aws.ToString(f.deleted.UserName) != "u1" {
		t.Errorf("Delete: %v, %+v", err, f.deleted)
	}
	f.errs["DeleteUser"] = &mdbtypes.UserNotFoundFault{}
	if err = c.Delete(ctx, "u1"); err != nil {
		t.Errorf("Delete of a user that is gone: %v", err)
	}
	f.errs["DeleteUser"] = errAPI
	if err = c.Delete(ctx, "u1"); !errors.Is(err, errAPI) {
		t.Errorf("Delete failure: %v", err)
	}
}

// The constructors wrap the SDK clients of a configuration; they make no call.
func TestConstructors(t *testing.T) {
	cfg := aws.Config{Region: "eu-west-1"}
	if c := NewElastiCache(cfg, "g", "rg"); c.API == nil || c.UserGroupID != "g" || c.ReplicationGroupID != "rg" {
		t.Errorf("NewElastiCache = %+v", c)
	}
	if c := NewMemoryDB(cfg, "acl", "c"); c.API == nil || c.ACLName != "acl" || c.ClusterName != "c" {
		t.Errorf("NewMemoryDB = %+v", c)
	}
}

func TestAccountOf(t *testing.T) {
	for in, want := range map[string]string{
		"arn:aws:elasticache:eu-west-1:111122223333:replicationgroup:rg": "111122223333",
		"arn:aws:memorydb:eu-west-1::cluster:c":                          "",
		"not-an-arn":                                                     "", "": "",
	} {
		if got := accountOf(in); got != want {
			t.Errorf("accountOf(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestElastiCacheDescribe(t *testing.T) {
	ctx := context.Background()
	f := newFakeEC()
	c := ElastiCache{API: f, UserGroupID: "g", ReplicationGroupID: "rg"}
	got, err := c.Describe(ctx)
	want := Target{Endpoint: "master.rg.example.test", Port: 6379, ResourceID: "rg", AccountID: "111122223333"}
	if err != nil || got != want {
		t.Errorf("Describe = %+v, %v; want %+v", got, err, want)
	}

	// A cluster-mode group has a configuration endpoint, which wins.
	f.rgs[0].ConfigurationEndpoint = &ectypes.Endpoint{Address: aws.String("clustercfg.rg.example.test"), Port: aws.Int32(6380)}
	if got, err = c.Describe(ctx); err != nil || got.Endpoint != "clustercfg.rg.example.test" || got.Port != 6380 {
		t.Errorf("Describe in cluster mode = %+v, %v", got, err)
	}

	// A group that is still being created has no endpoint yet.
	f.rgs[0] = ectypes.ReplicationGroup{}
	if got, err = c.Describe(ctx); err != nil || got.Endpoint != "" || got.Port != 0 || got.ResourceID != "rg" || got.AccountID != "" {
		t.Errorf("Describe without an endpoint = %+v, %v", got, err)
	}

	for name, tc := range map[string]struct {
		mutate func(f *fakeEC)
		want   error
	}{
		"group missing": {func(f *fakeEC) { f.groups = nil }, ErrTargetNotFound},
		"group failure": {func(f *fakeEC) { f.errs["DescribeUserGroups"] = errAPI }, errAPI},
		"rg fault":      {func(f *fakeEC) { f.errs["DescribeReplicationGroups"] = &ectypes.ReplicationGroupNotFoundFault{} }, ErrResourceNotFound},
		"no rg":         {func(f *fakeEC) { f.rgs = nil }, ErrResourceNotFound},
		"rg failure":    {func(f *fakeEC) { f.errs["DescribeReplicationGroups"] = errAPI }, errAPI},
	} {
		f := newFakeEC()
		tc.mutate(f)
		if _, err := (ElastiCache{API: f, UserGroupID: "g", ReplicationGroupID: "rg"}).Describe(ctx); !errors.Is(err, tc.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestMemoryDBDescribe(t *testing.T) {
	ctx := context.Background()
	f := newFakeMDB()
	c := MemoryDB{API: f, ACLName: "acl", ClusterName: "c"}
	got, err := c.Describe(ctx)
	want := Target{Endpoint: "clustercfg.c.example.test", Port: 6379, ResourceID: "c", AccountID: "111122223333", Version: "7.3"}
	if err != nil || got != want {
		t.Errorf("Describe = %+v, %v; want %+v", got, err, want)
	}

	f.cls[0] = mdbtypes.Cluster{}
	if got, err = c.Describe(ctx); err != nil || got.Endpoint != "" || got.Port != 0 || got.ResourceID != "c" {
		t.Errorf("Describe without an endpoint = %+v, %v", got, err)
	}

	for name, tc := range map[string]struct {
		mutate func(f *fakeMDB)
		want   error
	}{
		"acl missing":     {func(f *fakeMDB) { f.acls = nil }, ErrTargetNotFound},
		"acl failure":     {func(f *fakeMDB) { f.errs["DescribeACLs"] = errAPI }, errAPI},
		"cluster fault":   {func(f *fakeMDB) { f.errs["DescribeClusters"] = &mdbtypes.ClusterNotFoundFault{} }, ErrResourceNotFound},
		"no cluster":      {func(f *fakeMDB) { f.cls = nil }, ErrResourceNotFound},
		"cluster failure": {func(f *fakeMDB) { f.errs["DescribeClusters"] = errAPI }, errAPI},
	} {
		f := newFakeMDB()
		tc.mutate(f)
		if _, err := (MemoryDB{API: f, ACLName: "acl", ClusterName: "c"}).Describe(ctx); !errors.Is(err, tc.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
}
