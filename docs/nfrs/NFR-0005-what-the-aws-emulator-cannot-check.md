# NFR-0005. The AWS backends' tests say what they do not prove

## Status

Implemented (the `aws` job of the `Integration` workflow)

## Attribute

Maintainability and security (test confidence).

## Description

The ElastiCache and MemoryDB backends are exercised against
[moto](https://github.com/getmoto/moto) server 5.2.5, pinned by digest, in the
`aws` job of `integration.yml`, and against an in-memory client in unit tests
(100% coverage). moto lacks ElastiCache `ModifyUser`, `DescribeUserGroups` and
`ModifyUserGroup`, and MemoryDB `DescribeACLs` and `UpdateACL`, so a small fake
(`test/integration/awscloud/fake_test.go`) on a private address answers those and
forwards every other call to moto. The probe and the user lifecycle for both
engines (create, join, active, rotate, drift removing extra passwords, delete,
no adoption, and IAM users) are checked that way. Neither the emulator nor the
fake can check, and nothing in CI checks:

- **The data plane.** No login to a real cluster is made: neither a password
  login nor IAM AUTH with a signed token, and nothing listens on the endpoint a
  probe discovers.
- **Credentials.** moto is not IRSA or EKS Pod Identity, does not enforce IAM and
  accepts any signature. The workload identity path, `roleARN` assumption and the
  least-privilege permission list in the README are unverified until run against
  AWS.
- **Egress to AWS.** The tests reach the fake and moto at private addresses with
  `EGRESS_MODE=allow-private` set in the test's own wiring, never in the
  operator's. The strict-mode path to the real endpoints, and TLS to them, are
  covered by the library's tests only.
- **AWS's own rules.** The normalisation of an access string (the controller
  compares by rule set because AWS may reorder or rewrite it), the validity and
  number of passwords, user and group limits, throttling, durations and the real
  eventual consistency of a user group or ACL are not modelled. The fake settles
  a change after one read; AWS takes seconds to minutes.
- **The MemoryDB wire protocol.** The pinned MemoryDB SDK speaks smithy RPC v2
  CBOR by default, which moto does not serve. The MemoryDB tests switch the same
  `awscache.MemoryDB` to AWS JSON 1.1; the CBOR path is exercised only by real
  AWS.
- **Error text.** The operator shows only coarse categories (see
  [NFR-0002](NFR-0002-restricted-egress-and-proxy.md)); what AWS says is never
  asserted.

## Source

The cloud cache backend (NKcache-1) and
[ADR 0004](../adrs/0004-cloud-cache-users-on-elasticache-and-memorydb.md).

## Verification

The `aws` job runs on every push and pull request. The list above is reviewed
when the emulator's pin or the SDK's pin changes, and a manual run against a real
replication group and a real MemoryDB cluster is needed before a release that
changes this backend.
