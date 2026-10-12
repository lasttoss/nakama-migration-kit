#!/usr/bin/env bash
# The whole migration, from nothing, in one command.
#
# What it demonstrates, and each line is an assertion rather than a claim:
#   - a legacy title's players move into Nakama, and the run reports how many documents it wrote;
#   - running it again writes nothing, because the target compares what it holds against the meaning
#     of what it was sent - not because the importer remembered;
#   - the kit's own ledger agrees with the target's own storage about how many documents exist, which
#     is the difference between bookkeeping and evidence.
set -euo pipefail
cd "$(dirname "$0")/.."

PLAYERS=${PLAYERS:-10000}
DSN_IN=postgres://postgres:localdb@postgres:5432/mkit?sslmode=disable
fail() { echo "FAIL: $*" >&2; exit 1; }
trap 'echo "FAIL: line $LINENO: $BASH_COMMAND" >&2' ERR
# Exported, not passed to one command: `docker compose run` can recreate a service it depends on,
# and it interpolates the same file with whatever is in its environment. A LAB_PLAYERS that only
# applied to `up` meant the fake title was created with one size and recreated with another.
export LAB_PLAYERS=${PLAYERS:-10000}

cleanup() { docker compose down -v --remove-orphans >/dev/null 2>&1 || true; }
trap cleanup EXIT

psql() { docker compose exec -T postgres psql -U postgres -d mkit -tAc "$1"; }
# The extra settings go in as environment, not as arguments: an argument to `compose run` replaces the
# service's command, so `run importer -limit=300` would drop every other setting the service has.
# No -limit: the run covers the whole title, which is what makes the second run's "wrote nothing" a
# statement about the same players rather than about the next page of them.
run_importer() { docker compose run --rm importer "$@"; }
field() { awk -v k="$1" '$0 ~ k {print $NF}' "$2"; }

echo "=== building"
# The build output is kept and printed on failure. `--quiet` on a failing build prints one line about
# a build step, and the compiler's actual message - the one that says which file - is thrown away.
if ! docker compose build > /tmp/mkit-build.log 2>&1; then
  tail -30 /tmp/mkit-build.log >&2
  fail "the images did not build"
fi

echo "=== starting the legacy title, the database and Nakama ($PLAYERS players)"
# --force-recreate: the fake title's size comes from LAB_PLAYERS, and a container left over from an
# earlier run would otherwise still be serving the old title - which makes the whole demo a statement
# about a different number of players than the one it prints.
if ! docker compose up -d --wait --force-recreate postgres faketenant nakama; then
  # A lab that cannot start has to say why: without this, the next person to see the failure has to
  # re-run it to read the one line that explains it.
  echo "--- the containers' logs ---" >&2
  docker compose logs --tail=25 --no-log-prefix postgres faketenant nakama >&2 || true
  fail "the lab did not come up"
fi

echo "=== the first import"
if ! run_importer > /tmp/mkit-run1.txt 2>&1; then
  tail -25 /tmp/mkit-run1.txt >&2
  fail "the first import did not succeed"
fi
sed -n '/^  run /,$p' /tmp/mkit-run1.txt

seen1=$(field "players seen" /tmp/mkit-run1.txt)
docs1=$(field "documents written" /tmp/mkit-run1.txt)
errors1=$(field "errors" /tmp/mkit-run1.txt)
[ "$seen1" = "$PLAYERS" ] || fail "the first run saw $seen1 players, not $PLAYERS"
[ "$errors1" = "0" ] || fail "the first run reported $errors1 errors"
[ "$docs1" -gt 0 ] || fail "the first run wrote no documents"

echo "=== the second import: the same source, the same target, nothing changed"
if ! run_importer > /tmp/mkit-run2.txt 2>&1; then
  tail -25 /tmp/mkit-run2.txt >&2
  fail "the second import did not succeed"
fi
sed -n '/^  run /,$p' /tmp/mkit-run2.txt

docs2=$(field "documents written" /tmp/mkit-run2.txt)
identical2=$(field "players identical" /tmp/mkit-run2.txt)
[ "$docs2" = "0" ] || fail "the second run wrote $docs2 documents; it has to write nothing"
[ "$identical2" = "$PLAYERS" ] || fail "the second run reported $identical2 identical players, not $PLAYERS"

echo "=== the reconciler, which is the same question asked from the outside"
if ! docker compose run --rm reconcile > /tmp/mkit-reconcile.txt 2>&1; then
  tail -25 /tmp/mkit-reconcile.txt >&2
  fail "the reconciliation did not pass"
fi
sed -n '/^  players compared/,$p' /tmp/mkit-reconcile.txt
# Anchored on the first field: "mismatching" contains "matching" and the indented "missing" line sits
# under "mismatching", so a substring match reads the wrong number out of the right output.
missing=$(awk '$1 == "missing" {print $NF; exit}' /tmp/mkit-reconcile.txt)
changed=$(awk '$1 == "changed" {print $NF; exit}' /tmp/mkit-reconcile.txt)
[ "$missing" = "0" ] || fail "$missing documents are missing from the target"
[ "$changed" = "0" ] || fail "$changed documents changed meaning on the way"

echo "=== the ledger and the target agree"
identities=$(psql "select count(*) from identity_map")
ledger=$(psql "select count(*) from import_ledger")
[ "$identities" = "$PLAYERS" ] || fail "the ledger holds $identities identities, not $PLAYERS"
[ "$ledger" -gt 0 ] || fail "the ledger recorded no documents"
# By mode: the reconciler records a run of its own, and counting all runs would make this assertion
# fail for the right behaviour - which is how a check gets weakened instead of fixed.
imports=$(psql "select count(*) from import_run where mode = 'apply'")
verifies=$(psql "select count(*) from import_run where mode = 'verify'")
[ "$imports" = "2" ] || fail "the ledger holds $imports import runs, not 2"
[ "$verifies" = "1" ] || fail "the ledger holds $verifies verification runs, not 1"
# The column names come from the schema, not from what the summary calls them: `documents_written`,
# not `written`. A query invented from the report's wording fails at the last step of the demo, which
# is a slow way to find out that the ledger and the report do not name things the same way.
last_written=$(psql "select documents_written from import_run where mode = 'apply' order by id desc limit 1")
[ "$last_written" = "0" ] || fail "the last import recorded $last_written written documents"
last_seen=$(psql "select players_seen from import_run where mode = 'apply' order by id desc limit 1")
[ "$last_seen" = "$PLAYERS" ] || fail "the ledger says the last import saw $last_seen players, not $PLAYERS"

printf '\n  players imported      %s\n' "$identities"
printf '  documents in ledger   %s\n' "$ledger"
printf '  documents first run   %s\n' "$docs1"
printf '  documents second run  %s\n' "$docs2"
printf '  reconciled missing    %s\n' "$missing"
printf '  reconciled changed    %s\n' "$changed"
echo
echo "OK: $PLAYERS players imported, a second run that wrote nothing, and a ledger that agrees with it"
