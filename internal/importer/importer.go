// Package importer moves players from a legacy source into a target.
//
// It is a loop with three decisions in it, and each one is a place a migration silently goes wrong:
//
//   - identity is derived, never remembered. The custom id comes from the source record, so the
//     second run computes the same account instead of creating a second one, and a run that lost its
//     database cannot get it wrong;
//   - the ledger is written after the target says what it wrote, not before. Recording an intention
//     and then failing to carry it out is how a reconciliation ends up comparing the plan to itself;
//   - the checkpoint is written after a batch is done, and cleared when the whole stream is done. A
//     checkpoint left behind would make the second run skip the work, and "the second run writes
//     nothing" would be true for the wrong reason - because it did nothing.
package importer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"sync"
	"time"

	"github.com/lasttoss/nakama-migration-kit/internal/checkpoint"
	"github.com/lasttoss/nakama-migration-kit/internal/identity"
	"github.com/lasttoss/nakama-migration-kit/internal/ledger"
	"github.com/lasttoss/nakama-migration-kit/internal/mapping"
	"github.com/lasttoss/nakama-migration-kit/internal/profile"
	"github.com/lasttoss/nakama-migration-kit/internal/source"
	"github.com/lasttoss/nakama-migration-kit/internal/target"
)

// Ledger is the part of the ledger this loop uses. Narrow on purpose: the importer does not need to
// know how the numbers are stored, and a test does not need a database to check what it recorded.
type Ledger interface {
	Run(ctx context.Context, source, strategy, mode string) (int64, error)
	SeenIdentity(ctx context.Context, id ledger.Identity) error
	Append(ctx context.Context, runID int64, source, sourceID string, entities []mapping.Entity) error
	FinishRun(ctx context.Context, runID int64, seen, missing, written, identical, errors int64) error
}

// Checkpoints is where a stream of work stopped.
type Checkpoints interface {
	Load(ctx context.Context, source, stream string) (checkpoint.Checkpoint, bool, error)
	Save(ctx context.Context, c checkpoint.Checkpoint) error
	Clear(ctx context.Context, source, stream string) error
}

// Options is one run.
type Options struct {
	Source      source.Source
	Target      target.Target
	Ledger      Ledger
	Checkpoints Checkpoints
	Profile     profile.Profile

	// Strategy names the way the source is read ("playfab-admin-api", "title-export"), and goes into
	// the run record: two runs of the same title by different means are not the same run.
	Strategy string
	// DryRun asks the target what it would write. Nothing is written anywhere, including the ledger.
	DryRun bool
	// Limit stops after this many players (0 = all). It exists for a first run against a live title.
	Limit int
	// Batch is how many player ids one listing page holds, and how often a checkpoint is written.
	Batch int
	// Concurrency is how many players are in flight. It does not change what is written.
	Concurrency int

	// Log receives progress lines; nil means silence.
	Log io.Writer

	// runID is set by Run and carried into every player's ledger row. Unexported: it is not a
	// caller's decision, and a row recorded against the wrong run is a row no report can explain.
	runID int64
}

// Summary is what a run did.
type Summary struct {
	RunID     int64  `json:"run_id"`
	Source    string `json:"source"`
	Strategy  string `json:"strategy"`
	Mode      string `json:"mode"`
	Seen      int64  `json:"seen"`
	Changed   int64  `json:"players_changed"`
	Identical int64  `json:"players_identical"`
	Documents int64  `json:"documents_written"`
	// WouldWrite is what a dry run found it would write. It is a separate number from Documents on
	// purpose: a dry run reporting "documents written" would claim work it did not do, and the first
	// person to read the two numbers together would believe the wrong one.
	WouldWrite int64         `json:"documents_would_write,omitempty"`
	Skipped    int64         `json:"documents_skipped"`
	Errors     int64         `json:"errors"`
	Resumed    bool          `json:"resumed"`
	Batches    int           `json:"batches"`
	WallClock  time.Duration `json:"wall_clock"`
	Failures   []string      `json:"failures,omitempty"`
}

const streamPlayers = "players"

