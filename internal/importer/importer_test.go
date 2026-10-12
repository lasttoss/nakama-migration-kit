package importer_test

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lasttoss/nakama-migration-kit/internal/checkpoint"
	"github.com/lasttoss/nakama-migration-kit/internal/importer"
	"github.com/lasttoss/nakama-migration-kit/internal/ledger"
	"github.com/lasttoss/nakama-migration-kit/internal/mapping"
	"github.com/lasttoss/nakama-migration-kit/internal/profile"
	"github.com/lasttoss/nakama-migration-kit/internal/source"
	"github.com/lasttoss/nakama-migration-kit/internal/target"
)

// fakeSource lists players a page at a time and can fail one of them on demand.
type fakeSource struct {
	players map[string]*source.Player
	order   []string
	fail    map[string]bool
	calls   int
}

func (f *fakeSource) Name() string { return "playfab" }

func (f *fakeSource) PlayerIDs(_ context.Context, cursor string, limit int) ([]string, string, error) {
	f.calls++
	start := 0
	if cursor != "" {
		if _, err := fmt.Sscanf(cursor, "%d", &start); err != nil {
			return nil, "", err
		}
	}
	end := start + limit
	if end > len(f.order) {
		end = len(f.order)
	}
	if start >= len(f.order) {
		return nil, "", nil
	}
	next := ""
	if end < len(f.order) {
		next = fmt.Sprintf("%d", end)
	}
	return f.order[start:end], next, nil
}

func (f *fakeSource) Player(_ context.Context, id string) (*source.Player, error) {
	if f.fail[id] {
		return nil, fmt.Errorf("the backend refused to return %s", id)
	}
	p, ok := f.players[id]
	if !ok {
		return nil, source.ErrNotFound
	}
	return p, nil
}

func players(n int) *fakeSource {
	f := &fakeSource{players: map[string]*source.Player{}, fail: map[string]bool{}}
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("PF-%d", i)
		f.order = append(f.order, id)
		f.players[id] = &source.Player{
			ID: id, DisplayName: fmt.Sprintf("player-%d", i),
			UserData:   map[string]string{"level": "7", "energy": "30"},
			Currency:   map[string]int64{"GD": int64(100 + i)},
			Statistics: map[string]int64{"wins": int64(i)},
		}
	}
	return f
}

// fakeTarget behaves like the runtime module: it writes a document whose checksum it has not seen,
// and skips one it has. Writing this twice is the only way to test the claim that the second run
// writes nothing without a Nakama in the test.
type fakeTarget struct {
	mu      sync.Mutex
	known   map[string]string
	applies int
	dryRuns int
	docs    int
}

func newTarget() *fakeTarget { return &fakeTarget{known: map[string]string{}} }

func (f *fakeTarget) Name() string { return "nakama" }

func (f *fakeTarget) EnsureAccount(_ context.Context, customID string) (string, error) {
	return "user-" + customID[len(customID)-8:], nil
}

func (f *fakeTarget) Apply(_ context.Context, req target.Request) (target.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.applies++
	return f.apply(req)
}

func (f *fakeTarget) Preview(_ context.Context, req target.Request) (target.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dryRuns++
	return f.apply(req)
}

func (f *fakeTarget) apply(req target.Request) (target.Result, error) {
	res := target.Result{DryRun: req.DryRun}
	for _, d := range req.Documents {
		name := d.Collection + "/" + d.Key
		if f.known[req.PlayerID+name] == d.Checksum {
			res.Skipped = append(res.Skipped, name)
			continue
		}
		if !req.DryRun {
			f.known[req.PlayerID+name] = d.Checksum
			f.docs++
		}
		res.Written = append(res.Written, name)
	}
	sort.Strings(res.Written)
	sort.Strings(res.Skipped)
	return res, nil
}

func (f *fakeTarget) Inspect(context.Context, string, []string) (target.Inspect, error) {
	return target.Inspect{}, nil
}
func (f *fakeTarget) Status(context.Context) (target.Status, error) { return target.Status{}, nil }

// fakeLedger counts what the importer recorded.
type fakeLedger struct {
	mu            sync.Mutex
	runs          int
	appended      int
	seen          map[string]bool
	customIDOwner map[string]string
	finished      [5]int64
	runIDSeen     int64
}

func newLedger() *fakeLedger {
	return &fakeLedger{seen: map[string]bool{}, customIDOwner: map[string]string{}}
}

func (l *fakeLedger) Run(context.Context, string, string, string) (int64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.runs++
	return int64(l.runs), nil
}

