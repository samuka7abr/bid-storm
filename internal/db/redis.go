package db

import (
	"context"
	"fmt"

	"github.com/redis/go-redis/v9"
)

// NewRedis parses the URL, builds the client and proves it answers.
//
// It lives next to NewPool, and in the same package, because the Streams of
// etapa 4 need this same client: one place in the process opens infrastructure
// connections, and neither the HTTP layer nor the middleware learns what is
// behind them.
//
// The PING is here and not deferred to the first bid for the same reason an
// unknown BID_STRATEGY aborts the boot: a process that comes up and only then
// discovers it cannot reach Redis turns a wrong URL into a whole benchmark
// cell's worth of 503s. Redis dying later is a different matter — that keeps the
// process alive, turns /readyz red and /healthz green.
//
// The connection pool is left at the client's default on purpose: DB_POOL_SIZE
// is the pool of the experiment, and a second knob would be a fifth axis in a
// matrix that did not ask for one.
func NewRedis(ctx context.Context, url string) (*redis.Client, error) {
	opts, err := redis.ParseURL(url)
	if err != nil {
		return nil, fmt.Errorf("parse REDIS_URL: %w", err)
	}

	client := redis.NewClient(opts)
	if err := client.Ping(ctx).Err(); err != nil {
		client.Close()
		return nil, fmt.Errorf("ping redis: %w", err)
	}
	return client, nil
}
