package mapping_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/lasttoss/nakama-migration-kit/internal/mapping"
	"github.com/lasttoss/nakama-migration-kit/internal/source"
)

func player() *source.Player {
	return &source.Player{
		ID:          "ABC123",
		DisplayName: "An & Binh",
		UserData:    map[string]string{"tutorial": "done", "city": "hanoi"},
		Inventory: []source.Item{
			{InstanceID: "i2", CatalogID: "shield", Count: 1},
			{InstanceID: "i1", CatalogID: "sword", Count: 2},
		},
		Currency:   map[string]int64{"GD": 500, "GM": 12},
		Statistics: map[string]int64{"wins": 7},
		Progress:   &source.Progress{Level: 4, Energy: 30, EnergyUpdatedAt: time.Unix(1700000000, 0).UTC()},
		CreatedAt:  time.Unix(1600000000, 0).UTC(),
		UpdatedAt:  time.Unix(1700000000, 0).UTC(),
	}
}

func record(t *testing.T, p *source.Player) *mapping.Record {
	t.Helper()
	r, err := mapping.New("migration", p, "playfab")
	if err != nil {
		t.Fatalf("mapping.New: %v", err)
	}
	return r
}

func sum(t *testing.T, r *mapping.Record) string {
	t.Helper()
	s, err := r.Checksum()
	if err != nil {
		t.Fatalf("Checksum: %v", err)
	}
	return s
}

func TestInventoryOrderDoesNotChangeTheRecord(t *testing.T) {
	p := player()
	first := sum(t, record(t, p))

	p.Inventory[0], p.Inventory[1] = p.Inventory[1], p.Inventory[0]
	second := sum(t, record(t, p))

	if first != second {
		t.Errorf("the same inventory in another order hashed differently:\n  %s\n  %s", first, second)
	}
}

// The point of a per-document checksum: a one-currency change names exactly one document.
func TestACurrencyChangeTouchesOneDocument(t *testing.T) {
	before := record(t, player())
	p := player()
	p.Currency["GD"] = 501
	after := record(t, p)

	changed := []string{}
	for _, e := range before.Entities {
		other, ok := after.Entity(e.Name)
		if !ok {
			t.Fatalf("entity %s disappeared", e.Name)
		}
		if other.Checksum != e.Checksum {
			changed = append(changed, e.Name)
		}
	}
	if len(changed) != 1 || changed[0] != mapping.EntityCurrency {
		t.Errorf("a currency change marked %v as changed, want [currency]", changed)
	}
	if sum(t, before) == sum(t, after) {
		t.Error("the record checksum did not change when a currency did")
	}
}

// PlayFab reports the same instant with and without fractional seconds depending on the call.
func TestTimestampPrecisionDoesNotChangeTheRecord(t *testing.T) {
	p := player()
	p.Progress.EnergyUpdatedAt = time.Unix(1700000000, 123456789).UTC()
	p.UpdatedAt = time.Unix(1700000000, 999999999).UTC()

	other := player()
	if sum(t, record(t, p)) != sum(t, record(t, other)) {
		t.Error("the same instant with fractional seconds hashed differently")
	}
}

// Nakama answers "value must be a JSON-encoded object" to anything else, so a list has to be
// wrapped before it is sent rather than discovered in production.
func TestEveryDocumentIsAnObject(t *testing.T) {
	r := record(t, player())
	if len(r.Entities) != 6 {
		t.Fatalf("got %d documents, want 6: %v", len(r.Entities), r.Names())
	}
	for _, e := range r.Entities {
		raw, err := json.Marshal(e.Value)
		if err != nil {
			t.Fatalf("%s: %v", e.Name, err)
		}
		if len(raw) == 0 || raw[0] != '{' {
			t.Errorf("document %s is not a JSON object: %s", e.Name, raw)
		}
	}
}

// A backend without levels has no progress document, and one appearing later is a change.
func TestAbsentDocumentsAreAbsentRatherThanEmpty(t *testing.T) {
	p := player()
	p.Progress = nil
	p.Inventory = nil
	p.UserData = nil

	r := record(t, p)
	if got := r.Names(); len(got) != 3 {
		t.Errorf("documents = %v, want profile, currency, statistics", got)
	}
	if _, ok := r.Entity(mapping.EntityProgress); ok {
		t.Error("a player with no progress record produced a progress document")
	}
}

func TestRecordCarriesTheDerivedIdentity(t *testing.T) {
	r := record(t, player())
	if r.CustomID != "0d761980-c8fc-5cb0-9004-b54dd0d05c70" {
		t.Errorf("custom id = %s, want the frozen value for playfab:ABC123", r.CustomID)
	}
	if r.Source != "playfab" || r.SourceID != "ABC123" {
		t.Errorf("record lost its provenance: %s/%s", r.Source, r.SourceID)
	}
}

func TestDefaultCollection(t *testing.T) {
	r, err := mapping.New("", player(), "playfab")
	if err != nil {
		t.Fatal(err)
	}
	if r.Collection != mapping.DefaultCollection {
		t.Errorf("collection = %q, want %q", r.Collection, mapping.DefaultCollection)
	}
}

// An empty source id must fail here with a message, not panic inside identity.
func TestEmptyIDIsAnErrorNotAPanic(t *testing.T) {
	_, err := mapping.New("migration", &source.Player{}, "playfab")
	if err == nil {
		t.Fatal("a player with no id was mapped")
	}
}

func TestNamesAreUniqueAndStable(t *testing.T) {
	r := record(t, player())
	seen := map[string]bool{}
	for _, e := range r.Entities {
		if seen[e.Name] {
			t.Errorf("duplicate document %s", e.Name)
		}
		seen[e.Name] = true
		if e.Key != e.Name {
			t.Errorf("key %q does not match document %q", e.Key, e.Name)
		}
		if len(e.Checksum) != len("sha256:")+64 {
			t.Errorf("%s: checksum %q is not sha256:<64 hex>", e.Name, e.Checksum)
		}
	}
}

// Same input, same output, every time: the crash test compares this number across processes.
func TestChecksumIsReproducible(t *testing.T) {
	want := sum(t, record(t, player()))
	for i := 0; i < 50; i++ {
		if got := sum(t, record(t, player())); got != want {
			t.Fatalf("run %d: %s, want %s", i, got, want)
		}
	}
}
