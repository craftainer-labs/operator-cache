# operator-cache

> [!NOTE]
> This repository is vibe coded with [Claude](https://claude.com/product/claude-code).

A Kubernetes operator that provisions ACL users on
[Valkey](https://valkey.io/) and Redis servers. A team claims a user with a
`CacheUser`; the operator creates it on the server, limits it to the namespace's
key prefix, and writes the login to a binding Secret. It is built on
[`lib-operator-go`](https://github.com/craftainer-labs/lib-operator-go) and
kubebuilder v4, from the `template-k8s-operator` template. This repository holds
the API (types, CRDs, admission webhooks); the controller follows.

## Custom resources

All three are in the API group `cache.craftainer.dev/v1alpha1`. Samples are in
[`config/samples/`](config/samples/).

| Kind | Scope | What it is |
| --- | --- | --- |
| `ClusterCacheServer` | cluster | A server many namespaces may claim from. `allowedNamespaces` is required (it fails closed) and `adminCredentialsSecretRef.namespace` is required. |
| `CacheServer` | namespaced | A server only claims in its own namespace may use. Its admin Secret is in the same namespace. |
| `CacheUser` | namespaced | An ACL user on a server (`serverRef`), with a `profile` (`ReadOnly` or `ReadWrite`) and optional `channels`. Its login is written to a binding Secret of type `servicebinding.io/redis`. |

Both server kinds set `host`, an admin credentials Secret (keys `username` and
`password`), and optionally `port` (default 6379), `mode` (only `Standalone`),
`tls`, `proxyURL` and `healthCheckInterval`. The operator reaches a server over
TLS only. The key pattern of a user is always the tenant's key prefix,
`<namespace>:*`, and the channel pattern `&<namespace>:*`; neither is a field,
so a claim cannot name another namespace's keys. `@dangerous` and `@admin`
commands are never granted. A `CacheUser` has no `deletionPolicy`: its ACL user
is always deleted with the claim.

## Install

The operator needs a Kubernetes cluster at 1.33 or newer and
[cert-manager](https://cert-manager.io/) for its webhook certificates. A release
publishes three artifacts to the OCI registry (see "Releases" below):

- a Helm chart, `helm install operator-cache oci://<registry>/<path>/operator-cache --version X.Y.Z -n operator-cache-system --create-namespace`;
- an OLM bundle and a file-based catalog, installable with OLM v0 and OLM v1's
  `ClusterExtension`;
- the distroless `nonroot` image the chart and bundle run.

The registry and path come from the repository's `OCI_REGISTRY` variable and
the release workflow. From a checkout, `make deploy` installs the kustomize
manifests of `config/default`.

## Development

Open the repository in its devcontainer (the Go and Kubernetes tools are
installed there), then:

```sh
make test          # unit + envtest, race detector, coverage gate
make test-e2e      # kind cluster + Chainsaw (needs ~2 GB free memory and Docker)
make helm-lint     # lint the chart, check it matches config/
make bundle-validate   # OLM bundle (operator-sdk) and catalog (opm)
make operator-help # every operator target
prek run --all-files --hook-stage manual   # all checks
```

## Releases

Releases go through `release.yml` as in `template-docker`; right after each
one, dispatch `release-extras.yml` to publish the chart, bundle and catalog
and to sign and attest them:

```sh
gh workflow run release-extras.yml -f tag=vX.Y.Z
```

It never publishes over an existing tag. If a run failed part-way (say the
chart and bundle were pushed and signing failed), finish it with
`-f resume=true`: artifacts whose tag exists are not pushed again, their
existing digests are signed (not again if they already verify) but never
attested, and missing ones are published and attested. SLSA provenance must
describe the run that built the artifact, so a resumed run does not claim it:
an existing artifact without provenance is named in a warning, and if
provenance is required for it, cut a new patch version instead.

## License

Licensed under the [GNU General Public License v3.0](LICENSE).
