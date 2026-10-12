// Command reconcile compares a legacy source against the target, player by player.
//
// It exits non-zero when anything is missing or changed, because the thing it is for is being run in
// a pipeline as the gate on a cutover: a reconciliation that reports a mismatch and exits zero is a
// reconciliation nobody acts on.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lasttoss/nakama-migration-kit/internal/faketenant"
	"github.com/lasttoss/nakama-migration-kit/internal/identity"
	"github.com/lasttoss/nakama-migration-kit/internal/ledger"
	"github.com/lasttoss/nakama-migration-kit/internal/mapping"
	"github.com/lasttoss/nakama-migration-kit/internal/migrate"
	"github.com/lasttoss/nakama-migration-kit/internal/playfab"
	"github.com/lasttoss/nakama-migration-kit/internal/profile"
	"github.com/lasttoss/nakama-migration-kit/internal/reconcile"
	"github.com/lasttoss/nakama-migration-kit/internal/source"
	"github.com/lasttoss/nakama-migration-kit/internal/target"
	"github.com/lasttoss/nakama-migration-kit/internal/titleexport"
	"net/http/httptest"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "reconcile: %v\n", err)
		os.Exit(1)
	}
}

// envOr and envInt mirror the importer's: a container runs a fixed command, and `docker compose run
// <service> -limit=300` replaces that command rather than adding to it, so a flag-only CLI loses
// every other setting the service was configured with as soon as a caller passes one of its own.
func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

