#!/usr/bin/env sh
set -eu
chart="$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)"
kube_version="${AUTOSQL_HELM_TEST_KUBE_VERSION:-1.35.5}"
helm lint "$chart" --kube-version "$kube_version"
rendered="$(helm template acceptance "$chart" --namespace autosql-system --kube-version "$kube_version")"
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
identity="$(helm template acceptance "$chart" --namespace autosql-system --kube-version "$kube_version" --set workloadIdentity.audience=sts.amazonaws.com)"
printf '%s' "$identity" | grep -q 'audience: "sts.amazonaws.com"'
printf '%s' "$rendered" | grep -q 'name: acceptance-autosql-operator-metrics'
printf '%s' "$rendered" | grep -q -- '--metrics-bind-address=:8080'
printf '%s' "$rendered" | grep -q -- '--health-probe-bind-address=:8081'
printf '%s' "$rendered" | grep -q 'path: /readyz'
printf '%s' "$rendered" | grep -q 'prometheus.io/scrape: "true"'
disabled="$(helm template acceptance "$chart" --namespace autosql-system --set metrics.enabled=false --set healthProbe.enabled=false)"
printf '%s' "$disabled" | grep -q -- '--metrics-bind-address=0'
printf '%s' "$disabled" | grep -q -- '--health-probe-bind-address=0'
if printf '%s' "$disabled" | grep -Eq 'kind: Service$|livenessProbe:|readinessProbe:|containerPort:'; then
  echo 'disabled metrics or probes still rendered' >&2
  exit 1
fi
custom="$(helm template acceptance "$chart" --namespace autosql-system --set metrics.port=9090 --set healthProbe.port=9091 --set metrics.service.enabled=false)"
printf '%s' "$custom" | grep -q -- '--metrics-bind-address=:9090'
printf '%s' "$custom" | grep -q -- '--health-probe-bind-address=:9091'
if printf '%s' "$custom" | grep -q 'kind: Service$'; then
  echo 'disabled metrics Service still rendered' >&2
  exit 1
fi
if helm template acceptance "$chart" --set healthProbe.port=8080 >/dev/null 2>&1; then
  echo 'conflicting endpoint ports accepted' >&2
  exit 1
fi
