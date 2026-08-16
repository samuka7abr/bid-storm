// Package db owns the connection pool and the schema-version guard.
package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// NewPool builds the pgx pool with MaxConns pinned to size.
//
// The pool is the most dangerous parameter of the project, because it is the
// pessimistic engine's whole story: with a small pool the queue forms in
// pgxpool instead of in the Postgres lock, and a configuration cost gets
// charged to the concurrency mechanism. One knob, one value, all three engines.
func NewPool(ctx context.Context, url string, size int32) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("parse DATABASE_URL: %w", err)
	}
	cfg.MaxConns = size

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}
	return pool, nil
}
