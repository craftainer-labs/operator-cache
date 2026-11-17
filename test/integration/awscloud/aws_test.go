//go:build integration && moto

package awscloud

// The AWS backends of the cache operator (NKcache-1-ii) against moto server 5.2.5
// and the fake of fake_test.go in front of it, both reached at private addresses
// (never loopback: pkg/egress refuses it in both modes) with the egress mode
// allow-private set in this test's own wiring only.
//
// What this does not check, because neither moto nor the fake can:
//   - the data plane: a login to a real cluster with a password or an IAM token
//     (IAM AUTH) is not made, and nothing is served on the endpoint a probe
//     discovers;
//   - real IRSA or Pod Identity: moto does not validate a web identity token, so
//     only the static-credentials source of cloud.Egress is used;
//   - AWS's own rules: its normalisation of an access string (the controller
//     compares by rule set for that reason), its limits and timings, which
//     passwords it accepts, its IAM permissions, its throttling, and the real
//     eventual consistency of a user group or ACL;
//   - the wire protocol of the MemoryDB SDK: the SDK (v1.43.0) speaks smithy RPC v2
//     CBOR by default, which moto does not serve. The MemoryDB client here is the
//     same awscache.MemoryDB with the SDK switched to AWS JSON 1.1, the protocol
//     moto implements, so the CBOR path is exercised only by real AWS.

import (
	"context"
	"crypto/rand"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/elasticache"
	"github.com/aws/aws-sdk-go-v2/service/memorydb"
	mdbschemas "github.com/aws/aws-sdk-go-v2/service/memorydb/schemas"
	"github.com/aws/smithy-go/transport/http/protocol/awsjson"

	"github.com/craftainer-labs/lib-operator-go/pkg/cloud"
	"github.com/craftainer-labs/lib-operator-go/pkg/egress"
	"github.com/craftainer-labs/lib-operator-go/pkg/lifecycle"
	libserver "github.com/craftainer-labs/lib-operator-go/pkg/server"

	cachev1alpha1 "github.com/craftainer-labs/operator-cache/api/v1alpha1"
	"github.com/craftainer-labs/operator-cache/internal/awscache"
	"github.com/craftainer-labs/operator-cache/internal/controller"
)

const region = "eu-west-1"

var (
	creds = map[string][]byte{cloud.KeyAWSAccessKeyID: []byte("test"), cloud.KeyAWSSecretAccessKey: []byte("test")}
	spec  = cachev1alpha1.CacheUserSpec{Profile: cachev1alpha1.ProfileReadWrite}
)

// engines are the two services the operator drives.
var engines = []string{"ElastiCache", "MemoryDB"}

// motoEndpoint is the moto server, from MOTO_ENDPOINT (http://<private ip>:5000).
func motoEndpoint(t *testing.T) string {
	t.Helper()
	v := os.Getenv("MOTO_ENDPOINT")
	if v == "" {
		t.Fatal("MOTO_ENDPOINT is not set; see README.md in the parent directory")
	}
	return v
}

func unique(prefix string) string { return prefix + "-" + strings.ToLower(rand.Text()[:8]) }

func timeout(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	return ctx
}

// mdbJSON is the MemoryDB client of the operator with the SDK on AWS JSON 1.1.
func mdbJSON(cfg aws.Config, acl, cluster string) awscache.MemoryDB {
	c := awscache.NewMemoryDB(cfg, acl, cluster)
	c.API = memorydb.NewFromConfig(cfg, jsonProtocol)
	return c
}

func jsonProtocol(o *memorydb.Options) { o.Protocol = awsjson.New11(mdbschemas.AmazonMemoryDB) }

// rig is one server of one engine: a user group or ACL in the fake, a replication
// group or cluster in moto, and the server object that names them.
type rig struct {
	engine string
	fake   *fake
	server *cachev1alpha1.ClusterCacheServer
	// newAWS is what the operator builds its client with (controller.NewAWSClient),
	// except that MemoryDB's is on JSON.
	newAWS controller.NewAWSFunc
	// resource is the replication group or cluster in moto.
	resource string
}