// Run imports every player the source lists.
func Run(ctx context.Context, opts Options) (Summary, error) {
	if opts.Source == nil || opts.Target == nil || opts.Ledger == nil || opts.Checkpoints == nil {
		return Summary{}, errors.New("importer: source, target, ledger and checkpoints are required")
	}
	if opts.Batch <= 0 {
		opts.Batch = 500
	}
	if opts.Concurrency <= 0 {
		opts.Concurrency = 4
	}
	if opts.Strategy == "" {
		opts.Strategy = opts.Source.Name()
	}
	mode := "apply"
	if opts.DryRun {
		mode = "dry-run"
	}

	start := time.Now()
	summary := Summary{Source: opts.Source.Name(), Strategy: opts.Strategy, Mode: mode}

	runID, err := opts.Ledger.Run(ctx, summary.Source, opts.Strategy, mode)
	if err != nil {
		return summary, fmt.Errorf("start the run: %w", err)
	}
	summary.RunID = runID
	opts.runID = runID

	// Resume where the last run stopped, if it stopped.
	cursor := ""
	if cp, ok, err := opts.Checkpoints.Load(ctx, summary.Source, streamPlayers); err != nil {
		return summary, fmt.Errorf("read the checkpoint: %w", err)
	} else if ok && cp.Cursor != "" {
		cursor, summary.Resumed = cp.Cursor, true
		summary.Batches = cp.BatchNo
		summary.Seen = cp.PlayersDone
		if opts.Log != nil {
			fmt.Fprintf(opts.Log, "resuming at batch %d, %d players already done\n", cp.BatchNo, cp.PlayersDone)
		}
	}

	// thisRun counts the players this run moved, which is not the same number as Seen: a resumed run
	// has already seen some of them, and a limit that counted those would stop before doing any work -
	// a resumed run reporting "nothing to do" on a migration that is half finished.
	var thisRun int64
	for {
		if opts.Limit > 0 && thisRun >= int64(opts.Limit) {
			break
		}
		limit := opts.Batch
		if opts.Limit > 0 && int64(limit) > int64(opts.Limit)-thisRun {
			limit = opts.Limit - int(thisRun)
		}
		ids, next, err := opts.Source.PlayerIDs(ctx, cursor, limit)
		if err != nil {
			return summary, fmt.Errorf("list players: %w", err)
		}
		if len(ids) == 0 && next == "" {
			// The listing is done. An empty last page is still a last page: returning here without
			// clearing the checkpoint is how a finished run leaves one behind, and the next run then
			// reports "(resumed)" over a migration that was already complete.
			if err := opts.Checkpoints.Clear(ctx, summary.Source, streamPlayers); err != nil {
				return summary, fmt.Errorf("clear the checkpoint: %w", err)
			}
			break
		}

		results := importBatch(ctx, opts, ids)
		thisRun += int64(len(ids))
		for _, r := range results {
			summary.Seen++
			if r.err != nil {
				summary.Errors++
				if len(summary.Failures) < 20 {
					summary.Failures = append(summary.Failures, fmt.Sprintf("%s: %v", r.sourceID, r.err))
				}
				continue
			}
			summary.Changed += r.changed
			summary.Identical += r.identical
			if opts.DryRun {
				summary.WouldWrite += int64(len(r.written))
			} else {
				summary.Documents += int64(len(r.written))
			}
			summary.Skipped += int64(len(r.skipped))
		}

		summary.Batches++
		cursor = next
		if next == "" {
			// The stream is done. The checkpoint goes: leaving it would make the next run skip what
			// this one did, and "the second run writes nothing" would be true because it did nothing.
			if err := opts.Checkpoints.Clear(ctx, summary.Source, streamPlayers); err != nil {
				return summary, fmt.Errorf("clear the checkpoint: %w", err)
			}
			break
		}
		if err := opts.Checkpoints.Save(ctx, checkpoint.Checkpoint{
			Source: summary.Source, Stream: streamPlayers, Cursor: next,
			BatchNo: summary.Batches, PlayersDone: summary.Seen,
		}); err != nil {
			return summary, fmt.Errorf("save the checkpoint: %w", err)
		}
	}

	summary.WallClock = time.Since(start)
	missing := int64(0) // a target with a player the source does not have is reconcile's finding, not this run's
	if err := opts.Ledger.FinishRun(ctx, runID, summary.Seen, missing, summary.Documents, summary.Identical, summary.Errors); err != nil {
		return summary, fmt.Errorf("finish the run: %w", err)
	}
	return summary, nil
}

