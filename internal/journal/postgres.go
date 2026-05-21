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

// PoolConfig controls pgxpool connection-pool behaviour. Zero values use
// pgxpool defaults (MaxConns=4, MinConns=0, lifetimes unlimited).
type PoolConfig struct {
	MaxConns        int32
	MinConns        int32
	MaxConnLifetime time.Duration
	MaxConnIdleTime time.Duration
}

// PostgresWriter logs simulated trade fills into a PostgreSQL database.
type PostgresWriter struct {
	pool *pgxpool.Pool
}

// NewPostgresWriter connects to PostgreSQL, validates the connection, runs any
// pending schema migrations, and returns a ready writer.
//
// Pass a zero PoolConfig{} to use pgxpool defaults.
func NewPostgresWriter(connStr string, pool PoolConfig) (*PostgresWriter, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cfg, err := pgxpool.ParseConfig(connStr)
	if err != nil {
		return nil, fmt.Errorf("failed to parse connection string: %w", err)
	}

	if pool.MaxConns > 0 {
		cfg.MaxConns = pool.MaxConns
	}
	if pool.MinConns > 0 {
		cfg.MinConns = pool.MinConns
	}
	if pool.MaxConnLifetime > 0 {
		cfg.MaxConnLifetime = pool.MaxConnLifetime
	}
	if pool.MaxConnIdleTime > 0 {
		cfg.MaxConnIdleTime = pool.MaxConnIdleTime
	}

	p, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create connection pool: %w", err)
	}

	if err := p.Ping(ctx); err != nil {
		p.Close()
		return nil, fmt.Errorf("failed to ping database: %w", err)
	}

	pw := &PostgresWriter{pool: p}

	if err := runMigrations(ctx, p); err != nil {
		p.Close()
		return nil, fmt.Errorf("failed to run schema migrations: %w", err)
	}

	return pw, nil
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
