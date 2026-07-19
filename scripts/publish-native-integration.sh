#!/usr/bin/env bash
set -euo pipefail

platform="${1:-}"
version="${2:-}"
root="${3:-}"
check_only="${4:-}"

case "$version" in v[0-9]*.[0-9]*.[0-9]*) ;; *) echo 'version must be vMAJOR.MINOR.PATCH' >&2; exit 2;; esac
test -d "$root" || { echo 'extracted integration root is required' >&2; exit 2; }
semver="${version#v}"

require_env() {
  value="${!1:-}"
  test -n "$value" || { echo "$1 is required" >&2; exit 2; }
}

require_command() {
  command -v "$1" >/dev/null || { echo "$1 is required" >&2; exit 2; }
}

publish_git_mirror() {
  source_dir="$1"
  repository_url="$2"
  auth_header="$3"
  tag="$4"
  mirror="$(mktemp -d)"
  git_with_auth() {
    GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=http.extraHeader GIT_CONFIG_VALUE_0="$auth_header" git "$@"
  }
  git_with_auth clone --quiet "$repository_url" "$mirror"
  find "$mirror" -mindepth 1 -maxdepth 1 ! -name .git -exec rm -rf -- {} +
  rsync -a --delete --exclude .git/ "$source_dir/" "$mirror/"
  git -C "$mirror" config user.name "AutoSQL Release"
  git -C "$mirror" config user.email "release@autosql.io"
  git -C "$mirror" add --all
  if ! git -C "$mirror" diff --cached --quiet; then
    git -C "$mirror" commit --quiet -m "Release AutoSQL ${tag}"
  fi
  if git -C "$mirror" rev-parse -q --verify "refs/tags/${tag}" >/dev/null; then
    test "$(git -C "$mirror" rev-parse "refs/tags/${tag}^{}")" = "$(git -C "$mirror" rev-parse HEAD)" || {
      echo "existing ${tag} does not identify the packaged source" >&2
      exit 1
    }
  else
    git -C "$mirror" tag -a "$tag" -m "AutoSQL ${tag}"
  fi
  git_with_auth -C "$mirror" push --quiet origin HEAD:main "$tag"
}

case "$platform" in
  circleci)
    orb="$root/integrations/circleci/orb.yml"
    test -f "$orb" || { echo 'CircleCI orb source is missing' >&2; exit 2; }
    if test "$check_only" = --check; then exit 0; fi
    require_env CIRCLECI_CLI_TOKEN
    require_command circleci
    namespace="${CIRCLECI_ORB_NAMESPACE:-stigenai}"
    circleci orb validate "$orb"
    published="$(mktemp)"
    if circleci orb source "${namespace}/autosql@${semver}" >"$published" 2>/dev/null; then
      diff -u "$published" "$orb" >/dev/null || { echo 'published CircleCI orb differs from this release' >&2; exit 1; }
    else
      circleci orb publish "$orb" "${namespace}/autosql@${semver}"
    fi
    ;;
  azure)
    manifest="$root/integrations/azure-devops/vss-extension.json"
    test -f "$manifest" || { echo 'Azure extension manifest is missing' >&2; exit 2; }
    if test "$check_only" = --check; then exit 0; fi
    require_env AZURE_DEVOPS_EXT_PAT
    require_env AZURE_DEVOPS_PUBLISHER
    require_command tfx
    require_command unzip
    vsix="$(mktemp -d)/stigenai.autosql-${semver}.vsix"
    manifest_dir="$(cd "$(dirname "$manifest")" && pwd)"
    (cd "$manifest_dir" && tfx extension create --manifest-globs vss-extension.json --output-path "$vsix" --no-prompt)
    test -f "$vsix" || { echo 'Azure extension package was not created' >&2; exit 1; }
    unzip -Z1 "$vsix" | grep -qx 'tasks/autosql/task.json' || { echo 'Azure task is not rooted at tasks/autosql in the VSIX' >&2; exit 1; }
    if ! tfx extension show --publisher "$AZURE_DEVOPS_PUBLISHER" --vsix "$vsix" --auth-type pat -t "$AZURE_DEVOPS_EXT_PAT" --no-prompt >/dev/null 2>&1; then
      tfx extension publish --publisher "$AZURE_DEVOPS_PUBLISHER" --vsix "$vsix" --auth-type pat -t "$AZURE_DEVOPS_EXT_PAT" --no-prompt
    fi
    ;;
  gitlab)
    test -f "$root/templates/autosql/template.yml" || { echo 'GitLab component template is missing' >&2; exit 2; }
    test -f "$root/integrations/gitlab/README.md" || { echo 'GitLab component README is missing' >&2; exit 2; }
    if test "$check_only" = --check; then exit 0; fi
    require_env GITLAB_CATALOG_TOKEN
    require_env GITLAB_CATALOG_REPOSITORY_URL
    require_env GITLAB_CATALOG_PROJECT_ID
    require_env GITLAB_API_URL
    require_command git
    require_command rsync
    require_command curl
    case "$GITLAB_CATALOG_REPOSITORY_URL $GITLAB_API_URL" in https://*) ;; *) echo 'GitLab endpoints must use HTTPS' >&2; exit 2;; esac
    source_dir="$(mktemp -d)"
    mkdir -p "$source_dir/templates"
    cp -Rf "$root/templates/autosql" "$source_dir/templates/autosql"
    cp -f "$root/integrations/gitlab/README.md" "$source_dir/README.md"
    publish_git_mirror "$source_dir" "$GITLAB_CATALOG_REPOSITORY_URL" "Authorization: Bearer ${GITLAB_CATALOG_TOKEN}" "$semver"
    release_url="${GITLAB_API_URL%/}/projects/${GITLAB_CATALOG_PROJECT_ID}/releases/${semver}"
    if ! curl --fail --silent --show-error --header "PRIVATE-TOKEN: ${GITLAB_CATALOG_TOKEN}" "$release_url" >/dev/null 2>&1; then
      curl --fail --silent --show-error --request POST \
        --header "PRIVATE-TOKEN: ${GITLAB_CATALOG_TOKEN}" \
        --data-urlencode "name=AutoSQL ${semver}" --data-urlencode "tag_name=${semver}" \
        --data-urlencode "description=Signed AutoSQL CI/CD Catalog component ${semver}" \
        "${GITLAB_API_URL%/}/projects/${GITLAB_CATALOG_PROJECT_ID}/releases" >/dev/null
    fi
    ;;
  bitbucket)
    source_dir="$root/integrations/bitbucket"
    test -f "$source_dir/pipe.yml" && test -f "$source_dir/Dockerfile" || { echo 'Bitbucket Pipe source is missing' >&2; exit 2; }
    if test "$check_only" = --check; then exit 0; fi
    require_env BITBUCKET_PIPE_TOKEN
    require_env BITBUCKET_PIPE_USERNAME
    require_env BITBUCKET_PIPE_REPOSITORY_URL
    require_command git
    require_command rsync
    case "$BITBUCKET_PIPE_REPOSITORY_URL" in https://bitbucket.org/*) ;; *) echo 'Bitbucket repository must use bitbucket.org HTTPS' >&2; exit 2;; esac
    basic="$(printf '%s:%s' "$BITBUCKET_PIPE_USERNAME" "$BITBUCKET_PIPE_TOKEN" | base64 | tr -d '\n')"
    publish_git_mirror "$source_dir" "$BITBUCKET_PIPE_REPOSITORY_URL" "Authorization: Basic ${basic}" "$version"
    ;;
  *)
    echo 'platform must be circleci, azure, gitlab, or bitbucket' >&2
    exit 2
    ;;
esac
