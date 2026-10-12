// Package target is where migrated data is written.
//
// There is one implementation, Nakama, and it does not write to Nakama's storage tables: it calls
// the runtime module, which decides whether a document is a change and records the checksum it
// wrote beside the data. The split is deliberate. An importer that wrote directly would have to
// reimplement Nakama's storage API, and - more to the point - a run that died between its write and
// its own bookkeeping would have no way to tell the next run what already happened. Idempotency
// enforced at the target survives that; idempotency enforced here does not.
package target

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/lasttoss/nakama-migration-kit/internal/mapping"
)

// Document is one writable unit and the checksum of its meaning, as the runtime module defines it.
// The two definitions have to agree; the smoke script and the lab are what check that.
type Document struct {
	Collection string `json:"collection"`
	Key        string `json:"key"`
	Checksum   string `json:"checksum"`
	Value      string `json:"value"`
	Public     bool   `json:"public"`
}

// Request is one player's worth of work.
type Request struct {
	PlayerID string `json:"player_id"`
	SourceID string `json:"source_id"`
	// DryRun asks what would be written without writing it. It is the same payload either way, so a
	// dry run cannot disagree with the run it is previewing about what would change.
	DryRun    bool       `json:"dry_run,omitempty"`
	Documents []Document `json:"documents"`
}

// Result is what the target did, in the two lists that matter: what it wrote and what it skipped
// because the meaning was already there.
type Result struct {
	Written []string `json:"written"`
	Skipped []string `json:"skipped"`
	DryRun  bool     `json:"dry_run,omitempty"`
}

// Status is the target's own view of what it has recorded.
type Status struct {
	Checksums int `json:"checksums"`
}

// Inspect is what the target holds for one player: the documents, read back from storage.
type Inspect struct {
	PlayerID  string     `json:"player_id"`
	Documents []Document `json:"documents"`
}

// Target is where a migration writes.
type Target interface {
	// Name is the backend's short name ("nakama").
	Name() string
	// EnsureAccount returns the account for a derived custom id, creating it if it does not exist.
	// It is safe to call for every run: the custom id is derived from the source record, so the
	// second call returns the account the first one made instead of a second one.
	EnsureAccount(ctx context.Context, customID string) (string, error)
	// Apply writes the documents whose meaning changed.
	Apply(ctx context.Context, req Request) (Result, error)
	// Preview answers what Apply would write, and writes nothing.
	Preview(ctx context.Context, req Request) (Result, error)
	// Inspect reads back what the target holds for a player. The collections are the caller's,
	// because the target does not know which collection a title decided to import into.
	Inspect(ctx context.Context, playerID string, collections []string) (Inspect, error)
	// Status reports what the target has recorded.
	Status(ctx context.Context) (Status, error)
}

// Nakama is a Target backed by a Nakama server and the migration kit's runtime module in it.
type Nakama struct {
	// BaseURL is the Nakama client API root, e.g. http://127.0.0.1:7350.
	BaseURL string
	// HTTPKey is `runtime.http_key`: the runtime RPCs are reached with it, and the server key is
	// what reaches the client API. Using the server key for an RPC returns `HTTP key invalid`, and
	// using the http key to authenticate returns a different, less obvious error.
	HTTPKey string
	// ServerKey is Nakama's socket.server_key, which authenticates the client API.
	ServerKey string
	// Client is optional; the default has a timeout, because an importer that hangs on one player
	// looks exactly like an importer that is working.
	Client *http.Client
}

// NewNakama returns a target for a Nakama server with the usual keys.
func NewNakama(baseURL, httpKey, serverKey string) *Nakama {
	return &Nakama{
		BaseURL:   strings.TrimRight(baseURL, "/"),
		HTTPKey:   httpKey,
		ServerKey: serverKey,
		Client:    &http.Client{Timeout: 60 * time.Second},
	}
}

func (n *Nakama) Name() string { return "nakama" }

func (n *Nakama) client() *http.Client {
	if n.Client != nil {
		return n.Client
	}
	return &http.Client{Timeout: 60 * time.Second}
}

// EnsureAccount authenticates the custom id, creating the account when it is new.
//
// The response is a session token, not a user id: Nakama puts the account id in the token's `uid`
// claim. Reading it from there is what this does, and it is why the importer never needs a table of
// "which account is which player" - the derivation is the table.
func (n *Nakama) EnsureAccount(ctx context.Context, customID string) (string, error) {
	body, err := json.Marshal(map[string]any{"id": customID, "create": true})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.BaseURL+"/v2/account/authenticate/custom", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(n.ServerKey+":")))
	raw, err := n.do(req)
	if err != nil {
		return "", fmt.Errorf("authenticate %s: %w", customID, err)
	}
	var session struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(raw, &session); err != nil {
		return "", fmt.Errorf("authenticate %s: the response is not a session: %w", customID, err)
	}
	uid, err := userIDFromToken(session.Token)
	if err != nil {
		return "", fmt.Errorf("authenticate %s: %w", customID, err)
	}
	return uid, nil
}

