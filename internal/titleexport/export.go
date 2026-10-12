// Package titleexport reads players from a title export instead of from the Admin API.
//
// Why this exists, in one number: an Admin API import costs about five requests per player, so a
// million players is a few days of wall clock against a rate limit this kit does not control. A
// title export is one file, and reading one file is minutes. Same Source interface, same mapping,
// same checksums: the difference is where the bytes come from.
//
// The format is one JSON object per line, and that is a deliberate contract rather than a guess
// about PlayFab's warehouse layout. A real cutover writes a small transform between the export
// and this reader (PlayFab's warehouse is not one clean file either), and the transform is the
// thing to review with the title's team: it can be diffed, and the reader below fails loudly on
// anything it cannot parse, naming the line.
//
// The reader builds an offset index once, so reading a player out of a million is a seek rather
// than a scan. The index costs roughly 60 bytes per player, which a migration machine has; a
// scan per player would cost the wall clock this package exists to save.
package titleexport

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/lasttoss/nakama-migration-kit/internal/source"
)

// Config configures the reader.
type Config struct {
	// Path is the export file: one player per line.
	Path string
	// PageSize is how many ids one page of the listing returns. Defaults to 1000.
	PageSize int
}

// Source reads a title export.
type Source struct {
	cfg Config

	once    sync.Once
	index   map[string]int64
	order   []string
	loadErr error
}

// New returns a reader for an export file. The file is opened on first use, so a bad path fails
// when the run starts reading rather than at flag parsing.
func New(cfg Config) *Source {
	if cfg.PageSize <= 0 {
		cfg.PageSize = 1000
	}
	return &Source{cfg: cfg}
}

// Name is part of every derived identity, exactly as for the API adapter: the same player id in
// an export and behind the API is the same player, so both use "playfab".
func (s *Source) Name() string { return "playfab" }

// PlayerIDs returns a page of the ids in the export, in file order.
func (s *Source) PlayerIDs(ctx context.Context, cursor string, limit int) ([]string, string, error) {
	if err := s.load(); err != nil {
		return nil, "", err
	}
	if limit <= 0 {
		limit = s.cfg.PageSize
	}
	offset := 0
	if cursor != "" {
		if _, err := fmt.Sscanf(cursor, "%d", &offset); err != nil || offset < 0 {
			return nil, "", fmt.Errorf("titleexport: bad cursor %q", cursor)
		}
	}
	if offset >= len(s.order) {
		return nil, "", nil
	}
	end := offset + limit
	if end > len(s.order) {
		end = len(s.order)
	}
	page := make([]string, end-offset)
	copy(page, s.order[offset:end])

	next := ""
	if end < len(s.order) {
		next = fmt.Sprintf("%d", end)
	}
	return page, next, nil
}

// Player reads one player by seeking to its line.
func (s *Source) Player(ctx context.Context, id string) (*source.Player, error) {
	if err := s.load(); err != nil {
		return nil, err
	}
	offset, ok := s.index[id]
	if !ok {
		return nil, fmt.Errorf("titleexport: %s: %w", id, source.ErrNotFound)
	}
	f, err := os.Open(s.cfg.Path)
	if err != nil {
		return nil, fmt.Errorf("titleexport: %w", err)
	}
	defer f.Close()

	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, fmt.Errorf("titleexport: seek %s: %w", id, err)
	}
	line, err := bufio.NewReader(f).ReadBytes('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("titleexport: read %s: %w", id, err)
	}
	p, err := decode(line)
	if err != nil {
		return nil, fmt.Errorf("titleexport: %s: %w", id, err)
	}
	if p.ID != id {
		// The index and the file disagree, which means the file changed under a running import.
		// A migration that silently reads the wrong player is worse than one that stops.
		return nil, fmt.Errorf("titleexport: index says %s is at offset %d, but that line holds %s (was the export rewritten?)", id, offset, p.ID)
	}
	if err := source.Validate(p); err != nil {
		return nil, err
	}
	return p, nil
}

// Record is the export's line format. It is the kit's own contract, not PlayFab's: see the
// package comment.
type Record struct {
	ID          string            `json:"id"`
	DisplayName string            `json:"display_name"`
	CreatedAt   string            `json:"created_at,omitempty"`
	UpdatedAt   string            `json:"updated_at,omitempty"`
	UserData    map[string]string `json:"user_data,omitempty"`
	Currency    map[string]int64  `json:"currency,omitempty"`
	Statistics  map[string]int64  `json:"statistics,omitempty"`
	Inventory   []struct {
		InstanceID string `json:"instance_id,omitempty"`
		CatalogID  string `json:"catalog_id"`
		Count      int64  `json:"count"`
	} `json:"inventory,omitempty"`
}

// load indexes the file once.
func (s *Source) load() error {
	s.once.Do(func() {
		f, err := os.Open(s.cfg.Path)
		if err != nil {
			s.loadErr = fmt.Errorf("titleexport: %w", err)
			return
		}
		defer f.Close()

		s.index = map[string]int64{}
		reader := bufio.NewReaderSize(f, 1<<20)
		var offset int64
		for line := 1; ; line++ {
			raw, err := reader.ReadBytes('\n')
			if len(raw) > 0 {
				if isBlank(raw) {
					offset += int64(len(raw))
					if errors.Is(err, io.EOF) {
						return
					}
					continue
				}
				p, derr := decode(raw)
				if derr != nil {
					s.loadErr = fmt.Errorf("titleexport: %s line %d: %w", s.cfg.Path, line, derr)
					return
				}
				if _, dup := s.index[p.ID]; dup {
					s.loadErr = fmt.Errorf("titleexport: %s line %d: player %s appears twice; an export with duplicates cannot be reconciled", s.cfg.Path, line, p.ID)
					return
				}
				s.index[p.ID] = offset
				s.order = append(s.order, p.ID)
			}
			offset += int64(len(raw))
			if errors.Is(err, io.EOF) {
				return
			}
			if err != nil {
				s.loadErr = fmt.Errorf("titleexport: read %s: %w", s.cfg.Path, err)
				return
			}
		}
	})
	return s.loadErr
}

func isBlank(raw []byte) bool {
	for _, b := range raw {
		if b != ' ' && b != '\t' && b != '\r' && b != '\n' {
			return false
		}
	}
	return true
}

func decode(raw []byte) (*source.Player, error) {
	var rec Record
	dec := json.NewDecoder(newReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&rec); err != nil {
		return nil, fmt.Errorf("not a valid export line: %w", err)
	}
	p := &source.Player{
		ID:          rec.ID,
		DisplayName: rec.DisplayName,
		UserData:    rec.UserData,
		Currency:    rec.Currency,
		Statistics:  rec.Statistics,
		CreatedAt:   parseTime(rec.CreatedAt),
		UpdatedAt:   parseTime(rec.UpdatedAt),
	}
	for _, it := range rec.Inventory {
		p.Inventory = append(p.Inventory, source.Item{InstanceID: it.InstanceID, CatalogID: it.CatalogID, Count: it.Count})
	}
	return p, nil
}
