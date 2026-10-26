#!/bin/sh
set -eu

SCRIPT_DIR="$(CDPATH= cd -- "$(dirname "$0")" && pwd)"
REPO_ROOT="$(CDPATH= cd -- "$SCRIPT_DIR/../../../.." && pwd)"
DOCKERFILE="$REPO_ROOT/Cubelet/Dockerfile"
ENTRYPOINT="$REPO_ROOT/deploy/kubernetes/images/scripts/component-entrypoint.sh"

grep -Fq 'ENV PATH="/usr/local/services/cubetoolbox/Cubelet/bin:/usr/local/services/cubetoolbox/cube-shim/bin:${PATH}"' \
  "$DOCKERFILE" || {
    echo "cubelet image must expose the staged Cubelet bin directory through PATH" >&2
    exit 1
  }

grep -Fq '[[ -x "${dst}/bin/cubecli" ]] || fail "missing cubecli after stage"' \
  "$ENTRYPOINT" || {
    echo "cubelet staging must fail when cubecli is missing" >&2
    exit 1
  }

if grep -Fq 'ln -sfn "${dst}/bin/cubecli" /usr/local/bin/cubecli' "$ENTRYPOINT"; then
  echo "cubelet staging should not need a container-side cubecli symlink" >&2
  exit 1
fi

echo "Cubelet cubecli PATH guard passed"
