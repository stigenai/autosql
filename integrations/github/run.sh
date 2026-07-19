#!/usr/bin/env bash
set -euo pipefail
version="${INPUT_VERSION:-${GITHUB_ACTION_REF:-}}"
case "$version" in v[0-9]*.[0-9]*.[0-9]*) ;; *) echo 'AutoSQL version must be a release tag' >&2; exit 2;; esac
case "${INPUT_MODE:-verify}" in verify|run) ;; *) echo 'AutoSQL mode must be verify or run' >&2; exit 2;; esac
case "${INPUT_BINARY_SHA256:-}" in
  *[!0-9a-f]*|'') echo 'AutoSQL binary SHA-256 must be 64 lowercase hex characters' >&2; exit 2;;
esac
test "${#INPUT_BINARY_SHA256}" -eq 64 || { echo 'AutoSQL binary SHA-256 must be 64 lowercase hex characters' >&2; exit 2; }
case "$(uname -m)" in x86_64) arch=amd64;; aarch64|arm64) arch=arm64;; *) echo 'unsupported runner architecture' >&2; exit 2;; esac
name="autosql-${version}-linux-${arch}"
base="https://github.com/stigenai/autosql/releases/download/${version}"
work="${RUNNER_TEMP:?}/autosql-${version}"
mkdir -p "$work"
curl --fail --silent --show-error --location "$base/${name}.tar.gz" --output "$work/${name}.tar.gz"
(cd "$work" && printf '%s  %s\n' "$INPUT_BINARY_SHA256" "${name}.tar.gz" | sha256sum --check -)
tar -xzf "$work/${name}.tar.gz" -C "$work"
exec "$work/$name" integration "${INPUT_MODE:-verify}" --contract "${INPUT_CONTRACT:?}" --contract-digest "${INPUT_CONTRACT_DIGEST:?}" --json