// userIDFromToken reads the `uid` claim of a Nakama session token.
func userIDFromToken(token string) (string, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", fmt.Errorf("the session token is not a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", fmt.Errorf("the session token's payload is not base64: %w", err)
	}
	var claims struct {
		UID string `json:"uid"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", fmt.Errorf("the session token's payload is not JSON: %w", err)
	}
	if claims.UID == "" {
		return "", fmt.Errorf("the session token has no uid claim")
	}
	return claims.UID, nil
}

func (n *Nakama) Apply(ctx context.Context, req Request) (Result, error) {
	return n.rpc(ctx, "migration.apply", req)
}

func (n *Nakama) Preview(ctx context.Context, req Request) (Result, error) {
	return n.rpc(ctx, "migration.plan", req)
}

func (n *Nakama) Inspect(ctx context.Context, playerID string, collections []string) (Inspect, error) {
	body, err := json.Marshal(map[string]any{"player_id": playerID, "collections": collections})
	if err != nil {
		return Inspect{}, err
	}
	raw, err := n.rpcRaw(ctx, "migration.inspect", string(body))
	if err != nil {
		return Inspect{}, err
	}
	var out Inspect
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return Inspect{}, fmt.Errorf("migration.inspect: %w", err)
	}
	return out, nil
}

func (n *Nakama) Status(ctx context.Context) (Status, error) {
	raw, err := n.rpcRaw(ctx, "migration.status", "")
	if err != nil {
		return Status{}, err
	}
	var out Status
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return Status{}, fmt.Errorf("migration.status: %w", err)
	}
	return out, nil
}

func (n *Nakama) rpc(ctx context.Context, name string, req Request) (Result, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return Result{}, err
	}
	raw, err := n.rpcRaw(ctx, name, string(body))
	if err != nil {
		return Result{}, err
	}
	var out Result
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return Result{}, fmt.Errorf("%s: %w", name, err)
	}
	return out, nil
}

// rpcRaw calls a runtime RPC with the http key and returns the payload with the envelope removed.
func (n *Nakama) rpcRaw(ctx context.Context, name, payload string) (string, error) {
	u := fmt.Sprintf("%s/v2/rpc/%s?unwrap&http_key=%s", n.BaseURL, url.PathEscape(name), url.QueryEscape(n.HTTPKey))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	raw, err := n.do(req)
	if err != nil {
		return "", fmt.Errorf("%s: %w", name, err)
	}
	return string(raw), nil
}

// do sends a request and turns a non-200 into an error that carries what the server said. Nakama
// answers 200 with an error code inside the envelope for many failures, so the body is checked even
// on success: an RPC that refused the payload would otherwise look like an empty result.
func (n *Nakama) do(req *http.Request) ([]byte, error) {
	resp, err := n.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, preview(raw))
	}
	if code, message, ok := rpcError(raw); ok {
		return nil, fmt.Errorf("code %d: %s", code, message)
	}
	return raw, nil
}

// rpcError reads Nakama's error envelope, which an RPC returns with HTTP 200.
func rpcError(raw []byte) (int, string, bool) {
	var envelope struct {
		Code    int             `json:"code"`
		Message string          `json:"message"`
		Error   json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return 0, "", false
	}
	if envelope.Code == 0 {
		return 0, "", false
	}
	message := envelope.Message
	if message == "" && len(envelope.Error) > 0 {
		var inner struct {
			Message string `json:"Message"`
		}
		if json.Unmarshal(envelope.Error, &inner) == nil && inner.Message != "" {
			message = inner.Message
		}
	}
	if message == "" {
		message = strings.TrimSpace(string(envelope.Error))
	}
	return envelope.Code, message, true
}

func preview(raw []byte) string {
	const limit = 200
	s := strings.TrimSpace(string(raw))
	if len(s) > limit {
		return s[:limit] + "..."
	}
	return s
}

// Documents turns a mapped record into the documents the runtime module writes.
//
// A record is per player and the entities are its documents, so a run that finds one currency
// changed writes one document. Which entities a client may read is a per-title decision; the
// defaults here are the ones a game reads on its own screens (profile, progress, wallet, stats,
// inventory) and user data is left owner-only, because that is where a title keeps notes a player
// was never meant to read.
func Documents(rec *mapping.Record) ([]Document, error) {
	out := make([]Document, 0, len(rec.Entities))
	for _, e := range rec.Entities {
		value, err := json.Marshal(e.Value)
		if err != nil {
			return nil, fmt.Errorf("entity %s: %w", e.Name, err)
		}
		out = append(out, Document{
			Collection: rec.Collection,
			Key:        e.Key,
			Checksum:   e.Checksum,
			Value:      string(value),
			Public:     publicByDefault(e.Name),
		})
	}
	return out, nil
}

// publicByDefault says whether a client may read an entity.
func publicByDefault(entity string) bool {
	switch entity {
	case mapping.EntityUserData:
		return false
	default:
		return true
	}
}
