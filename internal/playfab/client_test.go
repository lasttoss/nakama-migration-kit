package playfab_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lasttoss/nakama-migration-kit/internal/faketenant"
	"github.com/lasttoss/nakama-migration-kit/internal/playfab"
	"github.com/lasttoss/nakama-migration-kit/internal/source"
)

func fast(cfg playfab.Config) playfab.Config {
	cfg.BaseBackoff = time.Microsecond
	cfg.MaxBackoff = time.Millisecond
	return cfg
}

func TestPlayerReadsEverythingTheKitImports(t *testing.T) {
	tenant := faketenant.New(faketenant.Options{Players: 40, Seed: 3})
	defer tenant.Close()

	c := playfab.New(fast(playfab.Config{BaseURL: tenant.BaseURL(), SecretKey: "test-secret"}))
	ctx := context.Background()

	want := tenant.Tenant().PlayerIndex()["PF00000007"]
	if want == nil {
		t.Fatal("the fake tenant is missing the player this test reads")
	}
	got, err := c.Player(ctx, "PF00000007")
	if err != nil {
		t.Fatalf("Player: %v", err)
	}

	if got.ID != want.ID || got.DisplayName != want.DisplayName {
		t.Errorf("identity = %q/%q, want %q/%q", got.ID, got.DisplayName, want.ID, want.DisplayName)
	}
	if len(got.Currency) != len(want.Currency) {
		t.Errorf("currency = %v, want %v", got.Currency, want.Currency)
	}
	for code, amount := range want.Currency {
		if got.Currency[code] != amount {
			t.Errorf("currency %s = %d, want %d", code, got.Currency[code], amount)
		}
	}
	if len(got.Inventory) != len(want.Inventory) {
		t.Errorf("inventory = %+v, want %+v", got.Inventory, want.Inventory)
	}
	if len(got.Statistics) != len(want.Statistics) {
		t.Errorf("statistics = %v, want %v", got.Statistics, want.Statistics)
	}
	if len(got.UserData) != len(want.UserData) {
		t.Errorf("user data = %v, want %v", got.UserData, want.UserData)
	}
	if !got.CreatedAt.Equal(want.CreatedAt) {
		t.Errorf("created = %s, want %s", got.CreatedAt, want.CreatedAt)
	}
	if err := source.Validate(got); err != nil {
		t.Errorf("the adapter returned a record the kit cannot import: %v", err)
	}
}

func TestListingWalksTheWholeSegment(t *testing.T) {
	tenant := faketenant.New(faketenant.Options{Players: 150, PageSize: 25})
	defer tenant.Close()

	c := playfab.New(fast(playfab.Config{BaseURL: tenant.BaseURL(), SecretKey: "s"}))
	seen := map[string]bool{}
	cursor, pages := "", 0
	for {
		ids, next, err := c.PlayerIDs(context.Background(), cursor, 25)
		if err != nil {
			t.Fatalf("page %d: %v", pages, err)
		}
		for _, id := range ids {
			if seen[id] {
				t.Errorf("%s arrived twice", id)
			}
			seen[id] = true
		}
		pages++
		if next == "" {
			break
		}
		cursor = next
		if pages > 50 {
			t.Fatal("the listing does not end")
		}
	}
	if len(seen) != 150 {
		t.Errorf("walked %d players, want 150", len(seen))
	}
}

// PlayFab rate limits a busy title, and the importer has to treat that as weather rather than as
// a failure: a 429 that stops a run stops it at an arbitrary point in a live title.
func TestRateLimitIsRetriedRatherThanFatal(t *testing.T) {
	tenant := faketenant.New(faketenant.Options{
		Players: 10,
		Faults:  faketenant.Faults{RateLimitEvery: 3, RetryAfter: "0"},
	})
	defer tenant.Close()

	c := playfab.New(fast(playfab.Config{BaseURL: tenant.BaseURL(), SecretKey: "s"}))
	if _, err := c.Player(context.Background(), "PF00000003"); err != nil {
		t.Fatalf("a rate limited read failed instead of retrying: %v", err)
	}
	if got := c.Stats().RateLimited; got == 0 {
		t.Error("the client did not notice it was rate limited")
	}
	if got := c.Stats().Retries; got == 0 {
		t.Error("the client did not retry")
	}
}

func TestServerErrorsAreRetried(t *testing.T) {
	tenant := faketenant.New(faketenant.Options{Players: 10, Faults: faketenant.Faults{FailEvery: 4}})
	defer tenant.Close()

	c := playfab.New(fast(playfab.Config{BaseURL: tenant.BaseURL(), SecretKey: "s"}))
	if _, err := c.Player(context.Background(), "PF00000002"); err != nil {
		t.Fatalf("a 500 failed the read instead of retrying: %v", err)
	}
	if c.Stats().ServerError == 0 {
		t.Error("the client did not record the server errors it retried")
	}
}

// The quirk that makes a naive client import nothing and report success: PlayFab answers HTTP
// 200 and puts the real code in the envelope.
func TestAnErrorCodeInsideA200IsStillAnError(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"code":429,"status":"RateLimitExceeded","errorMessage":"Too many requests","data":{}}`))
	}))
	defer srv.Close()

	c := playfab.New(fast(playfab.Config{BaseURL: srv.URL, SecretKey: "s", MaxRetries: 2}))
	_, err := c.Player(context.Background(), "PF1")
	if err == nil {
		t.Fatal("an envelope with code 429 was accepted as a success")
	}
	if !strings.Contains(err.Error(), "RateLimitExceeded") {
		t.Errorf("the error does not carry the title's own message: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Errorf("the client made %d attempts, want 3 (the first plus two retries)", got)
	}
}

// A 403 will never succeed on retry, and retrying it spends the wall clock the rate limit is
// already spending.
func TestAPermanentErrorIsNotRetried(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"code":403,"status":"NotAuthorized","errorMessage":"bad secret key"}`))
	}))
	defer srv.Close()

	c := playfab.New(fast(playfab.Config{BaseURL: srv.URL, SecretKey: "wrong", MaxRetries: 5}))
	_, err := c.Player(context.Background(), "PF1")
	if err == nil {
		t.Fatal("a 403 was treated as success")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("the client made %d attempts at a permanent failure, want 1", got)
	}
	if c.Stats().Permanent != 1 {
		t.Errorf("permanent errors = %d, want 1", c.Stats().Permanent)
	}
}

