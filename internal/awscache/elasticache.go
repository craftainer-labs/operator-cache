package awscache

import (
	"cmp"
	"context"
	"errors"
	"slices"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/elasticache"
	"github.com/aws/aws-sdk-go-v2/service/elasticache/types"
)

// ECAPI is the part of the ElastiCache SDK client the package uses;
// *elasticache.Client is the implementation, and tests replace it.
type ECAPI interface {
	DescribeUsers(context.Context, *elasticache.DescribeUsersInput, ...func(*elasticache.Options)) (*elasticache.DescribeUsersOutput, error)
	CreateUser(context.Context, *elasticache.CreateUserInput, ...func(*elasticache.Options)) (*elasticache.CreateUserOutput, error)
	ModifyUser(context.Context, *elasticache.ModifyUserInput, ...func(*elasticache.Options)) (*elasticache.ModifyUserOutput, error)
	DeleteUser(context.Context, *elasticache.DeleteUserInput, ...func(*elasticache.Options)) (*elasticache.DeleteUserOutput, error)
	DescribeUserGroups(context.Context, *elasticache.DescribeUserGroupsInput, ...func(*elasticache.Options)) (*elasticache.DescribeUserGroupsOutput, error)
	ModifyUserGroup(context.Context, *elasticache.ModifyUserGroupInput, ...func(*elasticache.Options)) (*elasticache.ModifyUserGroupOutput, error)
	ListTagsForResource(context.Context, *elasticache.ListTagsForResourceInput, ...func(*elasticache.Options)) (*elasticache.ListTagsForResourceOutput, error)
	DescribeReplicationGroups(context.Context, *elasticache.DescribeReplicationGroupsInput, ...func(*elasticache.Options)) (*elasticache.DescribeReplicationGroupsOutput, error)
}

var _ ECAPI = (*elasticache.Client)(nil)

// ElastiCache is the Client of an ElastiCache user group.
type ElastiCache struct {
	API ECAPI
	// UserGroupID is the user group the administrator attached to the
	// replication group.
	UserGroupID string
	// ReplicationGroupID is the replication group the user group is attached
	// to; only the probe reads it.
	ReplicationGroupID string
}

var _ Client = ElastiCache{}

// NewElastiCache returns the Client of the user group on the SDK's client.
func NewElastiCache(cfg aws.Config, userGroupID, replicationGroupID string) ElastiCache {
	return ElastiCache{API: elasticache.NewFromConfig(cfg), UserGroupID: userGroupID, ReplicationGroupID: replicationGroupID}
}

// group reads the user group, or ErrTargetNotFound.
func (c ElastiCache) group(ctx context.Context) (types.UserGroup, error) {
	out, err := c.API.DescribeUserGroups(ctx, &elasticache.DescribeUserGroupsInput{UserGroupId: aws.String(c.UserGroupID)})
	var missing *types.UserGroupNotFoundFault
	switch {
	case errors.As(err, &missing):
		return types.UserGroup{}, ErrTargetNotFound
	case err != nil:
		return types.UserGroup{}, err
	case len(out.UserGroups) == 0:
		return types.UserGroup{}, ErrTargetNotFound
	}
	return out.UserGroups[0], nil
}

// Lookup implements Client.
func (c ElastiCache) Lookup(ctx context.Context, id string) (User, error) {
	out, err := c.API.DescribeUsers(ctx, &elasticache.DescribeUsersInput{UserId: aws.String(id)})
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
	g, err := c.group(ctx)
	if err != nil {
		return User{}, err
	}
	tags, err := c.API.ListTagsForResource(ctx, &elasticache.ListTagsForResourceInput{ResourceName: u.ARN})
	if err != nil {
		return User{}, err
	}
	res := User{
		ARN: aws.ToString(u.ARN), Status: aws.ToString(u.Status), AccessString: aws.ToString(u.AccessString),
		Member: slices.Contains(g.UserIds, id), GroupStatus: aws.ToString(g.Status),
	}
	if a := u.Authentication; a != nil {
		res.Passwords = int(aws.ToInt32(a.PasswordCount))
	}
	for _, t := range tags.TagList {
		if aws.ToString(t.Key) == MarkerKey {
			res.Marker = aws.ToString(t.Value)
		}
	}
	return res, nil
}

