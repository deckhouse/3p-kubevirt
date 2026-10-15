#!/usr/bin/env bash

set -ex

export GO111MODULE=on
export _sync_only="false"

while true; do
    case "$1" in
    -s | --sync-only)
        _sync_only="true"
        shift 1
        ;;
    --)
        shift
        break
        ;;
    *) break ;;
    esac
done

(
    echo $_sync_only
    cd staging/src/kubevirt.io/api
    if [ "${_sync_only}" == "false" ]; then go get $@ ./...; fi
    go mod tidy
)
(
    echo $_sync_only
    cd staging/src/kubevirt.io/client-go
    if [ "${_sync_only}" == "false" ]; then go get $@ ./...; fi
    go mod tidy
)

(
    cd staging/src/kubevirt.io/client-go/examples/listvms
    if [ "${_sync_only}" == "false" ]; then go get $@ ./...; fi
    go mod tidy
)

go mod tidy
go work vendor
go work sync

# kubevirt.io/api is consumed by the virtualization module, which builds with
# Go 1.25. go work sync raises its requirements to the versions this workspace
# selects, and the x/net and x/text the fork pins for CVEs need Go 1.26, so put
# the API module back on the newest Go 1.25-compatible ones. The fork binaries
# still build with the pinned versions through the replace directives in go.mod.
(
    cd staging/src/kubevirt.io/api
    go mod edit -go=1.25.0 -require=golang.org/x/net@v0.58.0 -require=golang.org/x/text@v0.41.0
    GOWORK=off go mod tidy
)
