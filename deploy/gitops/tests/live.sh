#!/usr/bin/env bash
set -euo pipefail

cluster="${1:-autosql-conformance}"
context="kind-${cluster}"
plain_registry="autosql-${cluster}-flux-registry"
tls_registry="autosql-${cluster}-argo-registry"
plain_port="${AUTOSQL_FLUX_REGISTRY_PORT:-5001}"
tls_port="${AUTOSQL_ARGO_REGISTRY_PORT:-5002}"
tmp="$(mktemp -d)"

cleanup() {
  docker rm -f "$plain_registry" "$tls_registry" >/dev/null 2>&1 || true
  rm -rf "$tmp"
}
trap cleanup EXIT

for command in docker helm kubectl openssl; do
  command -v "$command" >/dev/null || {
    echo "$command is required" >&2
    exit 1
  }
done

kubectl --context "$context" get nodes >/dev/null

# Flux supports an explicitly insecure registry for isolated conformance. The
# shipped manifest remains keyless-Cosign verified against the public GHCR OCI
# chart; this registry only proves the controller-to-Helm reconciliation path.
docker run -d --name "$plain_registry" --network kind -p "${plain_port}:5000" registry:2 >/dev/null
plain_ip="$(docker inspect -f '{{(index .NetworkSettings.Networks "kind").IPAddress}}' "$plain_registry")"

# Argo CD's OCI client always uses HTTPS. Use an ephemeral self-signed endpoint
# and its repository-level insecure verification switch rather than weakening
# the registry transport to plain HTTP.
mkdir -p "$tmp/certs" "$tmp/chart"
openssl req -x509 -newkey rsa:2048 -nodes -days 1 \
  -keyout "$tmp/certs/tls.key" -out "$tmp/certs/tls.crt" \
  -subj "/CN=${tls_registry}" -addext "subjectAltName=DNS:${tls_registry}" >/dev/null 2>&1
docker run -d --name "$tls_registry" --network kind -p "${tls_port}:5000" \
  -v "$tmp/certs:/certs:ro" \
  -e REGISTRY_HTTP_TLS_CERTIFICATE=/certs/tls.crt \
  -e REGISTRY_HTTP_TLS_KEY=/certs/tls.key registry:2 >/dev/null
tls_ip="$(docker inspect -f '{{(index .NetworkSettings.Networks "kind").IPAddress}}' "$tls_registry")"

helm package deploy/helm/autosql-operator --destination "$tmp/chart" >/dev/null
chart="$tmp/chart/autosql-operator-0.1.0.tgz"
helm push "$chart" "oci://localhost:${plain_port}/autosql" --plain-http >/dev/null
helm push "$chart" "oci://localhost:${tls_port}/autosql" --insecure-skip-tls-verify >/dev/null

kubectl --context "$context" apply -f https://github.com/fluxcd/flux2/releases/download/v2.9.2/install.yaml >/dev/null
kubectl --context "$context" -n flux-system wait deployment/source-controller deployment/helm-controller \
  --for=condition=Available --timeout=5m
kubectl --context "$context" apply -f - <<YAML
apiVersion: source.toolkit.fluxcd.io/v1
kind: OCIRepository
metadata:
  name: autosql-conformance
  namespace: flux-system
spec:
  interval: 1m
  url: oci://${plain_ip}:5000/autosql/autosql-operator
  insecure: true
  ref: {tag: 0.1.0}
  layerSelector:
    mediaType: application/vnd.cncf.helm.chart.content.v1.tar+gzip
    operation: copy
---
apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: autosql-conformance
  namespace: flux-system
spec:
  interval: 1m
  targetNamespace: flux-autosql
  install: {createNamespace: true, crds: CreateReplace}
  upgrade: {crds: CreateReplace}
  chartRef:
    kind: OCIRepository
    name: autosql-conformance
  values:
    image: {repository: autosql, digest: "", tag: conformance, pullPolicy: Never}
YAML
kubectl --context "$context" -n flux-system wait ocirepository/autosql-conformance --for=condition=Ready --timeout=3m
kubectl --context "$context" -n flux-system wait helmrelease/autosql-conformance --for=condition=Ready --timeout=5m

helm repo add argo https://argoproj.github.io/argo-helm >/dev/null 2>&1 || true
helm repo update argo >/dev/null
helm upgrade --install argocd argo/argo-cd --version 10.1.4 --kube-context "$context" \
  --namespace argocd --create-namespace --set dex.enabled=false --set notifications.enabled=false \
  --set applicationSet.enabled=false --wait --timeout 8m >/dev/null
kubectl --context "$context" apply -f - <<YAML
apiVersion: v1
kind: Secret
metadata:
  name: autosql-conformance-oci
  namespace: argocd
  labels: {argocd.argoproj.io/secret-type: repository}
stringData:
  name: autosql-conformance
  type: helm
  enableOCI: "true"
  insecure: "true"
  url: ${tls_ip}:5000/autosql
---
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: autosql-conformance
  namespace: argocd
