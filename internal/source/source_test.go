package source_test

import (
	"testing"
	"time"

	"github.com/lasttoss/nakama-migration-kit/internal/source"
)

func valid() *source.Player {
	return &source.Player{
		ID:          "ABC123",
		DisplayName: "An",
		Currency:    map[string]int64{"GD": 100},
		Inventory:   []source.Item{{CatalogID: "sword", Count: 1}},
		Progress:    &source.Progress{Level: 3, Energy: 10, EnergyUpdatedAt: time.Unix(1000, 0).UTC()},
	}
}

func TestValidateAcceptsAWholePlayer(t *testing.T) {
	if err := source.Validate(valid()); err != nil {
		t.Fatalf("a complete player was rejected: %v", err)
	}
}

func TestValidateRejectsWhatWouldCorruptAnAccount(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*source.Player)
		want   string
	}{
		{"nil player", nil, "nil"},
		{"no id", func(p *source.Player) { p.ID = "" }, "no id"},
		{"negative currency", func(p *source.Player) { p.Currency = map[string]int64{"GD": -1} }, "negative"},
		{"empty currency code", func(p *source.Player) { p.Currency = map[string]int64{"": 1} }, "empty code"},
		{"negative item count", func(p *source.Player) { p.Inventory = []source.Item{{CatalogID: "x", Count: -2}} }, "negative count"},
		{"item with no identity", func(p *source.Player) { p.Inventory = []source.Item{{}} }, "no catalog id"},
		{"negative progress", func(p *source.Player) { p.Progress = &source.Progress{Level: -1} }, "negative level"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var p *source.Player
			if tc.mutate != nil {
				p = valid()
				tc.mutate(p)
			}
			err := source.Validate(p)
			if err == nil {
				t.Fatal("an unimportable record was accepted")
			}
			if !contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

// A long id is not a style question: the derived custom id goes into a storage key and a
// Postgres unique index, both of which reject unbounded input.
func TestValidateRejectsAnUnboundedID(t *testing.T) {
	p := valid()
	for len(p.ID) <= 256 {
		p.ID += "x"
	}
	if err := source.Validate(p); err == nil {
		t.Error("an id longer than 256 bytes was accepted")
	}
}

// Progress is optional: a backend without levels has no progress record, and that is not an
// error.
func TestValidateAcceptsNoProgress(t *testing.T) {
	p := valid()
	p.Progress = nil
	if err := source.Validate(p); err != nil {
		t.Errorf("a player with no progress record was rejected: %v", err)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
