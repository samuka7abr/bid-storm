// Package config reads the process configuration from the environment.
package config

import (
	"fmt"
	"os"
	"strconv"
)

// Config holds every knob the process reads at boot. The three engines are
// compared under one configuration, so nothing here is per-strategy — see
// docs/decisoes/etapa-1.md, decisão 7.
type Config struct {
	DatabaseURL string
	RedisURL    string
	HTTPAddr    string
	DBPoolSize  int32
	BidStrategy string
}

// Defaults for everything that has a sane one. DATABASE_URL deliberately does
// not: a process that silently points at the wrong database produces a
// benchmark nobody can explain.
const (
	DefaultRedisURL    = "redis://localhost:6379/0"
	DefaultHTTPAddr    = ":8080"
	DefaultDBPoolSize  = 25
	DefaultBidStrategy = "optimistic"
)

// Load reads the environment and fails on anything it cannot honour.
func Load() (Config, error) {
	c := Config{
		DatabaseURL: os.Getenv("DATABASE_URL"),
		RedisURL:    valueOr("REDIS_URL", DefaultRedisURL),
		HTTPAddr:    valueOr("HTTP_ADDR", DefaultHTTPAddr),
		DBPoolSize:  DefaultDBPoolSize,
		BidStrategy: valueOr("BID_STRATEGY", DefaultBidStrategy),
	}

	if c.DatabaseURL == "" {
		return Config{}, fmt.Errorf("DATABASE_URL is required and has no default")
	}

	if raw := os.Getenv("DB_POOL_SIZE"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			return Config{}, fmt.Errorf("DB_POOL_SIZE must be a positive integer, got %q", raw)
		}
		c.DBPoolSize = int32(n)
	}

	return c, nil
}

func valueOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
