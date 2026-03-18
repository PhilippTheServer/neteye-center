package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/neteye/center/internal/config"
)

// Connect opens a pgx connection pool and runs all pending migrations.
func Connect(ctx context.Context, cfg config.DatabaseConfig) (*pgxpool.Pool, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("parse db dsn: %w", err)
	}
	poolCfg.MaxConns = cfg.MaxConns

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("open pool: %w", err)
	}

	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping db: %w", err)
	}

	if err := migrate(ctx, pool); err != nil {
		pool.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}

	return pool, nil
}
