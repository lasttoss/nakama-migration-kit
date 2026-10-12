// Package mapping turns a legacy player record into the documents Nakama stores, and gives each
// document a semantic checksum.
//
// The checksum is per document rather than per player, because that is what makes "a second run
// writes nothing" a useful claim instead of a slogan: a run that finds the profile unchanged and
// one currency changed writes one document and says so. A per-player checksum could only say
// "this player changed" and would rewrite everything for a one-field edit.
//
// Three normalizations make the checksum semantic rather than byte-exact, and each one exists
// because a byte comparison reported a difference that was not there:
//
//   - inventory is sorted, because the backend does not promise an order;
//   - timestamps are seconds, because the backend reports the same instant with varying
//     precision (PlayFab returns fractional seconds on some calls and not others);
//   - values go through canon.Checksum, which sorts keys and does not escape HTML, because
//     Postgres jsonb re-renders what it stores.
//
// Every document is a JSON object. Nakama rejects a storage value that is not one, so a list
// is wrapped: inventory is {"items": [...]}, not [...]. TestEveryDocumentIsAnObject pins it.
package mapping

import (
	"fmt"
	"sort"
	"time"

	"github.com/lasttoss/nakama-migration-kit/internal/canon"
	"github.com/lasttoss/nakama-migration-kit/internal/identity"
	"github.com/lasttoss/nakama-migration-kit/internal/source"
)

// DefaultCollection is the Nakama storage collection the kit writes into. A real cutover
// points it at the game's own collection with --collection rather than importing into a
// namespace the game does not read.
const DefaultCollection = "migration"

// Entity names, in the order a record lists them.
const (
	EntityProfile    = "profile"
	EntityUserData   = "userdata"
	EntityInventory  = "inventory"
	EntityCurrency   = "currency"
	EntityStatistics = "statistics"
	EntityProgress   = "progress"
)

// Entity is one document: what to write, and what it means.
type Entity struct {
	Name     string
	Key      string
	Value    any
	Checksum string
}

// Record is one player's migrated state.
type Record struct {
	Source     string
	SourceID   string
	CustomID   string
	Collection string
	Entities   []Entity
}

// New builds the record for a player. The collection is where Nakama will store it.
func New(collection string, p *source.Player, src string) (*Record, error) {
	if collection == "" {
		collection = DefaultCollection
	}
	if err := source.Validate(p); err != nil {
		return nil, err
	}
	if src == "" {
		return nil, fmt.Errorf("mapping: source name is empty")
	}

	r := &Record{
		Source:     src,
		SourceID:   p.ID,
		CustomID:   identity.CustomID(src, p.ID),
		Collection: collection,
	}

	profile := map[string]any{
		"display_name": p.DisplayName,
		"source":       src,
		"source_id":    p.ID,
	}
	if !p.CreatedAt.IsZero() {
		profile["created_at"] = unix(p.CreatedAt)
	}
	if !p.UpdatedAt.IsZero() {
		profile["updated_at"] = unix(p.UpdatedAt)
	}
	if err := r.add(EntityProfile, EntityProfile, profile); err != nil {
		return nil, err
	}

	if len(p.UserData) > 0 {
		if err := r.add(EntityUserData, EntityUserData, map[string]any{"slots": p.UserData}); err != nil {
			return nil, err
		}
	}
	if len(p.Inventory) > 0 {
		items := make([]any, 0, len(p.Inventory))
		for _, it := range p.Inventory {
			items = append(items, map[string]any{
				"instance_id": it.InstanceID,
				"catalog_id":  it.CatalogID,
				"count":       it.Count,
			})
		}
		// Sorted so that the same inventory in another order is the same document.
		sort.Slice(items, func(i, j int) bool { return itemLess(items[i], items[j]) })
		if err := r.add(EntityInventory, EntityInventory, map[string]any{"items": items}); err != nil {
			return nil, err
		}
	}
	if len(p.Currency) > 0 {
		if err := r.add(EntityCurrency, EntityCurrency, map[string]any{"balances": p.Currency}); err != nil {
			return nil, err
		}
	}
	if len(p.Statistics) > 0 {
		if err := r.add(EntityStatistics, EntityStatistics, map[string]any{"stats": p.Statistics}); err != nil {
			return nil, err
		}
	}
	if p.Progress != nil {
		progress := map[string]any{
			"level":  p.Progress.Level,
			"energy": p.Progress.Energy,
		}
		if !p.Progress.EnergyUpdatedAt.IsZero() {
			progress["energy_updated_at"] = unix(p.Progress.EnergyUpdatedAt)
		}
		if err := r.add(EntityProgress, EntityProgress, progress); err != nil {
			return nil, err
		}
	}
	return r, nil
}

func (r *Record) add(name, key string, value any) error {
	sum, err := canon.Checksum(value)
	if err != nil {
		return fmt.Errorf("mapping: %s/%s: %w", r.SourceID, name, err)
	}
	r.Entities = append(r.Entities, Entity{Name: name, Key: key, Value: value, Checksum: sum})
	return nil
}

// Entity returns the named document, if the source had one.
func (r *Record) Entity(name string) (Entity, bool) {
	for _, e := range r.Entities {
		if e.Name == name {
			return e, true
		}
	}
	return Entity{}, false
}

// Checksum is the checksum of the whole record: the entity names, keys and checksums in order.
//
// This is the number make demo-crash compares, and the one a report quotes. It is derived from
// the per-entity checksums rather than from the raw source record, so it can be recomputed from
// what Nakama actually holds.
func (r *Record) Checksum() (string, error) {
	triples := make([]string, 0, len(r.Entities))
	for _, e := range r.Entities {
		triples = append(triples, e.Name+"\x00"+e.Key+"\x00"+e.Checksum)
	}
	return canon.Checksum(triples)
}

// Names lists the documents this record would write, in order.
func (r *Record) Names() []string {
	out := make([]string, 0, len(r.Entities))
	for _, e := range r.Entities {
		out = append(out, e.Name)
	}
	return out
}

func itemLess(a, b any) bool {
	ai, bi := a.(map[string]any), b.(map[string]any)
	ac, bc := fmt.Sprint(ai["catalog_id"]), fmt.Sprint(bi["catalog_id"])
	if ac != bc {
		return ac < bc
	}
	return fmt.Sprint(ai["instance_id"]) < fmt.Sprint(bi["instance_id"])
}

// unix is the one timestamp representation that reaches a document. Truncating to seconds is
// deliberate: the same instant reported with and without fractional seconds must produce the
// same checksum.
func unix(t time.Time) int64 { return t.Unix() }