spec:
  project: default
  destination: {namespace: argo-autosql, server: https://kubernetes.default.svc}
  source:
    repoURL: ${tls_ip}:5000/autosql
    chart: autosql-operator
    targetRevision: 0.1.0
    helm:
      valuesObject:
        image: {repository: autosql, digest: "", tag: conformance, pullPolicy: Never}
  syncPolicy:
    automated: {prune: true, selfHeal: true}
    syncOptions: [CreateNamespace=true]
YAML
kubectl --context "$context" -n argocd wait application/autosql-conformance \
  --for=jsonpath='{.status.sync.status}'=Synced --timeout=5m
kubectl --context "$context" -n argocd wait application/autosql-conformance \
  --for=jsonpath='{.status.health.status}'=Healthy --timeout=5m

helm repo add crossplane-stable https://charts.crossplane.io/stable >/dev/null 2>&1 || true
helm repo update crossplane-stable >/dev/null
helm upgrade --install crossplane crossplane-stable/crossplane --version 2.3.3 --kube-context "$context" \
  --namespace crossplane-system --create-namespace --wait --timeout 8m >/dev/null
kubectl --context "$context" apply -f deploy/gitops/crossplane/function.yaml >/dev/null
kubectl --context "$context" wait function/function-patch-and-transform --for=condition=Healthy --timeout=8m
kubectl --context "$context" apply -f deploy/gitops/crossplane/definition.yaml >/dev/null
kubectl --context "$context" wait xrd/xautosqlschemas.autosql.io --for=condition=Established --timeout=3m
kubectl --context "$context" apply -f deploy/gitops/crossplane/rbac.yaml >/dev/null
kubectl --context "$context" apply -f deploy/gitops/crossplane/composition.yaml >/dev/null

digest="sha256:$(sha256sum examples/hcl-postgres/schema-v1.hcl | awk '{print $1}')"
kubectl --context "$context" -n argo-autosql create secret generic crossplane-source \
  --from-file=schema.hcl=examples/hcl-postgres/schema-v1.hcl --dry-run=client -o yaml | kubectl --context "$context" apply -f - >/dev/null
kubectl --context "$context" -n argo-autosql create secret generic crossplane-database \
  --from-literal=url='postgres://conformance:unused@127.0.0.1:1/autosql?sslmode=require' \
  --dry-run=client -o yaml | kubectl --context "$context" apply -f - >/dev/null
kubectl --context "$context" apply -f - <<YAML
apiVersion: autosql.io/v1alpha1
kind: XAutoSQLSchema
metadata:
  name: crossplane-conformance
  namespace: argo-autosql
spec:
  kind: DeclarativeSchema
  artifactDigest: ${digest}
  sourceSecretName: crossplane-source
  sourceSecretKey: schema.hcl
  databaseSecretName: crossplane-database
  databaseSecretKey: url
  suspend: true
YAML

for _ in $(seq 1 90); do
  reason="$(kubectl --context "$context" -n argo-autosql get xautosqlschema crossplane-conformance \
    -o jsonpath='{.status.autosqlConditions[?(@.type=="Ready")].reason}' 2>/dev/null || true)"
  test "$reason" = Suspended && break
  sleep 2
done

test "$(kubectl --context "$context" -n argo-autosql get xautosqlschema crossplane-conformance \
  -o jsonpath='{.status.conditions[?(@.type=="Synced")].status}')" = True
test "$(kubectl --context "$context" -n argo-autosql get xautosqlschema crossplane-conformance \
  -o jsonpath='{.status.autosqlConditions[?(@.type=="Ready")].reason}')" = Suspended
test "$(kubectl --context "$context" -n argo-autosql get autosqlschema crossplane-conformance \
  -o jsonpath='{.status.retryCount}')" = 0

# Prove the composition carries the operator's non-secret convergence/drift
# identity, not only conditions. The suspended fixture makes this status-only
# probe stable and guarantees no database connection is attempted.
applied_digest="sha256:$(printf autosql-crossplane-applied | sha256sum | awk '{print $1}')"
applied_fingerprint="sha256:$(printf autosql-crossplane-fingerprint | sha256sum | awk '{print $1}')"
kubectl --context "$context" -n argo-autosql patch autosqlschema crossplane-conformance \
  --subresource=status --type=merge \
  -p "{\"status\":{\"appliedDigest\":\"${applied_digest}\",\"appliedFingerprint\":\"${applied_fingerprint}\"}}" >/dev/null
for _ in $(seq 1 60); do
  observed="$(kubectl --context "$context" -n argo-autosql get xautosqlschema crossplane-conformance \
    -o jsonpath='{.status.appliedFingerprint}' 2>/dev/null || true)"
  test "$observed" = "$applied_fingerprint" && break
  sleep 2
done
test "$(kubectl --context "$context" -n argo-autosql get xautosqlschema crossplane-conformance \
  -o jsonpath='{.status.appliedDigest}')" = "$applied_digest"
test "$(kubectl --context "$context" -n argo-autosql get xautosqlschema crossplane-conformance \
  -o jsonpath='{.status.appliedFingerprint}')" = "$applied_fingerprint"

echo "Flux Ready, Argo CD Synced/Healthy, and Crossplane AutoSQL status propagation passed on ${context}."
