// Package ledger is the importer's memory of what it has already written.
//
// It exists so that "nothing was written the second time" is a number the importer can print,
// and so that a run that stops in the middle can start again without deciding from scratch what
// is already there. It is append-only: a run records the checksum it wrote, and a later run
// records the next checksum. Nothing is updated in place, because the question an operator asks
// is "what did we write, when", and an overwritten row cannot answer it.
package ledger

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/lasttoss/nakama-migration-kit/internal/mapping"
)

// Ledger writes and reads the importer's tables.
type Ledger struct {
	conn *pgx.Conn
}

// Open returns a ledger over an existing connection.
func Open(conn *pgx.Conn) *Ledger { return &Ledger{conn: conn} }

// Identity is what the kit knows about a player's account.
type Identity struct {
	Source       string
	SourceID     string
	CustomID     string
	NakamaUserID string
}

// SeenIdentity records the account a player id maps to.
//
// The unique index on custom_id is the point: if a second source id derived the same custom id,
// the insert fails rather than attaching two players to one account. A run that hits this has a
// bug in its identity derivation, and stopping is the correct behaviour.
func (l *Ledger) SeenIdentity(ctx context.Context, id Identity) error {
	_, err := l.conn.Exec(ctx, `
		insert into identity_map (source, source_id, custom_id, nakama_user_id)
		values ($1, $2, $3, $4)
		on conflict (source, source_id) do update
		   set nakama_user_id = excluded.nakama_user_id,
		       last_seen_at = now()
		 where identity_map.custom_id = excluded.custom_id`,
		id.Source, id.SourceID, id.CustomID, id.NakamaUserID)
	if err != nil {
		return fmt.Errorf("ledger: record identity %s/%s: %w", id.Source, id.SourceID, err)
	}
	return nil
}

// Identity returns the account for a player, if the kit has seen it.
func (l *Ledger) Identity(ctx context.Context, source, sourceID string) (Identity, bool, error) {
	var id Identity
	err := l.conn.QueryRow(ctx, `
		select source, source_id, custom_id, nakama_user_id
		  from identity_map where source = $1 and source_id = $2`, source, sourceID).
		Scan(&id.Source, &id.SourceID, &id.CustomID, &id.NakamaUserID)
	if err == pgx.ErrNoRows {
		return Identity{}, false, nil
	}
	if err != nil {
		return Identity{}, false, fmt.Errorf("ledger: identity %s/%s: %w", source, sourceID, err)
	}
	return id, true, nil
}

// Latest returns the newest checksum the kit recorded for each document of a player.
func (l *Ledger) Latest(ctx context.Context, source, sourceID string) (map[string]string, error) {
	rows, err := l.conn.Query(ctx, `
		select entity, checksum from import_ledger_latest
		 where source = $1 and source_id = $2`, source, sourceID)
	if err != nil {
		return nil, fmt.Errorf("ledger: latest %s/%s: %w", source, sourceID, err)
	}
	defer rows.Close()

	out := map[string]string{}
	for rows.Next() {
		var entity, checksum string
		if err := rows.Scan(&entity, &checksum); err != nil {
			return nil, fmt.Errorf("ledger: scan: %w", err)
		}
		out[entity] = checksum
	}
	return out, rows.Err()
}

// Append records what a run wrote for one player.
//
// Appending the same checksum twice is a no-op rather than an error: a run that crashed after
// writing the target but before recording it repeats the append, and a migration that fails on
// its own retry is worse than one that does not.
func (l *Ledger) Append(ctx context.Context, runID int64, source, sourceID string, entities []mapping.Entity) error {
	batch := &pgx.Batch{}
	for _, e := range entities {
		batch.Queue(`
			insert into import_ledger (source, entity, source_id, checksum, run_id)
			values ($1, $2, $3, $4, $5)
			on conflict (source, entity, source_id, checksum) do nothing`,
			source, e.Name, sourceID, e.Checksum, runID)
	}
	results := l.conn.SendBatch(ctx, batch)
	defer results.Close()
	for range entities {
		if _, err := results.Exec(); err != nil {
			return fmt.Errorf("ledger: append %s/%s: %w", source, sourceID, err)
		}
	}
	return nil
}

// Counts is what a run did, as the report prints it.
type Counts struct {
	Players    int64
	Documents  int64
	Recorded   int64
	Errors     int64
	Mismatches int64
}

// Run starts a run and returns its id.
func (l *Ledger) Run(ctx context.Context, source, strategy, mode string) (int64, error) {
	var id int64
	err := l.conn.QueryRow(ctx, `
		insert into import_run (source, strategy, mode) values ($1, $2, $3) returning id`,
		source, strategy, mode).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("ledger: start run: %w", err)
	}
	return id, nil
}

// FinishRun closes a run with its counters.
func (l *Ledger) FinishRun(ctx context.Context, runID int64, seen, missing, written, identical, errors int64) error {
	_, err := l.conn.Exec(ctx, `
		update import_run
		   set finished_at = now(), players_seen = $2, players_missing = $3,
		       documents_written = $4, documents_identical = $5, errors = $6
		 where id = $1`, runID, seen, missing, written, identical, errors)
	if err != nil {
		return fmt.Errorf("ledger: finish run %d: %w", runID, err)
	}
	return nil
}

// RecordMismatch stores a difference reconciliation found, so it survives the terminal.
func (l *Ledger) RecordMismatch(ctx context.Context, runID int64, sourceID, entity, detail string) error {
	_, err := l.conn.Exec(ctx, `
		insert into import_mismatch (run_id, source_id, entity, detail)
		values ($1, $2, $3, $4)
		on conflict (run_id, source_id, entity) do update set detail = excluded.detail`,
		runID, sourceID, entity, detail)
	if err != nil {
		return fmt.Errorf("ledger: record mismatch: %w", err)
	}
	return nil
}

// Mismatch is one difference between the source and the target.
type Mismatch struct {
	SourceID string
	Entity   string
	Detail   string
}

// Mismatches lists what reconciliation found for a run, ordered so a report is stable.
func (l *Ledger) Mismatches(ctx context.Context, runID int64) ([]Mismatch, error) {
	rows, err := l.conn.Query(ctx, `
		select source_id, entity, detail from import_mismatch
		 where run_id = $1 order by source_id, entity`, runID)
	if err != nil {
		return nil, fmt.Errorf("ledger: list mismatches: %w", err)
	}
	defer rows.Close()

	var out []Mismatch
	for rows.Next() {
		var m Mismatch
		if err := rows.Scan(&m.SourceID, &m.Entity, &m.Detail); err != nil {
			return nil, fmt.Errorf("ledger: scan mismatch: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// CountsOf returns how many documents a run recorded.
func (l *Ledger) CountsOf(ctx context.Context, runID int64) (int64, error) {
	var n int64
	err := l.conn.QueryRow(ctx, `select count(*) from import_ledger where run_id = $1`, runID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("ledger: count run %d: %w", runID, err)
	}
	return n, nil
}

// IdentityCount is how many accounts the kit has mapped.
func (l *Ledger) IdentityCount(ctx context.Context, source string) (int64, error) {
	var n int64
	err := l.conn.QueryRow(ctx, `select count(*) from identity_map where source = $1`, source).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("ledger: count identities: %w", err)
	}
	return n, nil
}
