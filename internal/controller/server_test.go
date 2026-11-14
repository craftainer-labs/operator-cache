package controller

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/craftainer-labs/lib-operator-go/pkg/apis/common"
	"github.com/craftainer-labs/lib-operator-go/pkg/conditions"
	"github.com/craftainer-labs/lib-operator-go/pkg/lifecycle"
	"github.com/craftainer-labs/lib-operator-go/pkg/server"

	"github.com/craftainer-labs/operator-cache/internal/egress"
	"github.com/craftainer-labs/operator-cache/internal/valkeyacl"
)

func TestStampCoversEverythingTheConnectionIsMadeFrom(t *testing.T) {
	o := serverObject{uid: "uid", generation: 1}
	cfg := valkeyacl.Config{Address: "v:6379", Username: "u", Password: "p", CA: []byte("ca")}
	proxy, _ := url.Parse("http://proxy:3128")
	base := stamp(o, cfg, nil)
	if stamp(o, cfg, nil) != base || !strings.HasPrefix(base, "uid/1/") {
		t.Errorf("the stamp is not stable: %q", base)
	}
	with := func(mutate func(*valkeyacl.Config)) string {
		c := cfg
		mutate(&c)
		return stamp(o, c, nil)
	}
	for _, tc := range []struct {
		what string
		got  string
	}{
		{"proxy", stamp(o, cfg, proxy)},
		{"uid", stamp(serverObject{uid: "other", generation: 1}, cfg, nil)},
		{"generation", stamp(serverObject{uid: "uid", generation: 2}, cfg, nil)},
		{"address", with(func(c *valkeyacl.Config) { c.Address = "v:6380" })},
		{"admin password", with(func(c *valkeyacl.Config) { c.Password = "q" })},
		{"admin user", with(func(c *valkeyacl.Config) { c.Username = "v" })},
		{"CA bundle", with(func(c *valkeyacl.Config) { c.CA = []byte("cb") })},
		{"server name", with(func(c *valkeyacl.Config) { c.ServerName = "n" })},
		{"min TLS", with(func(c *valkeyacl.Config) { c.MinTLS = tls.VersionTLS13 })},
		{"field split", with(func(c *valkeyacl.Config) { c.Username, c.Password = "up", "" })},
	} {
		if tc.got == base {
			t.Errorf("a different %s gives the same stamp", tc.what)
		}
	}
}

func TestAdminValkey(t *testing.T) {
	if adminValkey(nil) != nil {
		t.Error("no Admin is no Valkey")
	}
	if adminValkey(&valkeyacl.Admin{}) == nil {
		t.Error("an Admin is a Valkey")
	}
	// A connection that cannot be made is an error and no Valkey.
	v, err := ConnectAdmin(context.Background(), valkeyacl.Config{Address: "127.0.0.1:1"})
	if v != nil || !errors.Is(err, valkeyacl.ErrInvalid) {
		t.Errorf("ConnectAdmin without a dialer: %v, %v", v, err)
	}
}

