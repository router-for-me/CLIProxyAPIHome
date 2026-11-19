#!/usr/bin/env bash
set -euo pipefail

# Checkout is pinned by the workflow. Capture the branch lease BEFORE rebasing;
# a maintainer push during tests must never be overwritten by the bot.
: "${GITHUB_OUTPUT:?}" "${TARGET_BRANCH:?}" "${UPSTREAM_REPO:?}" "${UPSTREAM_BRANCH:?}"
git fetch origin "$TARGET_BRANCH"
lease=$(git rev-parse FETCH_HEAD)
echo "fork_lease=$lease" >> "$GITHUB_OUTPUT"
if [[ ${GITHUB_EVENT_NAME:-} == push && $(git rev-parse HEAD) != "$lease" ]]; then
  echo "Obsolete push; a newer fork head already exists."
  echo "should_build=false" >> "$GITHUB_OUTPUT"
  exit 0
fi
if [[ ${GITHUB_EVENT_NAME:-} != push && $(git rev-parse HEAD) != "$lease" ]]; then
  echo "::error::Target changed since checkout; rerun against the current head."
  exit 1
fi
git fetch "$UPSTREAM_REPO" "$UPSTREAM_BRANCH"
upstream=$(git rev-parse FETCH_HEAD)
if [[ ${GITHUB_EVENT_NAME:-} != push && ${SKIP_REBASE:-false} != true ]]; then
  behind=$(git rev-list --count "HEAD..$upstream")
  if [[ $behind == 0 && ${GITHUB_EVENT_NAME:-} == schedule && ${FORCE_BUILD:-false} != true ]]; then
    echo "should_build=false" >> "$GITHUB_OUTPUT"
    exit 0
  fi
  echo "upstream_sha=$upstream" >> "$GITHUB_OUTPUT"
  echo "fork_sha=$(git rev-parse HEAD)" >> "$GITHUB_OUTPUT"
  if ! git rebase "$upstream"; then
    {
      echo 'conflict_files<<FORK_CONFLICT_FILES'
      git diff --name-only --diff-filter=U
      echo 'FORK_CONFLICT_FILES'
      echo "conflict_commit=$(git log -1 --format='%h %s' REBASE_HEAD 2>/dev/null || echo unknown)"
      echo 'should_build=false'
      echo 'conflict=true'
    } >> "$GITHUB_OUTPUT"
    git rebase --abort
    exit 1
  fi
fi
{
  echo "upstream_base=$(git merge-base HEAD "$upstream")"
  echo "commit_sha=$(git rev-parse HEAD)"
  echo "commit_short_sha=$(git rev-parse --short=7 HEAD)"
  echo 'should_build=true'
} >> "$GITHUB_OUTPUT"
