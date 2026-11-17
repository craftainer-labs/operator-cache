# 0004. Manage cache users on Amazon ElastiCache and MemoryDB through the AWS API

## Status

Accepted

## Context

Until now a server was a Valkey the operator logs in to over TLS with an admin
login it is given, and a claim was an ACL user made with `ACL SETUSER`.
ElastiCache and MemoryDB refuse `ACL SETUSER`: their users are API objects (RBAC
users with an access string and up to two passwords, or IAM authentication),
kept in a user group (ElastiCache) or an ACL (MemoryDB) that the administrator
attaches to the replication group or cluster. They are created and changed by
calls that return at once while the user's status moves through `creating`,
`modifying` and `deleting`. lib-operator-go v0.6.0 adds the shared pieces for
servers like this (`common.CloudServerCommon`, `server.ResolveCloud`,
`probe.CloudProber`, `ServerStatus.Discovered`, `lifecycle.PendingError`,
`lifecycle.NonRotator`, `lifecycle.BindingRebuilder`; its ADR 0002, section 16)
and the egress-checked AWS client of `pkg/cloud`.

What the services allow shapes the design:

- A user can carry a tag, so ownership has a marker, as in ADR 0002, section 10.
- A user's passwords are write-only: AWS reports how many there are, never which.
- A user group or ACL is modified one change at a time, and a change takes up to
  minutes.
- A user can hold two passwords, so a rotation can overlap.

## Decision

- **API, not ACL commands.** A server with `provider` (AWS only) has no admin
  Secret and no data-plane connection. Its claims drive the AWS API with
  `aws-sdk-go-v2` (`internal/awscache`) through the egress policy of
  `pkg/cloud`. The administrator creates the replication group or cluster and its
  user group or ACL and attaches them; the operator only makes users and adds
  them to that group or ACL. Self-hosted servers are unchanged.
- **One user per claim, with a marker.** The user id is derived from the
  namespace and claim name (at most 40 characters, a short hash) and kept in
  `status.externalName`; the ARN is `status.externalId`. The user is tagged
  `craftainer.dev/claim=<namespace>/<name>/<uid>` (its SHA-256 where that would
  pass 256 characters). A user of the id with another or no marker is a
  `NameCollision` and is never adopted, changed or deleted.
- **Asynchronous steps are `Pending`.** A write returns `lifecycle.Pending` (ten
  seconds) and `Observe` reports it again until the user and its group are
  `active`; the lifecycle maps that to `Ready=False` with `Creating`, `Updating`
  or `Deleting`. A claim that has been not Ready for more than 30 minutes gets a
  `ReconcileError` (`ErrPendingTimeout`) in place of one more `Pending`, so a
  cloud that never finishes is visible; the reconcile keeps looking, and the
  claim is Ready as soon as the cloud settles.
- **Passwords.** In `Password` mode `Create` returns the binding data, so the
  lifecycle writes the Secret before the user is active. A rotation holds the
  binding's password beside a new one (two is AWS's limit) and, after the grace
  period, only the binding's. Because AWS shows only a count, the operator
  cannot tell which password is extra: a user with more passwords than the
  binding and a rotation explain is repaired by setting the binding's password
  alone (a `PasswordDrift` Event), and a binding whose password is not usable is
  replaced by a rotation. It never adds a Secret's password back.
- **IAM.** In `IAM` mode the user has no password; the binding Secret names its
  ARN (`iamResourceArn`) and the consumer's own role signs the login token. The
  claim is a `NonRotator` (a rotation request is a `RotationNotApplicable` Event)
  and a `BindingRebuilder` (a lost Secret is rebuilt from the user).
  `spec.iam.principal` is refused on AWS: the role is authorized by the ARN.
- **The probe reads the cloud.** For a server with a provider the probe calls
  `DescribeUserGroups` and `DescribeReplicationGroups` (ElastiCache) or
  `DescribeACLs` and `DescribeClusters` (MemoryDB), which also checks that the
  group or ACL and the cache exist, and puts only drafted keys in
  `status.discovered`: `endpoint`, `port`, `resourceId`, `region` and, from the
  resource's ARN, `accountId`; a key whose value is unknown is left out. A failure
  leaves the server not Ready (`ServerUnreachable`, with the library's coarse
  message). The drafted server reasons `TargetNotFound` and
  `EndpointOverrideForbidden` cannot be expressed through the library yet, so
  they are a known gap until a typed reason on the probe error exists (NKlib-3);
  the operator does not write the status itself, which would race the library's
  write. There is no `CloudResolved` condition in this operator.
- **Errors are coarse.** Conditions and Events carry one fixed text per category;
  the raw error, which may hold an ARN or a request id, goes to the operator log.

## Consequences

- A claim on AWS needs the server's probe to have discovered an endpoint, or the
  server to set `host`; without one no user is made (`ErrAddressUnknown`).
- Each reconcile of a claim reads the user, its group or ACL and its tags (one
  `Lookup` is three calls) and builds its AWS client; at many claims or short
  poll intervals that is the throttling to watch (see the README's "Known
  limits"). Nothing is cached between reconciles.
- Wait durations are AWS's, and the 30 minute bound is a constant
  (`controller.PendingDeadline`), chosen well above what the services take.
- The emulator that CI runs cannot check the data plane, real credentials or
  AWS's own rules; [NFR-0005](../nfrs/NFR-0005-what-the-aws-emulator-cannot-check.md)
  lists what stays unverified until it is run against AWS.
- Azure Managed Redis is out of scope until its access-string API is GA, and
  Memorystore for Valkey (GCP) is a separate item.
