// Command importer moves players from a legacy source into Nakama.
//
// It is a thin layer: the loop, the identity derivation and the idempotency all live in packages
// that are tested without a network. What this does is read flags, connect three things (a source, a
// target, a database for the ledger and the checkpoints), and print what happened.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http/httptest"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lasttoss/nakama-migration-kit/internal/checkpoint"
	"github.com/lasttoss/nakama-migration-kit/internal/faketenant"
	"github.com/lasttoss/nakama-migration-kit/internal/importer"
	"github.com/lasttoss/nakama-migration-kit/internal/ledger"
	"github.com/lasttoss/nakama-migration-kit/internal/migrate"
	"github.com/lasttoss/nakama-migration-kit/internal/playfab"
	"github.com/lasttoss/nakama-migration-kit/internal/profile"
	"github.com/lasttoss/nakama-migration-kit/internal/source"
	"github.com/lasttoss/nakama-migration-kit/internal/target"
	"github.com/lasttoss/nakama-migration-kit/internal/titleexport"
)

type flags struct {
	sourceKind string
	exportPath string
	profilefi  string
	baseURL    string
	titleID    string
	secretKey  string
	fakeTenant int
	fakeSeed   int64

	targetURL       string
	targetHTTPKey   string
	targetServerKey string

	dsn         string
	dryRun      bool
	limit       int
	batch       int
	concurrency int
	quiet       bool
	jsonOut     string
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "importer: %v\n", err)
		os.Exit(1)
	}
}

// envOr reads a setting from the environment, for the same reason the compose lab sets one for the
// other repos: a container runs a fixed command, and `docker compose run <service> -limit=300`
// replaces that command rather than adding to it - so a flag-only CLI silently loses every other
// flag the service was configured with when a caller passes one of its own.
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
	fs := flag.NewFlagSet("importer", flag.ExitOnError)
	var f flags
	fs.StringVar(&f.sourceKind, "source", envOr("MIGRATION_SOURCE", "playfab"), "where players are read from: playfab (Admin API), export (a title export file), faketenant (a fake title, for the lab)")
	fs.StringVar(&f.exportPath, "export", "", "path to the title export, when -source=export")
	fs.StringVar(&f.profilefi, "profile", envOr("MIGRATION_PROFILE", ""), "path to the title's mapping profile (required)")
	fs.StringVar(&f.baseURL, "playfab-base-url", envOr("MIGRATION_PLAYFAB_BASE_URL", ""), "Admin API root; empty means PlayFab's, and the lab points it at the fake title")
	fs.StringVar(&f.titleID, "playfab-title", envOr("MIGRATION_PLAYFAB_TITLE", ""), "the title id the Admin API calls are scoped to")
	fs.StringVar(&f.secretKey, "playfab-secret", envOr("PLAYFAB_SECRET_KEY", ""), "the title's admin secret (or PLAYFAB_SECRET_KEY); never logged")
	fs.IntVar(&f.fakeTenant, "faketenant-players", envInt("MIGRATION_FAKETENANT_PLAYERS", 1000), "how many players the fake title holds, when -source=faketenant")
	fs.Int64Var(&f.fakeSeed, "faketenant-seed", 7, "the fake title's seed, so two runs see the same players")
	fs.StringVar(&f.targetURL, "target-url", envOr("MIGRATION_TARGET_URL", "http://127.0.0.1:7350"), "Nakama's client API root")
	fs.StringVar(&f.targetHTTPKey, "target-http-key", "defaulthttpkey", "Nakama's runtime.http_key, for the runtime RPCs")
	fs.StringVar(&f.targetServerKey, "target-server-key", "defaultkey", "Nakama's socket.server_key, for the client API")
	fs.StringVar(&f.dsn, "dsn", envOr("MIGRATION_DSN", ""), "PostgreSQL DSN for the kit's own ledger and checkpoints (required)")
	fs.BoolVar(&f.dryRun, "dry-run", false, "ask the target what it would write, and write nothing anywhere")
	fs.IntVar(&f.limit, "limit", envInt("MIGRATION_LIMIT", 0), "stop after this many players (0 = all)")
	fs.IntVar(&f.batch, "batch", envInt("MIGRATION_BATCH", 500), "how many players one listing page holds, and how often a checkpoint is written")
	fs.IntVar(&f.concurrency, "concurrency", envInt("MIGRATION_CONCURRENCY", 4), "how many players are in flight")
	fs.BoolVar(&f.quiet, "quiet", false, "print only the summary")
	fs.StringVar(&f.jsonOut, "json", "", "also write the summary JSON here")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if f.dsn == "" {
		return fmt.Errorf("-dsn is required: the ledger and the checkpoints are what make a re-run safe")
	}
	if f.profilefi == "" {
		return fmt.Errorf("-profile is required: without it the kit does not know what a title's user data keys mean")
	}

	prof, err := profile.Load(f.profilefi)
	if err != nil {
		return err
	}
	if err := prof.Validate(); err != nil {
		return err
	}
	if prof.Strict() && f.dryRun == false {
		// A profile that says "fail on anything unmapped" is a title that has finished deciding; the
		// flag exists so the first run against a live title can be a dry run instead.
		fmt.Fprintln(os.Stderr, "note: the profile is strict, so a player with unmapped data stops the import rather than being reported")
	}

	src, closeSource, err := buildSource(&f)
	if err != nil {
		return err
	}
	defer closeSource()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	migConn, err := pgx.Connect(ctx, f.dsn)
	if err != nil {
		return fmt.Errorf("connect to the kit's database: %w", err)
	}
	// The kit's schema is applied here rather than by a migration step a run can forget: a lab on a
	// clean machine has to work with one command.
	if err := migrate.Apply(ctx, migConn); err != nil {
		return fmt.Errorf("apply the kit's schema: %w", err)
	}
	_ = migConn.Close(context.Background())

	// The pool is the point of the change: the run writes the ledger and the checkpoint from several
	// workers at once, and one connection between them fails with "conn busy".
	pool, err := pgxpool.New(ctx, f.dsn)
	if err != nil {
		return fmt.Errorf("open the pool: %w", err)
	}
	defer pool.Close()

	tgt := target.NewNakama(f.targetURL, f.targetHTTPKey, f.targetServerKey)
	summary, err := importer.Run(ctx, importer.Options{
		Source: src, Target: tgt,
		Ledger:      ledger.Open(pool),
		Checkpoints: checkpoint.Open(pool),
		Profile:     prof,
		Strategy:    strategyOf(f.sourceKind),
		DryRun:      f.dryRun, Limit: f.limit, Batch: f.batch, Concurrency: f.concurrency,
		Log: logWriter(f.quiet),
	})
	if err != nil {
		return err
	}

	printSummary(os.Stdout, summary, f.quiet)
	if f.jsonOut != "" {
		raw, err := json.MarshalIndent(summary, "", "  ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(f.jsonOut, append(raw, '\n'), 0o644); err != nil {
			return err
		}
	}
	if summary.Errors > 0 {
		// A run that could not import some players has not done its job, and a shell cannot tell that
		// from a table of numbers.
		return fmt.Errorf("%d of %d players were not imported", summary.Errors, summary.Seen)
	}
	return nil
}

