#!/usr/bin/env sh
set -eu
chart="$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)"
helm lint "$chart"
rendered="$(helm template acceptance "$chart" --namespace autosql-system)"
printf '%s' "$rendered" | grep -q 'kind: Deployment'
printf '%s' "$rendered" | grep -q 'kind: ValidatingAdmissionPolicy'
printf '%s' "$rendered" | grep -q 'kind: Role'
if printf '%s' "$rendered" | grep -q 'kind: ClusterRole'; then
  echo 'default install requested cluster-wide RBAC' >&2
  exit 1
fi
printf '%s' "$rendered" | grep -Eq 'image: "?ghcr.io/stigenai/autosql@sha256:[0-9a-f]{64}"?'
if printf '%s' "$rendered" | grep -q ':latest'; then
  echo 'mutable latest image rendered' >&2
  exit 1
fi
identity="$(helm template acceptance "$chart" --namespace autosql-system --set workloadIdentity.audience=sts.amazonaws.com)"
printf '%s' "$identity" | grep -q 'audience: "sts.amazonaws.com"'
