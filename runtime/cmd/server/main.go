// Command server is the migration kit's Nakama runtime module.
//
// The importer does not write to Nakama's storage, wallet or leaderboards directly, and not only
// because it would have to reimplement them: a target that enforces its own idempotency is the only
// place the claim "a second run writes nothing" can be checked. The importer decides what a player
// should look like; this module decides whether that is a change, and records the checksum it wrote
// beside the data it wrote it for. A run that stops after the write and before its own ledger is
// therefore still idempotent: the checksum is already where the next run looks.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/heroiclabs/nakama-common/runtime"
)

// RPC names. They are a contract with internal/target in the main module, and with anyone running
// this kit against their own console.
const (
	rpcPlan   = "migration.plan"
	rpcApply  = "migration.apply"
	rpcStatus = "migration.status"
)

// checksumCollection is where this module records what it wrote, per document. It is one collection
// with one row per document rather than a table of its own: the row that decides whether a write is
// needed lives in the same transaction as the write, and neither can be lost without the other.
const checksumCollection = "migration_checksum"

// Document is one writable unit and the checksum of its meaning.
type Document struct {
	// Collection and Key say where it goes.
	Collection string `json:"collection"`
	Key        string `json:"key"`
	// Checksum is semantic, computed by internal/mapping: two documents that mean the same thing
	// carry the same checksum, so a run that finds the same value does not write.
	Checksum string `json:"checksum"`
	// Value is the document body, JSON.
	Value string `json:"value"`
	// Public is whether any client may read it. User data that a game shows in a menu is public;
	// a migration ledger is not.
	Public bool `json:"public,omitempty"`
}

// ApplyRequest is one player's worth of documents, plus how far along the run is.
type ApplyRequest struct {
	// PlayerID is the target account's Nakama user id.
	PlayerID string `json:"player_id"`
	// SourceID is the legacy id, kept for the report: an operator debugging a mismatch reads the old
	// id, and deriving it back from a UUIDv5 is not something a person can do in their head.
	SourceID string `json:"source_id"`
	// DryRun answers what would be written without writing it.
	DryRun bool       `json:"dry_run,omitempty"`
	Docs   []Document `json:"documents"`
}

// ApplyResponse is what happened, in numbers rather than in prose.
type ApplyResponse struct {
	PlayerID string   `json:"player_id"`
	SourceID string   `json:"source_id"`
	Written  []string `json:"written"`
	Skipped  []string `json:"skipped"`
	DryRun   bool     `json:"dry_run,omitempty"`
}

// statusResponse is the module's own view: what it has recorded, not what the importer believes.
//
// It counts checksums and nothing else. An earlier version also counted a "players" collection this
// module never writes, which reported zero for every player that had been imported - a number that
// is wrong in the direction that looks like an empty database.
type statusResponse struct {
	Checksums int `json:"checksums"`
}

func InitModule(ctx context.Context, logger runtime.Logger, db *sql.DB, nk runtime.NakamaModule, initializer runtime.Initializer) error {
	logger.Info("migration kit runtime loaded")

	if err := initializer.RegisterRpc(rpcApply, func(ctx context.Context, logger runtime.Logger, db *sql.DB, nk runtime.NakamaModule, payload string) (string, error) {
		var req ApplyRequest
		if err := json.Unmarshal([]byte(payload), &req); err != nil {
			return "", runtime.NewError("the payload is not an apply request", 3)
		}
		if req.PlayerID == "" {
			return "", runtime.NewError("player_id is required", 3)
		}
		resp, err := apply(ctx, nk, req)
		if err != nil {
			return "", err
		}
		out, err := json.Marshal(resp)
		if err != nil {
			return "", err
		}
		return string(out), nil
	}); err != nil {
		return err
	}

	if err := initializer.RegisterRpc(rpcPlan, func(ctx context.Context, logger runtime.Logger, db *sql.DB, nk runtime.NakamaModule, payload string) (string, error) {
		var req ApplyRequest
		if err := json.Unmarshal([]byte(payload), &req); err != nil {
			return "", runtime.NewError("the payload is not a plan request", 3)
		}
		req.DryRun = true
		resp, err := apply(ctx, nk, req)
		if err != nil {
			return "", err
		}
		out, err := json.Marshal(resp)
		if err != nil {
			return "", err
		}
		return string(out), nil
	}); err != nil {
		return err
	}

	if err := initializer.RegisterRpc(rpcStatus, func(ctx context.Context, logger runtime.Logger, db *sql.DB, nk runtime.NakamaModule, payload string) (string, error) {
		checksums, _, err := nk.StorageList(ctx, "", "", checksumCollection, 100, "")
		if err != nil {
			return "", err
		}
		out, err := json.Marshal(statusResponse{Checksums: len(checksums)})
		if err != nil {
			return "", err
		}
		return string(out), nil
	}); err != nil {
		return err
	}

	return nil
}

