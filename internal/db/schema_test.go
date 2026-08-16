package db_test

import (
	"context"
	"errors"
	"testing"

	"github.com/samuka7abr/auction-system/internal/db"
	"github.com/samuka7abr/auction-system/internal/testsupport"
)

// CheckSchema has to tell the three failures apart, because each one calls for
// a different move: run the migrations, repair a half-applied one, or deploy
// the binary that matches.
func TestCheckSchema(t *testing.T) {
	ctx := context.Background()
	pg := testsupport.Start(t)
	// Every case below mutates schema_migrations, and the container is shared by
	// the whole package.
	t.Cleanup(func() {
		if err := pg.RecordVersion(ctx, db.ExpectedSchemaVersion, false); err != nil {
			t.Fatalf("restore schema_migrations: %v", err)
		}
	})

	exec := func(t *testing.T, sql string) {
		t.Helper()
		if _, err := pg.Pool.Exec(ctx, sql); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}

	fault := func(t *testing.T, err error) *db.SchemaError {
		t.Helper()
		var se *db.SchemaError
		if !errors.As(err, &se) {
			t.Fatalf("error = %v, want a *db.SchemaError", err)
		}
		return se
	}

	t.Run("expected version passes", func(t *testing.T) {
		if err := pg.RecordVersion(ctx, db.ExpectedSchemaVersion, false); err != nil {
			t.Fatalf("record version: %v", err)
		}
		if err := db.CheckSchema(ctx, pg.Pool); err != nil {
			t.Fatalf("CheckSchema = %v, want nil", err)
		}
	})

	t.Run("dirty", func(t *testing.T) {
		exec(t, `UPDATE schema_migrations SET dirty = true`)
		se := fault(t, db.CheckSchema(ctx, pg.Pool))
		if se.Fault != db.SchemaDirty {
			t.Errorf("fault = %q, want %q", se.Fault, db.SchemaDirty)
		}
	})

	t.Run("wrong version", func(t *testing.T) {
		exec(t, `UPDATE schema_migrations SET version = 99, dirty = false`)
		se := fault(t, db.CheckSchema(ctx, pg.Pool))
		if se.Fault != db.SchemaMismatch {
			t.Errorf("fault = %q, want %q", se.Fault, db.SchemaMismatch)
		}
		if se.Expected != db.ExpectedSchemaVersion || se.Found != 99 {
			t.Errorf("expected/found = %d/%d, want %d/99", se.Expected, se.Found, db.ExpectedSchemaVersion)
		}
	})

	t.Run("table absent", func(t *testing.T) {
		exec(t, `DROP TABLE schema_migrations`)
		se := fault(t, db.CheckSchema(ctx, pg.Pool))
		if se.Fault != db.SchemaMissing {
			t.Errorf("fault = %q, want %q", se.Fault, db.SchemaMissing)
		}
	})

	t.Run("table present but empty", func(t *testing.T) {
		exec(t, `CREATE TABLE schema_migrations (version bigint NOT NULL PRIMARY KEY, dirty boolean NOT NULL)`)
		se := fault(t, db.CheckSchema(ctx, pg.Pool))
		if se.Fault != db.SchemaMissing {
			t.Errorf("fault = %q, want %q", se.Fault, db.SchemaMissing)
		}
	})
}
