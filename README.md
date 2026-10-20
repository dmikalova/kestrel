# kestrel

A dead-man's switch watcher. Jobs ping kestrel when they succeed. When a job
stops pinging, kestrel says so in Discord.

This exists because a backup that silently stops running looks exactly like a
backup that is working.

## How it works

1. Terraform declares each check and mints its token.
2. The job pings `https://kestrel.mklv.tech/ping/<token>` when it finishes.
3. Cloud Scheduler calls the sweep every five minutes.
4. The sweep announces any check that is past its deadline, and a later ping
   announces the recovery.

kestrel runs on Cloud Run against Supabase Postgres. It scales to zero.

## Adding a check

Add it to `infrastructure/gcp/apps/kestrel`, then apply. Terraform mints the
token, builds the ping URL and writes it to a secret for the caller to read.
There is no API and no UI for this on purpose: the check list and the
infrastructure that depends on it stay in one place.

## Routes

| Route             | Caller          | Auth                       |
| ----------------- | --------------- | -------------------------- |
| `POST /ping/{t}`  | Any job         | The token in the URL       |
| `POST /api/sweep` | Cloud Scheduler | OIDC, checked against both |
| `GET /health`     | Cloud Run       | None                       |

## Developing

```sh
mage -l                      # list tasks
mage ci:fix && mage ci:check # must print ALL GREEN
```

Run it locally against a config file:

```sh
go run ./cmd/kestrel -secrets ./local-config.json
```

## License

Apache-2.0.
