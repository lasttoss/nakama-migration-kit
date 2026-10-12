// Package profile holds one title's mapping decisions, and the discovery pass that produces a
// first draft of them.
//
// A PlayFab title does not have a schema. The game team chose the user data keys ("energy",
// "Energy", "player_energy"), the statistic names, the currency codes ("GD", "GOLD", "gold")
// and the catalog ids, and two titles in the same studio chose differently. A kit that hardcodes
// one of those choices imports one title and silently writes the wrong wallet for the next one.
//
// So the transport half of this package's world is generic (paging, backoff, error taxonomy in
// internal/playfab) and the semantic half is declared per title:
//
//   - currency: which legacy code is which balance in the game's wallet;
//   - progress: which user data keys carry level and energy, and which key is the regeneration
//     anchor;
//   - statistics: which legacy stats the game still reads;
//   - inventory: whether catalog ids map onto the game's own item kinds.
//
// Discovering those keys is a read-only pass over a sample of the real tenant, and it is the
// first thing to run on a live title: Discover reads players and reports the keys it saw, the
// keys it has no rule for, and how often each appears. Guessing them from the PlayFab docs is
// how an importer loses a currency.
package profile

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/lasttoss/nakama-migration-kit/internal/source"
)

// Profile is one title's mapping.
type Profile struct {
	// Name identifies the title this profile was written for.
	Name string `json:"name"`
	// Source is the legacy backend ("playfab").
	Source string `json:"source"`
	// Collection is the Nakama storage collection to import into. A real cutover points this at
	// the collection the game already reads, not at a namespace of its own.
	Collection string `json:"collection"`

	// Currency maps a legacy code to the balance name the game uses.
	Currency map[string]string `json:"currency"`

	// UserData maps a legacy user data key to a semantic name. The names progress needs are
	// level, energy and energy_anchor.
	UserData map[string]string `json:"user_data"`

	// Statistics maps a legacy statistic name to the name the game reads. A statistic that is
	// absent from this map is still reported by Discover, and is dropped only if the profile
	// says to drop it.
	Statistics map[string]string `json:"statistics"`

	// Unmapped says what to do with data this profile has no rule for: "note" (default) imports
	// everything else and reports it, "fail" refuses to start. A live cutover starts with "note"
	// because the report is the point, and switches to "fail" once the profile is complete.
	Unmapped string `json:"unmapped"`
}

// Semantic user data names the progress document is built from.
const (
	FieldLevel        = "level"
	FieldEnergy       = "energy"
	FieldEnergyAnchor = "energy_anchor"
)

// Default is the profile a title with no decisions yet would get: balances and statistics under
// their legacy names, no progress mapping, and unmapped data reported rather than refused.
func Default(source string) Profile {
	return Profile{
		Name:       source + "-default",
		Source:     source,
		Collection: "migration",
		Currency:   map[string]string{},
		UserData:   map[string]string{},
		Statistics: map[string]string{},
		Unmapped:   "note",
	}
}

// Load reads a profile from a JSON file.
//
// JSON rather than YAML on purpose: the importer half of this kit has one dependency (the
// Postgres driver) and a config format is not a reason to add a second one.
func Load(path string) (Profile, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Profile{}, fmt.Errorf("profile: %w", err)
	}
	var p Profile
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return Profile{}, fmt.Errorf("profile %s: %w", path, err)
	}
	if err := p.Validate(); err != nil {
		return Profile{}, fmt.Errorf("profile %s: %w", path, err)
	}
	return p, nil
}

// Validate rejects a profile that would import the wrong thing.
func (p Profile) Validate() error {
	if p.Source == "" {
		return fmt.Errorf("source is empty: the derived custom ids depend on it, and changing it later is not a rename but a second account per player")
	}
	if p.Collection == "" {
		return fmt.Errorf("collection is empty")
	}
	switch p.Unmapped {
	case "", "note", "fail":
	default:
		return fmt.Errorf("unmapped is %q, want \"note\" or \"fail\"", p.Unmapped)
	}
	for code, name := range p.Currency {
		if code == "" || name == "" {
			return fmt.Errorf("currency entry with an empty code or name")
		}
	}
	seen := map[string]string{}
	for key, semantic := range p.UserData {
		if key == "" || semantic == "" {
			return fmt.Errorf("user_data entry with an empty key or name")
		}
		if prev, ok := seen[semantic]; ok {
			return fmt.Errorf("user_data maps both %q and %q to %q: a semantic field can have one source", prev, key, semantic)
		}
		seen[semantic] = key
	}
	return nil
}

// Strict reports whether unmapped data is a failure. An empty Unmapped means the default.
func (p Profile) Strict() bool { return p.Unmapped == "fail" }