// strategyOf names the way the source is read. It goes into the run record, because two runs of one
// title by different means are not the same run and a report that conflates them answers nothing.
func strategyOf(kind string) string {
	switch kind {
	case "export":
		return "title-export"
	case "faketenant":
		return "fake-title"
	default:
		return "playfab-admin-api"
	}
}

func buildSource(f *flags) (source.Source, func(), error) {
	switch f.sourceKind {
	case "export":
		if f.exportPath == "" {
			return nil, nil, fmt.Errorf("-source=export needs -export <path>")
		}
		return titleexport.New(titleexport.Config{Path: f.exportPath}), func() {}, nil
	case "faketenant":
		// The fake title is a real HTTP server speaking the Admin API, so the code path under test is
		// the same one a live title takes - including the paging, the 429s and the injected faults.
		tenant := faketenant.New(faketenant.Options{Players: f.fakeTenant, Seed: f.fakeSeed})
		server := httptest.NewServer(tenant.Handler())
		client := playfab.New(playfab.Config{BaseURL: server.URL, TitleID: "FAKE", SecretKey: "fake-secret"})
		return client, server.Close, nil
	case "playfab":
		if f.titleID == "" || f.secretKey == "" {
			return nil, nil, fmt.Errorf("-source=playfab needs -playfab-title and -playfab-secret (or PLAYFAB_SECRET_KEY)")
		}
		client := playfab.New(playfab.Config{BaseURL: f.baseURL, TitleID: f.titleID, SecretKey: f.secretKey})
		return client, func() {}, nil
	default:
		return nil, nil, fmt.Errorf("unknown -source %q", f.sourceKind)
	}
}

func logWriter(quiet bool) *os.File {
	if quiet {
		return os.Stderr
	}
	return os.Stderr
}

func printSummary(w *os.File, s importer.Summary, quiet bool) {
	if quiet {
		fmt.Fprintf(w, "%s/%s %s: %d seen, %d written, %d skipped, %d errors\n",
			s.Source, s.Strategy, s.Mode, s.Seen, s.Documents, s.Skipped, s.Errors)
		return
	}
	fmt.Fprintf(w, "\n  run %d  %s / %s / %s\n", s.RunID, s.Source, s.Strategy, s.Mode)
	fmt.Fprintf(w, "  players seen        %d\n", s.Seen)
	fmt.Fprintf(w, "  players changed     %d\n", s.Changed)
	fmt.Fprintf(w, "  players identical   %d\n", s.Identical)
	if s.Mode == "dry-run" {
		fmt.Fprintf(w, "  would write         %d documents (nothing was written)\n", s.WouldWrite)
	} else {
		fmt.Fprintf(w, "  documents written   %d\n", s.Documents)
	}
	fmt.Fprintf(w, "  documents skipped   %d\n", s.Skipped)
	fmt.Fprintf(w, "  errors              %d\n", s.Errors)
	fmt.Fprintf(w, "  batches             %d%s\n", s.Batches, resumed(s))
	fmt.Fprintf(w, "  wall clock          %s\n", s.WallClock.Round(time.Millisecond))
	for _, f := range s.Failures {
		fmt.Fprintf(w, "  failed: %s\n", f)
	}
}

func resumed(s importer.Summary) string {
	if s.Resumed {
		return " (resumed)"
	}
	return ""
}