func (l *fakeLedger) SeenIdentity(_ context.Context, id ledger.Identity) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	// The real rule, which the ledger's own test states: recording the same identity again is what
	// every second run does, and is fine. What the unique index refuses is two source ids deriving
	// one account - so the fake keys on the custom id and remembers which source id claimed it.
	if owner, ok := l.customIDOwner[id.CustomID]; ok && owner != id.SourceID {
		return fmt.Errorf("custom id %s is claimed by %s and %s", id.CustomID, owner, id.SourceID)
	}
	l.customIDOwner[id.CustomID] = id.SourceID
	l.seen[id.SourceID] = true
	return nil
}

func (l *fakeLedger) Append(_ context.Context, runID int64, _, _ string, entities []mapping.Entity) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if runID == 0 {
		return fmt.Errorf("an entity was recorded against no run")
	}
	l.runIDSeen = runID
	l.appended += len(entities)
	return nil
}

func (l *fakeLedger) FinishRun(_ context.Context, _ int64, seen, missing, written, identical, errors int64) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.finished = [5]int64{seen, missing, written, identical, errors}
	return nil
}

// fakeCheckpoints is where a run stopped, in memory.
type fakeCheckpoints struct {
	mu   sync.Mutex
	cp   map[string]checkpoint.Checkpoint
	load int
	save int
	clr  int
}

func newCheckpoints() *fakeCheckpoints {
	return &fakeCheckpoints{cp: map[string]checkpoint.Checkpoint{}}
}

func (c *fakeCheckpoints) Load(_ context.Context, source, stream string) (checkpoint.Checkpoint, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.load++
	cp, ok := c.cp[source+"/"+stream]
	return cp, ok, nil
}

func (c *fakeCheckpoints) Save(_ context.Context, cp checkpoint.Checkpoint) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.save++
	c.cp[cp.Source+"/"+cp.Stream] = cp
	return nil
}

func (c *fakeCheckpoints) Clear(_ context.Context, source, stream string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.clr++
	delete(c.cp, source+"/"+stream)
	return nil
}

