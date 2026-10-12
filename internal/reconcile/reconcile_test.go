package reconcile_test

import (
	"strings"
	"testing"

	"github.com/lasttoss/nakama-migration-kit/internal/mapping"
	"github.com/lasttoss/nakama-migration-kit/internal/reconcile"
	"github.com/lasttoss/nakama-migration-kit/internal/target"
)

func record(entities ...mapping.Entity) *mapping.Record {
	return &mapping.Record{Source: "playfab", SourceID: "PF-1", Collection: "migration", Entities: entities}
}

func doc(key, checksum string) target.Document {
	return target.Document{Collection: "migration", Key: key, Checksum: checksum}
}

func TestAMatchingPlayerHasNoFindings(t *testing.T) {
	res := reconcile.Compare(
		record(mapping.Entity{Name: "currency", Key: "gold", Checksum: "a"}),
		[]target.Document{doc("gold", "a")},
	)
	if len(res.Findings) != 0 || !res.Matches() {
		t.Errorf("a matching player produced findings: %+v", res.Findings)
	}
	if res.Expected != 1 || res.Actual != 1 {
		t.Errorf("counts: %+v", res)
	}
}

func TestAChangedDocumentNamesBothSides(t *testing.T) {
	res := reconcile.Compare(
		record(mapping.Entity{Name: "currency", Key: "gold", Checksum: "a", Value: `{"gold":600}`}),
		[]target.Document{doc("gold", "b")},
	)
	if res.Matches() {
		t.Fatal("a changed document was reported as matching")
	}
	if len(res.Findings) != 1 || res.Findings[0].Kind != reconcile.Changed {
		t.Fatalf("findings: %+v", res.Findings)
	}
	f := res.Findings[0]
	if f.Expected != "a" || f.Actual != "b" {
		t.Errorf("the finding does not carry both checksums: %+v", f)
	}
	if !strings.Contains(f.String(), "gold") || !strings.Contains(f.String(), "source") {
		t.Errorf("the finding's text does not read: %q", f.String())
	}
}

func TestADocumentTheTargetDoesNotHaveIsMissing(t *testing.T) {
	res := reconcile.Compare(
		record(mapping.Entity{Name: "inventory", Key: "sword", Checksum: "a"}),
		nil,
	)
	if res.Matches() || len(res.Findings) != 1 || res.Findings[0].Kind != reconcile.Missing {
		t.Fatalf("a document the target does not have was not reported as missing: %+v", res.Findings)
	}
}

// The game writes its own storage after a player logs in. Calling that a migration failure would mean
// every reconciliation after the first cutover cries wolf, and a report that cries wolf is ignored.
func TestDataTheGameWroteItselfIsNotAMismatch(t *testing.T) {
	res := reconcile.Compare(
		record(mapping.Entity{Name: "currency", Key: "gold", Checksum: "a"}),
		[]target.Document{doc("gold", "a"), doc("last_login", "z")},
	)
	if !res.Matches() {
		t.Errorf("the game's own data made a player mismatch: %+v", res.Findings)
	}
	if len(res.Findings) != 1 || res.Findings[0].Kind != reconcile.Extra {
		t.Fatalf("the extra document was not reported at all: %+v", res.Findings)
	}
}

func TestFindingsAreOrderedSoTwoRunsReadTheSame(t *testing.T) {
	rec := record(
		mapping.Entity{Name: "statistics", Key: "wins", Checksum: "a"},
		mapping.Entity{Name: "currency", Key: "gold", Checksum: "a"},
		mapping.Entity{Name: "userdata", Key: "note", Checksum: "a"},
	)
	docs := []target.Document{doc("wins", "z"), doc("gold", "y"), doc("note", "x")}
	first := reconcile.Compare(rec, docs)
	second := reconcile.Compare(rec, docs)
	if len(first.Findings) != 3 {
		t.Fatalf("findings: %+v", first.Findings)
	}
	for i := range first.Findings {
		if first.Findings[i].Key != second.Findings[i].Key {
			t.Errorf("two comparisons of the same input disagree at %d", i)
		}
	}
	// And they are grouped by kind, so a reader sees all the missing documents together.
	if first.Findings[0].Kind != "changed" {
		t.Errorf("findings are not grouped by kind: %+v", first.Findings)
	}
}
