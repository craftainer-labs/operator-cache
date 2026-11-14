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

One claim controller, `CacheUser`, built on `lifecycle.Reconcile` of lib-operator-go,
and one health probe per server kind (see "Server health").
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

### Server health

`ClusterCacheServer` and `CacheServer` get the `ServerStatus` their CRDs promise
from one `probe.Reconciler` of `lib-operator-go` (`pkg/probe`) per kind, wired in
`internal/app`:

- **What is probed:** a login as the ACL administrator (`ACL WHOAMI`) over
  verified TLS. An answer proves both things the status promises: the server is
  reachable and it accepts the admin credentials.
- **The version** is read on the same connection: `INFO server`'s
  `valkey_version` (`redis_version` on Redis), or, when the admin's ACL does not
  allow `INFO`, the `version` of `HELLO`. A value that is not 1 to 64 bytes of
  printable ASCII without spaces, or that neither command gives, leaves
  `status.serverVersion` empty; the server is still `Ready` and reachable,
  because reachability and accepted credentials are what the conditions say.
  Only a connection that fails during the read makes the probe fail.
- `status.lastProbeTime`: the last probe that ran and whose result was recorded
  (a server that cannot be probed, `InvalidServer`, has none). It is rewritten
  when the result changes and otherwise every 10 minutes, so a healthy fleet does
  not write one status per server per minute.
- `Ready` and `ServerReachable` are `True` (reason `Available`) when the probe
  succeeded, and `False` with reason `ServerUnreachable` when it did not
  (unreachable, TLS failure, refused login, or a failed command). A server whose
  admin Secret is in a forbidden namespace is `AdminSecretNamespaceForbidden`;
  one whose connection settings are missing or invalid (an absent Secret or CA
  ConfigMap, a bad proxy URL, no `username` and `password`) is `Ready=False` with
  `ServerReachable=Unknown` and reason `InvalidServer`. A server that is
  `Ready=False` makes its claims wait (see the deletion rule for the exception).
  The status never shows why a call failed; the operator log has a fixed text per
  category (egress policy, TLS, login, timeout, other), once per change, with no
  address, login or password.
- A probe runs every `spec.healthCheckInterval` (default one minute, plus up to
  10% jitter), whether the last one succeeded or not; a server is also probed at
  once when it is created or its spec changes. A probe is bounded to 30 seconds.
- The probe connects exactly as the claims do: the egress policy, the server's
  TLS settings (its CA bundle, `tls.serverName`, minimum version) and proxy, as
  the admin user, with the settings built by the same helper (`adminConfig`). It
  makes **a fresh, short-lived connection of its own** and closes it on every
  path; it never takes a connection from the claims' reference-counted
  `ServerCache` or puts one there, so a probe can neither pin nor evict a cached
  connection.
- RBAC: `get`, `list` and `watch` on Secrets and ConfigMaps, and `get`, `update`
  and `patch` on the servers' `status`, in the chart role.
- Caches: the manager's Secret and ConfigMap caches keep only the objects with the
  `managed-by` label (so the operator does not hold every Secret or ConfigMap in
  memory), and the probe reads the admin Secret and the CA bundle live, not from
  the cache, so every probe sees the current object. A change to an unlabelled
  admin Secret, CA Secret or CA ConfigMap does not trigger a probe at once; it is
  picked up at the next probe interval (one minute by default). Do not widen
  either cache for that: it costs memory and widens exposure for a latency gain
  of at most one interval.

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

## Install

The operator needs a Kubernetes cluster at 1.33 or newer. Helm and OLM v1 need
[cert-manager](https://cert-manager.io/) for the webhooks' serving certificate;
OLM v0 issues its own, so it needs none. A release publishes the Helm chart, an
OLM bundle, a file-based catalog and the distroless `nonroot` image to GHCR,
signed and attested (`release-extras.yml`, see "Releases" below and
`docs/TEMPLATE.md`).

### Helm

```sh
helm install operator-cache oci://ghcr.io/craftainer-labs/charts/operator-cache \
  --version X.Y.Z --namespace operator-cache-system --create-namespace \
  --set egressMode=allow-private
```

A released chart pins the manager image by digest. Values worth knowing (the
schema, `charts/chart/values.schema.json`, rejects a misspelt enum or digest and
a name in `manager.extraEnv` that the chart sets itself):

| Value | Default | Meaning |
| --- | --- | --- |
| `egressMode` | `strict` | `allow-private` is the usual choice when the server is an in-cluster Service or lives on a private address |
| `defaultDataDeletionPolicy` | `Retain` | validated only: `CacheUser` is a credential kind with no `deletionPolicy` (see "Backup and restore") |
| `global.imageRegistry` | empty | pull the image from a mirror (air-gapped) |
| `manager.image.digest` | set when released | image by digest; wins over `tag` |
| `manager.extraEnv` | `[]` | extra environment, rendered after the chart's own (not `POD_NAMESPACE`, `EGRESS_MODE` or `DEFAULT_DATA_DELETION_POLICY`: the chart sets them, and the schema rejects them here), e.g. `NO_PROXY` or `HTTPS_PROXY` for the Kubernetes API client (the Go client reads them; the operator's own code does not), or `SSL_CERT_FILE` with a bundle mounted through `manager.extraVolumes` and `manager.extraVolumeMounts` |
| `manager.podSecurityContext`, `manager.securityContext` | restricted | non-root, seccomp `RuntimeDefault`, no privilege escalation, all capabilities dropped, read-only root filesystem; the e2e installs into a `restricted` namespace as an arbitrary UID |
| `crd.keep` | `true` | keep the CRDs, and with them every claim, when the release is uninstalled |

