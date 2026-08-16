package db_test

import (
	"context"
	"testing"

	"github.com/samuka7abr/auction-system/internal/testsupport"
)

// The migration has to be reversible, and reapplying it has to land on exactly
// the same schema. Otherwise `make migrate-down` between benchmark cells would
// slowly drift the database the results are measured against.
func TestMigrationRoundTripLeavesNoDrift(t *testing.T) {
	ctx := context.Background()
	pg := testsupport.Start(t)

	before, err := pg.DumpSchema(ctx)
	if err != nil {
		t.Fatalf("dump before: %v", err)
	}

	if err := pg.MigrateDown(ctx); err != nil {
		t.Fatalf("migrate down: %v", err)
	}
	for _, relation := range []string{"auctions", "bids"} {
		var exists bool
		if err := pg.Pool.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, relation).Scan(&exists); err != nil {
			t.Fatalf("look up %s: %v", relation, err)
		}
		if exists {
			t.Errorf("%s still exists after down", relation)
		}
	}

	if err := pg.Migrate(ctx); err != nil {
		t.Fatalf("migrate up again: %v", err)
	}
	after, err := pg.DumpSchema(ctx)
	if err != nil {
		t.Fatalf("dump after: %v", err)
	}

	if before != after {
		t.Errorf("schema drifted across up -> down -> up\nbefore:\n%s\nafter:\n%s", before, after)
	}
}
