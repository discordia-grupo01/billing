package repository

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/discordia-grupo01/billing/internal/telemetry"
)

func NewPool(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("repository: invalid database settings: %w", err)
	}
	config.ConnConfig.Tracer = telemetry.PgxTracer{}
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("repository: cannot create pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("repository: cannot ping database: %w", err)
	}
	return pool, nil
}
