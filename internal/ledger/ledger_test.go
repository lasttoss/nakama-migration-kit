package ledger_test

import (
	"context"
	"strings"
	"testing"

	"github.com/lasttoss/nakama-migration-kit/internal/ledger"
	"github.com/lasttoss/nakama-migration-kit/internal/mapping"
	"github.com/lasttoss/nakama-migration-kit/internal/pgtest"
)

func entities() []mapping.Entity {
	return []mapping.Entity{
		{Name: "profile", Key: "profile", Value: map[string]any{"display_name": "An"}, Checksum: "sha256:aaa"},
		{Name: "currency", Key: "currency", Value: map[string]any{"balances": map[string]any{"GD": 100}}, Checksum: "sha256:bbb"},
	}
}

// The constraint that turns "no duplicate account" from a promise into a fact.
func TestOneCustomIDIsOneAccount(t *testing.T) {
	conn := pgtest.ConnMigrated(t)
	l := ledger.Open(conn)
	ctx := context.Background()

	if err := l.SeenIdentity(ctx, ledger.Identity{Source: "playfab", SourceID: "A", CustomID: "same-uuid", NakamaUserID: "u1"}); err != nil {
		t.Fatal(err)
	}
	// A second player whose derived identity collides must be refused, not merged.
	err := l.SeenIdentity(ctx, ledger.Identity{Source: "playfab", SourceID: "B", CustomID: "same-uuid", NakamaUserID: "u2"})
	if err == nil {
		t.Fatal("two players were attached to one account")
	}
	if !strings.Contains(err.Error(), "custom_id") && !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("the error does not say what collided: %v", err)
	}
}

