#!/usr/bin/env bash
# Print a content-addressable tag for the unified builder/ci image: a short
# sha256 over the Dockerfile and everything COPY'd into it. The tag changes if
# and only if the recipe changes, so the CI job that builds/pushes the image
# and the jobs that pull it derive the same tag independently and never drift.
set -euo pipefail

cd "$(dirname "$0")/.."

# Files that define the image; keep in sync with hack/builder/Dockerfile COPYs.
files=(
    hack/builder/Dockerfile
    hack/builder/entrypoint.sh
    hack/builder/rsyncd.conf
    hack/builder/nsswitch.conf
    hack/builder/create_bazel_cache_rcs.sh
)

cat "${files[@]}" | sha256sum | cut -c1-12
