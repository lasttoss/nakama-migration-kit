package identity_test

import (
	"testing"

	"github.com/lasttoss/nakama-migration-kit/internal/identity"
)

// The freeze test. If this fails, every player imported so far is about to get a second
// Nakama account: the custom id changed, authentication created a new account, and the data
// that was on the old one is not where the new one looks.
func TestNamespaceIsTheOneWeChose(t *testing.T) {
	const wantNamespace = "4c94c380-c321-51e6-ae84-b514bf3d2150"
	if got := identity.NamespaceString(); got != wantNamespace {
		t.Fatalf("namespace changed:\n  have %s\n  want %s", got, wantNamespace)
	}

	golden := map[string]string{
		"playfab:ABC123":  "0d761980-c8fc-5cb0-9004-b54dd0d05c70",
		"playfab:1F2E3D":  "c2e7af18-edef-58fe-97b7-3a0f09e0f0a8",
		"smartfox:u-42":   "7502775c-85a4-58cd-ae19-4ec85124810b",
		"gamesparks:5b2b": "318341fe-533a-5a2f-a8c0-b569b81ee7e5",
	}
	for key, want := range golden {
		source, id := splitOnce(key, ':')
		if got := identity.CustomID(source, id); got != want {
			t.Errorf("CustomID(%q, %q) = %s, want %s", source, id, got, want)
		}
	}
}

func TestCustomIDIsDeterministic(t *testing.T) {
	first := identity.CustomID("playfab", "ABC123")
	for i := 0; i < 100; i++ {
		if got := identity.CustomID("playfab", "ABC123"); got != first {
			t.Fatalf("run %d produced a different id: %s vs %s", i, got, first)
		}
	}
}

// The same id in two different backends has to be two different players.
func TestSourceIsPartOfTheIdentity(t *testing.T) {
	if identity.CustomID("playfab", "42") == identity.CustomID("smartfox", "42") {
		t.Error("the same id from two backends mapped to one account")
	}
}

func TestDistinctIDsStayDistinct(t *testing.T) {
	seen := map[string]string{}
	for _, source := range []string{"playfab", "smartfox", "gamesparks"} {
		for _, id := range []string{"1", "2", "10", "42", "ABC123", "abc123", "00000000"} {
			custom := identity.CustomID(source, id)
			if prev, ok := seen[custom]; ok {
				t.Errorf("%s:%s collided with %s", source, id, prev)
			}
			seen[custom] = source + ":" + id
		}
	}
}

func TestCustomIDIsAUUIDv5(t *testing.T) {
	got := identity.CustomID("playfab", "ABC123")
	if len(got) != 36 || got[14] != '5' {
		t.Errorf("%s is not a version 5 UUID", got)
	}
	if got[19] != '8' && got[19] != '9' && got[19] != 'a' && got[19] != 'b' {
		t.Errorf("%s does not carry the RFC 4122 variant bits", got)
	}
}

// A caller with an empty source id has a bug, and deriving an id that every empty id shares
// would attach every one of those players to one account.
func TestEmptyInputIsABugNotAnID(t *testing.T) {
	for _, tc := range []struct{ source, id string }{{"", "1"}, {"playfab", ""}, {"", ""}} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("CustomID(%q, %q) derived an id instead of refusing", tc.source, tc.id)
				}
			}()
			identity.CustomID(tc.source, tc.id)
		}()
	}
}

func splitOnce(s string, sep byte) (string, string) {
	for i := 0; i < len(s); i++ {
		if s[i] == sep {
			return s[:i], s[i+1:]
		}
	}
	return s, ""
}