// apply writes the documents whose meaning changed, and says which those were.
func apply(ctx context.Context, nk runtime.NakamaModule, req ApplyRequest) (ApplyResponse, error) {
	resp := ApplyResponse{PlayerID: req.PlayerID, SourceID: req.SourceID, DryRun: req.DryRun}

	recorded, err := recordedChecksums(ctx, nk, req.PlayerID, req.Docs)
	if err != nil {
		return resp, err
	}

	var writes []*runtime.StorageWrite
	for _, doc := range req.Docs {
		name := doc.Collection + "/" + doc.Key
		if recorded[name] == doc.Checksum {
			// The same meaning is already there. Not writing is the product: it is what makes a
			// second run safe to do during a maintenance window that is already open.
			resp.Skipped = append(resp.Skipped, name)
			continue
		}
		if req.DryRun {
			resp.Written = append(resp.Written, name)
			continue
		}
		permission := 1 // owner read
		if doc.Public {
			permission = 2 // owner read and public read
		}
		writes = append(writes, &runtime.StorageWrite{
			Collection: doc.Collection, Key: doc.Key, UserID: req.PlayerID,
			Value: doc.Value, PermissionRead: permission, PermissionWrite: 1,
		})
		writes = append(writes, &runtime.StorageWrite{
			Collection: checksumCollection, Key: req.SourceID + ":" + doc.Collection + "/" + doc.Key,
			UserID: req.PlayerID, Value: fmt.Sprintf(`{"checksum":%q,"collection":%q,"key":%q}`, doc.Checksum, doc.Collection, doc.Key),
			PermissionRead: 0, PermissionWrite: 0,
		})
		resp.Written = append(resp.Written, name)
	}

	sort.Strings(resp.Written)
	sort.Strings(resp.Skipped)

	if req.DryRun || len(writes) == 0 {
		return resp, nil
	}
	if _, err := nk.StorageWrite(ctx, writes); err != nil {
		return resp, err
	}
	return resp, nil
}

// recordedChecksums reads back what this module wrote for these documents, in one call.
func recordedChecksums(ctx context.Context, nk runtime.NakamaModule, userID string, docs []Document) (map[string]string, error) {
	keys := make([]string, 0, len(docs))
	for _, doc := range docs {
		keys = append(keys, doc.Collection+"/"+doc.Key)
	}
	sort.Strings(keys)

	out := map[string]string{}
	// Nakama lists storage by collection and paging, not by key set: read the collection this module
	// owns and keep the rows the caller asked about.
	objects, _, err := nk.StorageList(ctx, "", userID, checksumCollection, 100, "")
	if err != nil {
		return nil, err
	}
	wanted := map[string]bool{}
	for _, k := range keys {
		wanted[k] = true
	}
	for _, o := range objects {
		var row struct {
			Checksum   string `json:"checksum"`
			Collection string `json:"collection"`
			Key        string `json:"key"`
		}
		if err := json.Unmarshal([]byte(o.GetValue()), &row); err != nil {
			// A row this module cannot read is a row it cannot trust: treat it as missing, which
			// makes the next run write rather than skip. Silence here would be a skipped write.
			continue
		}
		name := row.Collection + "/" + row.Key
		if !wanted[name] {
			continue
		}
		out[name] = row.Checksum
	}
	return out, nil
}