// ProgressFrom extracts level and energy from a player's user data using this profile.
//
// It returns nil when the profile maps no progress keys or the player has none of them, which
// is not an error: a title without levels has no progress document.
func (p Profile) ProgressFrom(p2 *source.Player) *source.Progress {
	if p2 == nil || len(p.UserData) == 0 {
		return nil
	}
	var level, energy int
	var haveLevel, haveEnergy bool
	for key, semantic := range p.UserData {
		raw, ok := p2.UserData[key]
		if !ok {
			continue
		}
		value, ok := parseInt(raw)
		if !ok {
			continue
		}
		switch semantic {
		case FieldLevel:
			level, haveLevel = value, true
		case FieldEnergy:
			energy, haveEnergy = value, true
		}
	}
	if !haveLevel && !haveEnergy {
		return nil
	}
	// The anchor is optional in the profile, so it is not read here: a title that maps one
	// keeps whatever the source reported on the progress record.
	if p2.Progress != nil {
		p2.Progress.Level, p2.Progress.Energy = level, energy
		return p2.Progress
	}
	return &source.Progress{Level: level, Energy: energy}
}

// Report is what a discovery pass found in a sample of a real tenant.
type Report struct {
	// Players is how many players were read.
	Players int `json:"players"`
	// CurrencySeen counts each currency code, and Mapped says whether the profile has a rule
	// for it.
	Currency []Observed `json:"currency"`
	// UserDataSeen is the same for user data keys.
	UserData []Observed `json:"user_data"`
	// StatisticsSeen is the same for statistic names.
	Statistics []Observed `json:"statistics"`
	// CatalogIDsSeen is the same for inventory catalog ids.
	CatalogIDs []Observed `json:"catalog_ids"`
	// Unmapped lists the names this profile has no rule for, sorted.
	Unmapped []string `json:"unmapped"`
	// Draft is a profile with this tenant's names filled in, for a person to review. A balance
	// or a statistic keeps its legacy name, which is right more often than it is wrong, and the
	// progress keys are proposed only when a single candidate is available.
	Draft Profile `json:"draft"`
}

// Observed is one name seen in the tenant, with how often and whether the profile covers it.
type Observed struct {
	Name   string `json:"name"`
	Count  int    `json:"count"`
	Mapped bool   `json:"mapped"`
}

// Discover reads a sample of players and reports what a profile would have to describe.
//
// This is the read-only pass to run against a live title before writing anything: it is where
// the answer to "what does this game call its gold" comes from, instead of from the docs.
func Discover(p Profile, players []*source.Player) Report {
	cur := map[string]int{}
	data := map[string]int{}
	stats := map[string]int{}
	catalogs := map[string]int{}

	for _, pl := range players {
		if pl == nil {
			continue
		}
		for code := range pl.Currency {
			cur[code]++
		}
		for key := range pl.UserData {
			data[key]++
		}
		for stat := range pl.Statistics {
			stats[stat]++
		}
		for _, it := range pl.Inventory {
			if it.CatalogID != "" {
				catalogs[it.CatalogID]++
			}
		}
	}

	rep := Report{
		Players:    len(players),
		Currency:   observed(cur, func(k string) bool { _, ok := p.Currency[k]; return ok }),
		UserData:   observed(data, func(k string) bool { _, ok := p.UserData[k]; return ok }),
		Statistics: observed(stats, func(k string) bool { _, ok := p.Statistics[k]; return ok }),
		CatalogIDs: observed(catalogs, func(k string) bool { return true }),
	}

	// The draft carries every name the tenant actually uses, because a person reviewing it is
	// faster at deleting a line than at remembering a key.
	rep.Draft = p
	rep.Draft.Currency = map[string]string{}
	for _, o := range rep.Currency {
		rep.Draft.Currency[o.Name] = strings.ToLower(o.Name)
	}
	rep.Draft.Statistics = map[string]string{}
	for _, o := range rep.Statistics {
		rep.Draft.Statistics[o.Name] = o.Name
	}
	for _, o := range rep.UserData {
		if _, ok := rep.Draft.UserData[o.Name]; !ok {
			rep.Draft.UserData[o.Name] = ""
		}
	}

	for _, group := range [][]Observed{rep.Currency, rep.UserData, rep.Statistics} {
		for _, o := range group {
			if !o.Mapped {
				rep.Unmapped = append(rep.Unmapped, o.Name)
			}
		}
	}
	sort.Strings(rep.Unmapped)
	return rep
}

func observed(counts map[string]int, mapped func(string) bool) []Observed {
	out := make([]Observed, 0, len(counts))
	for name, n := range counts {
		out = append(out, Observed{Name: name, Count: n, Mapped: mapped(name)})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func parseInt(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
		n = n*10 + int(s[i]-'0')
		if n > 1<<30 {
			return 0, false
		}
	}
	return n, true
}
