// Package testsupport starts an ephemeral Postgres for the tests.
//
// It is a package rather than a fixture buried inside one test file because the
// engine conformance suite of spec 03 runs against this same helper: one
// container per test binary, migrations already applied, real Postgres.
package testsupport

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcexec "github.com/testcontainers/testcontainers-go/exec"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/samuka7abr/auction-system/internal/db"
)

const (
	image    = "postgres:16-alpine"
	user     = "auction"
	password = "auction"
	database = "auction"
)

// DB is the container shared by every test in the package that asks for it.
type DB struct {
	Pool      *pgxpool.Pool
	URL       string
	container *postgres.PostgresContainer
}

var (
	once     sync.Once
	shared   *DB
	startErr error
)

// Start returns the package-wide Postgres, booting it on the first call.
func Start(t *testing.T) *DB {
	t.Helper()
	once.Do(func() { shared, startErr = start() })
	if startErr != nil {
		t.Fatalf("start postgres: %v", startErr)
	}
	return shared
}

func start() (*DB, error) {
	ctx := context.Background()

	container, err := postgres.Run(ctx, image,
		postgres.WithDatabase(database),
		postgres.WithUsername(user),
		postgres.WithPassword(password),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(90*time.Second),
		),
	)
	if err != nil {
		return nil, err
	}

	url, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		return nil, err
	}
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, err
	}

	d := &DB{Pool: pool, URL: url, container: container}
	if err := d.Migrate(ctx); err != nil {
		return nil, err
	}
	return d, nil
}

// Migrate applies migration 001 and records it the way golang-migrate would, so
// that db.CheckSchema sees under test exactly what it sees under the compose.
func (d *DB) Migrate(ctx context.Context) error {
	if err := d.apply(ctx, "000001_init.up.sql"); err != nil {
		return err
	}
	return d.RecordVersion(ctx, db.ExpectedSchemaVersion, false)
}

// RecordVersion writes schema_migrations the way golang-migrate would. Tests
// that wreck the marker on purpose put it back through here, without replaying
// the DDL on a database that already has it.
func (d *DB) RecordVersion(ctx context.Context, version int64, dirty bool) error {
	// One statement per Exec: a parameterised query goes through the extended
	// protocol, which refuses a multi-statement batch.
	statements := []struct {
		sql  string
		args []any
	}{
		{sql: `CREATE TABLE IF NOT EXISTS schema_migrations (
			version bigint NOT NULL PRIMARY KEY,
			dirty   boolean NOT NULL)`},
		{sql: `DELETE FROM schema_migrations`},
		{sql: `INSERT INTO schema_migrations (version, dirty) VALUES ($1, $2)`, args: []any{version, dirty}},
	}
	for _, s := range statements {
		if _, err := d.Pool.Exec(ctx, s.sql, s.args...); err != nil {
			return fmt.Errorf("record schema version: %w", err)
		}
	}
	return nil
}

// MigrateDown reverts migration 001 and forgets the recorded version.
func (d *DB) MigrateDown(ctx context.Context) error {
	if err := d.apply(ctx, "000001_init.down.sql"); err != nil {
		return err
	}
	_, err := d.Pool.Exec(ctx, `DELETE FROM schema_migrations`)
	return err
}

// DumpSchema returns `pg_dump -s`, which is what proves a down/up cycle left
// no drift behind.
func (d *DB) DumpSchema(ctx context.Context) (string, error) {
	code, out, err := d.container.Exec(ctx,
		[]string{"pg_dump", "--schema-only", "--no-owner", "-U", user, database},
		tcexec.Multiplexed())
	if err != nil {
		return "", err
	}
	dump, err := io.ReadAll(out)
	if err != nil {
		return "", err
	}
	if code != 0 {
		return "", fmt.Errorf("pg_dump exited %d: %s", code, dump)
	}

	// Recent pg_dump wraps its output in \restrict/\unrestrict guarded by a
	// random token. It differs on every run and describes nothing about the
	// schema, so leaving it in would make any two dumps compare unequal.
	var kept []string
	for _, line := range strings.Split(string(dump), "\n") {
		if strings.HasPrefix(line, `\restrict `) || strings.HasPrefix(line, `\unrestrict `) {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n"), nil
}

func (d *DB) apply(ctx context.Context, name string) error {
	_, file, _, _ := runtime.Caller(0)
	path := filepath.Join(filepath.Dir(file), "..", "..", "migrations", name)
	sql, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", name, err)
	}
	if _, err := d.Pool.Exec(ctx, string(sql)); err != nil {
		return fmt.Errorf("apply %s: %w", name, err)
	}
	return nil
}
