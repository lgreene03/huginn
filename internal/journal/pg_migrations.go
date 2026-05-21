package journal

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"
)

// pgMigration is a versioned, idempotent SQL delta. Versions must be
// consecutive integers starting at 1. Each migration is applied in a
// single transaction; a failure rolls back and the process exits loudly.
type pgMigration struct {
	version int
	sql     string
}

// pgMigrations is the ordered list of all schema versions.
// Append only — never edit or remove an existing entry.
var pgMigrations = []pgMigration{
	{
		version: 1,
		// Baseline schema: trade fills + strategy state.
		// Idempotent: CREATE TABLE/INDEX IF NOT EXISTS.
		sql: `
CREATE TABLE IF NOT EXISTS schema_migrations (
	version    INT PRIMARY KEY,
	applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS trade_fills (
	order_id         VARCHAR(64)      PRIMARY KEY,
	instrument       VARCHAR(32)      NOT NULL,
	side             INT              NOT NULL,
	quantity         DOUBLE PRECISION NOT NULL,
	fill_price       DOUBLE PRECISION NOT NULL,
	transaction_cost DOUBLE PRECISION NOT NULL,
	slippage_bps     DOUBLE PRECISION NOT NULL,
	timestamp        TIMESTAMPTZ      NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_trade_fills_timestamp ON trade_fills(timestamp);

CREATE TABLE IF NOT EXISTS strategy_state (
	strategy_key VARCHAR(64)  PRIMARY KEY,
	state_blob   JSONB        NOT NULL,
	updated_at   TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_strategy_state_updated ON strategy_state(updated_at);
`,
	},
}

// runMigrations creates the schema_migrations ledger if needed, then applies
// any migration whose version has not yet been recorded. Each migration runs
// in its own transaction; a failure rolls back the version and returns an
// error so the caller can abort boot.
func runMigrations(ctx context.Context, pool *pgxpool.Pool) error {
	// Bootstrap the ledger table; this is the only statement we execute
	// outside a migration transaction.
	if _, err := pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    INT PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		);
	`); err != nil {
		return fmt.Errorf("create schema_migrations ledger: %w", err)
	}

	for _, m := range pgMigrations {
		applied, err := isMigrationApplied(ctx, pool, m.version)
		if err != nil {
			return fmt.Errorf("check migration v%d: %w", m.version, err)
		}
		if applied {
			continue
		}

		if err := applyMigration(ctx, pool, m); err != nil {
			return fmt.Errorf("apply migration v%d: %w", m.version, err)
		}
		slog.Info("Schema migration applied", "version", m.version)
	}
	return nil
}

func isMigrationApplied(ctx context.Context, pool *pgxpool.Pool, version int) (bool, error) {
	var count int
	err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM schema_migrations WHERE version = $1`, version,
	).Scan(&count)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

func applyMigration(ctx context.Context, pool *pgxpool.Pool, m pgMigration) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback(ctx)
		}
	}()

	if _, err = tx.Exec(ctx, m.sql); err != nil {
		return fmt.Errorf("execute migration SQL: %w", err)
	}

	if _, err = tx.Exec(ctx,
		`INSERT INTO schema_migrations (version) VALUES ($1) ON CONFLICT DO NOTHING`, m.version,
	); err != nil {
		return fmt.Errorf("record migration version: %w", err)
	}

	return tx.Commit(ctx)
}