func newRig(t *testing.T, engine string) *rig {
	t.Helper()
	ctx := timeout(t)
	moto := motoEndpoint(t)
	id := unique("it")
	f := newFake(t, moto, region, "g-"+id, "acl-"+id)
	r := &rig{engine: engine, fake: f, resource: "r-" + id}

	direct, err := cloud.NewEgress(egress.ModeAllowPrivate, nil).AWS(ctx, cloud.ScopeCluster,
		cloud.AWSClusterBlock{AWSBlock: cloud.AWSBlock{Region: region}, EndpointOverride: moto},
		&cloud.Credentials{Source: cloud.SourceSecret}, creds)
	if err != nil {
		t.Fatalf("AWS configuration: %v", err)
	}
	p := &cachev1alpha1.ClusterAWSCache{AWSClusterBlock: cloud.AWSClusterBlock{AWSBlock: cloud.AWSBlock{Region: region}, EndpointOverride: f.URL()}}
	switch engine {
	case "ElastiCache":
		api := elasticache.NewFromConfig(direct)
		if _, err := api.CreateReplicationGroup(ctx, &elasticache.CreateReplicationGroupInput{
			ReplicationGroupId: aws.String(r.resource), ReplicationGroupDescription: aws.String("integration"),
		}); err != nil {
			t.Fatalf("create the replication group in moto: %v", err)
		}
		t.Cleanup(func() {
			_, _ = api.DeleteReplicationGroup(context.Background(), &elasticache.DeleteReplicationGroupInput{ReplicationGroupId: aws.String(r.resource)})
		})
		p.ElastiCache = &cachev1alpha1.ElastiCacheTarget{UserGroupID: f.group, ReplicationGroupID: r.resource}
		r.newAWS = controller.NewAWSClient
	case "MemoryDB":
		api := memorydb.NewFromConfig(direct, jsonProtocol)
		if _, err := api.CreateCluster(ctx, &memorydb.CreateClusterInput{
			ClusterName: aws.String(r.resource), NodeType: aws.String("db.t4g.small"), ACLName: aws.String(f.acl), Port: aws.Int32(6379),
		}); err != nil {
			t.Fatalf("create the cluster in moto: %v", err)
		}
		t.Cleanup(func() {
			_, _ = api.DeleteCluster(context.Background(), &memorydb.DeleteClusterInput{ClusterName: aws.String(r.resource)})
		})
		p.MemoryDB = &cachev1alpha1.MemoryDBTarget{ACLName: f.acl, ClusterName: r.resource}
		r.newAWS = func(ctx context.Context, s controller.AWSServer, eg cloud.Egress) (awscache.Client, error) {
			cfg, err := eg.AWS(ctx, s.Scope, s.Block, s.Credentials, s.Secret)
			if err != nil {
				return nil, err
			}
			return mdbJSON(cfg, s.MemoryDB.ACLName, s.MemoryDB.ClusterName), nil
		}
	}
	srv := &cachev1alpha1.ClusterCacheServer{}
	srv.Spec.Credentials = &cloud.Credentials{Source: cloud.SourceSecret}
	srv.Spec.Provider = &cachev1alpha1.ClusterProvider{Type: cloud.ProviderAWS, AWS: p}
	r.server = srv
	return r
}

func forEngines(t *testing.T, run func(t *testing.T, r *rig)) {
	t.Helper()
	for _, e := range engines {
		t.Run(e, func(t *testing.T) { run(t, newRig(t, e)) })
	}
}

func awsServer(s *cachev1alpha1.ClusterCacheServer) controller.AWSServer {
	p := s.Spec.Provider.AWS
	return controller.AWSServer{
		Scope: cloud.ScopeCluster, Block: p.AWSClusterBlock, Credentials: s.Spec.Credentials, Secret: creds,
		ElastiCache: p.ElastiCache, MemoryDB: p.MemoryDB,
	}
}

// api is the client of the server's user group or ACL, built as the operator
// builds it.
func (r *rig) api(t *testing.T) awscache.Client {
	t.Helper()
	c, err := r.newAWS(timeout(t), awsServer(r.server), cloud.NewEgress(egress.ModeAllowPrivate, nil))
	if err != nil {
		t.Fatalf("build the client: %v", err)
	}
	return c
}

func (r *rig) prober(mode egress.Mode) controller.Prober {
	return controller.Prober{Config: controller.Config{Egress: egress.NewPolicy(mode), EgressMode: mode, NewAWS: r.newAWS}}
}

// groupCalls are the operations only the fake answers, per engine.
func (r *rig) groupCalls() (read, write string) {
	if r.engine == "MemoryDB" {
		return "DescribeACLs", "UpdateACL"
	}
	return "DescribeUserGroups", "ModifyUserGroup"
}

