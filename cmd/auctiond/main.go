// Command auctiond serves the auction API.
//
// In this spec it serves health and metrics only: the bid engine and its routes
// arrive in spec 02, against the database and the process this one erects.
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

	"github.com/samuka7abr/bid-storm/internal/config"
	"github.com/samuka7abr/bid-storm/internal/db"
	"github.com/samuka7abr/bid-storm/internal/httpapi"
	"github.com/samuka7abr/bid-storm/internal/metrics"
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

	// BID_STRATEGY is read and logged here but selects nothing yet: the key
	// exists so spec 02 only has to plug the engine into it.
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

	gin.SetMode(gin.ReleaseMode)
	srv := &http.Server{
		Addr: cfg.HTTPAddr,
		Handler: httpapi.New(httpapi.Deps{
			Ping:    pool.Ping,
			Schema:  func(ctx context.Context) error { return db.CheckSchema(ctx, pool) },
			Metrics: metrics.Handler(registry),
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
