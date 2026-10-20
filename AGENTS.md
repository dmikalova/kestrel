# kestrel

Conventions for this repository.

## Validation

Run `mage ci:fix && mage ci:check` before calling work done. It must print
`ALL GREEN`.

## Package layout

| Package            | Role                                                  |
| ------------------ | ----------------------------------------------------- |
| `cmd/kestrel`      | The Cloud Run entrypoint: load, reconcile, serve      |
| `internal/config`  | Reading `/secrets/config.json` and validating it      |
| `internal/store`   | The Postgres schema's queries and state transitions   |
| `internal/discord` | Posting the outage and recovery embeds to the webhook |
| `internal/server`  | The three routes and the sweep's OIDC check           |
| `db/schema.hcl`    | The Atlas schema that CI applies to Supabase          |

## Rules

- **Terraform owns the check list, not an API.** A check is declared in
  `infrastructure/gcp/apps/kestrel`, which mints its token and writes the ping
  URL to a secret. kestrel reconciles its table from `/secrets/config.json` at
  boot: it upserts every declared name and deletes every name that is absent.
  Do not add an endpoint that creates a check. A registry that drifts from the
  infrastructure depending on it is worse than no registry.
- **A token is a credential.** It is never logged, never echoed in a response
  and never put in an error message. An unknown token gets a bare 404.
- **The service is public.** The `cloud-run-app` module grants `allUsers` the
  invoker role, because den pings without credentials. Every route other than
  the ping must therefore authenticate itself. `/api/sweep` requires a
  Google-signed ID token whose `email` claim is kestrel's scheduler service
  account. The signature alone proves nothing: any Google account can mint a
  token. The email is the gate, so do not make it optional.
- **The database decides who wins.** Each state change is one conditional
  `UPDATE ... RETURNING`. That is what lets kestrel run on more than one
  instance without announcing the same outage twice. Do not replace a claim
  with a read followed by a write.
- **`/health` never touches the database.** A database outage must not also
  make Cloud Run tear down the instance that reports the outage.

## Talking to Supabase

The connection string is the transaction pooler on port 6543. That pooler
hands a different backend to each statement, so prepared statements do not
survive. `internal/store` sets `pgx.QueryExecModeSimpleProtocol` and zeroes
both caches. Do not remove those lines, and do not switch the URL to port 5432
to work around a pooler error: the session port has far fewer connections.

Migrations run in CI, not at boot. CI finds `db/schema.hcl`, reads
`DATABASE_URL_SESSION` from the `kestrel-config` secret and runs
`atlas schema apply` against the `kestrel` schema. It refuses to drop.

## The check state machine

`new` means declared but never pinged. The first ping moves it to `up` without
an announcement. The sweep moves `new` or `up` to `down` and announces. A ping
moves `down` back to `up` and announces the recovery. There is no paused state;
the schema's check constraint permits `new`, `up` and `down` only.

A `new` check counts its deadline from `created_at`, not from a ping it never
sent. A switch nobody ever armed is the failure this exists to catch, so do not
exempt `new` from the sweep. Grace has to cover the gap between declaring a
check and wiring up the job that pings it.