func TestRecordingTheSameIdentityTwiceIsFine(t *testing.T) {
	conn := pgtest.ConnMigrated(t)
	l := ledger.Open(conn)
	ctx := context.Background()

	id := ledger.Identity{Source: "playfab", SourceID: "A", CustomID: "uuid-a", NakamaUserID: "u1"}
	for i := 0; i < 3; i++ {
		if err := l.SeenIdentity(ctx, id); err != nil {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	n, err := l.IdentityCount(ctx, "playfab")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("identity_map holds %d rows, want 1", n)
	}
	got, ok, err := l.Identity(ctx, "playfab", "A")
	if err != nil || !ok || got.NakamaUserID != "u1" {
		t.Errorf("identity = %+v, %v, %v", got, ok, err)
	}
	if _, ok, _ := l.Identity(ctx, "playfab", "missing"); ok {
		t.Error("an unknown player was found")
	}
}

// The ledger answers "what did we write, when". An update or a delete destroys that answer, so
// the database refuses both, not just the code.
func TestTheLedgerIsAppendOnly(t *testing.T) {
	conn := pgtest.ConnMigrated(t)
	l := ledger.Open(conn)
	ctx := context.Background()

	run, err := l.Run(ctx, "playfab", "admin-api", "apply")
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Append(ctx, run, "playfab", "A", entities()); err != nil {
		t.Fatal(err)
	}

	if _, err := conn.Exec(ctx, `update import_ledger set checksum = 'sha256:forged'`); err == nil {
		t.Error("a run rewrote ledger history")
	} else if !strings.Contains(err.Error(), "append-only") {
		t.Errorf("the refusal does not explain itself: %v", err)
	}
	if _, err := conn.Exec(ctx, `delete from import_ledger`); err == nil {
		t.Error("a run deleted ledger history")
	}

	// The rows are still there, and still say what happened.
	latest, err := l.Latest(ctx, "playfab", "A")
	if err != nil {
		t.Fatal(err)
	}
	if latest["currency"] != "sha256:bbb" {
		t.Errorf("latest = %v", latest)
	}
}

func TestAppendingTheSameChecksumTwiceIsOneRow(t *testing.T) {
	conn := pgtest.ConnMigrated(t)
	l := ledger.Open(conn)
	ctx := context.Background()

	run, _ := l.Run(ctx, "playfab", "admin-api", "apply")
	for i := 0; i < 2; i++ {
		if err := l.Append(ctx, run, "playfab", "A", entities()); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	n, err := l.CountsOf(ctx, run)
	if err != nil {
		t.Fatal(err)
	}
	// Two documents, appended twice, one row each: a run that crashed between writing and
	// recording repeats the append, and that must not look like two writes.
	if n != 2 {
		t.Errorf("ledger holds %d rows, want 2", n)
	}
}

func TestTheNewestChecksumWins(t *testing.T) {
	conn := pgtest.ConnMigrated(t)
	l := ledger.Open(conn)
	ctx := context.Background()

	first, _ := l.Run(ctx, "playfab", "admin-api", "apply")
	if err := l.Append(ctx, first, "playfab", "A", entities()); err != nil {
		t.Fatal(err)
	}

	// A delta pass: the player earned gold while the legacy title was still live.
	changed := entities()
	changed[1].Checksum = "sha256:ccc"
	second, _ := l.Run(ctx, "playfab", "admin-api", "apply")
	if err := l.Append(ctx, second, "playfab", "A", changed); err != nil {
		t.Fatal(err)
	}

	latest, err := l.Latest(ctx, "playfab", "A")
	if err != nil {
		t.Fatal(err)
	}
	if latest["currency"] != "sha256:ccc" {
		t.Errorf("currency checksum = %q, want the newest", latest["currency"])
	}
	if latest["profile"] != "sha256:aaa" {
		t.Errorf("profile checksum = %q, want the unchanged one", latest["profile"])
	}

	// Both rows survive: the history is the point.
	n, _ := l.CountsOf(ctx, first)
	if n != 2 {
		t.Errorf("the first run recorded %d rows, want 2", n)
	}
	n, _ = l.CountsOf(ctx, second)
	if n != 1 {
		t.Errorf("the delta run recorded %d rows, want 1 (only what changed)", n)
	}
}

func TestARunRecordsWhatItDid(t *testing.T) {
	conn := pgtest.ConnMigrated(t)
	l := ledger.Open(conn)
	ctx := context.Background()

	run, err := l.Run(ctx, "playfab", "title-export", "apply")
	if err != nil {
		t.Fatal(err)
	}
	if err := l.FinishRun(ctx, run, 1000, 3, 5000, 995, 1); err != nil {
		t.Fatal(err)
	}

	var seen, missing, written, identical, errs int64
	var finished bool
	err = conn.QueryRow(ctx, `
		select players_seen, players_missing, documents_written, documents_identical, errors, finished_at is not null
		  from import_run where id = $1`, run).Scan(&seen, &missing, &written, &identical, &errs, &finished)
	if err != nil {
		t.Fatal(err)
	}
	if seen != 1000 || missing != 3 || written != 5000 || identical != 995 || errs != 1 || !finished {
		t.Errorf("run = seen %d missing %d written %d identical %d errors %d finished %v",
			seen, missing, written, identical, errs, finished)
	}
}

func TestMismatchesAreRecordedPerRun(t *testing.T) {
	conn := pgtest.ConnMigrated(t)
	l := ledger.Open(conn)
	ctx := context.Background()

	run, _ := l.Run(ctx, "playfab", "admin-api", "reconcile")
	if err := l.RecordMismatch(ctx, run, "PF2", "currency", "source GD=500, target GD=400"); err != nil {
		t.Fatal(err)
	}
	// Re-running reconciliation over the same player replaces the note rather than duplicating it.
	if err := l.RecordMismatch(ctx, run, "PF2", "currency", "source GD=500, target GD=300"); err != nil {
		t.Fatal(err)
	}
	if err := l.RecordMismatch(ctx, run, "PF1", "profile", "missing"); err != nil {
		t.Fatal(err)
	}

	got, err := l.Mismatches(ctx, run)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("mismatches = %+v, want 2", got)
	}
	if got[0].SourceID != "PF1" || got[1].Detail != "source GD=500, target GD=300" {
		t.Errorf("mismatches = %+v", got)
	}
}