// coarse maps each kind of failure to one fixed text and keeps the raw error for
// errors.Is and the log.
func TestCoarse(t *testing.T) {
	ctx := context.Background()
	leak := "10.9.9.9:6379 hunter2"
	wrap := func(kind error) error { return fmt.Errorf("%w: %s", kind, leak) }
	cases := []struct {
		name        string
		err         error
		text        string
		unreachable bool
	}{
		{"not found", wrap(server.ErrServerNotFound), "the referenced server was not found", false},
		{"namespace", wrap(server.ErrNamespaceNotAllowed), "the claim's namespace is not allowed by the server", false},
		{"admin namespace", wrap(server.ErrAdminSecretNamespaceForbidden), "the server's admin credentials are not usable", false},
		{"invalid server", wrap(server.ErrInvalidServer), "the server's configuration or admin credentials are not usable", false},
		{"operator namespace", wrap(server.ErrOperatorNamespaceUnset), "the server's configuration or admin credentials are not usable", false},
		{"egress", fmt.Errorf("%w: %w", valkeyacl.ErrUnavailable, wrap(egress.ErrBlocked)), "the connection to the server was refused by the egress policy", true},
		{"proxy", fmt.Errorf("%w: %w", valkeyacl.ErrUnavailable, wrap(egress.ErrProxy)), "the connection to the server was refused by the egress policy", true},
		{"proxy credentials", wrap(egress.ErrProxyUserinfo), "the connection to the server was refused by the egress policy", true},
		{"certificate", fmt.Errorf("%w: %w", valkeyacl.ErrUnavailable, &tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}), "the TLS handshake with the server failed", true},
		{"hostname", fmt.Errorf("%w: %w", valkeyacl.ErrUnavailable, x509.HostnameError{Certificate: &x509.Certificate{}, Host: leak}), "the TLS handshake with the server failed", true},
		{"invalid certificate", fmt.Errorf("%w: %w", valkeyacl.ErrUnavailable, x509.CertificateInvalidError{}), "the TLS handshake with the server failed", true},
		{"alert", fmt.Errorf("%w: %w", valkeyacl.ErrUnavailable, tls.AlertError(40)), "the TLS handshake with the server failed", true},
		{"record", fmt.Errorf("%w: %w", valkeyacl.ErrUnavailable, tls.RecordHeaderError{Msg: leak}), "the TLS handshake with the server failed", true},
		{"auth", wrap(valkeyacl.ErrAuth), "the server refused the operator's admin login", false},
		{"timeout", wrap(valkeyacl.ErrTimeout), "the server did not answer in time", true},
		{"unavailable", wrap(valkeyacl.ErrUnavailable), "the connection to the server failed", true},
		{"invalid", wrap(valkeyacl.ErrInvalid), "the server refused the request", false},
		{"server", wrap(valkeyacl.ErrServer), "the server reported an error", false},
		{"unknown", errors.New(leak), defaultText, false},
	}
	for _, tc := range cases {
		got := coarse(ctx, "op", tc.err)
		if got.Error() != tc.text || strings.Contains(got.Error(), "hunter2") {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.text)
		}
		if !errors.Is(got, tc.err) {
			t.Errorf("%s: the raw error is lost", tc.name)
		}
		if errors.Is(got, lifecycle.ErrUnreachable) != tc.unreachable {
			t.Errorf("%s: unreachable = %t", tc.name, !tc.unreachable)
		}
	}

	// The errors of this package already have a fixed text and pass through,
	// and so does an error that was made coarse.
	for _, err := range []error{
		fmt.Errorf("%w: %q", lifecycle.ErrNameCollision, "n"), ErrBindingNotUsable, ErrServerNotReady,
		fmt.Errorf("%w: x", ErrInvalidSpec), ErrPasswordNotOnServer,
	} {
		if got := coarse(ctx, "op", err); got.Error() != err.Error() || !errors.Is(got, err) {
			t.Errorf("%v changed to %v", err, got)
		}
	}
	once := coarse(ctx, "op", wrap(valkeyacl.ErrTimeout))
	if again := coarse(ctx, "op", once); again.Error() != once.Error() || !errors.Is(again, valkeyacl.ErrTimeout) {
		t.Error("coarse is not idempotent")
	}
	if coarse(ctx, "op", nil) != nil {
		t.Error("coarse does not keep nil")
	}
}

// The controller reuses the connection to a server until the server or its
// credentials change or the connection dies, and drops it when the server is
// gone.
func TestBackendCachesTheConnection(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, nil)
	h.cfg.Cache = NewServerCache(time.Hour)
	builds := func() int {
		h.fake.mu.Lock()
		defer h.fake.mu.Unlock()
		return len(h.fake.configs)
	}
	connect := func() {
		t.Helper()
		be, err := h.cfg.backend(ctx, h.ns, h.ref())
		if err != nil {
			t.Fatal(err)
		}
		defer be.release()
		if _, err := be.server(ctx); err != nil {
			t.Fatal(err)
		}
	}

	connect()
	connect()
	if builds() != 1 || h.fake.configs[0].MinTLS != tls.VersionTLS12 || h.fake.configs[0].Dial == nil ||
		h.fake.configs[0].Address != "valkey.example.test:6379" {
		t.Fatalf("builds = %d, configs = %+v", builds(), h.fake.configs)
	}

	// The admin password is rotated: the connection is made again, so it does
	// not go on with a login the server no longer accepts.
	var admin corev1.Secret
	h.must(env.Client.Get(ctx, client.ObjectKey{Namespace: h.ops, Name: h.server.Spec.AdminCredentialsSecretRef.Name}, &admin))
	admin.Data["password"] = []byte("rotated")
	h.must(env.Client.Update(ctx, &admin))
	connect()
	if builds() != 2 || h.fake.configs[1].Password != "rotated" {
		t.Errorf("after a credentials change: builds = %d", builds())
	}

	// The server changes (its generation), and brings the TLS settings with it.
	h.patch(h.server, func() {
		h.server.Spec.Port = new(int32)
		*h.server.Spec.Port = 6380
		h.server.Spec.TLS = &common.TLS{ServerName: "valkey.internal", MinVersion: common.TLS13}
	})
	connect()
	connect()
	if builds() != 3 || h.fake.configs[2].Address != "valkey.example.test:6380" || h.fake.configs[2].ServerName != "valkey.internal" ||
		h.fake.configs[2].MinTLS != tls.VersionTLS13 {
		t.Errorf("after a spec change: builds = %d, config = %+v", builds(), h.fake.configs[len(h.fake.configs)-1])
	}

	// The connection dies.
	h.fake.dead = true
	connect()
	if builds() != 4 {
		t.Errorf("after the connection died: builds = %d", builds())
	}
	h.fake.dead = false

	// The server is deleted: its entry goes.
	h.must(env.Client.Delete(ctx, h.server))
	if _, err := h.cfg.backend(ctx, h.ns, h.ref()); !errors.Is(err, server.ErrServerNotFound) {
		t.Errorf("a deleted server: %v", err)
	}
	if h.cfg.Cache.Len() != 0 {
		t.Errorf("len = %d", h.cfg.Cache.Len())
	}
}

