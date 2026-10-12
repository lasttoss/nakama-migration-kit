package target_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/lasttoss/nakama-migration-kit/internal/mapping"
	"github.com/lasttoss/nakama-migration-kit/internal/source"
	"github.com/lasttoss/nakama-migration-kit/internal/target"
)

// session makes the token Nakama would return: the account id lives in the `uid` claim, and nowhere
// else in the response, which is the whole reason EnsureAccount decodes it.
func session(t *testing.T, uid string) string {
	t.Helper()
	claims, err := json.Marshal(map[string]string{"uid": uid, "usn": "player"})
	if err != nil {
		t.Fatal(err)
	}
	return "header." + base64.RawURLEncoding.EncodeToString(claims) + ".signature"
}

func TestEnsureAccountReadsTheUserIDFromTheToken(t *testing.T) {
	var gotBody map[string]any
	var gotAuth, gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		fmt.Fprintf(w, `{"token":%q,"refresh_token":"x"}`, session(t, "3346de5c-56b4-4b3e-ba11-901ce60f70df"))
	}))
	defer server.Close()

	uid, err := target.NewNakama(server.URL, "defaulthttpkey", "defaultkey").EnsureAccount(context.Background(), "custom-1")
	if err != nil {
		t.Fatal(err)
	}
	if uid != "3346de5c-56b4-4b3e-ba11-901ce60f70df" {
		t.Errorf("uid = %q", uid)
	}
	if gotPath != "/v2/account/authenticate/custom" {
		t.Errorf("path = %q", gotPath)
	}
	// Nakama's field is `id`. Sending `custom_id` answers "Custom ID is required." and reads like a
	// missing field rather than a wrong name.
	if gotBody["id"] != "custom-1" || gotBody["create"] != true {
		t.Errorf("body = %v", gotBody)
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("defaultkey:"))
	if gotAuth != want {
		t.Errorf("authorization = %q, want %q", gotAuth, want)
	}
}

func TestApplyUsesTheHTTPKeyAndUnwrapsThePayload(t *testing.T) {
	var gotQuery url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		fmt.Fprint(w, `{"written":["wallet/gold"],"skipped":["profile/level"]}`)
	}))
	defer server.Close()

	got, err := target.NewNakama(server.URL, "defaulthttpkey", "defaultkey").Apply(context.Background(),
		target.Request{PlayerID: "u1", SourceID: "PF-1", Documents: []target.Document{{Collection: "wallet", Key: "gold", Checksum: "c", Value: `{"gold":600}`}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Written) != 1 || got.Written[0] != "wallet/gold" || len(got.Skipped) != 1 {
		t.Errorf("result = %+v", got)
	}
	if gotQuery.Get("http_key") != "defaulthttpkey" || gotQuery.Has("unwrap") == false {
		t.Errorf("query = %v", gotQuery)
	}
}

// Nakama answers HTTP 200 with an error code inside the envelope. A client that only reads the status
// line reports a refusal as an empty success, which is how an import writes nothing and says it was
// fine.
func TestAnErrorInsideA200IsStillAnError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"code":3,"message":"player_id is required"}`)
	}))
	defer server.Close()

	_, err := target.NewNakama(server.URL, "k", "s").Apply(context.Background(), target.Request{})
	if err == nil {
		t.Fatal("an error envelope was read as a result")
	}
	if !strings.Contains(err.Error(), "player_id is required") {
		t.Errorf("error does not carry what the server said: %v", err)
	}
}

// A player's own data is not a client's to read; everything a game shows on a screen is.
func TestUserDataIsPrivateAndTheRestIsNot(t *testing.T) {
	p := &source.Player{ID: "PF-1", DisplayName: "player", UserData: map[string]string{"note": "private"}}
	rec, err := mapping.New("migration", p, "playfab")
	if err != nil {
		t.Fatal(err)
	}
	docs, err := target.Documents(rec)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) == 0 {
		t.Fatal("a player with user data mapped to no documents")
	}
	for _, d := range docs {
		switch d.Collection + "/" + d.Key {
		case mapping.DefaultCollection + "/" + mapping.EntityUserData:
			if d.Public {
				t.Error("user data was made public")
			}
		default:
			if !d.Public {
				t.Errorf("%s/%s is not public", d.Collection, d.Key)
			}
		}
	}
}
