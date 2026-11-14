# NFR-0001. Every server connection is TLS 1.2 or newer, verifies the server, and logs in as the ACL administrator

## Status

Implemented

## Attribute

Security (encryption in transit, FIPS-capable build, plan row EB-14).

## Description

- The operator reaches a Valkey or Redis server only over TLS (`rediss`). There
  is no plaintext fallback and no `InsecureSkipVerify`; the server kinds offer no
  plain port, and the binding's `uri` is `rediss://`. The ACL client takes
  `host:port` and never a URL, so there is no scheme or userinfo to carry
  credentials (`valkeyacl.Connect`).
- The server's certificate chain and host name are verified against the server's
  CA bundle (the `ca.crt` key of the CA Secret by default; the system roots when
  there is none) and `tls.serverName` when set. The TLS floor is 1.2; a server
  may raise it to 1.3. A bundle that holds no certificate is refused, not
  ignored.
- The connection is made by the dialer the caller hands in. In the operator it
  is the egress policy's `HostDialer`
  ([NFR-0002](NFR-0002-restricted-egress-and-proxy.md)), and valkey-go gets the
  finished `tls.Conn` through `DialCtxFn`, so no connection can bypass the dialer
  and no proxy environment variable is read. The connection speaks RESP2 only,
  has no client-side cache and does not retry on its own.
- The operator logs in as the administrator of the server's admin Secret (keys
  `username` and `password`, both required) and proves it with `ACL WHOAMI` before
  anything else. A refused login or command is `ErrAuth`; a connection that
  cannot be made or kept is `ErrUnavailable`; a deadline is `ErrTimeout`. The
  server's own wording is kept in the error for the operator log, and the
  conditions carry only a fixed text per kind. A refused `ACL SETUSER` is never
  repeated in an error, because the server echoes the modifier, which can be a
  password.
- The cached connection of a server is replaced as soon as the server object, its
  credentials, its CA bundle or its proxy change, so a rotated admin password or
  CA is picked up and a stale login is never retried.

## Source

Plan row EB-14 and the NR4 decision (valkey-go);
[ADR 0002 of lib-operator-go](https://github.com/craftainer-labs/lib-operator-go/blob/main/docs/adrs/0002-operator-crd-conventions.md), section 13.

## Verification

`TestConnectAndWhoami`, `TestConnectRefusesWhatItCannotUse`,
`TestConnectWrongPasswordIsAuthAndLeaksNothing`, `TestConnectVerifiesTheCertificate`
and `TestConnectDialFailures` (`internal/valkeyacl`) against an in-process
server, and, against a real Valkey with TLS and ACLs, `TestWrongCredentialsAreAuthAndLeakNothing`
(a wrong password, an unknown admin and the `default` user are `ErrAuth`) and
`TestTLSIsVerified` (the system roots do not know the CA; a wrong host name
fails; the server does not answer a plain connection) in `test/integration`. The controller
side: `TestStampCoversEverythingTheConnectionIsMadeFrom` and
`TestBackendCachesTheConnection` (`internal/controller`) show that a changed
admin password, CA, server name, minimum TLS version, address or proxy replaces
the connection. The e2e lane (`test/e2e`) reaches a TLS Valkey with a
cert-manager certificate through the operator, with readiness checked over TLS.
