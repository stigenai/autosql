#!/usr/bin/env bash

set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
preflight="${repo_root}/scripts/release-preflight.sh"

fail() {
  echo "release preflight test failed: $*" >&2
  exit 1
}

run_isolated() {
  env -i PATH="$PATH" "$@" "$preflight"
}

output="$(run_isolated GHCR_TOKEN=test-token)"
grep -q 'release publisher preflight passed' <<<"$output" || fail "core-only release did not pass"
grep -q 'optional publisher disabled: terraform' <<<"$output" || fail "disabled publishers were not reported"

if output="$(run_isolated 2>&1)"; then
  fail "missing core credentials passed"
fi
grep -q 'required core publisher configuration is missing: GHCR_TOKEN' <<<"$output" || fail "missing core credential was not identified"

while read -r flag publisher credential; do
  if output="$(run_isolated GHCR_TOKEN=test-token "${flag}=true" 2>&1)"; then
    fail "${publisher} passed without credentials"
  fi
  grep -q "enabled ${publisher} publisher configuration is missing: ${credential}" <<<"$output" || fail "${publisher} missing credential was not identified"
done <<'CASES'
PUBLISH_TERRAFORM_PROVIDER terraform TERRAFORM_PROVIDER_GITHUB_TOKEN
PUBLISH_CIRCLECI circleci CIRCLECI_CLI_TOKEN
PUBLISH_GITLAB gitlab GITLAB_CATALOG_TOKEN
PUBLISH_AZURE_DEVOPS azure-devops AZURE_DEVOPS_EXT_PAT
PUBLISH_BITBUCKET bitbucket BITBUCKET_PIPE_TOKEN
CASES

run_isolated \
  GHCR_TOKEN=test-token \
  PUBLISH_TERRAFORM_PROVIDER=true \
  TERRAFORM_PROVIDER_GITHUB_TOKEN=test-token \
  TERRAFORM_REGISTRY_GPG_PRIVATE_KEY=test-key \
  TERRAFORM_REGISTRY_GPG_PASSPHRASE=test-passphrase \
  PUBLISH_CIRCLECI=true \
  CIRCLECI_CLI_TOKEN=test-token \
  PUBLISH_GITLAB=true \
  GITLAB_CATALOG_TOKEN=test-token \
  GITLAB_CATALOG_REPOSITORY_URL=https://gitlab.example/autosql.git \
  GITLAB_CATALOG_PROJECT_ID=1 \
  GITLAB_API_URL=https://gitlab.example/api/v4 \
  PUBLISH_AZURE_DEVOPS=true \
  AZURE_DEVOPS_EXT_PAT=test-token \
  AZURE_DEVOPS_PUBLISHER=autosql \
  PUBLISH_BITBUCKET=true \
  BITBUCKET_PIPE_TOKEN=test-token \
  BITBUCKET_PIPE_USERNAME=autosql \
  BITBUCKET_PIPE_REPOSITORY_URL=https://bitbucket.org/autosql/pipe.git \
  >/dev/null

if output="$(run_isolated GHCR_TOKEN=test-token PUBLISH_CIRCLECI=yes 2>&1)"; then
  fail "invalid publisher flag passed"
fi
grep -q "publisher flag PUBLISH_CIRCLECI must be 'true' or 'false'" <<<"$output" || fail "invalid flag was not identified"

echo "release preflight tests passed"