func envInt(name string, fallback int) int {
	if v := os.Getenv(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

func run(args []string) error {
	fs := flag.NewFlagSet("reconcile", flag.ExitOnError)
	var (
		sourceKind = fs.String("source", envOr("MIGRATION_SOURCE", "playfab"), "where players are read from: playfab, export or faketenant")
		exportPath = fs.String("export", "", "path to the title export, when -source=export")
		profilefi  = fs.String("profile", envOr("MIGRATION_PROFILE", ""), "path to the title's mapping profile (required)")
		baseURL    = fs.String("playfab-base-url", envOr("MIGRATION_PLAYFAB_BASE_URL", ""), "Admin API root; empty means PlayFab's")
		titleID    = fs.String("playfab-title", envOr("MIGRATION_PLAYFAB_TITLE", ""), "the title id the Admin API calls are scoped to")
		secretKey  = fs.String("playfab-secret", envOr("PLAYFAB_SECRET_KEY", ""), "the title's admin secret (or PLAYFAB_SECRET_KEY); never logged")
		fakeN      = fs.Int("faketenant-players", envInt("MIGRATION_FAKETENANT_PLAYERS", 1000), "how many players the fake title holds")
		fakeSeed   = fs.Int64("faketenant-seed", 7, "the fake title's seed")
		targetURL  = fs.String("target-url", envOr("MIGRATION_TARGET_URL", "http://127.0.0.1:7350"), "Nakama's client API root")
		httpKey    = fs.String("target-http-key", envOr("MIGRATION_TARGET_HTTP_KEY", "defaulthttpkey"), "Nakama's runtime.http_key")
		serverKey  = fs.String("target-server-key", envOr("MIGRATION_TARGET_SERVER_KEY", "defaultkey"), "Nakama's socket.server_key")
		dsn        = fs.String("dsn", envOr("MIGRATION_DSN", ""), "PostgreSQL DSN for the kit's ledger (required)")
		limit      = fs.Int("limit", envInt("MIGRATION_LIMIT", 0), "compare this many players (0 = all)")
		batch      = fs.Int("batch", envInt("MIGRATION_BATCH", 500), "how many players one listing page holds")
		jsonOut    = fs.String("json", "", "also write the findings JSON here")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *dsn == "" {
		return fmt.Errorf("-dsn is required: the reconciliation is recorded, and a report nobody kept is a report nobody can quote")
	}
	if *profilefi == "" {
		return fmt.Errorf("-profile is required")
	}
	prof, err := profile.Load(*profilefi)
	if err != nil {
		return err
	}

	var (
		src source.Source
		cl  func()
	)
	switch *sourceKind {
	case "export":
		if *exportPath == "" {
			return fmt.Errorf("-source=export needs -export <path>")
		}
		src, cl = titleexport.New(titleexport.Config{Path: *exportPath}), func() {}
	case "faketenant":
		tenant := faketenant.New(faketenant.Options{Players: *fakeN, Seed: *fakeSeed})
		srv := httptest.NewServer(tenant.Handler())
		src, cl = playfab.New(playfab.Config{BaseURL: srv.URL, TitleID: "FAKE", SecretKey: "fake-secret"}), srv.Close
	default:
		if *titleID == "" || *secretKey == "" {
			return fmt.Errorf("-source=playfab needs -playfab-title and -playfab-secret (or PLAYFAB_SECRET_KEY)")
		}
		src, cl = playfab.New(playfab.Config{BaseURL: *baseURL, TitleID: *titleID, SecretKey: *secretKey}), func() {}
	}
	defer cl()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	conn, err := pgx.Connect(ctx, *dsn)
	if err != nil {
		return fmt.Errorf("connect to the kit's database: %w", err)
	}
	if err := migrate.Apply(ctx, conn); err != nil {
		return fmt.Errorf("apply the kit's schema: %w", err)
	}
	_ = conn.Close(context.Background())
	pool, err := pgxpool.New(ctx, *dsn)
	if err != nil {
		return fmt.Errorf("open the pool: %w", err)
	}
	defer pool.Close()

	led := ledger.Open(pool)
	tgt := target.NewNakama(*targetURL, *httpKey, *serverKey)

	runID, err := led.Run(ctx, src.Name(), "reconcile", "verify")
	if err != nil {
		return fmt.Errorf("start the run: %w", err)
	}

	start := time.Now()
	var (
		players, matches, mismatches, missing, changed, extra int64
		findings                                              []map[string]any
	)
	cursor := ""
	for {
		if *limit > 0 && players >= int64(*limit) {
			break
		}
		limitPage := *batch
		if *limit > 0 && int64(limitPage) > int64(*limit)-players {
			limitPage = *limit - int(players)
		}
		ids, next, err := src.PlayerIDs(ctx, cursor, limitPage)
		if err != nil {
			return fmt.Errorf("list players: %w", err)
		}
		if len(ids) == 0 && next == "" {
			break
		}
		for _, id := range ids {
			players++
			res, err := compareOne(ctx, src, tgt, prof, id)
			if err != nil {
				// A player the source will not return is a finding, not a crash: the point of this
				// command is to report, and a report that stops at the first problem reports one.
				mismatches++
				missing++
				if err := led.RecordMismatch(ctx, runID, id, "player", err.Error()); err != nil {
					return err
				}
				fmt.Printf("  %-8s %s: %v\n", "unreadable", id, err)
				continue
			}
			if res.Matches() {
				matches++
				continue
			}
			mismatches++
			for _, f := range res.Findings {
				switch f.Kind {
				case reconcile.Missing:
					missing++
				case reconcile.Changed:
					changed++
				case reconcile.Extra:
					extra++
				}
				if err := led.RecordMismatch(ctx, runID, id, f.Entity, f.String()); err != nil {
					return err
				}
				findings = append(findings, map[string]any{"player": id, "kind": string(f.Kind), "entity": f.Entity, "key": f.Key, "detail": f.String()})
				fmt.Printf("  %s  %s\n", id, f)
			}
		}
		cursor = next
		if next == "" {
			break
		}
	}

	if err := led.FinishRun(ctx, runID, players, missing, 0, matches, mismatches); err != nil {
		return err
	}

	fmt.Printf("\n  players compared  %d\n", players)
	fmt.Printf("  matching          %d\n", matches)
	fmt.Printf("  mismatching       %d\n", mismatches)
	fmt.Printf("    missing         %d\n", missing)
	fmt.Printf("    changed         %d\n", changed)
	fmt.Printf("    extra           %d\n", extra)
	fmt.Printf("  wall clock        %s\n", time.Since(start).Round(time.Millisecond))

	if *jsonOut != "" {
		raw, err := json.MarshalIndent(map[string]any{
			"run_id": runID, "players": players, "matching": matches, "mismatching": mismatches,
			"missing": missing, "changed": changed, "extra": extra, "findings": findings,
		}, "", "  ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(*jsonOut, append(raw, '\n'), 0o644); err != nil {
			return err
		}
	}

	if missing > 0 || changed > 0 {
		return fmt.Errorf("%d documents are missing and %d changed: the target does not account for the source", missing, changed)
	}
	return nil
}

// compareOne reads one player from the source, maps it, and compares the mapping against the target.
func compareOne(ctx context.Context, src source.Source, tgt target.Target, prof profile.Profile, id string) (reconcile.Result, error) {
	player, err := src.Player(ctx, id)
	if err != nil {
		return reconcile.Result{}, err
	}
	if err := source.Validate(player); err != nil {
		return reconcile.Result{}, err
	}
	rec, err := mapping.New(prof.Collection, player, src.Name())
	if err != nil {
		return reconcile.Result{}, err
	}
	customID := identity.CustomID(src.Name(), id)
	if customID != rec.CustomID {
		return reconcile.Result{}, fmt.Errorf("identity mismatch: record says %s, identity.CustomID says %s", rec.CustomID, customID)
	}
	userID, err := tgt.EnsureAccount(ctx, customID)
	if err != nil {
		return reconcile.Result{}, err
	}
	// Only the collection the import writes into: a game's own storage is not this kit's business,
	// and comparing against it would report the game's own writes as migration faults.
	inspect, err := tgt.Inspect(ctx, userID, []string{prof.Collection})
	if err != nil {
		return reconcile.Result{}, err
	}
	return reconcile.Compare(rec, inspect.Documents), nil
}
