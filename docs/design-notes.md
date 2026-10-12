# Design notes

The decisions here are the ones that cost a second migration if they are changed later. Each is
pinned by a test, because a decision in a document is a preference and a decision in a test is a
decision.

## 1. A player's Nakama account is derived from the legacy id, not remembered

`internal/identity` derives a UUIDv5 from the legacy player id. Nakama's custom authentication is
keyed by an opaque string, and authenticating the same string twice returns the same account, so the
importer does not need to remember what it did: on a re-run it computes the same id and finds the
account it already made.

A counter or a table of assignments would put "did we already import this player" into the run's own
state, and that state is exactly what is lost in the failure the resume logic exists for.

The namespace is frozen. Changing it does not merge accounts, it creates a second account for every
player and leaves the imported data on the first - so it is derived once, from
`nakama-migration-kit.lasttoss.dev` under the DNS namespace, and
`identity.TestNamespaceIsTheOneWeChose` pins both the derivation and three concrete ids.

## 2. Checksums are semantic, and taken over canonical bytes

A second run has to be able to say it wrote nothing. That claim is worth nothing if a record that
means the same thing hashes differently, and two ordinary things do that:

- a value that made a round trip through Postgres `jsonb` comes back with its keys reordered and its
  numbers re-rendered (`canon.TestNormalizeSurvivesAJSONBRoundTrip`,
  `canon.TestChecksumIgnoresKeyOrderAndWhitespace`);
- `encoding/json` escapes HTML by default, so a display name containing `&` or `<` hashes
  differently depending on which side of the wire it was encoded on.

`internal/mapping` takes the checksum per document rather than per player, so a run that finds the
profile unchanged and one currency changed writes one document and says so. It also normalises the
three things the source does not promise: inventory order, timestamp precision, and absent versus
empty (`mapping.TestAbsentDocumentsAreAbsentRatherThanEmpty`).

## 3. The ledger is append-only

`internal/ledger` records the checksum a run wrote; a later run records the next one. Nothing is
updated in place, because the question an operator asks after a bad import is "what did we write,
and when", and an overwritten row cannot answer it
(`ledger.TestAppendingTheSameChecksumTwiceIsOneRow`, `ledger.TestTheNewestChecksumWins`).

## 4. A stopped run resumes where it stopped

`internal/checkpoint` stores a cursor per source and stream. A checkpoint is not an optimisation: a
migration of a live title runs for hours against a rate limit, and the difference between "it
stopped" and "it stopped and the next run does 3.5 days again" is the whole reason
`checkpoint.TestResumeAfterACrashIsWhereItStopped` exists.

## 5. The API path is bounded by the source, and the export path is not

`internal/plan` computes a wall clock from request counts, per-call latency, rate limit and write
concurrency. It exists to answer a studio's question honestly: an Admin API import of a million
players is days, and the way to make it minutes is to stop calling the API per player. Hence two
`Source` implementations behind one interface, with the same mapping and the same checksums on both
sides (`source` tests read the same players through both).

## 6. Numbered SQL, not a migration framework

`internal/migrate` applies numbered files in order and records them in `schema_version`. A framework
would be a second dependency and a second way for a run to differ between a developer's machine and
CI; the tests assert the migration is idempotent
(`migrate.TestMigrationsAreIdempotent`) and that the schema has the tables the kit writes.

## 7. Each test package gets its own schema

`go test ./...` runs packages in parallel, so two packages creating the same table in the same
database fail each other's tests in a way that reads as a bug in the code under test. `internal/pgtest`
gives each package a schema and drops it when the test ends. When the DSN is unset the test skips
loudly, naming the variable, rather than passing in a way that looks like coverage
(`pgtest.Conn`).
