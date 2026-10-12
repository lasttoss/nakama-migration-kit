// Package migrate applies the importer's own schema.
//
// It is deliberately not a migration framework: the files are numbered SQL, applied in order, and
// recorded in schema_version. A migration tool would be a second dependency and a second way for
// a run to differ between the developer's machine and CI.
package migrate

import (
	"context"
	"embed"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

//go:embed migrations/*.sql
var files embed.FS

// Apply runs every migration that has not been applied yet, in file order.
func Apply(ctx context.Context, conn *pgx.Conn) error {
	if _, err := conn.Exec(ctx, `create table if not exists schema_version (
		version integer primary key,
		applied_at timestamptz not null default now()
	)`); err != nil {
		return fmt.Errorf("migrate: create schema_version: %w", err)
	}

	applied, err := appliedVersions(ctx, conn)
	if err != nil {
		return err
	}

	names, err := migrationNames()
	if err != nil {
		return err
	}
	for _, name := range names {
		version, err := versionOf(name)
		if err != nil {
			return err
		}
		if applied[version] {
			continue
		}
		body, err := files.ReadFile("migrations/" + name)
		if err != nil {
			return fmt.Errorf("migrate: read %s: %w", name, err)
		}
		// One transaction per file: a migration that fails half way leaves the schema_version
		// row unwritten and the next run retries the same file, rather than continuing from a
		// half-applied schema.
		tx, err := conn.Begin(ctx)
		if err != nil {
			return fmt.Errorf("migrate: begin %s: %w", name, err)
		}
		if _, err := tx.Exec(ctx, string(body)); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("migrate: apply %s: %w", name, err)
		}
		if _, err := tx.Exec(ctx, "insert into schema_version (version) values ($1)", version); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("migrate: record %s: %w", name, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("migrate: commit %s: %w", name, err)
		}
	}
	return nil
}

// Applied lists the versions already applied, which the run report prints.
func Applied(ctx context.Context, conn *pgx.Conn) ([]int, error) {
	rows, err := conn.Query(ctx, "select version from schema_version order by version")
	if err != nil {
		return nil, fmt.Errorf("migrate: list: %w", err)
	}
	defer rows.Close()
	var out []int
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("migrate: scan: %w", err)
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func appliedVersions(ctx context.Context, conn *pgx.Conn) (map[int]bool, error) {
	versions, err := Applied(ctx, conn)
	if err != nil {
		return nil, err
	}
	out := make(map[int]bool, len(versions))
	for _, v := range versions {
		out[v] = true
	}
	return out, nil
}

func migrationNames() ([]string, error) {
	entries, err := files.ReadDir("migrations")
	if err != nil {
		return nil, fmt.Errorf("migrate: list files: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

func versionOf(name string) (int, error) {
	base := strings.SplitN(name, "_", 2)[0]
	v, err := strconv.Atoi(base)
	if err != nil {
		return 0, fmt.Errorf("migrate: %s does not start with a number", name)
	}
	return v, nil
}
