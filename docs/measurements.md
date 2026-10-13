# Measurements

A migration tool has two modes and they count differently, so the README quotes both.

## Environment

| | |
|---|---|
| Machine | 12-core x86-64 (AMD64), Ubuntu 24.04, Docker 29.1.3 |
| Go | 1.27.2 |
| Date | 2026-10-13 (UTC) |

Every host port in this file is the one the repository publishes: bound to loopback and offset from
the default, so a machine already running a Postgres, a Redis or three other stacks does not have to
stop them first.


## 92 tests, and 105 with a database

```
$ make test
ok  ...   (the database tests skip and name the variable that would run them)

$ go test ./... -count=1 -v | grep -c '^--- PASS: Test'
92

$ make test-db          # real PostgreSQL on 15434, then the same suite
mkit-test-pg
postgres ready on 15434
MIGRATION_TEST_DSN='postgres://postgres:mkit@127.0.0.1:15434/mkit?sslmode=disable' go test ./... -count=1
ok  .../internal/canon        0.004s
ok  .../internal/checkpoint   0.274s
ok  .../internal/faketenant   0.012s
ok  .../internal/importer     0.017s
ok  .../internal/ledger       0.450s
ok  .../internal/mapping      0.010s
ok  .../internal/migrate      0.163s
ok  .../internal/playfab      1.037s
ok  .../internal/reconcile    0.006s
ok  .../internal/titleexport  0.645s
```

**Result.** Exact: 92 tests without a database, 105 with one. The database tests skip by name, so a
reader who runs `make test` sees what they are missing rather than a green run that checked less.

Total statement coverage is 55.3% over both configurations.
