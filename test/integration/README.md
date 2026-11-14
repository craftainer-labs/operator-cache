# test/integration/

Go tests of the operator's ACL client (`internal/valkeyacl`) and of the
controller's paths over it (`internal/controller`) against a real
Valkey 9.1.2 with TLS and ACLs on, behind the build tag `integration` so
`go test ./...` and the 100% coverage gate of `make test` never see them. The
unit suites use an in-process server that speaks just enough RESP; ACL behaviour
is server-side, so this lane checks what that fake only claims, against Valkey:

- the commands the operator sends (`ACL WHOAMI`, `GETUSER`, `SETUSER`,
  `DELUSER`), the shape of `ACL GETUSER` (the rule texts the controller
  compares with the tenant's rules to tell a user is up to date), and the error
  each refusal becomes;
- the wrong credentials of the administrator (a wrong password, an unknown user,
  the `default` user) are `ErrAuth`, and no error text holds a password, nor the
  rule a server refused;
- what a tenant's ACL lets a real client do, as the application would connect:
  its own prefix and nothing else (another tenant's keys, a name that only
  begins like the prefix, `MGET`, `RENAME` and `COPY` across prefixes), and never
  `KEYS`, `SCAN`, `RANDOMKEY`, `DBSIZE`, `FLUSHALL`, `CONFIG`, `ACL`,
  `CLIENT LIST`, `CLIENT TRACKING`, `FUNCTION` writes, scripts or modules; a
  read-only tenant cannot write; channels follow the prefix, and without them
  there is no publish;
- the overlap of a rotation: two passwords at once, then only the new one, by
  its hash, and the old one no longer logs in;
- an update that keeps the password and repairs rules someone widened by hand,
  and a delete that ends the user's sessions and leaves another user alone;
- verified TLS: the system roots do not know the CA, a wrong host name fails, and
  the server does not answer a plain connection;
- the controller's paths (`controller_test.go`, through `controller.Integration`,
  which exists only with the `integration` tag and wires the External of a
  `CacheUser` to the real server and an in-memory binding Secret): create, with a
  real login confined to its prefix; a change of the profile that keeps the
  password; a widened user noticed and repaired; a rotation (both passwords log
  in, a retry drops the failed attempt's password, the retire ends the old one,
  and a retire with a password the server does not list changes nothing); a
  create that crashed and is finished; a user without a record refused as a
  `NameCollision` and left unchanged; delete ending the user and its sessions.

Files:

- `controller_test.go` — the controller's paths, as above.
- `valkey_test.go` — the environment, the client of a tenant's application and
  the tests. They read `VALKEY_ADDRESS` (default `localhost:6379`),
  `VALKEY_CA_FILE` (required), `VALKEY_ADMIN_USERNAME` (default `admin`) and
  `VALKEY_ADMIN_PASSWORD` or `VALKEY_ADMIN_PASSWORD_FILE` (one required). A
  missing setting fails the run; it never skips. Every test uses names of its own
  and removes what it created. Each test waits until the administrator can log in.
- `gen-certs.sh <dir>` — a throwaway CA, a server certificate for `valkey`,
  `localhost` and `127.0.0.1` (`server.crt`, `server.key`, `ca.crt`), the
  administrator's random password (`admin-password`) and the server's ACL file
  (`users.acl`: that administrator with the password as a SHA-256 hash, and
  `default` off), with `openssl` and `sha256sum`. The key is world-readable
  because Valkey runs as another user than the one that generated it; the files
  live two days in a directory made for one run.
- `valkey.conf` — one TLS port and no plain one, the ACL file, nothing persisted.

## Running

In CI, `.github/workflows/integration.yml` generates the files, starts the pinned
`valkey/valkey:9.1.2-alpine` image (by digest) with them and runs the tests.

The devcontainer does not run a Valkey yet (that is a `.devcontainer/` change and
needs a rebuild). Anywhere else, run `gen-certs.sh` into a directory, start a
server the way the workflow does and export the `VALKEY_*` variables:

```sh
go test -tags integration -count=1 ./test/integration/...
```

The integration coverage of the client (informational, the gate stays on the
unit profile):

```sh
go test -tags integration -count=1 -coverpkg=./internal/valkeyacl/ -coverprofile=integration.cover ./test/integration/...
go tool cover -func=integration.cover
```

The tests dial directly, not through the egress policy, which refuses loopback
and is covered by its own unit tests.

## Don't

- Point the tests at a server that holds anything you want to keep. They only
  create and remove users with generated names, but they connect as the
  administrator.
