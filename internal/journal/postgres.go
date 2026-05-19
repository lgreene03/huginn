package journal

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lgreene/huginn/internal/model"
	"github.com/lgreene/huginn/internal/portfolio"
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
	`
	_, err := w.pool.Exec(ctx, query)
	return err
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
