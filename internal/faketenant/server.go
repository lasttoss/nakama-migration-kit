package faketenant

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"time"
)

// Faults are the ways this backend misbehaves on purpose.
//
// Every field exists because a real one does that, and the importer has to have an answer:
// a 429 that is treated as fatal stops a migration half way through a live title, and a short
// page that is treated as the end of the listing loses players while reporting success.
type Faults struct {
	// RateLimitEvery: every Nth request answers 429 with a Retry-After header. 0 disables it.
	RateLimitEvery int
	// RetryAfter is the header sent with a 429. Defaults to "1".
	RetryAfter string
	// FailEvery: every Nth request answers 500. 0 disables it.
	FailEvery int
	// DropPages: 1-based listing page numbers that come back missing players while still
	// reporting that the listing continues. The continuation token advances as if the
	// players had been sent, so nothing downstream can notice except reconciliation.
	DropPages []int
}

// Options configure a fake tenant.
type Options struct {
	// Players is how many players the tenant holds.
	Players int
	// PageSize is how many ids one listing page carries.
	PageSize int
	// Seed changes the generated values, not the ids.
	Seed int64
	// Faults are the injected failures.
	Faults Faults
	// Latency is added to every request, for tests that need a slow backend.
	Latency time.Duration
	// Tenant, if set, is served instead of generating one.
	Tenant *Tenant
}

// Stats is what the server has done, so a test can assert the importer saw the faults.
type Stats struct {
	Requests     int
	RateLimited  int
	ServerErrors int
	ListingPages int
	PlayersSent  int
}

// Server is an http test server pretending to be a legacy backend.
type Server struct {
	*httptest.Server

	tenant *Tenant
	opts   Options

	mu           sync.Mutex
	requests     int
	rateLimited  int
	serverErrors int
	listingPages int
	playersSent  int
}

// New starts a fake tenant on a loopback port.
func New(opts Options) *Server {
	if opts.PageSize <= 0 {
		opts.PageSize = 100
	}
	if opts.Tenant == nil {
		if opts.Players <= 0 {
			opts.Players = 10
		}
		opts.Tenant = NewTenant(opts.Players, opts.Seed)
	}
	s := &Server{tenant: opts.Tenant, opts: opts}
	s.Server = httptest.NewServer(s.routes())
	return s
}

// routes is the fake title's API. It is a method as well as what New serves, because a lab wants the
// title on an address it chose: cmd/faketenant serves these routes on a fixed port, where the
// importer's container can reach them, instead of on httptest's loopback one.
func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/Admin/GetPlayersInSegment", s.handlePlayersInSegment)
	mux.HandleFunc("/Admin/GetPlayerProfile", s.handlePlayerProfile)
	mux.HandleFunc("/Admin/GetUserData", s.handleUserData)
	mux.HandleFunc("/Admin/GetUserInventory", s.handleUserInventory)
	mux.HandleFunc("/Admin/GetUserVirtualCurrency", s.handleUserVirtualCurrency)
	mux.HandleFunc("/Admin/GetPlayerStatistics", s.handlePlayerStatistics)
	return mux
}

// Handler serves the fake title on a listener of the caller's choosing.
func (s *Server) Handler() http.Handler { return s.routes() }

// Tenant is the data being served, which a test uses as ground truth.
func (s *Server) Tenant() *Tenant { return s.tenant }

// BaseURL is the API root an adapter should be pointed at, the same shape as a real title's
// "https://<titleid>.playfabapi.com": the adapter adds the "/Admin/..." path itself.
func (s *Server) BaseURL() string { return s.Server.URL }

// Stats returns what happened so far.
func (s *Server) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Stats{
		Requests:     s.requests,
		RateLimited:  s.rateLimited,
		ServerErrors: s.serverErrors,
		ListingPages: s.listingPages,
		PlayersSent:  s.playersSent,
	}
}

// inject applies the fault rules to the nth request and reports whether it answered.
func (s *Server) inject(w http.ResponseWriter) bool {
	s.mu.Lock()
	s.requests++
	n := s.requests
	s.mu.Unlock()

	if s.opts.Latency > 0 {
		time.Sleep(s.opts.Latency)
	}
	if s.opts.Faults.RateLimitEvery > 0 && n%s.opts.Faults.RateLimitEvery == 0 {
		retry := s.opts.Faults.RetryAfter
		if retry == "" {
			retry = "1"
		}
		w.Header().Set("Retry-After", retry)
		s.mu.Lock()
		s.rateLimited++
		s.mu.Unlock()
		writeError(w, http.StatusTooManyRequests, 429, "RateLimitExceeded",
			fmt.Sprintf("request %d exceeded the title's Admin API rate limit", n))
		return true
	}
	if s.opts.Faults.FailEvery > 0 && n%s.opts.Faults.FailEvery == 0 {
		s.mu.Lock()
		s.serverErrors++
		s.mu.Unlock()
		writeError(w, http.StatusInternalServerError, 500, "InternalServerError",
			"the title backend could not answer this request")
		return true
	}
	return false
}

