#!/usr/bin/env bash
set -euo pipefail
: "${TARGET_BRANCH:?}" "${FORK_LEASE:?}" "${TESTED_SHA:?}"
[[ $(git rev-parse HEAD) == "$TESTED_SHA" ]]
if [[ $FORK_LEASE == "$TESTED_SHA" ]]; then
  echo "Nothing rebased; $TARGET_BRANCH already at $TESTED_SHA."
  exit 0
fi
# ai-gate pins Home by commit SHA and rollback rebuilds from source, so the
# replaced head must stay fetchable. Branches, not tags: tags trigger release.yaml.
archive="refs/heads/archive/home-${FORK_LEASE:0:12}"
if [[ -z $(git ls-remote origin "$archive") ]]; then
  git push "--force-with-lease=$archive:" origin "$FORK_LEASE:$archive"
fi
git push "--force-with-lease=refs/heads/$TARGET_BRANCH:$FORK_LEASE" origin "HEAD:refs/heads/$TARGET_BRANCH"
