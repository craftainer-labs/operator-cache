# NFR-0003. A claim reaches only the ACL user derived from its own namespace and name, and only keys and channels under its namespace prefix

## Status

Implemented

## Attribute

Security (multi-tenancy, plan row EB-3).

## Description

The namespace is the tenant. The operator logs in as an administrator that can
see every user and key, so only these things keep a claim away from another
tenant's users, keys and channels:

- the ACL user of a claim is named by derivation (`internal/controller/names.go`,
  `userName`) with `naming.Tenant` from the claim's namespace and name, at most
  128 bytes, with the long hash (80 bits, because an ACL user cannot carry an
  ownership marker); there is no field that sets a name. Observe goes by
  `status.externalName`, which is derived once;
- **`status.externalId` is the only proof that the claim made its user.** A user
  holds no marker, so the claim records `create-issued` before the first
  `ACL SETUSER`, and the user's name afterwards. A user that exists while
  `status.externalId` is empty is not ours: the claim is refused
  (`Synced=False`, reason `NameCollision`), nothing is written, and it is never
  read beyond `ACL GETUSER`, changed or deleted. Create asks the server first, so
  a user made in the meantime is refused too. A claim that never issued a create
  is released on deletion without the server being asked, and a user with its
  name is left alone, so the claim never deletes a user it has no record of;
- every user is confined by rules the operator builds from nothing a claim
  chooses (`internal/valkeyacl/rules.go`): the key pattern is always
  `<namespace>:*` (read-only users get `%R~`), the channel pattern
  `&<namespace>:*` and only when `channels` is set; `@dangerous`, `@admin`,
  `@scripting`, `SCAN`, `RANDOMKEY`, `DBSIZE`, `PUBSUB` and `CLIENT TRACKING`
  are taken out, because they reach beyond the prefix. The namespace is checked
  against a DNS-label pattern again where the rules are built, and so is the
  profile, so a glob character never reaches a key pattern. The rules are
  rebuilt at every write (create, update, rotation), not copied from an earlier
  one, and `Tenant.Matches` makes a user that was widened by hand not up to date,
  so the next reconcile repairs it;
- a cluster server serves only the namespaces its `allowedNamespaces` admits
  (`server.Resolve`), and a namespaced server only its own.

Known limit: Valkey has no "create unless exists" for `ACL SETUSER`. The
`ACL GETUSER` before the create and the `ACL SETUSER` are two commands, so a
client that makes a user of the same name inside that gap has its user reset by
the claim. The name carries an 80-bit hash and the gap is one round trip, so a
tenant cannot aim at it, but a second operator or a person using the admin
account is not told apart from this operator: the administrator's credentials
should be dedicated to the operator.

## Source

Plan row EB-3; the NO5b split and notes in the plan's Decision log;
[ADR 0002 of lib-operator-go](https://github.com/craftainer-labs/lib-operator-go/blob/main/docs/adrs/0002-operator-crd-conventions.md), sections 2 and 10.

## Verification

Unit and envtest suites, on every push: `TestNames`, `TestUserRules` (the
profile and the namespace are checked again), `TestUserLifecycleAndRotation`
(the rules, a widened user repaired, the password kept), `TestUserNeverAdoptsWhatItDidNotMake`
(a user without a record leaves no write, and deleting the claim leaves it),
`TestUserRecordsTheIntentBeforeItCreates`, `TestUserCreateRacesWithAnotherCreator`,
`TestUserFinishesACreateThatCrashed`, `TestUserRepeatsACreateThatFailed`,
`TestUserDelete`, `TestUserOnAnUnusableServer` (`NamespaceNotAllowed`) and the
validators in `internal/webhook/v1alpha1`. The rule texts and what a real client
can do with them (its own prefix and nothing else) are checked against a real
Valkey by the integration lane in `test/integration`; the controller paths
(NameCollision, rotation, delete) run against it in the same lane, and the e2e
lane logs in with a binding and checks it stays inside its prefix.
