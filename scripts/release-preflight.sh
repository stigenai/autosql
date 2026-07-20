#!/usr/bin/env bash

set -euo pipefail

errors=0

require_core() {
  local name="$1"
  if [[ -z "${!name:-}" ]]; then
    echo "required core publisher configuration is missing: ${name}" >&2
    errors=1
  fi
}

publisher_enabled() {
  local flag="$1"
  local value="${!flag:-false}"
  case "$value" in
    true)
      return 0
      ;;
    false|'')
      return 1
      ;;
    *)
      echo "publisher flag ${flag} must be 'true' or 'false', got '${value}'" >&2
      errors=1
      return 1
      ;;
  esac
}

check_publisher() {
  local flag="$1"
  local publisher="$2"
  shift 2

  if ! publisher_enabled "$flag"; then
    echo "optional publisher disabled: ${publisher} (${flag})"
    return
  fi

  echo "optional publisher enabled: ${publisher} (${flag})"
  local name
  for name in "$@"; do
    if [[ -z "${!name:-}" ]]; then
      echo "enabled ${publisher} publisher configuration is missing: ${name}" >&2
      errors=1
    fi
  done
}

require_core GHCR_TOKEN

check_publisher PUBLISH_TERRAFORM_PROVIDER terraform \
  TERRAFORM_PROVIDER_GITHUB_TOKEN \
  TERRAFORM_REGISTRY_GPG_PRIVATE_KEY \
  TERRAFORM_REGISTRY_GPG_PASSPHRASE
check_publisher PUBLISH_CIRCLECI circleci CIRCLECI_CLI_TOKEN
check_publisher PUBLISH_GITLAB gitlab \
  GITLAB_CATALOG_TOKEN \
  GITLAB_CATALOG_REPOSITORY_URL \
  GITLAB_CATALOG_PROJECT_ID \
  GITLAB_API_URL
check_publisher PUBLISH_AZURE_DEVOPS azure-devops \
  AZURE_DEVOPS_EXT_PAT \
  AZURE_DEVOPS_PUBLISHER
check_publisher PUBLISH_BITBUCKET bitbucket \
  BITBUCKET_PIPE_TOKEN \
  BITBUCKET_PIPE_USERNAME \
  BITBUCKET_PIPE_REPOSITORY_URL

if (( errors != 0 )); then
  exit 1
fi

echo "release publisher preflight passed"
