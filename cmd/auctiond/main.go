// Command auctiond serves the auction API.
//
// It runs exactly one bid strategy per process, chosen at boot by BID_STRATEGY:
// the benchmark compares three engines under one configuration, and a process
// that could switch mid-run would be measuring the switch.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/samuka7abr/bid-storm/internal/app"
	"github.com/samuka7abr/bid-storm/internal/config"
	"github.com/samuka7abr/bid-storm/internal/db"
	"github.com/samuka7abr/bid-storm/internal/httpapi"
	"github.com/samuka7abr/bid-storm/internal/metrics"
	"github.com/samuka7abr/bid-storm/internal/store"
)

const shutdownGrace = 10 * time.Second

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(log); err != nil {
		log.Error("auctiond stopped", "error", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := db.NewPool(ctx, cfg.DatabaseURL, cfg.DBPoolSize)
	if err != nil {
		return err
	}
	defer pool.Close()

	// A schema divergence does not kill the process. It keeps /healthz green and
	// /readyz red, so the cause reaches the orchestrator instead of turning into
	// a mute crashloop.
	schemaErr := db.CheckSchema(ctx, pool)
	found := db.ExpectedSchemaVersion
	var schemaFault *db.SchemaError
	if errors.As(schemaErr, &schemaFault) {
		found = schemaFault.Found
	}

	log.Info("auctiond boot",
		"strategy", cfg.BidStrategy,
		"pool_size", cfg.DBPoolSize,
		"schema_version", found,
		"addr", cfg.HTTPAddr,
	)
	if schemaErr != nil {
		log.Error("schema check failed, /readyz will answer 503",
			"expected", db.ExpectedSchemaVersion,
			"found", found,
			"error", schemaErr.Error(),
		)
	}

	registry := metrics.NewRegistry()
	metrics.RegisterPool(registry, pool)

	// A strategy with no engine behind it aborts the boot instead of serving a
	// whole benchmark cell's worth of 503s.
	engine, err := app.NewEngine(cfg.BidStrategy, pool, registry)
	if err != nil {
		return err
	}

	gin.SetMode(gin.ReleaseMode)
	srv := &http.Server{
		Addr: cfg.HTTPAddr,
		Handler: httpapi.New(httpapi.Deps{
			Ping:     pool.Ping,
			Schema:   func(ctx context.Context) error { return db.CheckSchema(ctx, pool) },
			Metrics:  metrics.Handler(registry),
			Engine:   engine,
			Auctions: store.New(pool),
			Log:      log,
		}),
	}

	serveErr := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
	}()

	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
	}

	log.Info("auctiond draining", "grace", shutdownGrace.String())
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}
