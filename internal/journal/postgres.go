package journal

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lgreene03/huginn/internal/model"
	"github.com/lgreene03/huginn/internal/portfolio"
)

// PostgresWriter logs simulated trade fills into a PostgreSQL database.
type PostgresWriter struct {
	pool *pgxpool.Pool
}

// NewPostgresWriter connects to PostgreSQL, validates the connection, and prepares the schema.
func NewPostgresWriter(connStr string) (*PostgresWriter, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	config, err := pgxpool.ParseConfig(connStr)
	if err != nil {
		return nil, fmt.Errorf("failed to parse connection string: %w", err)
	}

	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("failed to create connection pool: %w", err)
	}

	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("failed to ping database: %w", err)
	}

	pw := &PostgresWriter{pool: pool}

	if err := pw.initSchema(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("failed to initialize schema: %w", err)
	}

	return pw, nil
}

func (w *PostgresWriter) initSchema(ctx context.Context) error {
	query := `
	CREATE TABLE IF NOT EXISTS trade_fills (
		order_id VARCHAR(64) PRIMARY KEY,
		instrument VARCHAR(32) NOT NULL,
		side INT NOT NULL,
		quantity DOUBLE PRECISION NOT NULL,
		fill_price DOUBLE PRECISION NOT NULL,
		transaction_cost DOUBLE PRECISION NOT NULL,
		slippage_bps DOUBLE PRECISION NOT NULL,
		timestamp TIMESTAMPTZ NOT NULL
	);
	CREATE INDEX IF NOT EXISTS idx_trade_fills_timestamp ON trade_fills(timestamp);

	CREATE TABLE IF NOT EXISTS strategy_state (
		strategy_key VARCHAR(64) PRIMARY KEY,
		state_blob JSONB NOT NULL,
		updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	);
	CREATE INDEX IF NOT EXISTS idx_strategy_state_updated ON strategy_state(updated_at);
	`
	_, err := w.pool.Exec(ctx, query)
	return err
}

// AppendStrategyState upserts the latest state blob for the given strategy
// key. The PRIMARY KEY constraint guarantees latest-wins semantics; we don't
// keep history.
func (w *PostgresWriter) AppendStrategyState(key string, blob []byte) error {
	if len(blob) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	query := `
	INSERT INTO strategy_state (strategy_key, state_blob, updated_at)
	VALUES ($1, $2, NOW())
	ON CONFLICT (strategy_key) DO UPDATE
	SET state_blob = EXCLUDED.state_blob, updated_at = NOW();
	`
	_, err := w.pool.Exec(ctx, query, key, blob)
	return err
}

// LoadStrategyState returns the most recent state blob for the given key, or
// (nil, nil) if there is none. Used by the boot path to seed the strategy.
func (w *PostgresWriter) LoadStrategyState(key string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var blob []byte
	err := w.pool.QueryRow(ctx,
		`SELECT state_blob FROM strategy_state WHERE strategy_key = $1`, key,
	).Scan(&blob)
	if err != nil {
		// pgx returns ErrNoRows from the database/sql import path; tolerate
		// both that and the literal "no rows" string for cross-version safety.
		if err.Error() == "no rows in result set" {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to load strategy state: %w", err)
	}
	return blob, nil
}

// Append inserts a trade fill into the trade_fills table.
func (w *PostgresWriter) Append(fill model.Fill) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	query := `
	INSERT INTO trade_fills (
		order_id, instrument, side, quantity, fill_price, transaction_cost, slippage_bps, timestamp
	) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
	ON CONFLICT (order_id) DO NOTHING;
	`
	_, err := w.pool.Exec(ctx, query,
		fill.OrderID,
		fill.Instrument,
		int(fill.Side),
		fill.Quantity,
		fill.FillPrice,
		fill.TransactionCost,
		fill.SlippageBps,
		fill.Timestamp,
	)
	return err
}

// Close closes the connection pool.
func (w *PostgresWriter) Close() error {
	w.pool.Close()
	return nil
}

// RecoverPortfolioFromPostgres replays trade fills from PostgreSQL to rebuild the portfolio state.
func RecoverPortfolioFromPostgres(connStr string, initialCash float64) (*portfolio.Portfolio, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, connStr)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to database for recovery: %w", err)
	}
	defer pool.Close()

	port := portfolio.New(initialCash)

	query := `
	SELECT order_id, instrument, side, quantity, fill_price, transaction_cost, slippage_bps, timestamp
	FROM trade_fills
	ORDER BY timestamp ASC;
	`
	rows, err := pool.Query(ctx, query)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "42P01" {
			slog.Info("trade_fills table does not exist. Starting fresh portfolio.")
			return port, nil
		}
		return nil, fmt.Errorf("failed to query trade fills: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var fill model.Fill
		var sideInt int
		err := rows.Scan(
			&fill.OrderID,
			&fill.Instrument,
			&sideInt,
			&fill.Quantity,
			&fill.FillPrice,
			&fill.TransactionCost,
			&fill.SlippageBps,
			&fill.Timestamp,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan trade fill row: %w", err)
		}
		fill.Side = model.Side(sideInt)
		port.ApplyFill(fill)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error during trade fills row iteration: %w", err)
	}

	return port, nil
}
