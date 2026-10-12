package faketenant_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/lasttoss/nakama-migration-kit/internal/faketenant"
)

func post(t *testing.T, url, body string) (int, []byte) {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, raw
}

// The listing is what an import walks, so it has to be complete and ordered.
func TestListingReturnsEveryPlayerOnce(t *testing.T) {
	s := faketenant.New(faketenant.Options{Players: 250, PageSize: 40})
	defer s.Close()

	seen := map[string]int{}
	token, pages := "", 0
	for {
		status, raw := post(t, s.BaseURL()+"/Admin/GetPlayersInSegment",
			`{"SegmentId":"migration","MaxBatchSize":40,"ContinuationToken":"`+token+`"}`)
		if status != 200 {
			t.Fatalf("page %d: HTTP %d: %s", pages, status, raw)
		}
		var resp faketenant.GetPlayersInSegmentResponse
		if err := json.Unmarshal(raw, &resp); err != nil {
			t.Fatal(err)
		}
		pages++
		for _, p := range resp.Data.ProfilesInSegment {
			seen[p.PlayerID]++
		}
		if resp.Data.ContinuationToken == "" {
			break
		}
		token = resp.Data.ContinuationToken
		if pages > 100 {
			t.Fatal("the listing does not end")
		}
	}

	if len(seen) != 250 {
		t.Errorf("listed %d players, want 250", len(seen))
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("%s appeared %d times", id, n)
		}
	}
	if got := s.Stats().PlayersSent; got != 250 {
		t.Errorf("server says it sent %d players, want 250", got)
	}
}

func TestPageSizeIsRespected(t *testing.T) {
	s := faketenant.New(faketenant.Options{Players: 20, PageSize: 7})
	defer s.Close()

	_, raw := post(t, s.BaseURL()+"/Admin/GetPlayersInSegment", `{"MaxBatchSize":7}`)
	var resp faketenant.GetPlayersInSegmentResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatal(err)
	}
	if got := len(resp.Data.ProfilesInSegment); got != 7 {
		t.Errorf("first page held %d players, want 7", got)
	}
	if resp.Data.ContinuationToken != "7" {
		t.Errorf("continuation token = %q, want 7", resp.Data.ContinuationToken)
	}
}

