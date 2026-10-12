// Package faketenant is a legacy backend that can be trusted to behave like one.
//
// It answers the subset of the PlayFab Admin API a migration actually calls, in PlayFab's own
// response envelope, over HTTP: a segment export for listing players, then profile, user data,
// inventory, currency and statistics per player. The same adapter code runs against this and
// against a real tenant, because the only difference is the base URL.
//
// It also does what a real title does to an importer that is not careful:
//
//   - answers 429 with a Retry-After when it is asked too fast (PlayFab rate limits the Admin
//     API, and an importer that treats 429 as fatal stops half way through a live title);
//   - answers 500 occasionally, because five hundred is what a busy backend answers;
//   - hands back a short page while more players remain, which is the failure mode worth
//     having: an importer that reads "short page" as "end of listing" loses the players after
//     it and says the import succeeded. Reconciliation is what catches that, and there is a
//     test that proves it does.
//
// The players it serves are a pure function of a seed, so a run can compare the numbers it
// prints with the numbers it printed yesterday, and Tenant.Checksum is the ground truth a
// reconciliation report is checked against.
package faketenant

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"time"

	"github.com/lasttoss/nakama-migration-kit/internal/source"
)

// Tenant is the data the server serves.
type Tenant struct {
	Players []*source.Player
}

// PlayerIndex maps a player id to its position, for O(1) lookups under load.
func (t *Tenant) PlayerIndex() map[string]*source.Player {
	index := make(map[string]*source.Player, len(t.Players))
	for _, p := range t.Players {
		index[p.ID] = p
	}
	return index
}

