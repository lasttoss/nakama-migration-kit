#!/usr/bin/env bash
# Lint the chart, render every branch, and validate the result.
#
# The branches are the point: a chart whose only tested path is the default one has an untested path for
# every value a user sets - and this chart's whole job is to be run by somebody else, against their
# title, with their secrets.
set -euo pipefail
cd "$(dirname "$0")/.."
chart=charts/nakama-migration-kit

helm lint --strict "$chart"

render() {
  local label=$1; shift
  local manifest
  manifest=$(helm template "$chart" -f lab/values-kind.yaml "$@" 2>&1) || { echo "render failed: $label"; echo "$manifest"; exit 1; }
  printf '%s\n' "$manifest" | docker run --rm -i ghcr.io/yannh/kubeconform:v0.7.0 \
    -kubernetes-version 1.31.0 -strict -summary - || { echo "invalid manifest: $label"; exit 1; }
  printf '  %s\n' "$label"
}

echo "rendering every branch:"
render "defaults (import Job, profile ConfigMap, ServiceAccount)"
render "reconcile.enabled=true" --set reconcile.enabled=true
render "an existing profile ConfigMap and an existing ServiceAccount" \
  --set profile.inline=null --set profile.existingConfigMap=my-profile \
  --set serviceAccount.create=false --set serviceAccount.name=migrator
render "a dry run against the fake title, with no PlayFab secret" \
  --set import.dryRun=true --set playfab.fakeTenant.enabled=true --set playfab.secretKeySecret.name=""

# The DSN has to come from a Secret. A chart that also accepted a value for it would put a live
# database password in somebody's git history, and the person who does that is not being careless -
# they are using the option that was offered.
if helm template "$chart" -f lab/values-kind.yaml --set dsn.existingSecret= 2>/dev/null | grep -q "MIGRATION_DSN"; then
  echo "the chart rendered without a dsn Secret" >&2
  exit 1
fi
printf '  %s\n' "refuses to render without a dsn Secret"

echo "chart ok: every branch rendered and validated"
