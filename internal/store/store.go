// Package store holds kestrel's state in Postgres.
//
// Every state transition is a single conditional UPDATE. That is what makes it
// safe to run more than one Cloud Run instance: the database decides who wins,
// so two instances cannot both raise the alarm for one missed ping.
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrUnknownToken means the ping URL does not match a declared check.
var ErrUnknownToken = errors.New("no check matches that token")

// Store is a connection pool plus the queries kestrel needs.
type Store struct {
	pool *pgxpool.Pool
}

// Check is a declared dead-man's switch as the database holds it.
type Check struct {
	ID            string
	Name          string
	PeriodSeconds int
	GraceSeconds  int
	LastPingAt    *time.Time
}

// Declared is one check as Terraform declares it.
type Declared struct {
	Name          string
	Token         string
	PeriodSeconds int
	GraceSeconds  int
}

// Deadline is when the check is considered overdue.
func (c Check) Deadline() time.Time {
	if c.LastPingAt == nil {
		return time.Time{}
	}

	return c.LastPingAt.Add(time.Duration(c.PeriodSeconds+c.GraceSeconds) * time.Second)
}

// Open connects to Postgres through Supabase's transaction pooler.
func Open(ctx context.Context, databaseURL string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse the database URL: %w", err)
	}

	// Supabase's transaction pooler hands a different backend to each
	// statement, so a prepared statement cached on one is absent on the next.
	// The simple protocol sends the whole statement every time.
	cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	cfg.ConnConfig.StatementCacheCapacity = 0
	cfg.ConnConfig.DescriptionCacheCapacity = 0

	// Cloud Run scales to zero and the pooler charges for idle connections.
	cfg.MaxConns = 4
	cfg.MaxConnIdleTime = time.Minute

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect to the database: %w", err)
	}

	if err := pool.Ping(ctx); err != nil {
		pool.Close()

		return nil, fmt.Errorf("ping the database: %w", err)
	}

	return &Store{pool: pool}, nil
}

// Close releases the pool.
func (s *Store) Close() { s.pool.Close() }

// Ping reports whether the database is reachable.
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

// Reconcile makes the checks table match what Terraform declared. Checks that
// are no longer declared are removed, which cascades to their ping history.
func (s *Store) Reconcile(ctx context.Context, declared []Declared) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin the reconcile transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	names := make([]string, 0, len(declared))

	for _, d := range declared {
		names = append(names, d.Name)

		// state is left alone so a redeploy does not resurrect a down check.
		const upsert = `
			INSERT INTO kestrel.checks (name, token, period_seconds, grace_seconds)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (name) DO UPDATE SET
				token          = EXCLUDED.token,
				period_seconds = EXCLUDED.period_seconds,
				grace_seconds  = EXCLUDED.grace_seconds`

		if _, err := tx.Exec(
			ctx,
			upsert,
			d.Name,
			d.Token,
			d.PeriodSeconds,
			d.GraceSeconds,
		); err != nil {
			return fmt.Errorf("upsert the check %q: %w", d.Name, err)
		}
	}

	if _, err := tx.Exec(
		ctx,
		`DELETE FROM kestrel.checks WHERE NOT (name = ANY($1))`,
		names,
	); err != nil {
		return fmt.Errorf("remove undeclared checks: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit the reconcile transaction: %w", err)
	}

	return nil
}

// RecordPing marks a check alive and reports the state it was in beforehand.
// A previous state of "down" means the caller should announce a recovery.
func (s *Store) RecordPing(
	ctx context.Context,
	token string,
) (check Check, previousState string, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Check{}, "", fmt.Errorf("begin the ping transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// The row is locked before it is updated so a concurrent ping cannot read
	// the same previous state and announce the recovery twice.
	const markUp = `
		WITH previous AS (
			SELECT id, state FROM kestrel.checks WHERE token = $1 FOR UPDATE
		)
		UPDATE kestrel.checks c
		SET state = 'up', last_ping_at = now(), alerted_at = NULL
		FROM previous p
		WHERE c.id = p.id
		RETURNING c.id, c.name, c.period_seconds, c.grace_seconds, p.state`

	row := tx.QueryRow(ctx, markUp, token)

	if err := row.Scan(
		&check.ID,
		&check.Name,
		&check.PeriodSeconds,
		&check.GraceSeconds,
		&previousState,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Check{}, "", ErrUnknownToken
		}

		return Check{}, "", fmt.Errorf("record the ping: %w", err)
	}

	if _, err := tx.Exec(
		ctx,
		`INSERT INTO kestrel.pings (check_id) VALUES ($1)`,
		check.ID,
	); err != nil {
		return Check{}, "", fmt.Errorf("append the ping history: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return Check{}, "", fmt.Errorf("commit the ping transaction: %w", err)
	}

	return check, previousState, nil
}

// ClaimOverdue moves every check past its deadline to down and returns the ones
// it moved. A check still in state new has never pinged, so it is left alone.
//
// The UPDATE is the claim. Whichever instance runs it first gets the rows, and
// any other instance sees none, so each outage is announced exactly once.
func (s *Store) ClaimOverdue(ctx context.Context) ([]Check, error) {
	const claim = `
		UPDATE kestrel.checks
		SET state = 'down', alerted_at = now()
		WHERE state = 'up'
		  AND last_ping_at + make_interval(secs => period_seconds + grace_seconds) < now()
		RETURNING id, name, period_seconds, grace_seconds, last_ping_at`

	rows, err := s.pool.Query(ctx, claim)
	if err != nil {
		return nil, fmt.Errorf("claim the overdue checks: %w", err)
	}
	defer rows.Close()

	var overdue []Check

	for rows.Next() {
		var c Check
		if err := rows.Scan(
			&c.ID,
			&c.Name,
			&c.PeriodSeconds,
			&c.GraceSeconds,
			&c.LastPingAt,
		); err != nil {
			return nil, fmt.Errorf("read an overdue check: %w", err)
		}

		overdue = append(overdue, c)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the overdue checks: %w", err)
	}

	return overdue, nil
}