func isPending(err error) bool {
	var p *lifecycle.PendingError
	return errors.As(err, &p)
}

// until calls observe until it stops reporting a wait for the cloud. Each wait is
// a requeue of the real reconciler; here it is the next call.
func until[S, O any](t *testing.T, observe func(context.Context, S) (O, error), s S) O {
	t.Helper()
	ctx := timeout(t)
	for range 20 {
		o, err := observe(ctx, s)
		if isPending(err) {
			continue
		}
		if err != nil {
			t.Fatalf("observe: %v", err)
		}
		return o
	}
	t.Errorf("the user did not settle")
	var zero O
	return zero
}

// The probe reads the server from the service: the data-plane address, the
// resource and the account, with the user group or ACL checked on the way.
func TestProbe(t *testing.T) {
	forEngines(t, func(t *testing.T, r *rig) {
		res, err := r.prober(egress.ModeAllowPrivate).ProbeCloud(timeout(t), r.server, libserver.CloudConnection{Credentials: creds})
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		d := res.Discovered
		if d[cachev1alpha1.DiscoveredRegion] != region || d[cachev1alpha1.DiscoveredResourceID] != r.resource || d[cachev1alpha1.DiscoveredPort] != "6379" {
			t.Errorf("discovered = %v", d)
		}
		if ep := d[cachev1alpha1.DiscoveredEndpoint]; !strings.Contains(ep, r.resource) || strings.Contains(ep, "://") {
			t.Errorf("endpoint = %q", ep)
		}
		// moto's default account; the key is left out where the service names no ARN.
		if a, ok := d[cachev1alpha1.DiscoveredAccountID]; ok && a != motoAccount {
			t.Errorf("accountId = %q", a)
		}
		for k := range d {
			switch k {
			case cachev1alpha1.DiscoveredRegion, cachev1alpha1.DiscoveredResourceID, cachev1alpha1.DiscoveredPort,
				cachev1alpha1.DiscoveredEndpoint, cachev1alpha1.DiscoveredAccountID:
			default:
				t.Errorf("an undrafted key was discovered: %q", k)
			}
		}
		if r.engine == "MemoryDB" && res.Version == "" {
			t.Error("MemoryDB reports the engine version")
		}
		read, _ := r.groupCalls()
		if r.fake.called(read) == 0 {
			t.Errorf("the fake never answered %s", read)
		}
	})
}

// A server that cannot be probed fails with a fixed text of its category, and no
// address of the fake or of moto is in it.
func TestProbeRefusals(t *testing.T) {
	forEngines(t, func(t *testing.T, r *rig) {
		ctx := timeout(t)
		check := func(what string, err error, kind error, text string) {
			t.Helper()
			if err == nil {
				t.Errorf("%s: no error", what)
				return
			}
			if !errors.Is(err, kind) || err.Error() != text {
				t.Errorf("%s: %v", what, err)
			}
			host := strings.TrimPrefix(r.fake.URL(), "http://")
			if strings.Contains(err.Error(), host) || strings.Contains(err.Error(), r.resource) {
				t.Errorf("%s: the error leaks the address or the resource: %v", what, err)
			}
		}

		// Strict mode refuses the endpoint override before any request.
		_, err := r.prober(egress.ModeStrict).ProbeCloud(ctx, r.server, libserver.CloudConnection{Credentials: creds})
		check("strict", err, cloud.ErrEndpointOverrideForbidden, "the server's endpoint override is not allowed")

		// A credentials Secret without the secret key.
		_, err = r.prober(egress.ModeAllowPrivate).ProbeCloud(ctx, r.server,
			libserver.CloudConnection{Credentials: map[string][]byte{cloud.KeyAWSAccessKeyID: []byte("test")}})
		check("credentials", err, cloud.ErrCredentialsUnavailable, "the server's cloud credentials are not usable")

		// A user group or ACL that does not exist.
		missing := r.server.DeepCopy()
		if p := missing.Spec.Provider.AWS; r.engine == "MemoryDB" {
			p.MemoryDB.ACLName = "no-such-acl"
		} else {
			p.ElastiCache.UserGroupID = "no-such-group"
		}
		_, err = r.prober(egress.ModeAllowPrivate).ProbeCloud(ctx, missing, libserver.CloudConnection{Credentials: creds})
		check("group", err, awscache.ErrTargetNotFound, "the user group or ACL the server names does not exist")

		// A replication group or cluster that does not exist.
		missing = r.server.DeepCopy()
		if p := missing.Spec.Provider.AWS; r.engine == "MemoryDB" {
			p.MemoryDB.ClusterName = "no-such-cluster"
		} else {
			p.ElastiCache.ReplicationGroupID = "no-such-group"
		}
		_, err = r.prober(egress.ModeAllowPrivate).ProbeCloud(ctx, missing, libserver.CloudConnection{Credentials: creds})
		check("resource", err, awscache.ErrResourceNotFound, "the replication group or cluster the server names does not exist")
	})
}

