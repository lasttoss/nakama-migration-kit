package canon_test

import (
	"encoding/json"
	"testing"

	"github.com/lasttoss/nakama-migration-kit/internal/canon"
)

func TestChecksumIgnoresKeyOrderAndWhitespace(t *testing.T) {
	a := map[string]any{"level": 12, "energy": 30, "name": "An"}
	var b any
	if err := json.Unmarshal([]byte("{\n  \"name\":   \"An\",\n  \"energy\": 30,\n  \"level\": 12\n}"), &b); err != nil {
		t.Fatal(err)
	}

	ca, err := canon.Checksum(a)
	if err != nil {
		t.Fatal(err)
	}
	cb, err := canon.Checksum(b)
	if err != nil {
		t.Fatal(err)
	}
	if ca != cb {
		t.Errorf("the same record hashed differently:\n  %s\n  %s", ca, cb)
	}
}

func TestChecksumSeesARealChange(t *testing.T) {
	a := map[string]any{"currency": map[string]any{"gold": 100}}
	b := map[string]any{"currency": map[string]any{"gold": 101}}

	ca, _ := canon.Checksum(a)
	cb, _ := canon.Checksum(b)
	if ca == cb {
		t.Error("a currency change hashed the same as the value before it")
	}
}

// The one that bit liveops-console: jsonb re-renders numbers, so 100 stored as jsonb can come
// back as 100.0 and a byte comparison calls it a change.
func TestNormalizeSurvivesAJSONBRoundTrip(t *testing.T) {
	written := []byte(`{"gold":100,"ratio":0.5,"name":"An & Binh"}`)
	afterJsonb := []byte("{\n  \"ratio\": 0.5,\n  \"name\": \"An & Binh\",\n  \"gold\": 100\n}")

	a, err := canon.ChecksumBytes(written)
	if err != nil {
		t.Fatal(err)
	}
	b, err := canon.ChecksumBytes(afterJsonb)
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Errorf("a jsonb round trip looked like a change:\n  before %s\n  after  %s", a, b)
	}
}

// encoding/json escapes HTML by default, so the same name hashes two ways depending on who
// encoded it. The canonical form has to be the unescaped one.
func TestChecksumDoesNotDependOnHTMLEscaping(t *testing.T) {
	escaped := []byte(`{"name":"An \u0026 Binh"}`)
	plain := []byte(`{"name":"An & Binh"}`)

	a, err := canon.ChecksumBytes(escaped)
	if err != nil {
		t.Fatal(err)
	}
	b, err := canon.ChecksumBytes(plain)
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Errorf("the same name hashed two ways: %s vs %s", a, b)
	}
	if raw, err := canon.JSON(map[string]any{"name": "An & Binh"}); err != nil || string(raw) != `{"name":"An & Binh"}` {
		t.Errorf("JSON() escaped HTML: %s, %v", raw, err)
	}
}

// A balance too large for float64 has to survive: a float would round two different balances
// to the same number and the checksum would call them equal.
func TestChecksumKeepsLargeNumbersExact(t *testing.T) {
	a, err := canon.ChecksumBytes([]byte(`{"gold":9007199254740993}`))
	if err != nil {
		t.Fatal(err)
	}
	b, err := canon.ChecksumBytes([]byte(`{"gold":9007199254740992}`))
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Error("two balances that differ by one hashed the same: float64 rounding lost the difference")
	}
}

func TestEqual(t *testing.T) {
	same, err := canon.Equal(map[string]any{"a": 1}, map[string]any{"a": 1})
	if err != nil || !same {
		t.Errorf("Equal(same) = %v, %v", same, err)
	}
	diff, err := canon.Equal(map[string]any{"a": 1}, map[string]any{"a": 2})
	if err != nil || diff {
		t.Errorf("Equal(different) = %v, %v", diff, err)
	}
}

func TestChecksumRejectsWhatJSONCannotHold(t *testing.T) {
	if _, err := canon.Checksum(make(chan int)); err == nil {
		t.Error("a value that cannot be encoded returned a checksum")
	}
	if _, err := canon.ChecksumBytes([]byte("{not json")); err == nil {
		t.Error("malformed JSON returned a checksum")
	}
}
