#!/usr/bin/env bash
set -euo pipefail
# Runs inside an ai-gate checkout. Bumps only the Home source pin; ai-gate's own
# workflows build, test and later propose the digest-pinned images.
: "${DEPLOY_REPO:?}" "${DEPLOY_BRANCH:?}" "${NEW_SHA:?}" "${UPSTREAM_BASE:?}" "${FORK_DIR:?}" "${FORK_REPO:?}" "${RUN_URL:?}"
[[ $NEW_SHA =~ ^[0-9a-f]{40}$ ]]
workflow=.github/workflows/build-home-image.yml
script=deploy/home/build-home-image.sh
old=$(sed -n 's/^  HOME_COMMIT: \([0-9a-f]\{40\}\)$/\1/p' "$workflow")
[[ $old =~ ^[0-9a-f]{40}$ ]] || { echo "::error::Cannot find HOME_COMMIT in $workflow"; exit 1; }
grep -qxF "HOME_COMMIT=\"\${HOME_COMMIT:-$old}\"" "$script" || { echo "::error::$script and $workflow pin different Home commits"; exit 1; }
if [[ $old == "$NEW_SHA" ]]; then
  echo "ai-gate already pins $NEW_SHA"
  exit 0
fi
sed -i "s/^  HOME_COMMIT: $old\$/  HOME_COMMIT: $NEW_SHA/" "$workflow"
sed -i "s/^HOME_COMMIT=\"\${HOME_COMMIT:-$old}\"\$/HOME_COMMIT=\"\${HOME_COMMIT:-$NEW_SHA}\"/" "$script"
[[ $(git diff --name-only | sort | paste -sd' ') == "$(printf '%s\n' "$workflow" "$script" | sort | paste -sd' ')" ]]

body=$(mktemp)
{
  echo "Rebase \`$FORK_REPO\` onto upstream and pin the tested commit for the Home image build."
  echo
  echo "- Home source: \`${old:0:12}\` → \`${NEW_SHA:0:12}\`"
  echo "- Upstream base: \`${UPSTREAM_BASE:0:12}\`"
  echo "- Fork CI (rebase, ai-gate patches, Go tests): $RUN_URL"
  echo
  echo "### New upstream commits"
  echo '```'
  old_base=$(git -C "$FORK_DIR" merge-base "$old" "$UPSTREAM_BASE" 2>/dev/null || true)
  if [[ -n $old_base ]]; then
    git -C "$FORK_DIR" log --oneline --no-merges "$old_base..$UPSTREAM_BASE" | head -100
  else
    echo "previous pin $old is not available in the fork checkout"
  fi
  echo '```'
  echo
  echo "### Fork commits carried on top"
  echo '```'
  git -C "$FORK_DIR" log --oneline "$UPSTREAM_BASE..$NEW_SHA"
  echo '```'
  echo
  echo "No action needed: ai-gate's **Merge Tested Home Source Bump** merges this PR once **Build Home and Exporter Images** passes on its head, then publishes digest-pinned images and opens a \`deploy/home-images-*\` PR. Production changes only when that PR is merged."
  echo "The previous fork head stays fetchable as \`archive/home-<sha12>\` for rebuilds and rollback."
} > "$body"

# Capture the existing proposal branch; an empty lease permits creation only.
lease=$(git ls-remote origin "refs/heads/$DEPLOY_BRANCH" | awk '{print $1}')
git config user.name 'github-actions[bot]'
git config user.email 'github-actions[bot]@users.noreply.github.com'
git switch -c "$DEPLOY_BRANCH"
git add "$workflow" "$script"
git commit -m "deploy: bump Home source to ${NEW_SHA:0:12}"
git push "--force-with-lease=refs/heads/$DEPLOY_BRANCH:$lease" origin "HEAD:refs/heads/$DEPLOY_BRANCH"
title="deploy: bump Home source ${old:0:7} → ${NEW_SHA:0:7}"
existing=$(gh pr list --repo "$DEPLOY_REPO" --head "$DEPLOY_BRANCH" --state open --json number --jq '.[0].number')
if [[ -n $existing ]]; then
  gh pr edit "$existing" --repo "$DEPLOY_REPO" --title "$title" --body-file "$body"
else
  gh pr create --repo "$DEPLOY_REPO" --base main --head "$DEPLOY_BRANCH" --title "$title" --body-file "$body"
fi
