# NFR-0004. Least-privilege RBAC, an audit trail, and no credential outside its Secret

## Status

Implemented

## Attribute

Security (access control, audit, secrets, plan rows EB-2, EB-4 and EB-9).

## Description

- The manager's ClusterRole lists exact resources and verbs: the group's kinds,
  their `status` and `finalizers`, `namespaces` (get, list, watch), `configmaps`
  (get), `secrets` and `events`; no wildcard. The manager's Secret cache holds
  only the Secrets labelled `app.kubernetes.io/managed-by`; the admin Secrets and
  CA bundles of servers are read from the API server on demand. A cluster
  server's admin Secret is read from the operator namespace only
  (`server.Resolve`); a namespaced server's from its own namespace, which is why
  the `secrets` rule is a ClusterRole rule. **For the user to review:** the
  `secrets` rule grants create, update, patch and delete on every Secret the cache
  cannot narrow by namespace, because namespaced servers keep their admin Secret
  in tenant namespaces.
- The operator namespace must be known: from `--operator-namespace` or
  `POD_NAMESPACE` or the pod's service account. Without it startup fails
  (`ErrNoOperatorNamespace`) instead of guessing a namespace, which could be a
  tenant's. The chart's `values.schema.json` rejects `extraEnv` entries that
  would override the variables the chart owns (`POD_NAMESPACE`, `EGRESS_MODE`,
  `DEFAULT_DATA_DELETION_POLICY`).
- Each state change is an Event on the claim and a structured JSON log line (who,
  what, when, the server and the names of the changed fields, never their
  values), written by `lifecycle.Reconcile`.
- The administrator's login, the users' passwords and the proxy URL never appear
  in Events, logs, conditions or status. A password is generated per issue
  (`crypto/rand`, 52 characters) and reaches only the binding Secret and the
  server. The ACL client logs nothing, never repeats a refused `ACL SETUSER` (the
  server echoes the modifier, which is a password) and `Config` prints without its
  password; the controller never logs a command. The binding's `uri` carries no
  credentials. Conditions and Events carry one fixed text per kind of failure; the
  raw error, which may hold an address or the server's own words, is in the
  operator log only.
- A rotation is two-phase on one user: it keeps the password of the binding
  Secret and adds a new one (`resetpass #<hash of the current> ><new>`), the
  binding Secret is written, and when the grace period ends (`gracePeriod`, one
  hour by default) the user is cut back to the binding's password
  (`resetpass #<hash>`), which ends the old one. Retiring first checks that the
  binding's hash is on the user, and changes nothing if it is not, so a user is
  never left with no password. A change of the rules or the spec never changes the
  password: the rules are applied without `reset`, and a binding Secret that is
  missing or does not hold the user leads to a rotation, not a reset.
- The webhooks admit an object that is being deleted, so finalizer removal is
  never blocked.
- A cached server connection is closed when it is replaced, forgotten or idle for
  ten minutes (a sweeper that every replica runs), never while a reconcile holds
  it; the number of concurrent reconciles is `--max-concurrent-reconciles`.

## Source

Plan rows EB-2, EB-4 and EB-9; [ADR 0002 of lib-operator-go](https://github.com/craftainer-labs/lib-operator-go/blob/main/docs/adrs/0002-operator-crd-conventions.md), sections 8, 11 and 12.

## Verification

`config/rbac/role.yaml` is generated from the markers in `internal/app/app.go`
and checked for drift by the `generated-current` hook (the chart's copy by
`scripts/check-chart-sync.sh`). `TestControllerSettings` and
`TestRunRejectsInvalidSettings` (`internal/app`) cover the operator namespace and
the settings. `TestUserLifecycleAndRotation` asserts that no Event or condition
holds a password, that a rotation keeps the previous password valid until its
grace period ends and that a change of the profile keeps the password;
`TestUserFailuresAreCoarse` and `TestUserOnAnUnusableServer` assert that no Event
or condition holds an address, a server's words, an admin Secret's name or a
proxy credential; `TestUserUpdateWithoutAValidSecretRotates` and `TestUserUpdate`
check that a change never resets the password; `TestUserRotateBegin` and
`TestUserRotateFinish` check that a retried rotation drops only the failed
attempt's password and that a retire with an unknown hash changes nothing;
`TestServerCache*` (`internal/controller`) cover the lease-counted cache and its
sweeper. The e2e lane installs the chart as an arbitrary UID under Pod Security
`restricted` and checks the rotation against a real server.