// ecAuthentication is the authentication mode a Spec asks for.
func (s Spec) ecAuthentication() *types.AuthenticationMode {
	if s.IAM {
		return &types.AuthenticationMode{Type: types.InputAuthenticationTypeIam}
	}
	return &types.AuthenticationMode{Type: types.InputAuthenticationTypePassword, Passwords: s.Passwords}
}

// Create implements Client. The user's engine is the user group's.
func (c ElastiCache) Create(ctx context.Context, id string, s Spec) (string, error) {
	g, err := c.group(ctx)
	if err != nil {
		return "", err
	}
	out, err := c.API.CreateUser(ctx, &elasticache.CreateUserInput{
		UserId: aws.String(id), UserName: aws.String(id), Engine: aws.String(cmp.Or(aws.ToString(g.Engine), "valkey")),
		AccessString: aws.String(s.AccessString), AuthenticationMode: s.ecAuthentication(),
		Tags: []types.Tag{{Key: aws.String(MarkerKey), Value: aws.String(s.Marker)}},
	})
	if err != nil {
		return "", err
	}
	return aws.ToString(out.ARN), nil
}

// Modify implements Client.
func (c ElastiCache) Modify(ctx context.Context, id string, s Spec) error {
	in := &elasticache.ModifyUserInput{UserId: aws.String(id)}
	if s.AccessString != "" {
		in.AccessString = aws.String(s.AccessString)
	}
	if s.Passwords != nil {
		in.AuthenticationMode = s.ecAuthentication()
	}
	_, err := c.API.ModifyUser(ctx, in)
	return err
}

// Join implements Client.
func (c ElastiCache) Join(ctx context.Context, id string) error {
	_, err := c.API.ModifyUserGroup(ctx, &elasticache.ModifyUserGroupInput{
		UserGroupId: aws.String(c.UserGroupID), UserIdsToAdd: []string{id},
	})
	return err
}

// Delete implements Client.
func (c ElastiCache) Delete(ctx context.Context, id string) error {
	_, err := c.API.DeleteUser(ctx, &elasticache.DeleteUserInput{UserId: aws.String(id)})
	var missing *types.UserNotFoundFault
	if errors.As(err, &missing) {
		return nil
	}
	return err
}

// Describe implements Client. The address is the replication group's
// configuration endpoint (cluster mode) or the primary endpoint of its first
// node group.
func (c ElastiCache) Describe(ctx context.Context) (Target, error) {
	if _, err := c.group(ctx); err != nil {
		return Target{}, err
	}
	out, err := c.API.DescribeReplicationGroups(ctx, &elasticache.DescribeReplicationGroupsInput{ReplicationGroupId: aws.String(c.ReplicationGroupID)})
	var missing *types.ReplicationGroupNotFoundFault
	switch {
	case errors.As(err, &missing):
		return Target{}, ErrResourceNotFound
	case err != nil:
		return Target{}, err
	case len(out.ReplicationGroups) == 0:
		return Target{}, ErrResourceNotFound
	}
	g := out.ReplicationGroups[0]
	t := Target{ResourceID: c.ReplicationGroupID, AccountID: accountOf(aws.ToString(g.ARN))}
	ep := g.ConfigurationEndpoint
	if ep == nil && len(g.NodeGroups) > 0 {
		ep = g.NodeGroups[0].PrimaryEndpoint
	}
	if ep != nil {
		t.Endpoint, t.Port = aws.ToString(ep.Address), aws.ToInt32(ep.Port)
	}
	return t, nil
}
