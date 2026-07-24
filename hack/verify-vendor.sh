#!/usr/bin/env bash
#
# Verifies the committed vendor directory is in sync with go.work.
#
# go work vendor strips the bazel-generated BUILD.bazel files that upstream
# keeps in vendor/, so they are excluded from the comparison and the original
# vendor/ is restored afterwards to keep the checkout clean.
#
# TODO: the canonical check is `make deps-sync` (go mod tidy over the staging
# modules, go work vendor + sync, then hack/bazel-generate.sh to regenerate
# BUILD.bazel via gazelle) followed by hack/verify-generate.sh, and
# `make generate` + `make generate-verify` for generated files. Both run in
# the builder image (hack/builder, now Go 1.25); switch to them once that
# image is rebuilt and published to a registry the CI runners can pull.

set -e

cp -a vendor /tmp/vendor.orig
go work vendor
rc=0
diff -r -q --exclude=BUILD.bazel /tmp/vendor.orig vendor >/tmp/vendor.diff || rc=$?
rm -rf vendor && mv /tmp/vendor.orig vendor
if [ "$rc" -ne 0 ]; then
    cat /tmp/vendor.diff
    echo "vendor/ is out of sync with go.work: run go work vendor and restore the BUILD.bazel files"
    exit 1
fi
echo "vendor/ is in sync with go.work"