func (s *Server) handlePlayersInSegment(w http.ResponseWriter, r *http.Request) {
	if s.inject(w) {
		return
	}
	var req struct {
		SegmentID         string `json:"SegmentId"`
		MaxBatchSize      int    `json:"MaxBatchSize"`
		ContinuationToken string `json:"ContinuationToken"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, 400, "InvalidRequest", "malformed request body")
		return
	}
	offset, err := parseToken(req.ContinuationToken)
	if err != nil {
		writeError(w, http.StatusBadRequest, 400, "InvalidRequest", err.Error())
		return
	}
	size := req.MaxBatchSize
	if size <= 0 {
		size = s.opts.PageSize
	}

	s.mu.Lock()
	s.listingPages++
	page := s.listingPages
	s.mu.Unlock()

	end := offset + size
	if end > len(s.tenant.Players) {
		end = len(s.tenant.Players)
	}
	if offset > len(s.tenant.Players) {
		offset = len(s.tenant.Players)
	}
	page_ := s.tenant.Players[offset:end]

	// A dropped page is half the players, with a token that claims they were all sent.
	if dropPage(s.opts.Faults.DropPages, page) {
		if len(page_) > 1 {
			page_ = page_[:len(page_)/2]
		}
	}

	var resp GetPlayersInSegmentResponse
	resp.Code = 200
	for _, p := range page_ {
		resp.Data.ProfilesInSegment = append(resp.Data.ProfilesInSegment, struct {
			PlayerID    string `json:"PlayerId"`
			DisplayName string `json:"DisplayName"`
		}{PlayerID: p.ID, DisplayName: p.DisplayName})
	}
	// The token advances over what the page should have held, not over what it holds: this is
	// how a page loss stays invisible until something counts.
	if end < len(s.tenant.Players) {
		resp.Data.ContinuationToken = strconv.Itoa(end)
	}
	resp.Data.ProfilesProcessed = len(page_)

	s.mu.Lock()
	s.playersSent += len(page_)
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, resp)
}

func dropPage(drop []int, page int) bool {
	for _, d := range drop {
		if d == page {
			return true
		}
	}
	return false
}

func (s *Server) player(w http.ResponseWriter, r *http.Request) (*playerReq, bool) {
	var req playerReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, 400, "InvalidRequest", "malformed request body")
		return nil, false
	}
	if req.PlayFabID == "" {
		writeError(w, http.StatusBadRequest, 400, "InvalidRequest", "PlayFabId is required")
		return nil, false
	}
	if _, ok := s.tenant.PlayerIndex()[req.PlayFabID]; !ok {
		writeError(w, http.StatusNotFound, 404, "PlayerNotFound",
			fmt.Sprintf("no player %s in this title", req.PlayFabID))
		return nil, false
	}
	return &req, true
}

type playerReq struct {
	PlayFabID string `json:"PlayFabId"`
}

func (s *Server) handlePlayerProfile(w http.ResponseWriter, r *http.Request) {
	if s.inject(w) {
		return
	}
	req, ok := s.player(w, r)
	if !ok {
		return
	}
	p := s.tenant.PlayerIndex()[req.PlayFabID]

	var resp GetPlayerProfileResponse
	resp.Code = 200
	resp.Data.PlayerProfile.PlayerID = p.ID
	resp.Data.PlayerProfile.DisplayName = p.DisplayName
	resp.Data.PlayerProfile.Created = p.CreatedAt.Format(time.RFC3339)
	// A real title reports fractional seconds on some calls and not on others; the second call
	// for the same player in this fake returns the truncated form, so a checksum that counts
	// nanoseconds sees a change that is not there.
	resp.Data.PlayerProfile.LastLogin = p.UpdatedAt.Truncate(time.Second).Format(time.RFC3339)
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleUserData(w http.ResponseWriter, r *http.Request) {
	if s.inject(w) {
		return
	}
	req, ok := s.player(w, r)
	if !ok {
		return
	}
	p := s.tenant.PlayerIndex()[req.PlayFabID]

	var resp GetUserDataResponse
	resp.Code = 200
	resp.Data.Data = map[string]string{}
	for k, v := range p.UserData {
		resp.Data.Data[k] = v
	}
	resp.Data.DataVersion = int(p.UpdatedAt.Unix())
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleUserInventory(w http.ResponseWriter, r *http.Request) {
	if s.inject(w) {
		return
	}
	req, ok := s.player(w, r)
	if !ok {
		return
	}
	p := s.tenant.PlayerIndex()[req.PlayFabID]

	var resp GetUserInventoryResponse
	resp.Code = 200
	for _, it := range p.Inventory {
		resp.Data.Inventory = append(resp.Data.Inventory, struct {
			ItemInstanceID string `json:"ItemInstanceId"`
			ItemID         string `json:"ItemId"`
			RemainingUses  int64  `json:"RemainingUses"`
			CatalogVersion string `json:"CatalogVersion"`
		}{ItemInstanceID: it.InstanceID, ItemID: it.CatalogID, RemainingUses: it.Count, CatalogVersion: "1"})
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleUserVirtualCurrency(w http.ResponseWriter, r *http.Request) {
	if s.inject(w) {
		return
	}
	req, ok := s.player(w, r)
	if !ok {
		return
	}
	p := s.tenant.PlayerIndex()[req.PlayFabID]

	var resp GetUserVirtualCurrencyResponse
	resp.Code = 200
	resp.Data.VirtualCurrency = map[string]int64{}
	for code, amount := range p.Currency {
		resp.Data.VirtualCurrency[code] = amount
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handlePlayerStatistics(w http.ResponseWriter, r *http.Request) {
	if s.inject(w) {
		return
	}
	req, ok := s.player(w, r)
	if !ok {
		return
	}
	p := s.tenant.PlayerIndex()[req.PlayFabID]

	var resp GetPlayerStatisticsResponse
	resp.Code = 200
	for name, value := range p.Statistics {
		resp.Data.Statistics = append(resp.Data.Statistics, struct {
			StatisticName string `json:"StatisticName"`
			Value         int64  `json:"Value"`
		}{StatisticName: name, Value: value})
	}
	writeJSON(w, http.StatusOK, resp)
}

func parseToken(token string) (int, error) {
	if token == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(token)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("bad continuation token %q", token)
	}
	return n, nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	body, err := json.Marshal(v)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func writeError(w http.ResponseWriter, status, code int, name, message string) {
	writeJSON(w, status, apiError{Code: code, Status: name, Message: message})
}
