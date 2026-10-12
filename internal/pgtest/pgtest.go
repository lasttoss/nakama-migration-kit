// Package pgtest gives every package its own PostgreSQL schema.
//
// `go test ./...` runs packages in parallel, so two packages that create a table with the same
// name in the same database fail each other's tests in a way that looks like a bug in the code
// under test. Each package gets its own schema, and this has bitten the house before.
//
// When MIGRATION_TEST_DSN is not set the tests skip, and the skip says what did not run rather
// than passing quietly: `make test` runs the unit tests, `make test-db` sets the variable and
// runs everything.
package pgtest

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/lasttoss/nakama-migration-kit/internal/migrate"
)

// DSNEnv is the environment variable that turns database tests on.
const DSNEnv = "MIGRATION_TEST_DSN"

// Conn returns a connection whose search_path is a schema private to this test.
//
// The schema is dropped when the test ends, so a failing test does not leave state behind that
// makes the next run pass.
func Conn(t *testing.T) *pgx.Conn {
	t.Helper()
	dsn := os.Getenv(DSNEnv)
	if dsn == "" {
		t.Skipf("skipping the database test: %s is not set (run `make test-db`, or start PostgreSQL and set it)", DSNEnv)
	}

	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to %s: %v", DSNEnv, err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })

	schema := schemaName(t.Name())
	if _, err := conn.Exec(ctx, "create schema if not exists "+quote(schema)); err != nil {
		t.Fatalf("create schema %s: %v", schema, err)
	}
	if _, err := conn.Exec(ctx, "set search_path to "+quote(schema)+", public"); err != nil {
		t.Fatalf("set search_path: %v", err)
	}
	t.Cleanup(func() {
		c, err := pgx.Connect(context.Background(), dsn)
		if err != nil {
			return
		}
		defer c.Close(context.Background())
		_, _ = c.Exec(context.Background(), "drop schema if exists "+quote(schema)+" cascade")
	})
	return conn
}

// ConnMigrated is Conn with the importer's schema applied.
func ConnMigrated(t *testing.T) *pgx.Conn {
	t.Helper()
	conn := Conn(t)
	if err := migrate.Apply(context.Background(), conn); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	return conn
}

// schemaName turns a test name into a schema name: TestLedger/append_only becomes
// t_ledger_append_only.
func schemaName(testName string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(testName) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '/':
			b.WriteByte('_')
		case r == '-':
			b.WriteByte('_')
		}
	}
	name := "t_" + b.String()
	if len(name) > 60 {
		name = name[:60]
	}
	if name == "t_" {
		name = "t_anon"
	}
	return name
}

func quote(ident string) string {
	return `"` + strings.ReplaceAll(ident, `"`, `""`) + `"`
}

// Explain is used by tests that want to prove a query uses an index, not by the code under test.
func Explain(t *testing.T, conn *pgx.Conn, query string, args ...any) string {
	t.Helper()
	rows, err := conn.Query(context.Background(), "explain "+query, args...)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

var _ = fmt.Sprintf
