#!/usr/bin/env bash
set -euo pipefail
: "${GITHUB_REPOSITORY:?}" "${UPSTREAM_BRANCH:?}" "${RUN_URL:?}"
label=upstream-sync-conflict
if [[ ${1:-} == close ]]; then
  for number in $(gh issue list --repo "$GITHUB_REPOSITORY" --label "$label" --state open --json number --jq '.[].number'); do
    gh issue close "$number" --repo "$GITHUB_REPOSITORY" --comment "Rebase and tests passed for upstream/$UPSTREAM_BRANCH in $RUN_URL."
  done
  exit 0
fi
: "${UPSTREAM_SHA:?}" "${FORK_SHA:?}" "${TARGET_BRANCH:?}"
label_args=(--label "$label")
if ! gh label list --repo "$GITHUB_REPOSITORY" --json name --jq '.[].name' | grep -qx "$label"; then
  if ! gh label create "$label" --repo "$GITHUB_REPOSITORY" --color B60205 --description 'Nightly upstream rebase failed'; then
    echo '::warning::Cannot create conflict label; filing an unlabelled issue'
    label_args=()
  fi
fi
title="Upstream sync conflict: $UPSTREAM_BRANCH@$UPSTREAM_SHA vs fork $FORK_SHA"
body=$(cat <<EOF
Rebase of \`$TARGET_BRANCH\` onto \`upstream/$UPSTREAM_BRANCH\` failed.

Fork: \`$FORK_SHA\`
Upstream: \`$UPSTREAM_SHA\`
First conflicting patch: \`${CONFLICT_COMMIT:-unknown}\`
Run: $RUN_URL

Conflicting files:
\`\`\`
${CONFLICT_FILES:-unknown}
\`\`\`

No ai-gate proposal is opened. Production stays on its pinned Home commit.
Resolve locally, verify with ai-gate's \`deploy/home/build-home-image.sh\` (RUN_TESTS=true), then push with an explicit lease.
EOF
)
existing=$(gh issue list --repo "$GITHUB_REPOSITORY" --label "$label" --state open --json number --jq '.[0].number')
if [[ -n $existing ]]; then
  gh issue edit "$existing" --repo "$GITHUB_REPOSITORY" --title "$title" --body "$body"
else
  gh issue create --repo "$GITHUB_REPOSITORY" --title "$title" --body "$body" "${label_args[@]}"
fi
