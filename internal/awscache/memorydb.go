package awscache

import (
	"context"
	"errors"
	"slices"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/memorydb"
	"github.com/aws/aws-sdk-go-v2/service/memorydb/types"
)

// MDBAPI is the part of the MemoryDB SDK client the package uses;
// *memorydb.Client is the implementation, and tests replace it.
type MDBAPI interface {
	DescribeUsers(context.Context, *memorydb.DescribeUsersInput, ...func(*memorydb.Options)) (*memorydb.DescribeUsersOutput, error)
	CreateUser(context.Context, *memorydb.CreateUserInput, ...func(*memorydb.Options)) (*memorydb.CreateUserOutput, error)
	UpdateUser(context.Context, *memorydb.UpdateUserInput, ...func(*memorydb.Options)) (*memorydb.UpdateUserOutput, error)
	DeleteUser(context.Context, *memorydb.DeleteUserInput, ...func(*memorydb.Options)) (*memorydb.DeleteUserOutput, error)
	DescribeACLs(context.Context, *memorydb.DescribeACLsInput, ...func(*memorydb.Options)) (*memorydb.DescribeACLsOutput, error)
	UpdateACL(context.Context, *memorydb.UpdateACLInput, ...func(*memorydb.Options)) (*memorydb.UpdateACLOutput, error)
	ListTags(context.Context, *memorydb.ListTagsInput, ...func(*memorydb.Options)) (*memorydb.ListTagsOutput, error)
	DescribeClusters(context.Context, *memorydb.DescribeClustersInput, ...func(*memorydb.Options)) (*memorydb.DescribeClustersOutput, error)
}

var _ MDBAPI = (*memorydb.Client)(nil)

// MemoryDB is the Client of a MemoryDB ACL.
type MemoryDB struct {
	API MDBAPI
	// ACLName is the ACL the administrator attached to the cluster.
	ACLName string
	// ClusterName is the cluster the ACL is attached to; only the probe reads it.
	ClusterName string
}

var _ Client = MemoryDB{}

// NewMemoryDB returns the Client of the ACL on the SDK's client.
func NewMemoryDB(cfg aws.Config, aclName, clusterName string) MemoryDB {
	return MemoryDB{API: memorydb.NewFromConfig(cfg), ACLName: aclName, ClusterName: clusterName}
}

// acl reads the ACL, or ErrTargetNotFound.
func (c MemoryDB) acl(ctx context.Context) (types.ACL, error) {
	out, err := c.API.DescribeACLs(ctx, &memorydb.DescribeACLsInput{ACLName: aws.String(c.ACLName)})
	var missing *types.ACLNotFoundFault
	switch {
	case errors.As(err, &missing):
		return types.ACL{}, ErrTargetNotFound
	case err != nil:
		return types.ACL{}, err
	case len(out.ACLs) == 0:
		return types.ACL{}, ErrTargetNotFound
	}
	return out.ACLs[0], nil
}

// Lookup implements Client.
func (c MemoryDB) Lookup(ctx context.Context, id string) (User, error) {
	out, err := c.API.DescribeUsers(ctx, &memorydb.DescribeUsersInput{UserName: aws.String(id)})
	var missing *types.UserNotFoundFault
	switch {
	case errors.As(err, &missing):
		return User{}, ErrNotFound
	case err != nil:
		return User{}, err
	case len(out.Users) == 0:
		return User{}, ErrNotFound
	}
	u := out.Users[0]
	a, err := c.acl(ctx)
	if err != nil {
		return User{}, err
	}
	tags, err := c.API.ListTags(ctx, &memorydb.ListTagsInput{ResourceArn: u.ARN})
	if err != nil {
		return User{}, err
	}
	res := User{
		ARN: aws.ToString(u.ARN), Status: aws.ToString(u.Status), AccessString: aws.ToString(u.AccessString),
		Member: slices.Contains(a.UserNames, id), GroupStatus: aws.ToString(a.Status),
	}
	if au := u.Authentication; au != nil {
		res.Passwords = int(aws.ToInt32(au.PasswordCount))
	}
	for _, t := range tags.TagList {
		if aws.ToString(t.Key) == MarkerKey {
			res.Marker = aws.ToString(t.Value)
		}
	}
	return res, nil
}

// mdbAuthentication is the authentication mode a Spec asks for.
func (s Spec) mdbAuthentication() *types.AuthenticationMode {
	if s.IAM {
		return &types.AuthenticationMode{Type: types.InputAuthenticationTypeIam}
	}
	return &types.AuthenticationMode{Type: types.InputAuthenticationTypePassword, Passwords: s.Passwords}
}

// Create implements Client.
func (c MemoryDB) Create(ctx context.Context, id string, s Spec) (string, error) {
	out, err := c.API.CreateUser(ctx, &memorydb.CreateUserInput{
		UserName: aws.String(id), AccessString: aws.String(s.AccessString), AuthenticationMode: s.mdbAuthentication(),
		Tags: []types.Tag{{Key: aws.String(MarkerKey), Value: aws.String(s.Marker)}},
	})
	if err != nil {
		return "", err
	}
	return aws.ToString(out.User.ARN), nil
}

// Modify implements Client.
func (c MemoryDB) Modify(ctx context.Context, id string, s Spec) error {
	in := &memorydb.UpdateUserInput{UserName: aws.String(id)}
	if s.AccessString != "" {
		in.AccessString = aws.String(s.AccessString)
	}
	if s.Passwords != nil {
		in.AuthenticationMode = s.mdbAuthentication()
	}
	_, err := c.API.UpdateUser(ctx, in)
	return err
}

// Join implements Client.
func (c MemoryDB) Join(ctx context.Context, id string) error {
	_, err := c.API.UpdateACL(ctx, &memorydb.UpdateACLInput{ACLName: aws.String(c.ACLName), UserNamesToAdd: []string{id}})
	return err
}

// Delete implements Client.
func (c MemoryDB) Delete(ctx context.Context, id string) error {
	_, err := c.API.DeleteUser(ctx, &memorydb.DeleteUserInput{UserName: aws.String(id)})
	var missing *types.UserNotFoundFault
	if errors.As(err, &missing) {
		return nil
	}
	return err
}

// Describe implements Client. The address is the cluster's endpoint.
func (c MemoryDB) Describe(ctx context.Context) (Target, error) {
	if _, err := c.acl(ctx); err != nil {
		return Target{}, err
	}
	out, err := c.API.DescribeClusters(ctx, &memorydb.DescribeClustersInput{ClusterName: aws.String(c.ClusterName)})
	var missing *types.ClusterNotFoundFault
	switch {
	case errors.As(err, &missing):
		return Target{}, ErrResourceNotFound
	case err != nil:
		return Target{}, err
	case len(out.Clusters) == 0:
		return Target{}, ErrResourceNotFound
	}
	cl := out.Clusters[0]
	t := Target{ResourceID: c.ClusterName, AccountID: accountOf(aws.ToString(cl.ARN)), Version: aws.ToString(cl.EngineVersion)}
	if ep := cl.ClusterEndpoint; ep != nil {
		t.Endpoint, t.Port = aws.ToString(ep.Address), ep.Port
	}
	return t, nil
}