// result is one player's outcome.
type result struct {
	sourceID  string
	written   []string
	skipped   []string
	changed   int64
	identical int64
	err       error
}

// importBatch works through one page of ids, up to Concurrency at a time.
//
// The order of the results is the order of the ids, so a report reads like the listing and two runs
// of the same page produce the same output.
func importBatch(ctx context.Context, opts Options, ids []string) []result {
	results := make([]result, len(ids))
	sem := make(chan struct{}, opts.Concurrency)
	var wg sync.WaitGroup
	for i, id := range ids {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, id string) {
			defer wg.Done()
			defer func() { <-sem }()
			results[i] = importOne(ctx, opts, id)
		}(i, id)
	}
	wg.Wait()
	return results
}

// importOne is one player: read, map, derive, write, record.
func importOne(ctx context.Context, opts Options, id string) result {
	out := result{sourceID: id}

	player, err := opts.Source.Player(ctx, id)
	if err != nil {
		out.err = err
		return out
	}
	// A record the source cannot vouch for is not mapped: half a player imported silently is worse
	// than a player reported as failed.
	if err := source.Validate(player); err != nil {
		out.err = err
		return out
	}
	rec, err := mapping.New(opts.Profile.Collection, player, opts.Source.Name())
	if err != nil {
		out.err = err
		return out
	}
	docs, err := target.Documents(rec)
	if err != nil {
		out.err = err
		return out
	}

	customID := identity.CustomID(opts.Source.Name(), id)
	if customID != rec.CustomID {
		// The record derived one identity and this loop derived another. That cannot happen without
		// a bug, and importing under either of them silently is how a player ends up with two accounts.
		out.err = fmt.Errorf("identity mismatch: record says %s, identity.CustomID says %s", rec.CustomID, customID)
		return out
	}

	userID, err := opts.Target.EnsureAccount(ctx, customID)
	if err != nil {
		out.err = err
		return out
	}

	req := target.Request{PlayerID: userID, SourceID: id, DryRun: opts.DryRun, Documents: docs}
	var res target.Result
	if opts.DryRun {
		res, err = opts.Target.Preview(ctx, req)
	} else {
		res, err = opts.Target.Apply(ctx, req)
	}
	if err != nil {
		out.err = err
		return out
	}
	out.written, out.skipped = res.Written, res.Skipped

	if opts.DryRun {
		return out
	}

	// The ledger records what the target wrote, and only that: an entity the target skipped is
	// already recorded, and recording it again would report a second write that did not happen.
	if err := opts.Ledger.SeenIdentity(ctx, ledger.Identity{
		Source: opts.Source.Name(), SourceID: id, CustomID: customID, NakamaUserID: userID,
	}); err != nil {
		out.err = err
		return out
	}
	if written := entitiesWritten(rec, res.Written); len(written) > 0 {
		if err := opts.Ledger.Append(ctx, opts.runID, opts.Source.Name(), id, written); err != nil {
			out.err = err
			return out
		}
	}
	if len(res.Written) == 0 {
		out.identical = 1
	} else {
		out.changed = 1
	}
	return out
}

// entitiesWritten keeps the entities the target reported writing, in a stable order.
//
// The two sides name the same thing differently, and getting that wrong is silent: the target says
// "migration/profile" because a storage document is a collection and a key, and the record says
// "profile" because that is what a person calls it. An earlier version compared the target's name
// against the entity's bare key, matched nothing, and recorded nothing - which looks exactly like an
// import that wrote nothing.
func entitiesWritten(rec *mapping.Record, written []string) []mapping.Entity {
	want := make(map[string]bool, len(written))
	for _, name := range written {
		want[name] = true
	}
	var out []mapping.Entity
	for _, e := range rec.Entities {
		if want[rec.Collection+"/"+e.Key] || want[e.Name] {
			out = append(out, e)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
