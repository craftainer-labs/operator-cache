// Package v1alpha1 holds the validating admission webhooks of the
// cache.craftainer.dev API. Defaults and nearly every rule live in the CRD
// schema (OpenAPI and CEL); the webhooks check only what CEL cannot do well:
// that a server's host is a host name or an IP address, and that its proxy URL
// parses, has a host and carries no credentials (credentials come only from
// Secret references). Admission errors never echo a host or a URL, because a
// misplaced value can hold a password. Updates of an object that is being
// deleted are always admitted, so finalizer removal is never blocked, for
// example while the webhook's own pod is down.
package v1alpha1
