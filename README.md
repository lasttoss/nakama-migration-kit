# nakama-migration-kit

Move a live title's players off a legacy backend and into Nakama, and be able to say with a number
that the second run wrote nothing.

## What is here

Twelve packages and no CLI. This is the data layer: everything that has to be right before a run is
allowed to touch a live title.

| package | what it decides |
|---|---|
| `internal/identity` | a Nakama custom id derived from the legacy id, so a re-run cannot create a second account |
| `internal/mapping` | a legacy player record into Nakama documents, with a per-document semantic checksum |
| `internal/canon` | the bytes a checksum is taken over, so a round trip through `jsonb` is not a change |
| `internal/ledger` | what a run wrote, append-only, so "wrote nothing the second time" is a number |
| `internal/checkpoint` | where a run stopped, so a stopped run resumes instead of starting over |
| `internal/source` | the import source: a live Admin API, or a title export read off disk |
| `internal/playfab` | transport for the live API: paging, `Retry-After`, and the error taxonomy |
| `internal/plan` | how long, as arithmetic rather than optimism |
| `internal/profile` | one title's mapping decisions (`profiles/faketenant.json`), and the discovery pass that drafts them |
| `internal/faketenant` | a fake legacy title to import from, deterministic and awkward on purpose |
| `internal/migrate` | numbered SQL, applied in order, recorded in `schema_version` |
| `internal/pgtest` | a schema per test package, because `go test ./...` runs packages in parallel |

## The numbers that decide a migration plan

Measured by `go test ./internal/plan -run TestTheNumbersTheReadmeQuotes -v`, which prints them rather
than asserting them, because they are a budget and not a test result:

| | wall clock at 20 req/s |
|---|---|
| 10,000 players over the Admin API | 50.0 min (60,000 calls) |
| 100,000 players | 8.3 h |
| 1,000,000 players | 3.5 days |
| 1,000,000 players from a title export, batched writes | 1.0 min |
| 1,000,000 players from an export, 8 writers | 16.0 s |

The first three are the number a studio needs before agreeing to a cutover window: an API import is
bounded by the source's rate limit, which this kit does not control, and no amount of reading
Goroutines changes that. The last two are why `Source` has a second implementation for an export
instead of a faster loop for the API.

## What is not here yet

- `cmd/importer` and `cmd/reconcile`: the CLI. Empty directories, not stubs.
- `docker-compose.yml` and `charts/`: these need an entrypoint to run. A compose file for a binary
  that does not exist is a file that passes review and fails on first use, so they come after the
  CLI and the runtime plugin, against a lab that actually starts (`postgres` + the fake tenant +
  Nakama + the plugin).

## Running the tests

```
make test      # 75 tests; 13 skip and name the variable that would have run them
make test-db   # starts a throwaway PostgreSQL and runs everything: 88 tests, 0 skipped
make lint      # gofmt and vet
```

`MIGRATION_TEST_DSN` turns the database tests on. They are the interesting half - an import writing
nothing the second time is a database property - so CI runs them against a service container rather
than reporting a green suite with a quarter of it skipped.

## Design notes

`docs/design-notes.md` records the decisions that are expensive to reverse, each with the test that
pins it.
