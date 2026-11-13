package v1alpha1

import (
	"net"
	"net/url"
	"slices"
	"strconv"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"

	cachev1alpha1 "github.com/craftainer-labs/operator-cache/api/v1alpha1"
)

// redacted stands in for a host or URL in an error: a value can carry
// credentials (user:password@host, https://user:password@proxy), and an
// admission error is shown to the user and written to logs.
const redacted = "<redacted>"

// invalid turns a list of field errors into the 422 the API server returns,
// or nil for an empty list.
func invalid(kind, name string, errs field.ErrorList) error {
	if len(errs) == 0 {
		return nil
	}
	return apierrors.NewInvalid(schema.GroupKind{Group: cachev1alpha1.GroupVersion.Group, Kind: kind}, name, errs)
}

// validateServer checks the host and the proxy URL of a server, for both
// server kinds. Neither value appears in an error.
func validateServer(spec *cachev1alpha1.ServerConfig, proxyURL string) field.ErrorList {
	var errs field.ErrorList
	path := field.NewPath("spec")
	if !validHost(spec.Host) {
		errs = append(errs, field.Invalid(path.Child("host"), redacted, "must be a host name or an IP address"))
	}
	if proxyURL != "" {
		if msg := proxyProblem(proxyURL); msg != "" {
			errs = append(errs, field.Invalid(path.Child("proxyURL"), redacted, msg))
		}
	}
	return errs
}

// validHost reports whether host is an IP address or a DNS-1123 subdomain
// (case-insensitive, as host names are; one trailing dot, the fully qualified
// form, is accepted).
func validHost(host string) bool {
	if net.ParseIP(host) != nil {
		return true
	}
	return len(validation.IsDNS1123Subdomain(strings.ToLower(strings.TrimSuffix(host, ".")))) == 0
}

// proxyProblem says what is wrong with a proxy URL, or returns "" for a good
// one: an http, https or socks5 URL with a host and without userinfo.
func proxyProblem(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || !slices.Contains([]string{"http", "https", "socks5"}, u.Scheme) || u.Hostname() == "" {
		return "must be an http, https or socks5 URL with a host"
	}
	if p := u.Port(); p != "" {
		if n, err := strconv.Atoi(p); err != nil || n < 1 || n > 65535 {
			return "port must be between 1 and 65535"
		}
	}
	if u.User != nil {
		return "must not carry credentials (user:password@); proxy credentials are not supported"
	}
	return ""
}