// A server that became unusable keeps no connection either.
func TestBackendForgetsAnInvalidServer(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, nil)
	h.cfg.Cache = NewServerCache(time.Hour)
	be, err := h.cfg.backend(ctx, h.ns, h.ref())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := be.server(ctx); err != nil {
		t.Fatal(err)
	}
	be.release()
	h.patch(h.server, func() {
		h.server.Spec.TLS = &common.TLS{CABundle: &common.CABundle{SecretRef: &common.KeyReference{Name: "missing-ca"}}}
	})
	if _, err := h.cfg.backend(ctx, h.ns, h.ref()); err == nil {
		t.Fatal("a server with a missing CA Secret was resolved")
	}
	// The Secret is missing altogether, which is not ErrInvalidServer: the entry
	// stays for a transient failure of the API server, and the next success
	// replaces it.
	if h.cfg.Cache.Len() != 1 {
		t.Errorf("len = %d", h.cfg.Cache.Len())
	}

	// A CA Secret without the key is an invalid server: the entry goes.
	h.must(env.Client.Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "missing-ca", Namespace: h.ops}, Data: map[string][]byte{"other": []byte("x")}}))
	if _, err := h.cfg.backend(ctx, h.ns, h.ref()); !errors.Is(err, server.ErrInvalidServer) {
		t.Errorf("invalid server: %v", err)
	}
	if h.cfg.Cache.Len() != 0 {
		t.Errorf("len = %d", h.cfg.Cache.Len())
	}
}

// The status conditions Ready says False: only that blocks a claim.
func TestServerNotReady(t *testing.T) {
	cond := func(status metav1.ConditionStatus) []metav1.Condition {
		return []metav1.Condition{{Type: conditions.ServerReachable, Status: metav1.ConditionFalse}, {Type: conditions.Ready, Status: status}}
	}
	if serverNotReady(nil) || serverNotReady(cond(metav1.ConditionTrue)) || serverNotReady(cond(metav1.ConditionUnknown)) ||
		serverNotReady([]metav1.Condition{{Type: conditions.ServerReachable, Status: metav1.ConditionFalse}}) {
		t.Error("a server that does not say False is Ready")
	}
	if !serverNotReady(cond(metav1.ConditionFalse)) {
		t.Error("Ready=False is not Ready")
	}
}

func TestNames(t *testing.T) {
	// The long hash: 16 characters after the readable part.
	got := userName("tenant", "app")
	if !strings.HasPrefix(got, "tenant-app-") || len(got) != len("tenant-app-")+16 {
		t.Errorf("userName = %q", got)
	}
	if userName("tenant", "app") != got || userName("tenant", "app2") == got || userName("tenant2", "app") == got {
		t.Error("userName is not a function of the namespace and the name")
	}
	long := userName(strings.Repeat("n", 63), strings.Repeat("a", 253))
	if len(long) > maxUserNameLen {
		t.Errorf("long name: %d", len(long))
	}
	ref := common.ServerRef{Kind: kindCacheServer, Name: "s"}
	cluster := common.ServerRef{Kind: kindClusterCacheServer, Name: "s"}
	if serverKey("ns1", ref) == serverKey("ns2", ref) || serverKey("ns1", cluster) != serverKey("ns2", cluster) ||
		serverKey("ns", ref) == serverKey("ns", cluster) {
		t.Error("server keys")
	}
}
