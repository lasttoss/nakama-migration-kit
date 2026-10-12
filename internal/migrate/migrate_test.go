package migrate_test

import (
	"context"
	"testing"

	"github.com/lasttoss/nakama-migration-kit/internal/migrate"
	"github.com/lasttoss/nakama-migration-kit/internal/pgtest"
)

// Applying twice is normal: every test that needs a schema applies it, and CI runs them all.
func TestMigrationsAreIdempotent(t *testing.T) {
	conn := pgtest.Conn(t)
	ctx := context.Background()

	if err := migrate.Apply(ctx, conn); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	applied, err := migrate.Applied(ctx, conn)
	if err != nil {
		t.Fatal(err)
	}
	if len(applied) == 0 {
		t.Fatal("nothing was recorded as applied")
	}

	if err := migrate.Apply(ctx, conn); err != nil {
		t.Fatalf("second apply: %v", err)
	}
	again, err := migrate.Applied(ctx, conn)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != len(applied) {
		t.Errorf("applying twice recorded %d versions, want %d", len(again), len(applied))
	}
}

func TestTheSchemaHasTheTablesTheKitWrites(t *testing.T) {
	conn := pgtest.ConnMigrated(t)
	for _, table := range []string{"identity_map", "import_ledger", "import_checkpoint", "import_run", "import_mismatch"} {
		var exists bool
		err := conn.QueryRow(context.Background(),
			`select exists (select 1 from information_schema.tables where table_schema = current_schema() and table_name = $1)`,
			table).Scan(&exists)
		if err != nil {
			t.Fatalf("%s: %v", table, err)
		}
		if !exists {
			t.Errorf("table %s is missing", table)
		}
	}
}