The operator makes no outbound call except to the Kubernetes API and to the
servers named by the server kinds, and it sends no telemetry. A server's proxy
and CA bundle belong to the server object (`spec.proxyURL`, `spec.tls.caBundle`),
not to the chart.

The CSV therefore declares `features.operators.openshift.io/proxy-aware: "false"`:
the operator reads no `HTTP(S)_PROXY` (the server connection takes its proxy from
`spec.proxyURL`), and the in-cluster Kubernetes API client needs no proxy.
`disconnected` is `"true"` and the fips, tls-profiles and token-auth features are
`"false"`.

### OLM

`make bundle` writes the bundle from `config/manifests` (`make bundle-validate`
checks it offline; `bundle/` is generated and not committed). It supports
`AllNamespaces` only, pins the manager image by digest and lists it in
`spec.relatedImages`. The released bundle and a single-bundle catalog are
`ghcr.io/craftainer-labs/operator-cache-bundle` and `...-catalog`; add the catalog
as a `CatalogSource` (OLM v0) or a `ClusterCatalog` (OLM v1) and subscribe or
create a `ClusterExtension`.

From a checkout, `make deploy` installs the kustomize manifests of
`config/default`, which also need cert-manager.

## Backup and restore

The operator holds no data of its own: everything it knows is in Kubernetes
objects and in the server's ACL.

| Kept in | What | Back it up with |
| --- | --- | --- |
| Kubernetes | the three kinds and their status (`status.externalName`, `status.externalId`, the rotation state), the binding Secrets (`servicebinding.io/redis`), the admin Secrets and CA bundles that servers reference | your cluster backup (Velero, GitOps, etcd). Restore the servers and their Secrets first, then the claims |
| Valkey or Redis | the ACL users, their rules and password hashes, and the data in the keys | the server's own persistence and backup: an ACL file (`aclfile`) or the `user` lines of its configuration, and its RDB or AOF files. The operator never runs `ACL SAVE`, so a server that keeps its users only in memory loses them on a restart |

The operator never copies or restores server state, and it cannot read a
password back: the server keeps only a hash. What it does on a restore:

- **There is no ownership marker.** An ACL user has no field for one. The only
  proof that a claim made its user is the claim's own `status.externalId` (the
  user's name, or `create-issued` while a create is in flight). A restore of the
  claims whose status is lost therefore loses the proof. A restore from
  manifests (GitOps) carries no status; a cluster backup does only if it
  includes the status subresource.
- **Restoring claims without their status, onto a server that still has the
  users:** the claim has no `status.externalId`, so the user of its name is
  foreign. The claim reports `Synced=False`, reason `NameCollision`, and nothing
  is changed (see "Resolving a NameCollision"). To re-adopt, delete the user
  (`ACL DELUSER <name>`, after checking nobody uses it) and let the claim create
  it again, which issues a new password and rewrites the binding Secret.
- **Restoring the server from an older copy:** users and their rules come back as
  they were. A password rotated since the copy is lost while the binding Secret
  still holds it, and the operator does not notice (it does not read passwords
  back). A claim made after the copy has no user on the server, and a claim
  whose recorded user is missing is created again: the server reports no such
  user, so the operator makes it with a new password and rewrites the binding
  Secret. A user that is still there has its rules repaired at the next
  reconcile. Rotate the claim (`craftainer.dev/rotate`) to issue a
  new password and rewrite the binding Secret; consumers read the new Secret.
- **Binding Secrets** are rewritten by the operator, so a restore of them is not
  required. A missing Secret, or one that does not hold the user's login, is
  replaced through a rotation, not by resetting the password in place.
- **The rotation grace period:** after a rotation both the previous and the new
  password log in for `rotationPolicy.gracePeriod` (default `1h`, from `5m` to
  `168h`), then the operator removes the previous one. A restore of the server
  from a copy taken inside that window brings back the old password too; a rotation
  ends it after its own grace period.

`deletionPolicy` does not apply to `CacheUser`: it is a credential kind and has
no `deletionPolicy` (`DEFAULT_DATA_DELETION_POLICY` is only validated). Deleting
a claim deletes its ACL user, which ends its sessions, and never touches the
keys: the data a user wrote stays on the server.

- A claim that never issued a create (`status.externalId` is not set, for example
  a `NameCollision`) is released without contacting the server.
- Otherwise the operator deletes the user it recorded; one that is already gone
  is a success.
- Delete the claims before the server object: with the server or its admin
  Secret gone, the claim cannot be cleaned up on the server: its finalizer stays,
  and it reports `Synced=False` ("the referenced server was not found") until the
  server object and its Secret are back.
- Delete the claims before you uninstall the chart. Their finalizers need the
  running operator: with the operator gone first, claims stay `Terminating` and
  their ACL users stay on the server. Deleting a CRD (or uninstalling with
  `crd.keep=false`) deletes every claim of that kind, so the same order applies.

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
