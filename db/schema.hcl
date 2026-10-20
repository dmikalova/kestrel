schema "kestrel" {}

// A check is a dead-man's switch. Terraform declares it; kestrel never creates one.
table "checks" {
  schema = schema.kestrel

  column "id" {
    type    = uuid
    null    = false
    default = sql("gen_random_uuid()")
  }

  column "name" {
    type = text
    null = false
  }

  // Secret. It is the whole of the ping URL's authority, so it is never logged.
  column "token" {
    type = text
    null = false
  }

  // How often the job is expected to ping.
  column "period_seconds" {
    type = integer
    null = false
  }

  // Slack added to period_seconds before a missed ping counts as down.
  column "grace_seconds" {
    type    = integer
    null    = false
    default = 300
  }

  // new until the first ping ever arrives, so provisioning does not page anyone.
  column "state" {
    type    = text
    null    = false
    default = "new"
  }

  column "last_ping_at" {
    type = timestamptz
    null = true
  }

  column "alerted_at" {
    type = timestamptz
    null = true
  }

  column "created_at" {
    type    = timestamptz
    null    = false
    default = sql("now()")
  }

  primary_key {
    columns = [column.id]
  }

  index "checks_name_key" {
    columns = [column.name]
    unique  = true
  }

  index "checks_token_key" {
    columns = [column.token]
    unique  = true
  }

  // The sweep scans on this. Without it every sweep is a full scan.
  index "checks_state_idx" {
    columns = [column.state]
  }

  check "checks_state_valid" {
    expr = "state IN ('new', 'up', 'down')"
  }

  check "checks_period_positive" {
    expr = "period_seconds > 0"
  }

  check "checks_grace_non_negative" {
    expr = "grace_seconds >= 0"
  }
}

// Append-only ping history. The sweep does not read this; checks.last_ping_at
// carries the live state. This exists to answer "when did it actually run".
table "pings" {
  schema = schema.kestrel

  column "id" {
    type = bigint
    null = false
    identity {
      generated = ALWAYS
    }
  }

  column "check_id" {
    type = uuid
    null = false
  }

  column "received_at" {
    type    = timestamptz
    null    = false
    default = sql("now()")
  }

  primary_key {
    columns = [column.id]
  }

  foreign_key "pings_check_id_fkey" {
    columns     = [column.check_id]
    ref_columns = [table.checks.column.id]
    on_delete   = CASCADE
    on_update   = NO_ACTION
  }

  index "pings_check_id_received_at_idx" {
    on {
      column = column.check_id
    }
    on {
      column = column.received_at
      desc   = true
    }
  }
}
