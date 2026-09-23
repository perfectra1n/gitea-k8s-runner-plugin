#!/bin/sh
# Regenerate runner/patches/ from the commits on top of UPSTREAM in
# $BUILD_DIR/runner (the workflow: apply, commit in the clone, refresh).
set -eu

root=$(cd "$(dirname "$0")/../.." && pwd)
dir=${BUILD_DIR:-$root/.build}/runner
tag=$(tr -d '[:space:]' <"$root/runner/UPSTREAM")

cd "$dir"
if [ -n "$(git status --porcelain)" ]; then
  echo "error: uncommitted changes in $dir; commit them first" >&2
  exit 1
fi
rm -f "$root"/runner/patches/*.patch
git format-patch --quiet --no-signature --zero-commit --no-stat -o "$root/runner/patches" "refs/tags/$tag..HEAD"
git rev-parse HEAD >.git/series-applied
echo "wrote $(find "$root/runner/patches" -name '*.patch' | wc -l | tr -d ' ') patch(es) to runner/patches"
