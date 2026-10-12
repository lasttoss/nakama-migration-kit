// Package identity derives a Nakama custom id from a legacy backend's own player id.
//
// This is the decision the rest of the migration rests on. Nakama's "custom" authentication is
// keyed by an opaque string: authenticating the same custom id twice returns the same account,
// so an importer that derives the string from the source record cannot create a second account
// on a re-run, and does not need to remember what it did last time. A counter or a random id
// would put "did we already import this player" into the importer's state, and the answer would
// be wrong the first time that state is lost.
//
// Namespace is frozen. UUIDv5 is namespace + name, so changing the namespace changes every
// custom id, and that does not merge accounts: it creates a second account for every player and
// leaves the imported data on the first one. The namespace below is derived, once, from
// "nakama-migration-kit.lasttoss.dev" under the DNS namespace, and TestNamespaceIsTheOneWeChose
// pins both the derivation and three concrete ids. See docs/design-notes.md.
package identity

import (
	"crypto/sha1"
	"fmt"
)

// namespaceLiteral is the frozen namespace. Do not change it: see the package comment.
const namespaceLiteral = "4c94c380-c321-51e6-ae84-b514bf3d2150"

// dnsNamespace is RFC 4122's DNS namespace, used only to derive namespaceLiteral once.
const dnsNamespace = "6ba7b810-9dad-11d1-80b4-00c04fd430c8"

// Namespace is the UUIDv5 namespace every custom id in this kit is derived under.
var Namespace = mustParse(namespaceLiteral)

// CustomID returns the Nakama custom id for a player of the given legacy backend.
//
// The source name is part of the name so that the same id in two backends (PlayFab and
// SmartFox both like short strings) does not collide into one account.
func CustomID(source, sourceID string) string {
	if source == "" {
		// The caller has a bug; make it visible rather than silently deriving an id that
		// every empty source shares.
		panic("identity: CustomID called with an empty source")
	}
	if sourceID == "" {
		panic("identity: CustomID called with an empty source id")
	}
	return format(uuidV5(Namespace, source+":"+sourceID))
}

// uuidV5 is RFC 4122 version 5: SHA-1 over the namespace bytes and the name, with the version
// and variant bits overwritten.
func uuidV5(namespace [16]byte, name string) [16]byte {
	h := sha1.New()
	h.Write(namespace[:])
	h.Write([]byte(name))
	sum := h.Sum(nil)

	var out [16]byte
	copy(out[:], sum[:16])
	out[6] = (out[6] & 0x0f) | 0x50 // version 5
	out[8] = (out[8] & 0x3f) | 0x80 // RFC 4122 variant
	return out
}

// NamespaceString returns the frozen namespace in its canonical form, for the test that pins
// it and for anything that has to record which namespace produced a given import.
func NamespaceString() string { return format(Namespace) }

func format(u [16]byte) string {
	return fmt.Sprintf("%x-%x-%x-%x-%x", u[0:4], u[4:6], u[6:8], u[8:10], u[10:16])
}

func mustParse(s string) [16]byte {
	var out [16]byte
	clean := make([]byte, 0, 32)
	for i := 0; i < len(s); i++ {
		if s[i] == '-' {
			continue
		}
		clean = append(clean, s[i])
	}
	if len(clean) != 32 {
		panic("identity: namespace literal is not a UUID: " + s)
	}
	for i := 0; i < 16; i++ {
		var b byte
		for j := 0; j < 2; j++ {
			c := clean[i*2+j]
			switch {
			case c >= '0' && c <= '9':
				b = b<<4 | (c - '0')
			case c >= 'a' && c <= 'f':
				b = b<<4 | (c - 'a' + 10)
			default:
				panic("identity: bad hex in namespace literal: " + s)
			}
		}
		out[i] = b
	}
	return out
}
