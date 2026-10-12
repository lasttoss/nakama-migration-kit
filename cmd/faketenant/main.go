// Command faketenant serves a fake legacy title for the lab.
//
// It speaks the Admin API shape the importer already reads, so the lab exercises the same paging,
// retries and error handling a live title does. It is a separate process because that is what a
// legacy backend is: something across a network, that can be slow, that can rate limit a run, and
// that is not the thing the run is supposed to be testing.
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/lasttoss/nakama-migration-kit/internal/faketenant"
)

func main() {
	addr := flag.String("addr", ":8080", "address to listen on")
	players := flag.Int("players", 10000, "how many players the title holds")
	seed := flag.Int64("seed", 7, "the seed, so two runs see the same players")
	rateLimit := flag.Float64("rate-limit", 0, "429 every Nth request (0 = never)")
	healthz := flag.Bool("healthz", false, "check that the title at -addr is answering, and exit")
	flag.Parse()

	if *healthz {
		// Distroless has no shell and no curl, so the container's own healthcheck is this binary
		// asking its own address for a page. Any HTTP response counts: the fake title answers 401 to
		// a request without a token, and 401 is a running server.
		addr := *addr
		if strings.HasPrefix(addr, ":") {
			addr = "127.0.0.1" + addr
		}
		client := &http.Client{Timeout: 2 * time.Second}
		resp, err := client.Get("http://" + addr + "/Admin/GetPlayersInSegment")
		if err != nil {
			fmt.Fprintf(os.Stderr, "not answering: %v\n", err)
			os.Exit(1)
		}
		_ = resp.Body.Close()
		os.Exit(0)
	}

	tenant := faketenant.New(faketenant.Options{Players: *players, Seed: *seed})
	// New also started an httptest listener on a loopback port. This process serves the same routes
	// on -addr, so that one is closed rather than left for the lifetime of the container.
	defer tenant.Close()
	if *rateLimit > 0 {
		fmt.Fprintf(os.Stderr, "fake title: %d players, seed %d, a 429 every %d requests\n", *players, *seed, int(*rateLimit))
	} else {
		fmt.Fprintf(os.Stderr, "fake title: %d players, seed %d, checksum %s\n", *players, *seed, tenant.Tenant().Checksum())
	}
	server := &http.Server{Addr: *addr, Handler: tenant.Handler()}
	log.Fatal(server.ListenAndServe())
}
