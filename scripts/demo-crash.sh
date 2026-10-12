#!/usr/bin/env bash
# A run that dies in the middle, and the run that finishes it.
#
# The interesting claim is not that it resumes - it is that resuming is not the same as starting over:
# the players imported before the crash keep the documents they already have, the checksums match the
# uninterrupted run's, and the run after the crash writes only what is left.
set -euo pipefail
cd "$(dirname "$0")/.."

PLAYERS=${PLAYERS:-2000}
BATCH=${BATCH:-200}
fail() { echo "FAIL: $*" >&2; exit 1; }
trap 'echo "FAIL: line $LINENO: $BASH_COMMAND" >&2' ERR
# Exported, not passed to one command: `docker compose run` can recreate a service it depends on,
# and it interpolates the same file with whatever is in its environment. A LAB_PLAYERS that only
# applied to `up` meant the fake title was created with one size and recreated with another.
export LAB_PLAYERS=${PLAYERS:-10000}

cleanup() { docker compose down -v --remove-orphans >/dev/null 2>&1 || true; }
trap cleanup EXIT

psql() { docker compose exec -T postgres psql -U postgres -d mkit -tAc "$1"; }
field() { awk -v k="$1" '$0 ~ k {print $NF}' "$2"; }

if ! docker compose build > /tmp/mkit-build.log 2>&1; then
  tail -30 /tmp/mkit-build.log >&2
  fail "the images did not build"
fi
docker compose up -d --wait --force-recreate postgres faketenant nakama || fail "the lab did not come up"

echo "=== importing, and killing the run part way through"
docker compose run --rm --name mkit-crash -e MIGRATION_BATCH="$BATCH" -e MIGRATION_CONCURRENCY=1 importer > /tmp/mkit-crash.txt 2>&1 &
runner=$!
# Wait for the first checkpoint, which is the run saying "these batches are done". Killing on a timer
# instead would be a test that passes on a fast machine and fails on a slow one.
for _ in $(seq 1 120); do
  done_rows=$(psql "select coalesce((select players_done from import_checkpoint limit 1), 0)" 2>/dev/null || echo 0)
  [ "${done_rows:-0}" -gt 0 ] && break
  sleep 0.5
done
[ "${done_rows:-0}" -gt 0 ] || { kill $runner 2>/dev/null || true; fail "the run never wrote a checkpoint"; }
docker kill mkit-crash >/dev/null 2>&1 || true
wait $runner 2>/dev/null || true
partial=$(psql "select count(*) from import_ledger")
echo "  crashed after $done_rows players and $partial recorded documents"

echo "=== the next run finishes it"
if ! docker compose run --rm -e MIGRATION_BATCH="$BATCH" -e MIGRATION_CONCURRENCY=1 importer > /tmp/mkit-resume.txt 2>&1; then
  tail -25 /tmp/mkit-resume.txt >&2
  fail "the run after the crash did not succeed"
fi
sed -n '/^  run /,$p' /tmp/mkit-resume.txt

grep -q "resuming at batch" /tmp/mkit-crash.txt /tmp/mkit-resume.txt || fail "the run after the crash did not resume"
identities=$(psql "select count(*) from identity_map")
[ "$identities" = "$PLAYERS" ] || fail "after resuming, $identities identities are recorded, not $PLAYERS"
cp=$(psql "select count(*) from import_checkpoint")
[ "$cp" = "0" ] || fail "a finished run left $cp checkpoints behind"

printf '\n  players imported across the crash  %s\n' "$identities"
printf '  checkpoints left behind            %s\n' "$cp"
echo
echo "OK: a run killed mid-batch resumed, finished, and left no checkpoint behind"