// A password user through its life: created and joined to the group or ACL,
// active, rotated with two passwords at once and back to one, an extra password
// removed again, and deleted.
func TestPasswordUserLifecycle(t *testing.T) {
	forEngines(t, func(t *testing.T, r *rig) {
		ctx := timeout(t)
		api := r.api(t)
		ns := unique("it")
		u := controller.NewIntegrationAWS(api, ns, "app", region, "cache.example.test", "6379")
		id := u.Claim().GetExternalName()
		t.Cleanup(func() { _ = api.Delete(context.Background(), id) })

		if obs, err := u.Observe(ctx, spec); err != nil || obs.Exists {
			t.Fatalf("Observe of nobody: %+v, %v", obs, err)
		}

		// Create: one password, the tenant's rules, the claim's marker.
		data, err := u.Create(ctx, spec)
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		u.Binding = data
		pw := string(data["password"])
		if pw == "" || string(data["username"]) != id || string(data["uri"]) != "rediss://cache.example.test:6379" ||
			string(data["authMode"]) != "Password" || string(data["cloud"]) != "aws" || string(data["region"]) != region ||
			string(data["keyPrefix"]) != ns+":" {
			t.Fatalf("binding = %v", data.Keys())
		}
		if !strings.HasPrefix(u.Claim().GetExternalID(), "arn:") {
			t.Errorf("externalId = %q", u.Claim().GetExternalID())
		}
		obs := until(t, u.Observe, spec)
		if !obs.Exists || !obs.UpToDate {
			t.Fatalf("after Create: %+v", obs)
		}
		st, err := api.Lookup(ctx, id)
		if err != nil || st.Passwords != 1 || !st.Member || st.Marker != awscache.MarkerValue(ns, "app", "integration-aws") ||
			!strings.Contains(st.AccessString, "~"+ns+":*") {
			t.Fatalf("user = %+v, %v", st, err)
		}
		read, write := r.groupCalls()
		if r.fake.called(read) == 0 || r.fake.called(write) == 0 {
			t.Errorf("the fake did not answer %s and %s", read, write)
		}

		// A rotation: both passwords at once, then the new one alone.
		data2, _, err := u.RotateBegin(ctx, spec, obs.Current, lifecycle.RotationState{})
		if err != nil {
			t.Fatalf("RotateBegin: %v", err)
		}
		u.Binding = data2
		if string(data2["password"]) == pw {
			t.Fatal("the rotation kept the password")
		}
		until(t, u.Observe, spec)
		if st, err = api.Lookup(ctx, id); err != nil || st.Passwords != 2 {
			t.Fatalf("during the rotation: %+v, %v", st, err)
		}
		if err := u.RotateFinish(ctx, spec, obs.Current, lifecycle.RotationState{Active: "1", Sequence: 1}); err != nil {
			t.Fatalf("RotateFinish: %v", err)
		}
		obs = until(t, u.Observe, spec)
		if st, err = api.Lookup(ctx, id); err != nil || st.Passwords != 1 || !obs.UpToDate {
			t.Fatalf("after the rotation: %+v, %v, %+v", st, err, obs)
		}

		// Drift: a password the operator did not issue is removed, and the user's
		// own is kept.
		if err := api.Modify(ctx, id, awscache.Spec{Passwords: []string{string(data2["password"]), "extra-password-0123456789"}}); err != nil {
			t.Fatalf("add a password: %v", err)
		}
		obs = until(t, u.Observe, spec)
		if st, err = api.Lookup(ctx, id); err != nil || st.Passwords != 2 || obs.UpToDate {
			t.Fatalf("with an extra password: %+v, %v, %+v", st, err, obs)
		}
		if _, err := u.Update(ctx, spec, obs.Current); !isPending(err) {
			t.Fatalf("Update: %v", err)
		}
		obs = until(t, u.Observe, spec)
		if st, err = api.Lookup(ctx, id); err != nil || st.Passwords != 1 || !obs.UpToDate {
			t.Fatalf("after the repair: %+v, %v, %+v", st, err, obs)
		}

		// Delete: the cloud takes its time, then the user is gone.
		if err := u.Delete(ctx, obs.Current); !isPending(err) {
			t.Fatalf("Delete: %v", err)
		}
		if obs = until(t, u.Observe, spec); obs.Exists {
			t.Errorf("the user is still there: %+v", obs)
		}
		if _, err := api.Lookup(ctx, id); !errors.Is(err, awscache.ErrNotFound) {
			t.Errorf("Lookup after Delete: %v", err)
		}
	})
}