func TestGivingUpNamesTheAttempts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"code":500,"status":"InternalServerError","errorMessage":"nope"}`))
	}))
	defer srv.Close()

	c := playfab.New(fast(playfab.Config{BaseURL: srv.URL, SecretKey: "s", MaxRetries: 2}))
	_, err := c.Player(context.Background(), "PF1")
	if err == nil {
		t.Fatal("a title that always fails produced a player")
	}
	if !strings.Contains(err.Error(), "3 attempts") {
		t.Errorf("the error does not say how hard it tried: %v", err)
	}
}

func TestRetryAfterIsHonoured(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"code":429,"status":"RateLimitExceeded","errorMessage":"slow down"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":200,"data":{"PlayerProfile":{"PlayerId":"PF1"}}}`))
	}))
	defer srv.Close()

	c := playfab.New(playfab.Config{
		BaseURL: srv.URL, SecretKey: "s", MaxRetries: 3,
		BaseBackoff: time.Microsecond, MaxBackoff: 5 * time.Second,
	})
	start := time.Now()
	if _, err := c.Player(context.Background(), "PF1"); err != nil {
		t.Fatalf("the retry did not succeed: %v", err)
	}
	// The header said one second; the exponential fallback would have been a microsecond.
	if elapsed := time.Since(start); elapsed < time.Second {
		t.Errorf("the retry waited %s, but the title asked for 1s", elapsed)
	}
}

func TestNotFoundIsRecognisableSoAPlayerCanBeSkipped(t *testing.T) {
	tenant := faketenant.New(faketenant.Options{Players: 5})
	defer tenant.Close()

	c := playfab.New(fast(playfab.Config{BaseURL: tenant.BaseURL(), SecretKey: "s"}))
	_, err := c.Player(context.Background(), "PF99999999")
	if err == nil {
		t.Fatal("a player that is not in the title produced a record")
	}
	if !playfab.IsNotFound(err) {
		t.Errorf("the error is not recognisable as a missing player: %v", err)
	}
}

// The secret is sent and never echoed: an error message ends up in a log on a machine that
// somebody else can read.
func TestSecretIsSentButNeverAppearsInAnError(t *testing.T) {
	const secret = "ABCDEF-not-a-real-secret"
	var gotHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get("X-SecretKey")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"code":403,"status":"NotAuthorized","errorMessage":"the secret key ABCDEF-not-a-real-secret is not valid"}`))
	}))
	defer srv.Close()

	c := playfab.New(fast(playfab.Config{BaseURL: srv.URL, SecretKey: secret}))
	_, err := c.Player(context.Background(), "PF1")
	if gotHeader != secret {
		t.Errorf("X-SecretKey = %q, want the configured secret", gotHeader)
	}
	if err == nil {
		t.Fatal("no error was returned")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("the error echoes the secret: %v", err)
	}
}

// The importer runs players concurrently, so the client has to be safe under -race.
func TestConcurrentReads(t *testing.T) {
	tenant := faketenant.New(faketenant.Options{Players: 60, Seed: 5})
	defer tenant.Close()

	c := playfab.New(fast(playfab.Config{BaseURL: tenant.BaseURL(), SecretKey: "s"}))

	var wg sync.WaitGroup
	errs := make(chan error, 30)
	for i := 1; i <= 30; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := c.Player(context.Background(), fmt.Sprintf("PF%08d", i)); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("a concurrent read failed: %v", err)
	}
	// Five reads per player, and nothing else.
	if got := c.Stats().Requests; got != 150 {
		t.Errorf("requests = %d, want 150", got)
	}
}

func TestAPIErrorCarriesTheTitleIsOwnMessage(t *testing.T) {
	tenant := faketenant.New(faketenant.Options{Players: 3})
	defer tenant.Close()

	c := playfab.New(fast(playfab.Config{BaseURL: tenant.BaseURL(), SecretKey: "s"}))
	_, err := c.Player(context.Background(), "PF99999999")
	var apiErr *playfab.APIError
	if err == nil {
		t.Fatal("no error")
	}
	if !asErr(err, &apiErr) {
		t.Fatalf("the error is not an APIError: %T", err)
	}
	if apiErr.Code != 404 || apiErr.Name != "PlayerNotFound" {
		t.Errorf("code/name = %d/%q, want 404/PlayerNotFound", apiErr.Code, apiErr.Name)
	}
	if apiErr.Retryable() {
		t.Error("a missing player was reported as retryable")
	}
}

func asErr(err error, target **playfab.APIError) bool {
	for err != nil {
		if e, ok := err.(*playfab.APIError); ok {
			*target = e
			return true
		}
		unwrapped := unwrap(err)
		if unwrapped == nil {
			return false
		}
		err = unwrapped
	}
	return false
}

type wrapper interface{ Unwrap() error }

func unwrap(err error) error {
	if w, ok := err.(wrapper); ok {
		return w.Unwrap()
	}
	return nil
}

var _ = json.Marshal
var _ = os.Getenv
