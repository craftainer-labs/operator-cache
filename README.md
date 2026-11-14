# operator-cache

> [!NOTE]
> This repository is vibe coded with [Claude](https://claude.com/product/claude-code).

A Kubernetes operator that provisions ACL users on
[Valkey](https://valkey.io/) and Redis servers. A team claims a user with a
`CacheUser`; the operator creates it on the server, limits it to the namespace's
key prefix, and writes the login to a binding Secret. It is built on
[`lib-operator-go`](https://github.com/craftainer-labs/lib-operator-go) and
kubebuilder v4, from the `template-k8s-operator` template.

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

## Controllers

One controller, `CacheUser`, built on `lifecycle.Reconcile` of lib-operator-go.
It watches the claims, the binding Secrets it owns and the servers the claims
name (a field index on `spec.serverRef` maps a server to its users, so a user
that waits for its server is reconciled when the server is; a server's status
writes that change nothing else do not wake its users).

For each `CacheUser` the operator makes **one ACL user** on the server named by
`serverRef`, with `ACL SETUSER`:

- its name is `<namespace>-<claim name>` cut to fit and ending in an 80-bit hash
  (at most 128 bytes), kept in `status.externalName`. There is no field that
  sets it;
- its rules are built from the profile and the namespace and nothing else: `on`,
  the key pattern `<namespace>:*` (`%R~` for `ReadOnly`), the channel pattern
  `&<namespace>:*` only with `channels`, the `@read` commands (and `@write` for
  `ReadWrite`, `@pubsub` with `channels`) and never `@dangerous`, `@admin`,
  `@scripting`, `SCAN`, `RANDOMKEY`, `DBSIZE`, `PUBSUB` or `CLIENT TRACKING`. The
  rules are rebuilt and checked again at every write, and a user that someone
  widened by hand is repaired at the next reconcile;
- the binding Secret (type `servicebinding.io/redis`) holds `host`, `port`,
  `username`, `password`, `uri` (`rediss://host:port`, no credentials),
  `keyPrefix` (`<namespace>:`) and `sslrootcert` (the server's CA bundle).

A **rotation** (the `craftainer.dev/rotate` annotation or `rotationPolicy`)
keeps every password the user has and adds a new one (`resetpass #<hash>...
><new>`), writes the binding Secret, and after
`gracePeriod` cuts the user back to the binding's password (`resetpass
#<hash>`), which ends the old one. Retrying a rotation is safe: the
previous passwords come from the server, not from the Secret, so a retry after
the Secret was written but before the rotation was recorded does not cut off
consumers that still use the older password; the passwords of a failed attempt
stay until the retire. The retire first looks for the hash on the user
and, when it is not there, changes nothing and reports an error, so a user is
never left without a password. A change of the profile or of `channels`
reapplies the rules without `reset`, so the password and the sessions in use
survive; if the binding Secret is missing or does not hold the user, the
operator applies the rules, reports `the binding Secret does not hold the user's
login` and rotates on the next reconcile instead of resetting the password.
Deleting the claim deletes the user, which ends its sessions.

**`status.externalId` is the only proof that the claim made its user.** An ACL
user has no marker, so the claim records `create-issued` before the first
`ACL SETUSER` and the user's name after it. A user of the derived name that
exists while `status.externalId` is empty is a `NameCollision` and is never
changed or deleted (see below). A user a crashed reconcile left half made is
recognised by the record and finished. A claim that never issued a create is
released on deletion without the server being asked, which matters when the
server is unreachable or gone.

Conditions and Events carry one fixed text per kind of failure (the connection
was refused by the egress policy, the TLS handshake failed, the login was
refused, the server did not answer, the connection failed); the server's own
words, addresses and Secret names are in the operator log only. A server whose
`Ready` condition is `False` is not used (the claim waits and no connection is
made), except to delete a claim. The operator keeps one logged-in connection per
server between reconciles; it is replaced when the server object, its admin
credentials, its CA bundle or its proxy change (so a rotated admin password is
picked up), and closed when nobody has used it for ten minutes.

### Settings

The manager reads these from flags or the environment; the Helm chart sets them
from values (`values.schema.json` checks the two enums and refuses `extraEnv`
entries that would override the variables the chart sets).

| Setting | Environment | Chart value | Meaning |
| --- | --- | --- | --- |
| `--max-concurrent-reconciles` | none | none (`1`) | How many CacheUsers reconcile at once. The default `1` serializes them, so one reconcile that waits on an unreachable server (until its connection timeout) holds up every other user. Raise it to bound that; it must be at least 1. |
| `--egress-mode` | `EGRESS_MODE` | `egressMode` (`strict`) | Which server and proxy addresses the operator may not dial: `strict` refuses loopback, link-local, cloud metadata and the private ranges; `allow-private` allows the private ranges. Set `allow-private` when the server is an in-cluster Service. |
| `--operator-namespace` | `POD_NAMESPACE` | (downward API) | The namespace the admin Secrets and CA bundles of cluster servers are read from. Without it, and without the pod's service account, startup fails. |
| `--default-data-deletion-policy` | `DEFAULT_DATA_DELETION_POLICY` | `defaultDataDeletionPolicy` (`Retain`) | The default for data kinds. `CacheUser` is a credential kind and has no `deletionPolicy`, so it is only validated. |

### Resolving a NameCollision

`Synced=False`, reason `NameCollision`, and a message that a user of that name
exists and this claim did not make it, mean the server already has an ACL user
named `status.externalName` that the operator did not create. The operator
leaves it alone and does not retry with backoff. Either:

1. delete the user on the server yourself (`ACL DELUSER <name>`), after checking
   nobody uses it, and the next reconcile creates it; or
2. rename the `CacheUser`, which gives it another name.

Deleting a claim in this state releases it without touching the server; the
foreign user stays.

### The deletion rule

This is the rule every Craftainer operator follows when a claim is deleted:

1. A claim that never issued a create (`status.externalId` is not set) is
   released without contacting the server. There is nothing of its own to remove.
2. Otherwise the operator deletes the user it recorded; one that is already gone
   is a success.
3. The operator does not look at a server's `Ready` condition to delete; it
   connects whenever the claim issued a create.

## Known limits

- **A create is not atomic.** Valkey has no "create unless exists" for
  `ACL SETUSER`: the operator asks `ACL GETUSER` first and sends `ACL SETUSER`
  after, so a client that makes a user of the same name in between has it reset.
  The name carries an 80-bit hash and the window is one round trip. Give the
  operator dedicated administrator credentials.
- **Standalone servers only.** ACLs are per node, so Cluster and Sentinel
  layouts (every command to every node) are out of v1alpha1.
- **Changing a rule or a password ends the sessions that hold it.** The server
  disconnects a user whose rules or passwords are removed, so a revoked right
  takes effect at once; a rotation's first phase only adds a password.
- **A retried rotation after the binding Secret was written but not marked**
  reads the new password as the current one and drops the previous generation's
  early. The window is the two writes between `RotateBegin` and the mark.
- **Changes to a server's admin Secret or CA bundle are picked up at the next
  poll** (ten minutes by default), not at once: the operator watches the server
  object and the binding Secrets, not the Secrets a server names. A changed
  server spec is seen at once, and a changed credential replaces the connection
  at the next reconcile.
- **A server that does not answer holds up other users for the connect timeout
  (ten seconds),** because the controller runs one worker unless
  `--max-concurrent-reconciles` is raised and the connection cache connects with
  its lock held. The cache is safe for concurrent reconciles: a connection that
  is replaced while a reconcile uses it is closed when that reconcile ends, and a
  timer (checking every five minutes) closes the connections nobody has used for
  ten minutes.
- **The server's health is not probed yet.** `ClusterCacheServer` and
  `CacheServer` have no `Ready` condition until the probe lands; a claim uses a
  server with no condition.

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
