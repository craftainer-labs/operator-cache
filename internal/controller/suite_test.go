package controller

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/craftainer-labs/lib-operator-go/pkg/apis/common"
	"github.com/craftainer-labs/lib-operator-go/pkg/testenv"

	cachev1alpha1 "github.com/craftainer-labs/operator-cache/api/v1alpha1"
	"github.com/craftainer-labs/operator-cache/internal/egress"
	"github.com/craftainer-labs/operator-cache/internal/valkeyacl"
)

// The reconcile flow runs against envtest's kube-apiserver with an in-memory
// server behind the Valkey interface, so it needs no Valkey. The real server
// lane is test/integration. The suite needs the binaries scripts/go-coverage.sh
// and `make test` fetch with setup-envtest.

var env *testenv.Env

func TestMain(m *testing.M) {
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(cachev1alpha1.AddToScheme(scheme))
	var err error
	env, err = testenv.Start(context.Background(), testenv.Options{
		Scheme: scheme, CRDPaths: []string{filepath.Join("..", "..", "config", "crd", "bases")},
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "cannot start envtest:", err)
		os.Exit(1)
	}
	code := m.Run()
	if err := env.Stop(); err != nil {
		fmt.Fprintln(os.Stderr, "cannot stop envtest:", err)
		code = 1
	}
	os.Exit(code)
}

var errServer = errors.New("server failed")

// memUser is an ACL user of the in-memory server, as ACL GETUSER shows it.
type memUser struct {
	on                       bool
	hashes                   []string
	keys, channels, commands string
}

// apply runs ACL SETUSER rules in order, as far as the operator uses them.
func (u *memUser) apply(rules []string) {
	for _, r := range rules {
		switch {
		case r == "reset":
			*u = memUser{commands: "-@all"}
		case r == "on":
			u.on = true
		case r == "off":
			u.on = false
		case r == "resetpass":
			u.hashes = nil
		case r == "resetkeys":
			u.keys = ""
		case r == "resetchannels":
			u.channels = ""
		case r == "clearselectors":
		case strings.HasPrefix(r, ">"):
			u.addHash(valkeyacl.HashPassword(r[1:]))
		case strings.HasPrefix(r, "#"):
			u.addHash(r[1:])
		case strings.HasPrefix(r, "~"), strings.HasPrefix(r, "%R~"):
			u.keys = r
		case strings.HasPrefix(r, "&"):
			u.channels = r
		case r == "-@all":
			u.commands = r
		default:
			u.commands += " " + r
		}
	}
}

func (u *memUser) addHash(h string) {
	if !slices.Contains(u.hashes, h) {
		u.hashes = append(u.hashes, h)
	}
}

func (u *memUser) hasPassword(pw string) bool {
	return slices.Contains(u.hashes, valkeyacl.HashPassword(pw))
}

// memValkey is a Valkey in memory behind the Valkey interface. It records every
// call, fails the methods named in fail with the error given, and runs onCall at
// each call (after the call is recorded).
type memValkey struct {
	mu      sync.Mutex
	calls   []string
	users   map[string]*memUser
	fail    map[string]error
	dead    bool
	closed  int
	configs []valkeyacl.Config
	onCall  func(call string)
}

func newMemValkey() *memValkey {
	return &memValkey{users: map[string]*memUser{}, fail: map[string]error{}}
}

// enter records a call and returns the error injected for it.
func (m *memValkey) enter(call string) error {
	m.mu.Lock()
	m.calls = append(m.calls, call)
	name, _, _ := strings.Cut(call, " ")
	err, hook := m.fail[name], m.onCall
	m.mu.Unlock()
	if hook != nil {
		hook(call)
	}
	return err
}

func (m *memValkey) reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = nil
}

func (m *memValkey) joined() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return strings.Join(m.calls, "\n")
}

func (m *memValkey) want(t *testing.T, subs ...string) {
	t.Helper()
	for _, s := range subs {
		if !strings.Contains(m.joined(), s) {
			t.Errorf("no call contains %q; calls:\n%s", s, m.joined())
		}
	}
}

func (m *memValkey) wantNot(t *testing.T, subs ...string) {
	t.Helper()
	for _, s := range subs {
		if strings.Contains(m.joined(), s) {
			t.Errorf("a call contains %q; calls:\n%s", s, m.joined())
		}
	}
}

func (m *memValkey) user(name string) *memUser {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.users[name]
}

func (m *memValkey) put(name string, u *memUser) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.users[name] = u
}

