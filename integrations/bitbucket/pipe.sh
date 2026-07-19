#!/usr/bin/env sh
set -eu
case "${MODE:-verify}" in verify|run) ;; *) echo 'MODE must be verify or run' >&2; exit 2;; esac
exec autosql integration "${MODE:-verify}" --contract "${CONTRACT:?CONTRACT is required}" --contract-digest "${CONTRACT_DIGEST:?CONTRACT_DIGEST is required}" --json
