// Package source describes a legacy backend the kit imports players from.
//
// The interface is deliberately two calls: a page of ids, then one player at a time. That is
// the shape of the PlayFab Admin API (list, then profile, user data, inventory, currency and
// statistics per player) and it is also the shape that survives pagination and rate limits,
// because the importer can checkpoint between pages without holding a player in memory.
//
// Nothing here knows about Nakama. The mapping from a Player to what Nakama stores lives in
// internal/mapping, so a second backend is a new implementation of this interface and no
// change to the write path.
package source

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ErrNotFound is what a source returns for an id it does not have. The importer treats it as a
// skipped player and counts it, rather than failing a run over a player who was deleted in the
// legacy backend while the listing was being walked.
var ErrNotFound = errors.New("player not found in the source")

// Player is one player's data in a legacy backend, as the backend reports it.
type Player struct {
	// ID is the backend's own identifier (a PlayFabId, a SmartFox user name). It is not
	// unique across backends, which is why identity.CustomID takes the source name too.
	ID string
	// DisplayName is what the player is called. PlayFab allows it to be empty.
	DisplayName string
	// UserData is the key/value store the backend exposes to game code.
	UserData map[string]string
	// Inventory is unordered: the backend does not promise an order, and two runs that
	// differ only in the order of this slice are the same state.
	Inventory []Item
	// Currency is keyed by the backend's currency code ("GD" for PlayFab gold).
	Currency map[string]int64
	// Statistics is the backend's stat store, keyed by stat name.
	Statistics map[string]int64
	// Progress is the game's own progress record, if the backend has one.
	Progress *Progress
	// CreatedAt and UpdatedAt are the backend's timestamps for the account.
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Item is one stack in a player's inventory.
type Item struct {
	// InstanceID identifies this stack in the backend (PlayFab's ItemInstanceId).
	InstanceID string
	// CatalogID is what kind of item it is (PlayFab's ItemId).
	CatalogID string
	Count     int64
}

// Progress is the level and energy a game tracks per player.
//
// EnergyUpdatedAt is the anchor the client uses to compute regeneration, so it is part of the
// migrated state rather than decoration: a player who was at 30 of 100 energy two hours ago
// comes back with a full bar, and importing the wrong anchor changes their account.
type Progress struct {
	Level           int
	Energy          int
	EnergyUpdatedAt time.Time
}

// Source is a legacy backend.
type Source interface {
	// Name is the backend's short name and part of every derived identity ("playfab").
	Name() string

	// PlayerIDs returns one page of player ids.
	//
	// cursor is empty for the first page. A non-empty next cursor means there is more; an
	// empty one means the listing is done. An implementation must be safe to call with the
	// same cursor twice: a run that crashes after fetching a page and before checkpointing
	// repeats it.
	PlayerIDs(ctx context.Context, cursor string, limit int) (ids []string, next string, err error)

	// Player returns one player's data. A backend that returns a partial record for a
	// player it cannot read must say so with an error rather than return the partial
	// record: the importer treats a missing field as absent data, not as unknown.
	Player(ctx context.Context, id string) (*Player, error)
}

// Validate reports whether a player record is safe to map.
//
// The checks are the ones that would otherwise corrupt an account silently: an id that cannot
// be used to derive an identity, a negative count, a name that a storage write would reject.
// A backend that returns something else is a bug in that adapter, and the importer stops
// rather than importing half a player.
func Validate(p *Player) error {
	if p == nil {
		return fmt.Errorf("source: player is nil")
	}
	if p.ID == "" {
		return fmt.Errorf("source: player has no id")
	}
	if len(p.ID) > 256 {
		return fmt.Errorf("source: player %q: id is longer than 256 bytes", p.ID)
	}
	for _, it := range p.Inventory {
		if it.CatalogID == "" && it.InstanceID == "" {
			return fmt.Errorf("source: player %q: inventory entry with no catalog id and no instance id", p.ID)
		}
		if it.Count < 0 {
			return fmt.Errorf("source: player %q: item %q has a negative count", p.ID, it.CatalogID)
		}
	}
	for code, amount := range p.Currency {
		if code == "" {
			return fmt.Errorf("source: player %q: currency with an empty code", p.ID)
		}
		if amount < 0 {
			return fmt.Errorf("source: player %q: currency %q is negative (%d)", p.ID, code, amount)
		}
	}
	if p.Progress != nil {
		if p.Progress.Level < 0 || p.Progress.Energy < 0 {
			return fmt.Errorf("source: player %q: negative level or energy", p.ID)
		}
	}
	return nil
}
