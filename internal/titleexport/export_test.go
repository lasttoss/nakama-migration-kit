package titleexport_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lasttoss/nakama-migration-kit/internal/source"
	"github.com/lasttoss/nakama-migration-kit/internal/titleexport"
)

func writeExport(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "export.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func line(i int) string {
	return fmt.Sprintf(`{"id":"PF%08d","display_name":"player-%d","user_data":{"city":"c%d"},"currency":{"GD":%d},"statistics":{"wins":%d},"inventory":[{"catalog_id":"sword","count":1}]}`,
		i, i, i%12, 100+i, i%7)
}

func TestListingWalksTheFileInOrder(t *testing.T) {
	path := writeExport(t, line(1), line(2), line(3))
	s := titleexport.New(titleexport.Config{Path: path, PageSize: 2})

	ids, next, err := s.PlayerIDs(context.Background(), "", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 || ids[0] != "PF00000001" || ids[1] != "PF00000002" {
		t.Errorf("first page = %v", ids)
	}
	if next != "2" {
		t.Errorf("cursor = %q, want 2", next)
	}

	ids, next, err = s.PlayerIDs(context.Background(), next, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != "PF00000003" || next != "" {
		t.Errorf("last page = %v, cursor %q", ids, next)
	}
}

func TestPlayerReadsTheWholeRecord(t *testing.T) {
	path := writeExport(t, line(1), line(2), line(3))
	s := titleexport.New(titleexport.Config{Path: path})

	got, err := s.Player(context.Background(), "PF00000002")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "PF00000002" || got.DisplayName != "player-2" {
		t.Errorf("read the wrong player: %+v", got)
	}
	if got.Currency["GD"] != 102 || got.Statistics["wins"] != 2 {
		t.Errorf("currency/statistics wrong: %v %v", got.Currency, got.Statistics)
	}
	if len(got.Inventory) != 1 || got.Inventory[0].CatalogID != "sword" {
		t.Errorf("inventory wrong: %+v", got.Inventory)
	}
	if got.UserData["city"] != "c2" {
		t.Errorf("user data wrong: %v", got.UserData)
	}
	if err := source.Validate(got); err != nil {
		t.Errorf("the reader returned a record the kit cannot import: %v", err)
	}
}

// A million-line file has to be read by seeking. Reading the file per player would cost the wall
// clock this package exists to save, so the reader indexes once and this test measures it.
func TestTheIndexIsBuiltOnceAndReadingIsASeek(t *testing.T) {
	const players = 100000
	path := filepath.Join(t.TempDir(), "big.jsonl")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= players; i++ {
		if _, err := fmt.Fprintln(f, line(i)); err != nil {
			t.Fatal(err)
		}
	}
	f.Close()
	info, _ := os.Stat(path)

	s := titleexport.New(titleexport.Config{Path: path, PageSize: 1000})

	start := time.Now()
	ids, _, err := s.PlayerIDs(context.Background(), "", 1000)
	if err != nil {
		t.Fatal(err)
	}
	indexed := time.Since(start)
	if len(ids) != 1000 {
		t.Fatalf("first page = %d ids", len(ids))
	}

	start = time.Now()
	for i := 0; i < 1000; i++ {
		id := fmt.Sprintf("PF%08d", (i*97)%players+1)
		if _, err := s.Player(context.Background(), id); err != nil {
			t.Fatalf("reading %s: %v", id, err)
		}
	}
	random := time.Since(start)

	all := time.Now()
	seen, cursor, pages := 0, "", 0
	for {
		page, next, err := s.PlayerIDs(context.Background(), cursor, 5000)
		if err != nil {
			t.Fatal(err)
		}
		seen += len(page)
		pages++
		if next == "" {
			break
		}
		cursor = next
	}
	walk := time.Since(all)
	if seen != players {
		t.Fatalf("walked %d players, want %d", seen, players)
	}

	t.Logf("export of %d players, %.1f MB (file writes excluded):", players, float64(info.Size())/(1<<20))
	t.Logf("  index built in %s", indexed.Round(time.Millisecond))
	t.Logf("  1000 random players read in %s (%s each)", random.Round(time.Millisecond), (random / 1000).Round(time.Microsecond))
	t.Logf("  full listing of %d players in %d pages: %s", seen, pages, walk.Round(time.Millisecond))
	// Extrapolate from the step that actually costs: indexing, not listing. An extrapolation
	// from the cheap step would print a comforting zero.
	t.Logf("  at this rate, 1M players: index %s, then listing and reads only", (indexed * 10).Round(time.Second))

	if indexed > 20*time.Second {
		t.Errorf("indexing %d players took %s", players, indexed)
	}
}

// An export with a broken line has to stop the run with the line number: a half-read export that
// reports success is the failure this whole package is about not having.
func TestABrokenLineNamesItself(t *testing.T) {
	path := writeExport(t, line(1), `{"id":"PF2","currency":`, line(3))
	s := titleexport.New(titleexport.Config{Path: path})

	_, _, err := s.PlayerIDs(context.Background(), "", 10)
	if err == nil {
		t.Fatal("a broken line was accepted")
	}
	if !strings.Contains(err.Error(), "line 2") {
		t.Errorf("the error does not name the line: %v", err)
	}
}

func TestDuplicatePlayersAreRefused(t *testing.T) {
	path := writeExport(t, line(1), line(2), line(1))
	s := titleexport.New(titleexport.Config{Path: path})

	_, _, err := s.PlayerIDs(context.Background(), "", 10)
	if err == nil {
		t.Fatal("an export with a duplicate player was accepted")
	}
	if !strings.Contains(err.Error(), "appears twice") {
		t.Errorf("the error does not explain: %v", err)
	}
}

func TestUnknownPlayerIsNotFoundNotAFailure(t *testing.T) {
	path := writeExport(t, line(1))
	s := titleexport.New(titleexport.Config{Path: path})

	_, err := s.Player(context.Background(), "PF99999999")
	if err == nil {
		t.Fatal("an unknown player produced a record")
	}
	if !errors.Is(err, source.ErrNotFound) {
		t.Errorf("the error is not source.ErrNotFound: %v", err)
	}
}

// If the file is rewritten while an import is running, the offsets point at the wrong lines. The
// reader refuses rather than importing the wrong player's balance.
func TestARewrittenExportIsDetected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "export.jsonl")
	if err := os.WriteFile(path, []byte(line(1)+"\n"+line(2)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := titleexport.New(titleexport.Config{Path: path})
	if _, _, err := s.PlayerIDs(context.Background(), "", 10); err != nil {
		t.Fatal(err)
	}

	// A different export of the same length would be worse: the ids move.
	other := strings.ReplaceAll(line(2), "player-2", "player-9")
	if err := os.WriteFile(path, []byte(other+"\n"+line(1)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Player(context.Background(), "PF00000001"); err == nil {
		t.Fatal("a player was read out of a rewritten export")
	} else if !strings.Contains(err.Error(), "rewritten") {
		t.Errorf("the error does not explain what happened: %v", err)
	}
}

func TestBlankLinesAndUnknownFields(t *testing.T) {
	path := writeExport(t, ``, line(1), `   `, line(2))
	s := titleexport.New(titleexport.Config{Path: path})
	ids, _, err := s.PlayerIDs(context.Background(), "", 10)
	if err != nil {
		t.Fatalf("blank lines broke the listing: %v", err)
	}
	if len(ids) != 2 {
		t.Errorf("ids = %v, want 2", ids)
	}

	// An unknown field is a change to the export format, and silently ignoring it drops data.
	bad := writeExport(t, `{"id":"PF1","display_name":"x","new_thing":true}`)
	if _, _, err := titleexport.New(titleexport.Config{Path: bad}).PlayerIDs(context.Background(), "", 1); err == nil {
		t.Error("an unknown field was ignored")
	}
}

func TestMissingFileIsAnError(t *testing.T) {
	s := titleexport.New(titleexport.Config{Path: filepath.Join(t.TempDir(), "nope.jsonl")})
	if _, _, err := s.PlayerIDs(context.Background(), "", 1); err == nil {
		t.Fatal("a missing export was accepted")
	}
}

func TestBadCursor(t *testing.T) {
	path := writeExport(t, line(1))
	s := titleexport.New(titleexport.Config{Path: path})
	if _, _, err := s.PlayerIDs(context.Background(), "not-a-number", 1); err == nil {
		t.Error("a bad cursor was accepted")
	}
	if ids, next, err := s.PlayerIDs(context.Background(), "99", 1); err != nil || len(ids) != 0 || next != "" {
		t.Errorf("a cursor past the end returned %v %q %v", ids, next, err)
	}
}

// The importer reads players from several goroutines, and each read opens its own handle.
func TestConcurrentReads(t *testing.T) {
	lines := make([]string, 50)
	for i := range lines {
		lines[i] = line(i + 1)
	}
	s := titleexport.New(titleexport.Config{Path: writeExport(t, lines...)})

	var wg sync.WaitGroup
	for i := 1; i <= 40; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := s.Player(context.Background(), fmt.Sprintf("PF%08d", i)); err != nil {
				t.Errorf("concurrent read: %v", err)
			}
		}(i)
	}
	wg.Wait()
}
