package controller

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"

	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/craftainer-labs/lib-operator-go/pkg/cloud"
	"github.com/craftainer-labs/lib-operator-go/pkg/egress"
	"github.com/craftainer-labs/lib-operator-go/pkg/lifecycle"
	"github.com/craftainer-labs/lib-operator-go/pkg/server"

	"github.com/craftainer-labs/operator-cache/internal/awscache"
	"github.com/craftainer-labs/operator-cache/internal/valkeyacl"
)

var (
	// ErrBindingNotUsable means the binding Secret does not hold the user's
	// login, so the user cannot be updated in place; the next reconcile rotates.
	ErrBindingNotUsable = errors.New("the binding Secret does not hold the user's login; the next reconcile rotates it")
	// ErrServerNotReady means the server the claim names reports Ready=False.
	ErrServerNotReady = errors.New("the referenced server is not Ready")
	// ErrInvalidSpec means the profile or the namespace would not make the
	// rules of a tenant, which the schema and the webhook already enforce; the
	// controller checks again before it writes a rule to the server.
	ErrInvalidSpec = errors.New("the user's profile or namespace is not valid")
	// ErrAdminCredentials means the admin Secret lacks the username or password.
	ErrAdminCredentials = errors.New("the admin Secret needs the keys username and password")
	// ErrAuthModeUnsupported means the claim asks for authMode IAM on a server
	// that has no provider. The text starts with the claim reason of ADR 0002,
	// section 16.1, which is also the Event's reason.
	ErrAuthModeUnsupported = errors.New("AuthModeUnsupported: authMode IAM needs a server with a provider")
	// ErrIAMPrincipalRefused means iam.principal is set on AWS, where the login
	// is named by the operator and the consumer's role is authorized by the ARN
	// in the binding Secret.
	ErrIAMPrincipalRefused = errors.New("IAMPrincipalRequired: iam.principal is not used on AWS")
	// ErrPendingTimeout means the cloud has not finished changing the user, its
	// group or its ACL within PendingDeadline of the claim last going not Ready.
	ErrPendingTimeout = errors.New("the cloud did not finish changing the user in time; the operator keeps looking")
	// ErrAddressUnknown means the server sets no host and its probe has not
	// discovered an endpoint, so no binding can be built.
	ErrAddressUnknown = errors.New("the server's address is not known yet: it sets no host and none has been discovered")
	// ErrPasswordNotOnServer means the password the binding Secret holds is not
	// one of the user's, so no password is dropped.
	ErrPasswordNotOnServer = errors.New("the binding Secret's password is not one of the user's; nothing was changed")
)

// coarseError is what a condition and an Event show: one fixed text per kind of
// failure. The raw error (a server's own words, an address, a Secret's name) is
// written to the operator log only, and stays reachable through errors.Is and
// errors.As for the code that decides what to do.
type coarseError struct {
	text string
	errs []error
}

func (e *coarseError) Error() string   { return e.text }
func (e *coarseError) Unwrap() []error { return e.errs }

// rule maps the errors of one kind to the text shown for them. A rule with no
// text names an error of this package, whose own text is already fixed and
// passes through unchanged. unreachable marks the failures Reconcile reports
// as ServerUnreachable.
type rule struct {
	match       func(error) bool
	text        string
	unreachable bool
}

func is(target error) func(error) bool {
	return func(err error) bool { return errors.Is(err, target) }
}

// isTLS reports a failed TLS handshake: an alert, a bad record or a
// certificate that does not verify.
func isTLS(err error) bool {
	var (
		verify  *tls.CertificateVerificationError
		unknown x509.UnknownAuthorityError
		host    x509.HostnameError
		invalid x509.CertificateInvalidError
		alert   tls.AlertError
		record  tls.RecordHeaderError
	)
	return errors.As(err, &verify) || errors.As(err, &unknown) || errors.As(err, &host) ||
		errors.As(err, &invalid) || errors.As(err, &alert) || errors.As(err, &record)
}

