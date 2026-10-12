package profile_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lasttoss/nakama-migration-kit/internal/faketenant"
	"github.com/lasttoss/nakama-migration-kit/internal/profile"
	"github.com/lasttoss/nakama-migration-kit/internal/source"
)

func write(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "profile.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadAcceptsAProfiles(t *testing.T) {
	p, err := profile.Load(write(t, `{
	  "name": "farm-blast",
	  "source": "playfab",
	  "collection": "game",
	  "currency": {"GD": "gold", "GM": "gems"},
	  "user_data": {"energy": "energy", "level": "level"},
	  "statistics": {"wins": "wins"},
	  "unmapped": "note"
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if p.Currency["GD"] != "gold" || p.UserData["energy"] != profile.FieldEnergy {
		t.Errorf("profile loaded wrong: %+v", p)
	}
	if p.Strict() {
		t.Error("unmapped=note should not be strict")
	}
}

// A typo in a profile is a silent import of the wrong field, so unknown keys are refused.
func TestLoadRefusesAnUnknownField(t *testing.T) {
	_, err := profile.Load(write(t, `{"source":"playfab","collection":"game","currancy":{"GD":"gold"}}`))
	if err == nil {
		t.Fatal("a misspelled field was accepted")
	}
	if !strings.Contains(err.Error(), "currancy") {
		t.Errorf("the error does not name the field: %v", err)
	}
}

func TestValidateRejectsWhatWouldImportTheWrongThing(t *testing.T) {
	cases := map[string]string{
		"no source":             `{"collection":"game"}`,
		"no collection":         `{"source":"playfab"}`,
		"bad unmapped":          `{"source":"playfab","collection":"game","unmapped":"maybe"}`,
		"empty currency":        `{"source":"playfab","collection":"game","currency":{"GD":""}}`,
		"two sources one field": `{"source":"playfab","collection":"game","user_data":{"energy":"energy","Energy":"energy"}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := profile.Load(write(t, body)); err == nil {
				t.Error("a profile that would import the wrong thing was accepted")
			}
		})
	}
}

func TestProgressFromUsesTheProfilesKeyNames(t *testing.T) {
	p := profile.Profile{
		Source: "playfab", Collection: "game",
		UserData: map[string]string{"player_energy": profile.FieldEnergy, "player_level": profile.FieldLevel},
	}
	got := p.ProgressFrom(&source.Player{
		ID:       "PF1",
		UserData: map[string]string{"player_energy": "42", "player_level": "7", "junk": "x"},
	})
	if got == nil || got.Energy != 42 || got.Level != 7 {
		t.Fatalf("progress = %+v, want level 7 energy 42", got)
	}

	// A title whose keys are different is the same code with a different profile.
	other := profile.Profile{UserData: map[string]string{"E": profile.FieldEnergy}}
	if got := other.ProgressFrom(&source.Player{UserData: map[string]string{"player_energy": "42"}}); got != nil {
		t.Errorf("a profile with different keys still found progress: %+v", got)
	}
}

func TestProgressFromIgnoresWhatIsNotANumber(t *testing.T) {
	p := profile.Profile{UserData: map[string]string{"energy": profile.FieldEnergy}}
	got := p.ProgressFrom(&source.Player{UserData: map[string]string{"energy": "not a number"}})
	if got != nil {
		t.Errorf("a non-numeric energy produced progress: %+v", got)
	}
}

// Discovery is the answer to "what does this title call its gold", so it has to report what is
// there rather than what the docs say.
func TestDiscoverReportsTheTenantsOwnNames(t *testing.T) {
	players := []*source.Player{
		{ID: "1", Currency: map[string]int64{"GD": 1, "GM": 2}, UserData: map[string]string{"energy": "5"}, Statistics: map[string]int64{"wins": 1}, Inventory: []source.Item{{CatalogID: "sword"}}},
		{ID: "2", Currency: map[string]int64{"GD": 3, "XX": 4}, UserData: map[string]string{"energy": "6", "level": "2"}, Statistics: map[string]int64{"wins": 1, "win_streak": 3}, Inventory: []source.Item{{CatalogID: "sword"}, {CatalogID: "shield"}}},
	}
	p := profile.Default("playfab")
	p.Currency = map[string]string{"GD": "gold"}
	p.UserData = map[string]string{"energy": profile.FieldEnergy}

	rep := profile.Discover(p, players)

	if rep.Players != 2 {
		t.Errorf("players = %d", rep.Players)
	}
	if len(rep.Currency) != 3 {
		t.Errorf("currency seen = %+v, want GD, GM, XX", rep.Currency)
	}
	if rep.Currency[0].Name != "GD" || rep.Currency[0].Count != 2 || !rep.Currency[0].Mapped {
		t.Errorf("most common currency = %+v, want GD unmapped=false", rep.Currency[0])
	}
	// GM and XX have no rule; GD does.
	if !strings.Contains(strings.Join(rep.Unmapped, ","), "GM") || !strings.Contains(strings.Join(rep.Unmapped, ","), "win_streak") {
		t.Errorf("unmapped = %v, want it to name GM and win_streak", rep.Unmapped)
	}
	if strings.Contains(strings.Join(rep.Unmapped, ","), "GD") {
		t.Errorf("GD has a rule but was reported unmapped: %v", rep.Unmapped)
	}

	// The draft carries the names found, so a person reviews and deletes rather than types.
	if rep.Draft.Currency["XX"] != "xx" {
		t.Errorf("draft currency = %+v, want XX present", rep.Draft.Currency)
	}
	if rep.Draft.Statistics["win_streak"] != "win_streak" {
		t.Errorf("draft statistics = %+v, want win_streak present", rep.Draft.Statistics)
	}
}

// The report a person actually reads on a live tenant. Printed on purpose: it is the artifact
// that turns "it depends on the title" into a list of decisions.
func TestDiscoverOnTheFakeTenant(t *testing.T) {
	tenant := faketenant.NewTenant(200, 1)
	p := profile.Default("playfab")
	p.Currency = map[string]string{"GD": "gold"}

	rep := profile.Discover(p, tenant.Players)

	t.Logf("tenant of %d players:", rep.Players)
	t.Logf("  currency   %s", formatObserved(rep.Currency))
	t.Logf("  user data  %s", formatObserved(rep.UserData))
	t.Logf("  statistics %s", formatObserved(rep.Statistics))
	t.Logf("  catalogs   %s", formatObserved(rep.CatalogIDs))
	t.Logf("  unmapped   %v", rep.Unmapped)

	if len(rep.Currency) != 2 || len(rep.UserData) != 3 || len(rep.Statistics) != 2 || len(rep.CatalogIDs) != 3 {
		t.Errorf("discovery missed names: currency=%d userdata=%d statistics=%d catalogs=%d",
			len(rep.Currency), len(rep.UserData), len(rep.Statistics), len(rep.CatalogIDs))
	}
	// GD is mapped, GM is not, and the report has to say so rather than staying quiet.
	if len(rep.Unmapped) == 0 {
		t.Error("the report found nothing unmapped in a tenant where GM has no rule")
	}
}

func formatObserved(in []profile.Observed) string {
	parts := make([]string, 0, len(in))
	for _, o := range in {
		mark := "unmapped"
		if o.Mapped {
			mark = "mapped"
		}
		parts = append(parts, fmt.Sprintf("%s(%d,%s)", o.Name, o.Count, mark))
	}
	return strings.Join(parts, " ")
}