// A 429 has to arrive with a Retry-After, or an importer cannot tell "slow down" from "broken".
func TestRateLimitIsInjectedWithRetryAfter(t *testing.T) {
	s := faketenant.New(faketenant.Options{
		Players: 5,
		Faults:  faketenant.Faults{RateLimitEvery: 2, RetryAfter: "3"},
	})
	defer s.Close()

	var got429 int
	for i := 0; i < 4; i++ {
		resp, err := http.Post(s.BaseURL()+"/Admin/GetPlayersInSegment", "application/json", strings.NewReader(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode == http.StatusTooManyRequests {
			got429++
			if after := resp.Header.Get("Retry-After"); after != "3" {
				t.Errorf("Retry-After = %q, want 3", after)
			}
			raw, _ := io.ReadAll(resp.Body)
			if !strings.Contains(string(raw), "RateLimitExceeded") {
				t.Errorf("the 429 body does not name the fault: %s", raw)
			}
		}
		resp.Body.Close()
	}
	if got429 != 2 {
		t.Errorf("got %d rate limits in 4 requests, want 2", got429)
	}
	if s.Stats().RateLimited != 2 {
		t.Errorf("stats say %d rate limits", s.Stats().RateLimited)
	}
}

func TestServerErrorsAreInjected(t *testing.T) {
	s := faketenant.New(faketenant.Options{Players: 5, Faults: faketenant.Faults{FailEvery: 3}})
	defer s.Close()

	status, _ := post(t, s.BaseURL()+"/Admin/GetPlayersInSegment", `{}`)
	if status != 200 {
		t.Fatalf("first request: HTTP %d", status)
	}
	post(t, s.BaseURL()+"/Admin/GetPlayersInSegment", `{}`)
	status, raw := post(t, s.BaseURL()+"/Admin/GetPlayersInSegment", `{}`)
	if status != http.StatusInternalServerError {
		t.Fatalf("third request: HTTP %d, want 500", status)
	}
	if !strings.Contains(string(raw), "InternalServerError") {
		t.Errorf("the 500 body does not name the fault: %s", raw)
	}
}

// The failure mode worth having: a page comes back short, the token says the listing
// continued, and nothing downstream can tell except a count.
func TestADroppedPageIsInvisibleExceptByCounting(t *testing.T) {
	s := faketenant.New(faketenant.Options{Players: 100, PageSize: 20, Faults: faketenant.Faults{DropPages: []int{2}}})
	defer s.Close()

	seen := map[string]bool{}
	token := ""
	for {
		_, raw := post(t, s.BaseURL()+"/Admin/GetPlayersInSegment",
			`{"MaxBatchSize":20,"ContinuationToken":"`+token+`"}`)
		var resp faketenant.GetPlayersInSegmentResponse
		if err := json.Unmarshal(raw, &resp); err != nil {
			t.Fatal(err)
		}
		for _, p := range resp.Data.ProfilesInSegment {
			seen[p.PlayerID] = true
		}
		if resp.Data.ContinuationToken == "" {
			break
		}
		token = resp.Data.ContinuationToken
	}

	if len(seen) == 100 {
		t.Fatal("the dropped page was not dropped")
	}
	// Page 2 should have carried players 21..40. The fake sends the first half and drops the
	// second, while the token still advances over all twenty: players 31..40 are gone and
	// nothing in the listing says so.
	for i := 21; i <= 30; i++ {
		if id := fmt.Sprintf("PF%08d", i); !seen[id] {
			t.Errorf("%s is missing, but only the second half of page 2 was dropped", id)
		}
	}
	for i := 31; i <= 40; i++ {
		if id := fmt.Sprintf("PF%08d", i); seen[id] {
			t.Errorf("%s arrived, but page 2 was supposed to drop its second half", id)
		}
	}
	if !seen["PF00000041"] {
		t.Error("the page after the dropped one did not arrive: the token is wrong")
	}
	if got := len(seen); got != 90 {
		t.Errorf("walked %d players, want 90 (half of one page lost)", got)
	}
}

func TestUnknownPlayerIsAFailureWithAName(t *testing.T) {
	s := faketenant.New(faketenant.Options{Players: 3})
	defer s.Close()

	status, raw := post(t, s.BaseURL()+"/Admin/GetPlayerProfile", `{"PlayFabId":"NOPE"}`)
	if status != http.StatusNotFound {
		t.Fatalf("HTTP %d, want 404", status)
	}
	if !strings.Contains(string(raw), "PlayerNotFound") {
		t.Errorf("body does not name the condition: %s", raw)
	}

	if status, _ := post(t, s.BaseURL()+"/Admin/GetPlayerProfile", `{}`); status != http.StatusBadRequest {
		t.Errorf("a request with no id: HTTP %d, want 400", status)
	}
}

func TestPlayerShapesAreDeterministic(t *testing.T) {
	s := faketenant.New(faketenant.Options{Players: 40, Seed: 7})
	defer s.Close()

	first := s.Tenant().Checksum()
	other := faketenant.New(faketenant.Options{Players: 40, Seed: 7})
	defer other.Close()
	if first != other.Tenant().Checksum() {
		t.Error("the same seed produced different tenants")
	}
	different := faketenant.New(faketenant.Options{Players: 40, Seed: 8})
	defer different.Close()
	if first == different.Tenant().Checksum() {
		t.Error("a different seed produced the same tenant")
	}
}

// The generator is deliberately lumpy: no inventory for some, no progress for others, empty
// display names, and names with characters a careless encoder escapes.
func TestGeneratedPlayersCoverTheAwkwardCases(t *testing.T) {
	tenant := faketenant.NewTenant(100, 1)
	var noInventory, noProgress, emptyName, escapedName int
	for _, p := range tenant.Players {
		if len(p.Inventory) == 0 {
			noInventory++
		}
		if p.Progress == nil {
			noProgress++
		}
		if p.DisplayName == "" {
			emptyName++
		}
		if strings.ContainsAny(p.DisplayName, "&<>\"") {
			escapedName++
		}
	}
	for name, got := range map[string]int{
		"players with no inventory":  noInventory,
		"players with no progress":   noProgress,
		"players with no name":       emptyName,
		"players with awkward names": escapedName,
	} {
		if got == 0 {
			t.Errorf("no %s in 100 generated players", name)
		}
	}
}
