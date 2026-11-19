#!/usr/bin/env bash
set -euo pipefail
# Apply ai-gate's tracked patches to the candidate and run the same Go tests as
# ai-gate's image build. The full image build runs on the resulting ai-gate PR.
: "${AI_GATE_DIR:?}" "${TESTED_SHA:?}" "${RUNNER_TEMP:?}"
[[ $(git rev-parse HEAD) == "$TESTED_SHA" ]]
# The build script fetches by SHA, falling back to all branches; keep the
# candidate on a local branch so a detached checkout is still fetchable.
git branch -f fork-ci-candidate HEAD
printf '<!doctype html><title>fork-ci</title>\n' > "$RUNNER_TEMP/management.html"
HOME_REPO="$PWD" HOME_COMMIT="$TESTED_SHA" WORK_DIR="$RUNNER_TEMP/home-candidate" \
  PREPARE_ONLY=true RUN_TESTS=true REFRESH_MODELS=false \
  MANAGEMENT_HTML_PATH="$RUNNER_TEMP/management.html" \
  bash "$AI_GATE_DIR/deploy/home/build-home-image.sh"
