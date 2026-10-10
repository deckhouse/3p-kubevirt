#!/usr/bin/env bash
#
# Updates Go dependencies and the vendor tree for the workspace.
#
# Default (no args): sync-only -- go mod tidy across the workspace modules,
# refresh vendor/ and go.work.sum, without bumping any versions.
# Pass extra args through to go get, e.g. `task update -- -u` to upgrade.

set -e

# go work vendor (run by dep-update.sh) strips the bazel-generated BUILD.bazel
# files that upstream keeps under vendor/. There is no bazel/gazelle in this
# image to regenerate them, so preserve and restore the existing ones; new
# vendored packages will still lack a BUILD.bazel (see the note below).
bazel_backup=/tmp/vendor-bazel.tar
find vendor -name BUILD.bazel -print0 2>/dev/null | tar --null -cf "$bazel_backup" -T - 2>/dev/null || true

if [ "$#" -eq 0 ]; then
    bash hack/dep-update.sh --sync-only
else
    bash hack/dep-update.sh "$@"
fi

tar -xf "$bazel_backup" 2>/dev/null || true
rm -f "$bazel_backup"

# Hand ownership of the touched files back to the checkout owner (the
# container runs as root over the bind mount).
owner="$(stat -c '%u:%g' .)"
chown -R "$owner" vendor go.work.sum staging 2>/dev/null || true

cat <<'EOF'

Dependencies and vendor/ updated.
NOTE: code generation (deepcopy, mocks, swagger, protobuf) and BUILD.bazel
files for newly vendored packages are produced by the bazel toolchain in the
builder image, which is not part of this container. Run `make generate` /
`make deps-sync` once the builder image is rebuilt with Go 1.25 and published.
EOF