// Checksum is a checksum of the whole tenant, used as the ground truth for a run: the reconciler
// recomputes it from what Nakama holds and compares.
func (t *Tenant) Checksum() string {
	ids := make([]string, 0, len(t.Players))
	byID := t.PlayerIndex()
	for id := range byID {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	h := sha256.New()
	for _, id := range ids {
		p := byID[id]
		// Not every player has a progress record, and the ground truth has to describe the
		// tenant that exists rather than the one that would be easier to hash.
		energy, level := -1, -1
		if p.Progress != nil {
			energy, level = p.Progress.Energy, p.Progress.Level
		}
		fmt.Fprintf(h, "%s\x00%s\x00%d\x00%d\x00", p.ID, p.DisplayName, level, energy)
		codes := make([]string, 0, len(p.Currency))
		for code := range p.Currency {
			codes = append(codes, code)
		}
		sort.Strings(codes)
		for _, code := range codes {
			fmt.Fprintf(h, "%s=%d\x00", code, p.Currency[code])
		}
		fmt.Fprintf(h, "\n")
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// NewTenant builds n deterministic players.
//
// The shape of the data is deliberate: every fourth player has no inventory, every fifth has no
// progress record, one in ten has an empty display name, and one in a hundred has a display name
// with a character that a careless encoder escapes ("&", "<", a quote). Those are the cases that
// turn a migration into an afternoon.
func NewTenant(n int, seed int64) *Tenant {
	t := &Tenant{Players: make([]*source.Player, 0, n)}
	for i := 0; i < n; i++ {
		t.Players = append(t.Players, newPlayer(i, seed))
	}
	return t
}

func newPlayer(i int, seed int64) *source.Player {
	id := fmt.Sprintf("PF%08d", i+1)
	created := time.Unix(1600000000+int64(i)*37, 0).UTC()

	p := &source.Player{
		ID:        id,
		CreatedAt: created,
		UpdatedAt: created.Add(time.Duration(i%97) * time.Hour),
		Currency: map[string]int64{
			"GD": int64(100 + (i*7919+int(seed))%900000),
			"GM": int64((i * 13) % 500),
		},
		Statistics: map[string]int64{
			"wins":   int64(i % 250),
			"losses": int64((i * 3) % 250),
		},
	}
	if i%10 != 0 {
		p.DisplayName = fmt.Sprintf("player-%05d", i)
	}
	if i%10 == 3 { // "&" and "<" are HTML-escaped by a naive encoder, and then the checksums differ
		p.DisplayName = fmt.Sprintf("R&D <%d> \"dragon\"", i)
	}
	if i%4 != 0 {
		p.Inventory = []source.Item{
			{InstanceID: id + "-i1", CatalogID: "sword", Count: 1},
			{InstanceID: id + "-i2", CatalogID: "shield", Count: int64(1 + i%3)},
		}
		if i%3 == 0 {
			p.Inventory = append(p.Inventory, source.Item{InstanceID: id + "-i3", CatalogID: "potion", Count: 10})
		}
	}
	if i%5 != 0 {
		p.Progress = &source.Progress{
			Level:           1 + i%60,
			Energy:          10 + (i*7)%90,
			EnergyUpdatedAt: created.Add(time.Duration(i%24) * time.Hour),
		}
	}
	p.UserData = map[string]string{
		"tutorial": "done",
		"city":     fmt.Sprintf("c%d", i%12),
	}
	if i%7 == 0 {
		p.UserData["note"] = "quoted \"text\" & more"
	}
	return p
}

// Response envelopes, as PlayFab writes them.

type apiError struct {
	Code    int    `json:"code"`
	Status  string `json:"status"`
	Message string `json:"errorMessage"`
}

func (e apiError) Error() string { return e.Message }

// GetPlayersInSegmentResponse is PlayFab's Admin/GetPlayersInSegment.
type GetPlayersInSegmentResponse struct {
	Code int `json:"code"`
	Data struct {
		ProfilesInSegment []struct {
			PlayerID    string `json:"PlayerId"`
			DisplayName string `json:"DisplayName"`
		} `json:"ProfilesInSegment"`
		ContinuationToken string `json:"ContinuationToken"`
		ProfilesProcessed int    `json:"ProfilesProcessed"`
	} `json:"data"`
}

// GetPlayerProfileResponse is PlayFab's Admin/GetPlayerProfile.
type GetPlayerProfileResponse struct {
	Code int `json:"code"`
	Data struct {
		PlayerProfile struct {
			PlayerID    string `json:"PlayerId"`
			DisplayName string `json:"DisplayName"`
			Created     string `json:"Created"`
			LastLogin   string `json:"LastLogin"`
		} `json:"PlayerProfile"`
	} `json:"data"`
}

// GetUserDataResponse is PlayFab's Admin/GetUserData.
type GetUserDataResponse struct {
	Code int `json:"code"`
	Data struct {
		Data        map[string]string `json:"Data"`
		DataVersion int               `json:"DataVersion"`
	} `json:"data"`
}

// GetUserInventoryResponse is PlayFab's Admin/GetUserInventory.
type GetUserInventoryResponse struct {
	Code int `json:"code"`
	Data struct {
		Inventory []struct {
			ItemInstanceID string `json:"ItemInstanceId"`
			ItemID         string `json:"ItemId"`
			RemainingUses  int64  `json:"RemainingUses"`
			CatalogVersion string `json:"CatalogVersion"`
		} `json:"Inventory"`
	} `json:"data"`
}

// GetUserVirtualCurrencyResponse is PlayFab's Admin/GetUserVirtualCurrency.
type GetUserVirtualCurrencyResponse struct {
	Code int `json:"code"`
	Data struct {
		VirtualCurrency map[string]int64 `json:"VirtualCurrency"`
	} `json:"data"`
}

// GetPlayerStatisticsResponse is PlayFab's Admin/GetPlayerStatistics.
type GetPlayerStatisticsResponse struct {
	Code int `json:"code"`
	Data struct {
		Statistics []struct {
			StatisticName string `json:"StatisticName"`
			Value         int64  `json:"Value"`
		} `json:"Statistics"`
	} `json:"data"`
}
