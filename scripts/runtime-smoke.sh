#!/usr/bin/env bash
# The runtime module against a real Nakama, from nothing: build the plugin with the toolchain that
# built the server, migrate the database, load the module, and assert what the kit claims.
#
# This is the smallest end-to-end proof in the repository: an import writes the documents the first
# time and writes nothing the second time, a dry run writes nothing at all, and a changed field
# writes exactly that field. The compose lab does the same thing with the importer in front of it;
# this one exists so the runtime can be checked without one.
set -euo pipefail
cd "$(dirname "$0")/../runtime"

NAKAMA_IMAGE=${NAKAMA_IMAGE:-heroiclabs/nakama:3.41.0}
PLUGINBUILDER=${PLUGINBUILDER:-heroiclabs/nakama-pluginbuilder:3.41.0}
PG=mkit-smoke-pg; NK=mkit-smoke-nk; NET=mkit-smoke; PORT=${PORT:-17371}
HTTP_KEY=${HTTP_KEY:-defaulthttpkey}; SERVER_KEY=${SERVER_KEY:-defaultkey}
fail() { echo "FAIL: $*" >&2; exit 1; }
# `set -e` stops on a failure but says nothing about which one: a script that can stop silently is a
# script whose next failure costs an afternoon. This names the command and the line. (It was added
# after exactly that: `docker network create` on a network left behind by an earlier failed run
# exited 1, and the only symptom was that the script printed nothing more.)
trap 'echo "FAIL: line $LINENO: $BASH_COMMAND" >&2' ERR
cleanup() { docker rm -f "$NK" "$PG" >/dev/null 2>&1 || true; docker network rm "$NET" >/dev/null 2>&1 || true; }
trap cleanup EXIT

echo "building the plugin with $PLUGINBUILDER"
docker run --rm -v "$PWD":/app -w /app --entrypoint sh "$PLUGINBUILDER" \
  -c "GOFLAGS=-mod=mod go build --trimpath --buildmode=plugin -o modules/backend.so ./cmd/server"
[ -s modules/backend.so ] || fail "the plugin was not built"

docker rm -f "$NK" "$PG" >/dev/null 2>&1 || true
docker network rm "$NET" >/dev/null 2>&1 || true
docker network create "$NET" >/dev/null
docker run -d --name "$PG" --network "$NET" -e POSTGRES_PASSWORD=localdb -e POSTGRES_DB=nakama postgres:16-alpine >/dev/null
# -h 127.0.0.1 forces a TCP check. Without it pg_isready answers over the unix socket, which during
# initdb belongs to a temporary server that is not listening on TCP yet: the wait returns, the
# migrate then fails with "connection refused", and the two lines look unrelated.
until docker exec "$PG" pg_isready -h 127.0.0.1 -p 5432 -U postgres -d nakama >/dev/null 2>&1; do sleep 0.3; done
# Without this the server dies on start with `relation "migration_info" does not exist`.
# The migrate output is kept on failure rather than discarded: a fatal here is the difference
# between a lab that runs and a lab that reports "connection refused" with nothing else.
if ! migrate_out=$(docker run --rm --network "$NET" "$NAKAMA_IMAGE" migrate up --database.address postgres:localdb@"$PG":5432/nakama 2>&1); then
  printf '%s\n' "$migrate_out" >&2
  fail "nakama migrate did not succeed"
fi
docker run -d --name "$NK" --network "$NET" -p "$PORT":7350 -v "$PWD/modules":/nakama/data/modules "$NAKAMA_IMAGE" \
  --name smoke --database.address postgres:localdb@"$PG":5432/nakama --session.token_expiry_sec 3600 >/dev/null
for _ in $(seq 1 60); do curl -sf "http://127.0.0.1:$PORT/" >/dev/null 2>&1 && break; sleep 0.5; done

H="http://127.0.0.1:$PORT"; K="$HTTP_KEY"
docker logs "$NK" 2>&1 | grep -q "Registered Go runtime RPC function invocation" || fail "the module did not load"

# The account is created by the same derived custom id the kit uses, twice: the second call has to
# return the same account, or the kit would create a duplicate on every run.
auth() { curl -s -X POST "$H/v2/account/authenticate/custom" -H "Authorization: Basic $(printf '%s:' "$SERVER_KEY" | base64)" \
  -H "Content-Type: application/json" -d "{\"id\":\"$1\",\"create\":true}"; }
