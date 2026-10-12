// Package reconcile compares what a source says a player has against what the target holds.
//
// It is the answer to the question a studio asks after a cutover: "is it all there?". The comparison
// is over checksums, not over values, and that is not a shortcut - Nakama re-serialises what it
// stores, so the same document that was written as `{"gold": 600}` comes back with a space in it. A
// reconciler that compared bytes would report every player as changed and be believed by nobody.
//
// The findings are deliberately few. "Changed" without saying what changed is a number; a finding
// that names the entity, and shows both sides when they disagree, is something an operator can act on.
package reconcile

import (
	"fmt"
	"sort"

	"github.com/lasttoss/nakama-migration-kit/internal/mapping"
	"github.com/lasttoss/nakama-migration-kit/internal/target"
)

// Kind is how a document differs.
type Kind string

const (
	// Missing: the source has it and the target does not. This is lost data.
	Missing Kind = "missing"
	// Changed: both have it, and they disagree. This is data that moved and changed meaning.
	Changed Kind = "changed"
	// Extra: the target has it and the source does not. This is not a migration failure; it is what a
	// game writes on its own after a player logs in, and a reconciler that called it a mismatch would
	// cry wolf on every run after the first.
	Extra Kind = "extra"
)

// Finding is one document that does not match.
type Finding struct {
	Kind       Kind   `json:"kind"`
	Entity     string `json:"entity"`
	Key        string `json:"key"`
	Expected   string `json:"expected_checksum,omitempty"`
	Actual     string `json:"actual_checksum,omitempty"`
	ExpectedJS string `json:"expected_value,omitempty"`
	ActualJS   string `json:"actual_value,omitempty"`
}

func (f Finding) String() string {
	switch f.Kind {
	case Changed:
		return fmt.Sprintf("%-8s %s/%s: source %s, target %s", f.Kind, f.Entity, f.Key, short(f.Expected), short(f.Actual))
	case Missing:
		return fmt.Sprintf("%-8s %s/%s: in the source, not in the target (source %s)", f.Kind, f.Entity, f.Key, short(f.Expected))
	default:
		return fmt.Sprintf("%-8s %s/%s: in the target, not in the source (target %s)", f.Kind, f.Entity, f.Key, short(f.Actual))
	}
}

// short is enough of a checksum to be read out loud and to be searched for.
func short(sum string) string {
	if sum == "" {
		return "no checksum"
	}
	if len(sum) > 12 {
		return sum[:12]
	}
	return sum
}

// Result is one player's comparison.
type Result struct {
	Expected int       `json:"expected_documents"`
	Actual   int       `json:"actual_documents"`
	Findings []Finding `json:"findings,omitempty"`
}

// Matches reports whether the player is accounted for. "Extra" does not make a player mismatch: it
// is data the game wrote, not data the migration lost.
func (r Result) Matches() bool {
	for _, f := range r.Findings {
		if f.Kind != Extra {
			return false
		}
	}
	return true
}

// Compare checks a mapped record against what the target holds.
//
// It takes documents rather than a live client so the rule can be tested without one, and so the
// caller decides what the target is: today that is Nakama, and the comparison does not care.
func Compare(rec *mapping.Record, docs []target.Document) Result {
	actual := make(map[string]target.Document, len(docs))
	for _, d := range docs {
		actual[d.Key] = d
	}

	res := Result{Expected: len(rec.Entities), Actual: len(docs)}
	seen := make(map[string]bool, len(rec.Entities))
	for _, e := range rec.Entities {
		seen[e.Key] = true
		d, ok := actual[e.Key]
		if !ok {
			res.Findings = append(res.Findings, Finding{Kind: Missing, Entity: e.Name, Key: e.Key, Expected: e.Checksum})
			continue
		}
		if d.Checksum == e.Checksum {
			continue
		}
		res.Findings = append(res.Findings, Finding{
			Kind: Changed, Entity: e.Name, Key: e.Key,
			Expected: e.Checksum, Actual: d.Checksum,
			ExpectedJS: jsonOf(e.Value), ActualJS: d.Value,
		})
	}
	for key, d := range actual {
		if seen[key] {
			continue
		}
		res.Findings = append(res.Findings, Finding{
			Kind: Extra, Entity: d.Collection, Key: key, Actual: d.Checksum, ActualJS: d.Value,
		})
	}
	sort.Slice(res.Findings, func(i, j int) bool {
		if res.Findings[i].Kind != res.Findings[j].Kind {
			return res.Findings[i].Kind < res.Findings[j].Kind
		}
		return res.Findings[i].Key < res.Findings[j].Key
	})
	return res
}

func jsonOf(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case nil:
		return ""
	default:
		return fmt.Sprintf("%v", t)
	}
}