func (m *memValkey) GetUser(_ context.Context, name string) (valkeyacl.UserInfo, error) {
	if err := m.enter("GetUser " + name); err != nil {
		return valkeyacl.UserInfo{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	u, ok := m.users[name]
	if !ok {
		return valkeyacl.UserInfo{}, fmt.Errorf("%w: %s", valkeyacl.ErrNotFound, name)
	}
	flags := []string{"off"}
	if u.on {
		flags = []string{"on"}
	}
	return valkeyacl.UserInfo{
		Flags: flags, PasswordHashes: slices.Clone(u.hashes), Commands: u.commands, Keys: u.keys, Channels: u.channels,
	}, nil
}

func (m *memValkey) SetUser(_ context.Context, name string, rules ...string) error {
	if err := m.enter("SetUser " + name + " " + strings.Join(rules, " ")); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	u, ok := m.users[name]
	if !ok {
		u = &memUser{commands: "-@all"}
		m.users[name] = u
	}
	u.apply(rules)
	return nil
}

func (m *memValkey) DelUser(_ context.Context, name string) (bool, error) {
	if err := m.enter("DelUser " + name); err != nil {
		return false, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.users[name]
	delete(m.users, name)
	return ok, nil
}

func (m *memValkey) Alive() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return !m.dead
}

func (m *memValkey) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed++
}

// harness is one test's namespaces, server, recorder and in-memory Valkey.
type harness struct {
	t      *testing.T
	ns     string // the tenant namespace
	ops    string // the operator namespace, holding the admin Secret
	server *cachev1alpha1.ClusterCacheServer
	fake   *memValkey
	rec    *testenv.Recorder
	cfg    Config
	now    time.Time

	// connectErr, when set, is what the next connections fail with.
	connectErr error
}

func newHarness(t *testing.T, mutate func(*cachev1alpha1.ClusterCacheServer)) *harness {
	t.Helper()
	h := &harness{t: t, ns: env.Namespace(t, nil), ops: env.Namespace(t, nil), fake: newMemValkey(), rec: &testenv.Recorder{}, now: time.Now()}
	name := "valkey-" + strings.ToLower(rand.Text())[:10]
	admin := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name + "-admin", Namespace: h.ops},
		Data:       map[string][]byte{"username": []byte("admin"), "password": []byte("admin-pw")},
	}
	srv := &cachev1alpha1.ClusterCacheServer{ObjectMeta: metav1.ObjectMeta{Name: name}}
	srv.Spec.Host = "valkey.example.test"
	srv.Spec.AdminCredentialsSecretRef = common.AdminSecretRef{Name: admin.Name, Namespace: h.ops}
	srv.Spec.AllowedNamespaces = common.AllowedNamespaces{From: common.NamespacesAll}
	if mutate != nil {
		mutate(srv)
	}
	h.must(env.Client.Create(context.Background(), admin))
	h.must(env.Client.Create(context.Background(), srv))
	t.Cleanup(func() { _ = env.Client.Delete(context.Background(), srv) })
	h.server = srv
	h.cfg = Config{
		Client: env.Client, APIReader: env.Client, Recorder: h.rec, ManagedBy: "operator-cache-test",
		OperatorNamespace: h.ops, DefaultDeletionPolicy: common.DeletionRetain, Egress: egress.NewPolicy(egress.ModeStrict),
		Connect: func(_ context.Context, c valkeyacl.Config) (Valkey, error) {
			h.fake.mu.Lock()
			defer h.fake.mu.Unlock()
			h.fake.configs = append(h.fake.configs, c)
			if h.connectErr != nil {
				return nil, h.connectErr
			}
			return h.fake, nil
		},
		Now: func() time.Time { return h.now },
	}
	return h
}

func (h *harness) must(err error) {
	h.t.Helper()
	if err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) ref() common.ServerRef {
	return common.ServerRef{Kind: kindClusterCacheServer, Name: h.server.Name}
}

// user creates a CacheUser of the tenant namespace on the harness's server.
func (h *harness) user(name string, mutate func(*cachev1alpha1.CacheUser)) *cachev1alpha1.CacheUser {
	h.t.Helper()
	u := &cachev1alpha1.CacheUser{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: h.ns}}
	u.Spec.ServerRef = h.ref()
	if mutate != nil {
		mutate(u)
	}
	h.must(env.Client.Create(context.Background(), u))
	return u
}

func (h *harness) reconcile(r reconcilerOf, o client.Object) error {
	h.t.Helper()
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(o)})
	return err
}

func (h *harness) reconcileOK(r reconcilerOf, o client.Object) {
	h.t.Helper()
	h.must(h.reconcile(r, o))
}

type reconcilerOf interface {
	Reconcile(context.Context, ctrl.Request) (ctrl.Result, error)
}

func (h *harness) get(o client.Object) {
	h.t.Helper()
	h.must(env.Client.Get(context.Background(), client.ObjectKeyFromObject(o), o))
}

func (h *harness) gone(o client.Object) bool {
	h.t.Helper()
	err := env.Client.Get(context.Background(), client.ObjectKeyFromObject(o), o)
	return apierrors.IsNotFound(err)
}

// patch applies mutate to a fresh copy of o and sends it as a merge patch.
func (h *harness) patch(o client.Object, mutate func()) {
	h.t.Helper()
	h.get(o)
	p := client.MergeFrom(o.DeepCopyObject().(client.Object))
	mutate()
	h.must(env.Client.Patch(context.Background(), o, p))
}

// condition returns the status, reason and message of a condition of a claim.
func condition(conds []metav1.Condition, typ string) (metav1.ConditionStatus, string, string) {
	c := meta.FindStatusCondition(conds, typ)
	if c == nil {
		return "", "", ""
	}
	return c.Status, c.Reason, c.Message
}

func (h *harness) wantCondition(conds []metav1.Condition, typ string, status metav1.ConditionStatus, reason string) string {
	h.t.Helper()
	s, r, msg := condition(conds, typ)
	if s != status || r != reason {
		h.t.Errorf("condition %s = %s/%s (%s), want %s/%s", typ, s, r, msg, status, reason)
	}
	return msg
}

// bindingSecret reads the binding Secret of a user as strings.
func (h *harness) bindingSecret(u *cachev1alpha1.CacheUser) map[string]string {
	h.t.Helper()
	var s corev1.Secret
	h.must(env.Client.Get(context.Background(), client.ObjectKey{Namespace: h.ns, Name: u.GetBindingSecretName()}, &s))
	if s.Type != "servicebinding.io/redis" {
		h.t.Errorf("Secret type = %q", s.Type)
	}
	out := map[string]string{}
	for k, v := range s.Data {
		out[k] = string(v)
	}
	return out
}