uid_of() { python3 -c "
import base64,json,sys
p=sys.argv[1].split('.')[1]; p+='='*(-len(p)%4)
print(json.loads(base64.urlsafe_b64decode(p))['uid'])" "$1"; }
CUSTOM=11111111-2222-3333-4444-555555555555
U1=$(uid_of "$(python3 -c "import json,sys; print(json.load(sys.stdin)['token'])" <<<"$(auth "$CUSTOM")")")
U2=$(uid_of "$(python3 -c "import json,sys; print(json.load(sys.stdin)['token'])" <<<"$(auth "$CUSTOM")")")
[ "$U1" = "$U2" ] || fail "the same custom id produced two accounts: $U1 and $U2"

payload() { python3 - "$U1" "$1" <<'PY'
import json,sys
uid, gold_checksum = sys.argv[1], sys.argv[2]
gold = "500" if gold_checksum == "c2" else "600"
print(json.dumps({"player_id": uid, "source_id": "PF-ABC123", "documents": [
    {"collection": "player_profile", "key": "level", "checksum": "c1", "value": '{"level":7}'},
    {"collection": "wallet", "key": "gold", "checksum": gold_checksum, "value": '{"gold":%s}' % gold, "public": True},
]}))
PY
}
rpc() { curl -s -X POST "$H/v2/rpc/$1?unwrap&http_key=$K" -H "Content-Type: application/json" -d "$2"; }
field() { python3 -c "import json,sys; print(json.dumps(json.load(sys.stdin).get(sys.argv[1])))" "$1"; }

echo "the first run writes both documents"
rpc migration.apply "$(payload c2)" | tee /tmp/mkit-apply1.json
[ "$(field written </tmp/mkit-apply1.json)" = '["player_profile/level", "wallet/gold"]' ] || fail "the first run did not write both documents"

echo "the second run writes nothing"
rpc migration.apply "$(payload c2)" | tee /tmp/mkit-apply2.json
[ "$(field written </tmp/mkit-apply2.json)" = 'null' ] || fail "the second run wrote something"

echo "a dry run reports the change and writes nothing"
rpc migration.plan "$(payload c2-CHANGED)" | tee /tmp/mkit-plan.json
[ "$(field written </tmp/mkit-plan.json)" = '["wallet/gold"]' ] || fail "the dry run did not find the changed document"
[ "$(field skipped </tmp/mkit-plan.json)" = '["player_profile/level"]' ] || fail "the dry run did not find the unchanged document"

echo "the changed document is the only one written"
rpc migration.apply "$(payload c2-CHANGED)" | tee /tmp/mkit-apply3.json
[ "$(field written </tmp/mkit-apply3.json)" = '["wallet/gold"]' ] || fail "the changed run wrote the wrong documents"

echo "the database agrees"
rows=$(docker exec "$PG" psql -U postgres -d nakama -tAc "select collection||'/'||key from storage where collection<>'migration_checksum' order by 1")
[ "$rows" = "player_profile/level
wallet/gold" ] || fail "the stored rows are not what the module reported: $rows"
gold=$(docker exec "$PG" psql -U postgres -d nakama -tAc "select value->>'gold' from storage where collection='wallet'")
[ "$gold" = "600" ] || fail "the wallet holds $gold, not the changed 600"

echo "the target reads back what it holds"
rpc migration.inspect "{\"player_id\":\"$U1\",\"collections\":[\"player_profile\",\"wallet\"]}" | tee /tmp/mkit-inspect.json
python3 - <<'PY2' || fail "inspect does not agree with what was written"
import json
d = json.load(open("/tmp/mkit-inspect.json"))
by = {x["collection"] + "/" + x["key"]: x for x in d["documents"]}
assert set(by) == {"player_profile/level", "wallet/gold"}, f"wrong documents: {sorted(by)}"
assert by["wallet/gold"]["checksum"] == "c2-CHANGED", f"wrong checksum: {by['wallet/gold']['checksum']}"
assert json.loads(by["wallet/gold"]["value"])["gold"] == 600, f"wrong value: {by['wallet/gold']['value']}"
assert by["player_profile/level"]["public"] is False and by["wallet/gold"]["public"] is True, "permissions are not what was asked for"
print("inspect: 2 documents, the changed checksum, the changed value, the per-document permission")
PY2

echo "OK: one account, a second run that writes nothing, a dry run that writes nothing, one changed field written"