// prof loads the profile the lab uses. A hand-written one in the test would stop compiling the day
// the profile format changes, and would not be the profile the demo runs.
func prof(t *testing.T) profile.Profile {
	t.Helper()
	p, err := profile.Load("../../profiles/faketenant.json")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestAFullRunIsFollowedByARunThatWritesNothing(t *testing.T) {
	src, tgt, led, cps := players(25), newTarget(), newLedger(), newCheckpoints()
	opts := importer.Options{
		Source: src, Target: tgt, Ledger: led, Checkpoints: cps,
		Profile: prof(t), Strategy: "title-export", Batch: 10, Concurrency: 3,
	}

	first, err := importer.Run(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if first.Seen != 25 || first.Errors != 0 {
		t.Fatalf("first run: %+v", first)
	}
	if first.Documents == 0 || first.Changed != 25 {
		t.Fatalf("the first run wrote nothing: %+v", first)
	}

	second, err := importer.Run(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if second.Documents != 0 || second.Changed != 0 {
		t.Errorf("the second run wrote %d documents across %d players", second.Documents, second.Changed)
	}
	if second.Identical != 25 {
		t.Errorf("the second run did not report the players as identical: %+v", second)
	}
	if second.Seen != 25 {
		t.Errorf("the second run did not see every player: %d", second.Seen)
	}
	if led.runs != 2 {
		t.Errorf("the ledger recorded %d runs, want 2", led.runs)
	}
}

func TestARunRecordsWhatTheTargetWroteAndNotWhatItSkipped(t *testing.T) {
	src, tgt, led, cps := players(3), newTarget(), newLedger(), newCheckpoints()
	opts := importer.Options{Source: src, Target: tgt, Ledger: led, Checkpoints: cps, Profile: prof(t), Batch: 10}

	if _, err := importer.Run(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	first := led.appended
	if first == 0 {
		t.Fatal("nothing was recorded")
	}
	if _, err := importer.Run(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	if led.appended != first {
		t.Errorf("the second run recorded %d more entities than the first", led.appended-first)
	}
	if led.runIDSeen == 0 {
		t.Error("an entity was recorded against run 0")
	}
}

func TestADryRunWritesNothingAndRecordsNothing(t *testing.T) {
	src, tgt, led, cps := players(4), newTarget(), newLedger(), newCheckpoints()
	summary, err := importer.Run(context.Background(), importer.Options{
		Source: src, Target: tgt, Ledger: led, Checkpoints: cps, Profile: prof(t),
		DryRun: true, Batch: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if tgt.docs != 0 || tgt.applies != 0 || tgt.dryRuns == 0 {
		t.Errorf("a dry run wrote: applies=%d dryRuns=%d documents=%d", tgt.applies, tgt.dryRuns, tgt.docs)
	}
	if led.appended != 0 || led.seen != nil && len(led.seen) != 0 {
		t.Error("a dry run recorded identities")
	}
	if summary.Documents != 0 {
		t.Errorf("a dry run reported writing %d documents", summary.Documents)
	}
	if summary.WouldWrite == 0 {
		t.Error("a dry run found nothing to write, which cannot be right against an empty target")
	}
	if summary.Changed != 0 || summary.Identical != 0 {
		t.Errorf("a dry run reported a change: %+v", summary)
	}
}

func TestAPlayerTheSourceRefusesIsReportedAndTheRunContinues(t *testing.T) {
	src, tgt, led, cps := players(6), newTarget(), newLedger(), newCheckpoints()
	src.fail["PF-3"] = true
	summary, err := importer.Run(context.Background(), importer.Options{
		Source: src, Target: tgt, Ledger: led, Checkpoints: cps, Profile: prof(t), Batch: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if summary.Errors != 1 {
		t.Fatalf("errors = %d, want 1: %+v", summary.Errors, summary.Failures)
	}
	if summary.Seen != 6 || summary.Changed != 5 {
		t.Errorf("the rest of the page was not imported: seen=%d changed=%d", summary.Seen, summary.Changed)
	}
	if len(summary.Failures) != 1 || !strings.Contains(summary.Failures[0], "PF-3") {
		t.Errorf("the failure does not name the player: %v", summary.Failures)
	}
}

func TestAStoppedRunResumesAndClearsItsCheckpointWhenItFinishes(t *testing.T) {
	src, tgt, led, cps := players(20), newTarget(), newLedger(), newCheckpoints()
	// A previous run stopped after the first page of ten.
	if err := cps.Save(context.Background(), checkpoint.Checkpoint{
		Source: "playfab", Stream: "players", Cursor: "10", BatchNo: 1, PlayersDone: 10,
	}); err != nil {
		t.Fatal(err)
	}
	summary, err := importer.Run(context.Background(), importer.Options{
		Source: src, Target: tgt, Ledger: led, Checkpoints: cps, Profile: prof(t), Batch: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !summary.Resumed {
		t.Error("the run did not report that it resumed")
	}
	if summary.Changed != 10 {
		t.Errorf("a resumed run imported %d players, want the 10 that were left", summary.Changed)
	}
	if cps.clr != 1 {
		t.Errorf("the checkpoint was cleared %d times, want once when the stream finished", cps.clr)
	}
	if _, ok := cps.cp["playfab/players"]; ok {
		t.Error("a finished run left a checkpoint behind: the next run would skip the work and 'writes nothing' would be true for the wrong reason")
	}
}

// The listing's last page is often empty, and that is exactly the case that left a checkpoint behind:
// the loop saw no ids and no cursor, returned, and never reached the code that clears it. The next run
// then reported "(resumed)" over a migration that was already complete, and - with a limit - did
// nothing at all.
func TestAnEmptyLastPageStillClearsTheCheckpoint(t *testing.T) {
	// 20 players, pages of 10: the listing answers ten, ten, then nothing, and says it is done.
	src, tgt, led, cps := players(20), newTarget(), newLedger(), newCheckpoints()
	opts := importer.Options{Source: src, Target: tgt, Ledger: led, Checkpoints: cps, Profile: prof(t), Batch: 10}
	if _, err := importer.Run(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	if _, ok := cps.cp["playfab/players"]; ok {
		t.Fatal("a finished run left its checkpoint behind, so the next run would report (resumed) and do nothing")
	}
	second, err := importer.Run(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if second.Resumed {
		t.Error("the second run thinks it resumed a run that finished")
	}
	if second.Identical != 20 {
		t.Errorf("the second run reported %d identical players, want 20", second.Identical)
	}
}

// A limit bounds the work of one run, not the work of the migration so far: a resumed run with the
// same limit has to do the players that are left, or the flag means "stop before doing anything".
func TestTheLimitCountsThisRunsWorkNotTheTotalSoFar(t *testing.T) {
	src, tgt, led, cps := players(20), newTarget(), newLedger(), newCheckpoints()
	if err := cps.Save(context.Background(), checkpoint.Checkpoint{
		Source: "playfab", Stream: "players", Cursor: "10", BatchNo: 1, PlayersDone: 10,
	}); err != nil {
		t.Fatal(err)
	}
	summary, err := importer.Run(context.Background(), importer.Options{
		Source: src, Target: tgt, Ledger: led, Checkpoints: cps, Profile: prof(t), Batch: 10, Limit: 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	if summary.Changed != 10 {
		t.Errorf("a resumed run with -limit=20 imported %d players, want the 10 that were left", summary.Changed)
	}
}

func TestTheRunIsBoundedByTheLimit(t *testing.T) {
	src, tgt, led, cps := players(50), newTarget(), newLedger(), newCheckpoints()
	summary, err := importer.Run(context.Background(), importer.Options{
		Source: src, Target: tgt, Ledger: led, Checkpoints: cps, Profile: prof(t), Batch: 7, Limit: 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	if summary.Seen != 20 {
		t.Errorf("seen = %d, want 20", summary.Seen)
	}
	_ = time.Second
}
