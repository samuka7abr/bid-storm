package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ExpectedSchemaVersion is the migration version this binary was written
// against. It is a constant rather than configuration on purpose: serving
// requests against a differently shaped database would not crash, it would
// produce an inexplicable number in the results table three hours later.
const ExpectedSchemaVersion int64 = 1

// SchemaFault names the three ways the check can fail. They are kept apart
// because each one needs a different move from whoever is on call: run the
// migrations, repair a half-applied one, or deploy the matching binary.
type SchemaFault string

const (
	SchemaMissing  SchemaFault = "missing"  // migrations never ran
	SchemaDirty    SchemaFault = "dirty"    // a migration failed halfway through
	SchemaMismatch SchemaFault = "mismatch" // migrations ran, wrong version
)

// SchemaError carries what the operator needs in order to fix the divergence.
type SchemaError struct {
	Fault    SchemaFault
	Expected int64
	Found    int64
}

func (e *SchemaError) Error() string {
	switch e.Fault {
	case SchemaMissing:
		return "schema_migrations not found: migrations have not been applied"
	case SchemaDirty:
		return fmt.Sprintf("schema is dirty at version %d: a migration failed halfway", e.Found)
	default:
		return fmt.Sprintf("schema version mismatch: expected %d, found %d", e.Expected, e.Found)
	}
}

// undefinedTable is the SQLSTATE Postgres raises for a missing relation.
const undefinedTable = "42P01"

// CheckSchema reads the version golang-migrate recorded and compares it to
// ExpectedSchemaVersion. It is called at boot and again on every /readyz, so a
// schema changed underneath a running process is caught too.
func CheckSchema(ctx context.Context, pool *pgxpool.Pool) error {
	var (
		version int64
		dirty   bool
	)
	err := pool.QueryRow(ctx, `SELECT version, dirty FROM schema_migrations`).Scan(&version, &dirty)

	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// The table exists but holds no version: golang-migrate never got to run.
		return &SchemaError{Fault: SchemaMissing, Expected: ExpectedSchemaVersion}
	case err != nil:
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == undefinedTable {
			return &SchemaError{Fault: SchemaMissing, Expected: ExpectedSchemaVersion}
		}
		return fmt.Errorf("read schema_migrations: %w", err)
	case dirty:
		return &SchemaError{Fault: SchemaDirty, Expected: ExpectedSchemaVersion, Found: version}
	case version != ExpectedSchemaVersion:
		return &SchemaError{Fault: SchemaMismatch, Expected: ExpectedSchemaVersion, Found: version}
	}

	return nil
}
