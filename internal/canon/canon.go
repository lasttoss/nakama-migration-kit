// Package canon turns a value into bytes that do not depend on how it was built, and a
// checksum of those bytes.
//
// Two records that mean the same thing have to hash the same way. If they do not, a second
// import looks like a change and writes again, and the "zero writes on a re-run" claim stops
// being true. The two places this bites:
//
//   - a value that made a round trip through Postgres jsonb comes back with its keys in a
//     different order and its numbers re-rendered, so comparing the string that went in with
//     the string that came out reports a difference that is not there;
//   - encoding/json escapes HTML by default, so a display name containing "&" or "<" hashes
//     differently depending on which side of the wire it was encoded on.
//
// Everything compared across a run boundary goes through Checksum.
package canon

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// JSON renders v deterministically: map keys sorted (encoding/json does this), no HTML
// escaping, no trailing newline. A nil slice renders as null, which is deliberate: "absent"
// and "empty" are different states in the source data and the checksum should say so.
func JSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	// The default escapes <, > and & to \u003c, \u003e and \u0026. On the wire the same
	// value arrives unescaped when it comes from a database, so the escaped form is not a
	// canonical form.
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, fmt.Errorf("canon: encode: %w", err)
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// Normalize decodes raw JSON and re-renders it through JSON, so a value that has been
// through a database compares equal to the value that was written.
func Normalize(raw []byte) ([]byte, error) {
	var v any
	dec := json.NewDecoder(bytes.NewReader(raw))
	// Numbers keep their exact representation: decoding into float64 loses the difference
	// between 1 and 1.0, and it cannot represent a large currency balance exactly.
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("canon: decode: %w", err)
	}
	return JSON(v)
}

// Checksum returns "sha256:<hex>" of the canonical form of v.
func Checksum(v any) (string, error) {
	raw, err := JSON(v)
	if err != nil {
		return "", err
	}
	return ChecksumBytes(raw)
}

// ChecksumBytes returns "sha256:<hex>" of raw after normalizing it.
func ChecksumBytes(raw []byte) (string, error) {
	norm, err := Normalize(raw)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(norm)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// Equal reports whether two values have the same canonical form.
func Equal(a, b any) (bool, error) {
	ca, err := Checksum(a)
	if err != nil {
		return false, err
	}
	cb, err := Checksum(b)
	if err != nil {
		return false, err
	}
	return ca == cb, nil
}