// isEgress reports a connection the egress policy refused, or a proxy that
// could not set up the tunnel.
func isEgress(err error) bool {
	return errors.Is(err, egress.ErrBlocked) || errors.Is(err, egress.ErrProxy) || errors.Is(err, egress.ErrProxyUserinfo)
}

// rules are tried in order: the egress policy and TLS come before the
// valkeyacl kinds they are wrapped in.
var rules = []rule{
	{is(lifecycle.ErrNameCollision), "", false},
	{is(ErrBindingNotUsable), "", false},
	{is(ErrServerNotReady), "", false},
	{is(ErrInvalidSpec), "", false},
	{is(ErrPasswordNotOnServer), "", false},
	{is(ErrAuthModeUnsupported), "", false},
	{is(ErrIAMPrincipalRefused), "", false},
	{is(ErrAddressUnknown), "", false},
	{is(ErrPendingTimeout), "", false},
	{is(awscache.ErrTargetNotFound), "the user group or ACL the server names does not exist", false},
	{is(awscache.ErrResourceNotFound), "the replication group or cluster the server names does not exist", false},
	{is(cloud.ErrCredentialsUnavailable), "the server's cloud credentials are not usable", false},
	{is(cloud.ErrWorkloadIdentityForbidden), "the server's cloud credentials are not usable", false},
	{is(cloud.ErrEndpointOverrideForbidden), "the server's endpoint override is not allowed", false},
	{is(cloud.ErrInvalidEndpoint), "the server's endpoint override is not allowed", false},
	{is(server.ErrServerNotFound), "the referenced server was not found", false},
	{is(server.ErrNamespaceNotAllowed), "the claim's namespace is not allowed by the server", false},
	{is(server.ErrAdminSecretNamespaceForbidden), "the server's admin credentials are not usable", false},
	{is(server.ErrCredentialsSecretNamespaceForbidden), "the server's cloud credentials are not usable", false},
	{is(server.ErrCredentialsUnavailable), "the server's cloud credentials are not usable", false},
	{is(server.ErrInvalidServer), "the server's configuration or admin credentials are not usable", false},
	{is(server.ErrOperatorNamespaceUnset), "the server's configuration or admin credentials are not usable", false},
	{isEgress, "the connection to the server was refused by the egress policy", true},
	{isTLS, "the TLS handshake with the server failed", true},
	{is(valkeyacl.ErrAuth), "the server refused the operator's admin login", false},
	{is(valkeyacl.ErrTimeout), "the server did not answer in time", true},
	{is(valkeyacl.ErrUnavailable), "the connection to the server failed", true},
	{is(valkeyacl.ErrInvalid), "the server refused the request", false},
	{is(valkeyacl.ErrServer), "the server reported an error", false},
	{is(valkeyacl.ErrReplyLimit), "the server's reply was over the operator's limits", false},
}

// defaultText is shown for an error no rule knows: a failed call of the
// Kubernetes API, say.
const defaultText = "the operation failed; the operator log has the details"

// coarse turns err into what conditions and Events may carry, and logs the raw
// error with the operation it came from. nil stays nil.
func coarse(ctx context.Context, op string, err error) error {
	var (
		already *coarseError
		pending *lifecycle.PendingError
	)
	// A pending state is not a failure: it needs no text and no log line.
	if err == nil || errors.As(err, &already) || errors.As(err, &pending) {
		return err
	}
	text, unreachable := defaultText, false
	for _, r := range rules {
		if r.match(err) {
			if r.text == "" {
				return err
			}
			text, unreachable = r.text, r.unreachable
			break
		}
	}
	log.FromContext(ctx).Error(err, "the operation failed", "operation", op)
	errs := []error{err}
	if unreachable {
		errs = append(errs, lifecycle.ErrUnreachable)
	}
	return &coarseError{text: text, errs: errs}
}
