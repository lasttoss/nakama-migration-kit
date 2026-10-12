#!/usr/bin/env bash
# The image is a deliverable, so it is run rather than inspected.
#
# Three things are asserted, and the first one is the point of distroless: there is no shell in the
# image, so nothing that needs one can be run against a live title's data by accident.
set -euo pipefail
cd "$(dirname "$0")/.."

IMAGE=${IMAGE:-nakama-migration-kit:local}
fail() { echo "FAIL: $*" >&2; exit 1; }
trap 'echo "FAIL: line $LINENO: $BASH_COMMAND" >&2' ERR

echo "building $IMAGE"
docker build --quiet -t "$IMAGE" . >/dev/null

if docker run --rm --entrypoint /bin/sh "$IMAGE" -c true >/dev/null 2>&1; then
  fail "the image has a shell in it"
fi
echo "  no shell in the image"

user=$(docker inspect --format '{{.Config.User}}' "$IMAGE")
[ "$user" = "nonroot" ] || fail "the image runs as $user"
echo "  runs as $user"

if out=$(docker run --rm "$IMAGE" 2>&1); then
  fail "the importer ran with no arguments and reported success"
fi
printf '%s' "$out" | grep -q -- "-dsn is required" || fail "the importer did not say why it refused: $out"
echo "  the importer refuses to start without a database, and says so"

if ! docker run --rm --entrypoint /faketenant "$IMAGE" -help 2>&1 | grep -q -- "-players"; then
  fail "the fake title binary is not in the image"
fi
echo "  the fake title binary is in the image"

echo "OK: the image runs, as nonroot, without a shell"