// An IAM user has no password and a binding that names its ARN; its binding is
// rebuilt from the user when the Secret is lost.
func TestIAMUserLifecycle(t *testing.T) {
	forEngines(t, func(t *testing.T, r *rig) {
		ctx := timeout(t)
		api := r.api(t)
		ns := unique("it")
		u := controller.NewIntegrationAWSIAM(api, ns, "app", region, "cache.example.test", "6379")
		id := u.Claim().GetExternalName()
		t.Cleanup(func() { _ = api.Delete(context.Background(), id) })

		data, err := u.Create(ctx, spec)
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		u.Binding = data
		arn := u.Claim().GetExternalID()
		if _, ok := data["password"]; ok || string(data["authMode"]) != "IAM" || string(data["iamResourceArn"]) != arn || !strings.HasPrefix(arn, "arn:") {
			t.Fatalf("binding = %v, arn %q", data.Keys(), arn)
		}
		obs := until(t, u.Observe, spec)
		st, err := api.Lookup(ctx, id)
		if !obs.Exists || !obs.UpToDate || err != nil || st.Passwords != 0 || !st.Member || st.Marker != awscache.MarkerValue(ns, "app", "integration-aws") {
			t.Fatalf("after Create: %+v, %+v, %v", obs, st, err)
		}

		// The Secret is lost: the same binding comes back from the user.
		rebuilt, err := u.RebuildBinding(ctx, spec, obs.Current)
		if err != nil || string(rebuilt["iamResourceArn"]) != string(data["iamResourceArn"]) || string(rebuilt["username"]) != id {
			t.Fatalf("RebuildBinding: %v, %v", rebuilt.Keys(), err)
		}
		if !u.RotationNotApplicable() {
			t.Error("an IAM login has something to rotate")
		}

		if err := u.Delete(ctx, obs.Current); !isPending(err) {
			t.Fatalf("Delete: %v", err)
		}
		if obs = until(t, u.Observe, spec); obs.Exists {
			t.Errorf("the user is still there: %+v", obs)
		}
	})
}

// A user of the claim's id that another claim made is never adopted, changed or
// deleted: the claim is refused as a name collision.
func TestNoAdoption(t *testing.T) {
	forEngines(t, func(t *testing.T, r *rig) {
		ctx := timeout(t)
		api := r.api(t)
		ns := unique("it")
		id := awscache.UserID(ns, "app")
		const foreignAccess = "on ~foreign:* -@all +get"
		foreign := awscache.Spec{AccessString: foreignAccess, Passwords: []string{"foreign-password-0123456789"}, Marker: "other/claim/uid"}
		if _, err := api.Create(ctx, id, foreign); err != nil {
			t.Fatalf("make the foreign user: %v", err)
		}
		t.Cleanup(func() { _ = api.Delete(context.Background(), id) })

		pw := controller.NewIntegrationAWS(api, ns, "app", region, "cache.example.test", "6379")
		if _, err := pw.Observe(ctx, spec); !errors.Is(err, lifecycle.ErrNameCollision) {
			t.Errorf("password claim: %v", err)
		}
		iam := controller.NewIntegrationAWSIAM(api, ns, "app", region, "cache.example.test", "6379")
		if _, err := iam.Observe(ctx, spec); !errors.Is(err, lifecycle.ErrNameCollision) {
			t.Errorf("IAM claim: %v", err)
		}

		st, err := api.Lookup(ctx, id)
		if err != nil || st.Marker != "other/claim/uid" || st.AccessString != foreignAccess || st.Passwords != 1 || st.Member {
			t.Errorf("the foreign user changed: %+v, %v", st, err)
		}
	})
}
