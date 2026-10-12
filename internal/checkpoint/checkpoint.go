// Package checkpoint remembers where a run stopped.
//
// A migration of a live title takes hours, and it will be interrupted: a deploy, a laptop, a
// twenty-hour run that dies at hour nineteen. The checkpoint is what makes the nineteenth hour
// worth keeping. It is written after the batch's target writes and before the batch is reported
// as done, so a crash loses at most one batch of work rather than the run.
package checkpoint

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Checkpoint is where a stream of work stopped.
type Checkpoint struct {
	Source      string
	Stream      string
	Cursor      string
	BatchNo     int
	PlayersDone int64
}

// Store reads and writes checkpoints.
// DB is what a checkpoint needs. Two methods, because a checkpoint is two statements: read where the
// run stopped, write where it stopped. *pgxpool.Pool and *pgx.Conn both satisfy it, and the pool is
// what a concurrent run has to pass - see the same note in internal/ledger.
type DB interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

type Store struct{ db DB }

// Open returns a store over an existing connection.
func Open(db DB) *Store { return &Store{db: db} }

// Load returns the checkpoint for a stream. The second return is false when the stream has never
// run, which is how a first run and a resumed run are told apart without a flag.
func (s *Store) Load(ctx context.Context, source, stream string) (Checkpoint, bool, error) {
	var c Checkpoint
	err := s.db.QueryRow(ctx, `
		select source, stream, cursor, batch_no, players_done
		  from import_checkpoint where source = $1 and stream = $2`, source, stream).
		Scan(&c.Source, &c.Stream, &c.Cursor, &c.BatchNo, &c.PlayersDone)
	if err == pgx.ErrNoRows {
		return Checkpoint{}, false, nil
	}
	if err != nil {
		return Checkpoint{}, false, fmt.Errorf("checkpoint: load %s/%s: %w", source, stream, err)
	}
	return c, true, nil
}

// Save records where a stream has got to.
func (s *Store) Save(ctx context.Context, c Checkpoint) error {
	_, err := s.db.Exec(ctx, `
		insert into import_checkpoint (source, stream, cursor, batch_no, players_done)
		values ($1, $2, $3, $4, $5)
		on conflict (source, stream) do update
		   set cursor = excluded.cursor,
		       batch_no = excluded.batch_no,
		       players_done = excluded.players_done,
		       updated_at = now()`,
		c.Source, c.Stream, c.Cursor, c.BatchNo, c.PlayersDone)
	if err != nil {
		return fmt.Errorf("checkpoint: save %s/%s: %w", c.Source, c.Stream, err)
	}
	return nil
}

// Clear deletes a stream's checkpoint, for a run that has finished the listing.
func (s *Store) Clear(ctx context.Context, source, stream string) error {
	_, err := s.db.Exec(ctx, `delete from import_checkpoint where source = $1 and stream = $2`, source, stream)
	if err != nil {
		return fmt.Errorf("checkpoint: clear %s/%s: %w", source, stream, err)
	}
	return nil
}
