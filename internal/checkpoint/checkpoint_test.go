package checkpoint_test

import (
	"context"
	"testing"

	"github.com/lasttoss/nakama-migration-kit/internal/checkpoint"
	"github.com/lasttoss/nakama-migration-kit/internal/pgtest"
)

func TestAFirstRunHasNoCheckpoint(t *testing.T) {
	store := checkpoint.Open(pgtest.ConnMigrated(t))
	_, ok, err := store.Load(context.Background(), "playfab", "players")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("a stream that never ran reported a checkpoint")
	}
}

// The point of the checkpoint: a run that dies at hour nineteen resumes at hour nineteen.
func TestResumeAfterACrashIsWhereItStopped(t *testing.T) {
	conn := pgtest.ConnMigrated(t)
	ctx := context.Background()

	first := checkpoint.Open(conn)
	for batch := 1; batch <= 5; batch++ {
		if err := first.Save(ctx, checkpoint.Checkpoint{
			Source: "playfab", Stream: "players",
			Cursor: "token-" + string(rune('0'+batch)), BatchNo: batch, PlayersDone: int64(batch) * 250,
		}); err != nil {
			t.Fatal(err)
		}
	}

	// A different process, reading what the crashed one left.
	second := checkpoint.Open(conn)
	got, ok, err := second.Load(ctx, "playfab", "players")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("the checkpoint did not survive")
	}
	if got.BatchNo != 5 || got.Cursor != "token-5" || got.PlayersDone != 1250 {
		t.Errorf("resumed at %+v, want batch 5 cursor token-5 after 1250 players", got)
	}
}

func TestCheckpointsArePerStream(t *testing.T) {
	store := checkpoint.Open(pgtest.ConnMigrated(t))
	ctx := context.Background()

	for _, stream := range []string{"players", "inventory"} {
		if err := store.Save(ctx, checkpoint.Checkpoint{Source: "playfab", Stream: stream, Cursor: stream + "-cursor", BatchNo: 2, PlayersDone: 10}); err != nil {
			t.Fatal(err)
		}
	}
	got, ok, err := store.Load(ctx, "playfab", "inventory")
	if err != nil || !ok || got.Cursor != "inventory-cursor" {
		t.Errorf("inventory checkpoint = %+v, %v, %v", got, ok, err)
	}
	if _, ok, _ := store.Load(ctx, "smartfox", "players"); ok {
		t.Error("a checkpoint leaked across sources")
	}
}

func TestClearEndsAStream(t *testing.T) {
	store := checkpoint.Open(pgtest.ConnMigrated(t))
	ctx := context.Background()

	if err := store.Save(ctx, checkpoint.Checkpoint{Source: "playfab", Stream: "players", Cursor: "c", BatchNo: 1, PlayersDone: 1}); err != nil {
		t.Fatal(err)
	}
	if err := store.Clear(ctx, "playfab", "players"); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.Load(ctx, "playfab", "players"); err != nil || ok {
		t.Errorf("after Clear: ok=%v err=%v", ok, err)
	}
}
