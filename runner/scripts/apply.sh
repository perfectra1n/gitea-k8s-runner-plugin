#!/bin/sh
# Apply runner/patches/*.patch to a pristine clone of gitea/runner at the tag in
# runner/UPSTREAM, under $BUILD_DIR/runner. An existing clone is reused and
# hard-reset, so this is always safe to re-run. Refuses to discard local
# commits made on top of the series unless FORCE=1 (run refresh.sh first).
set -eu

root=$(cd "$(dirname "$0")/../.." && pwd)
build=${BUILD_DIR:-$root/.build}
dir=$build/runner
upstream_url=${UPSTREAM_URL:-https://gitea.com/gitea/runner}
tag=$(tr -d '[:space:]' <"$root/runner/UPSTREAM")

if [ ! -d "$dir/.git" ]; then
  mkdir -p "$build"
  git clone --quiet --branch "$tag" --depth 1 "$upstream_url" "$dir"
fi

cd "$dir"
git am --abort >/dev/null 2>&1 || true
if [ "${FORCE:-0}" != 1 ] && [ -n "$(git status --porcelain)" ]; then
  echo "error: uncommitted changes in $dir; commit and refresh them, or FORCE=1 to discard" >&2
  exit 1
fi
if [ "${FORCE:-0}" != 1 ] && [ -f .git/series-applied ] &&
  [ "$(cat .git/series-applied)" != "$(git rev-parse HEAD)" ]; then
  echo "error: $dir has commits beyond the applied series; run 'mise run runner:refresh' to keep them or FORCE=1 to discard" >&2
  exit 1
fi

git fetch --quiet --depth 1 origin "refs/tags/$tag:refs/tags/$tag" 2>/dev/null || true
git checkout --quiet --force --no-track -B patched "refs/tags/$tag"
# A tag clone records `merge = refs/tags/...` for the branch, which go-git
# (used by upstream's tests to derive GITHUB_SHA/ref) rejects as invalid.
git config --remove-section branch.patched 2>/dev/null || true
git clean --quiet -fdx

set -- "$root"/runner/patches/*.patch
if [ -e "$1" ]; then
  # Patch authorship is preserved; only the committer needs an identity.
  git -c user.name="${GIT_COMMITTER_NAME:-gitea-k8s-runner-plugin}" \
    -c user.email="${GIT_COMMITTER_EMAIL:-noreply@github.com}" \
    am --quiet --committer-date-is-author-date "$@"
fi
git rev-parse HEAD >.git/series-applied
echo "applied $(git rev-list --count "refs/tags/$tag..HEAD") patch(es) onto gitea/runner $tag in $dir"
