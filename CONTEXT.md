# Context

The vocabulary of this repository.

## Check

One dead-man's switch. It has a name, a secret token, a period and a grace.
Terraform declares it. kestrel watches it. A check is not a monitor: kestrel
never reaches out to anything, it only waits to be told.

## Ping

A job's report that it finished. `POST /ping/{token}` with an empty body and
no auth header. The URL is the whole credential. den sends these from
`internal/notify`.

## Period

How long kestrel waits between expected pings, in seconds. This is the job's
schedule, not a timeout.

## Grace

How much lateness is tolerated on top of the period, in seconds. A nightly
backup that sometimes runs long needs grace, not a longer period.

## Deadline

`last_ping_at + period + grace`. A check that has never pinged counts from
`created_at` instead. A check past its deadline is overdue.

## Sweep

The pass that moves every overdue check to down and announces each one.
Cloud Scheduler calls `POST /api/sweep` every five minutes. The sweep is the
only thing that raises an alarm; nothing is announced on a timer inside the
process, because Cloud Run scales to zero.

## Claim

The conditional `UPDATE` that the sweep uses. Claiming is what makes an
announcement exactly-once across instances: the rows the statement returns are
the rows this instance alone changed.

## Reconcile

The boot-time pass that makes the `checks` table match the declared list.
Upsert by name, delete by absence.

## State

`new`, `up` or `down`. See the state machine in AGENTS.md.

## Config secret

The single JSON object the `cloud-run-app` module mounts at
`/secrets/config.json`. It is not a set of environment variables. Adding a
setting means adding a key to the module's `secrets` map in Terraform.
