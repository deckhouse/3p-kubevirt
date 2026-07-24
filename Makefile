export GO15VENDOREXPERIMENT := 1

ifeq (${CI}, true)
  # If we're running under a test lane, enable timestamps and disable progress output
  TIMESTAMP=1
  ifeq (,$(wildcard ci.bazelrc))
    $(shell echo 'build --noshow_progress' > ci.bazelrc)
  endif
endif

ifeq (${TIMESTAMP}, 1)
  SHELL = ./hack/timestamps.sh
endif

all: format bazel-build manifests

go-all: go-build manifests-no-bazel

bazel-generate:
	SYNC_VENDOR=true hack/dockerized "./hack/bazel-generate.sh"

bazel-build:
	hack/dockerized "export BUILD_ARCH=${BUILD_ARCH} && export DOCKER_TAG=${DOCKER_TAG} && export CI=${CI} && export KUBEVIRT_RELEASE=${KUBEVIRT_RELEASE} && hack/bazel-fmt.sh && ./hack/multi-arch.sh build"

bazel-build-functests:
	hack/dockerized "hack/bazel-fmt.sh && hack/bazel-build-functests.sh"

build-functests: bazel-build-functests

bazel-build-image-bundle:
	hack/dockerized "export BUILD_ARCH=${BUILD_ARCH} && hack/bazel-fmt.sh && DOCKER_PREFIX=${DOCKER_PREFIX} DOCKER_TAG=${DOCKER_TAG} IMAGE_PREFIX=${IMAGE_PREFIX} hack/multi-arch.sh build-image-bundle"

bazel-build-verify: bazel-build
	./hack/dockerized "hack/bazel-fmt.sh"
	./hack/verify-generate.sh
	./hack/build-verify.sh
	./hack/dockerized "hack/bazel-test.sh"

bazel-build-images:
	hack/dockerized "export BUILD_ARCH=${BUILD_ARCH} && DOCKER_PREFIX=${DOCKER_PREFIX} DOCKER_TAG=${DOCKER_TAG} DOCKER_TAG_ALT=${DOCKER_TAG_ALT} IMAGE_PREFIX=${IMAGE_PREFIX} IMAGE_PREFIX_ALT=${IMAGE_PREFIX_ALT} ./hack/multi-arch.sh build-images"

bazel-push-images:
	hack/dockerized "export BUILD_ARCH=${BUILD_ARCH} && hack/bazel-fmt.sh && DOCKER_PREFIX=${DOCKER_PREFIX} DOCKER_TAG=${DOCKER_TAG} DOCKER_TAG_ALT=${DOCKER_TAG_ALT} IMAGE_PREFIX=${IMAGE_PREFIX} IMAGE_PREFIX_ALT=${IMAGE_PREFIX_ALT} KUBEVIRT_PROVIDER=${KUBEVIRT_PROVIDER} PUSH_TARGETS='${PUSH_TARGETS}' ./hack/multi-arch.sh push-images"
	BUILD_ARCH=${BUILD_ARCH} DOCKER_PREFIX=${DOCKER_PREFIX} DOCKER_TAG=${DOCKER_TAG} hack/push-container-manifest.sh

push: bazel-push-images

bazel-test:
	hack/dockerized "hack/bazel-fmt.sh && CI=${CI} ARTIFACTS=${ARTIFACTS} WHAT=${WHAT}  hack/bazel-test.sh"

gen-proto:
	hack/dockerized "DOCKER_PREFIX=${DOCKER_PREFIX} DOCKER_TAG=${DOCKER_TAG} IMAGE_PULL_POLICY=${IMAGE_PULL_POLICY} VERBOSITY=${VERBOSITY} ./hack/gen-proto.sh"

generate:
	hack/dockerized hack/build-ginkgo.sh
	hack/dockerized "DOCKER_PREFIX=${DOCKER_PREFIX} DOCKER_TAG=${DOCKER_TAG} IMAGE_PULL_POLICY=${IMAGE_PULL_POLICY} VERBOSITY=${VERBOSITY} ./hack/generate.sh"
	SYNC_VENDOR=true hack/dockerized "./hack/bazel-generate.sh && hack/bazel-fmt.sh"
	hack/dockerized hack/sync-kubevirtci.sh
	hack/dockerized hack/common-instancetypes/sync.sh
	./hack/update-generated-api-testdata.sh

generate-verify: generate
	./hack/verify-generate.sh
	./hack/check-for-binaries.sh

apidocs:
	hack/dockerized "./hack/gen-swagger-doc/gen-swagger-docs.sh v1 html"

client-python:
	hack/dockerized "DOCKER_TAG=${DOCKER_TAG} ./hack/gen-client-python/generate.sh"

go-build:
	KUBEVIRT_NO_BAZEL=true hack/dockerized "export KUBEVIRT_VERSION=${KUBEVIRT_VERSION} && KUBEVIRT_GO_BUILD_TAGS=${KUBEVIRT_GO_BUILD_TAGS} KUBEVIRT_RELEASE=${KUBEVIRT_RELEASE} ./hack/build-go.sh install ${WHAT}" && ./hack/build-copy-artifacts.sh ${WHAT}

go-build-functests:
	hack/dockerized "export KUBEVIRT_NO_BAZEL=true && KUBEVIRT_GO_BUILD_TAGS=${KUBEVIRT_GO_BUILD_TAGS} ./hack/go-build-functests.sh"

gosec:
	hack/dockerized "GOSEC=${GOSEC} ARTIFACTS=${ARTIFACTS} ./hack/gosec.sh"

coverage:
	hack/dockerized "./hack/coverage.sh ${WHAT}"

goveralls:
	SYNC_OUT=false hack/dockerized "COVERALLS_TOKEN_FILE=${COVERALLS_TOKEN_FILE} COVERALLS_TOKEN=${COVERALLS_TOKEN} CI_NAME=prow CI_BRANCH=${PULL_BASE_REF} CI_PR_NUMBER=${PULL_NUMBER} GIT_ID=${PULL_PULL_SHA} PROW_JOB_ID=${PROW_JOB_ID} ./hack/bazel-goveralls.sh"

coverage-report:
	hack/dockerized "CI=${CI} WHAT=${WHAT} ./hack/bazel-coverage-report.sh"

go-test: go-build
	SYNC_OUT=false KUBEVIRT_NO_BAZEL=true hack/dockerized "export KUBEVIRT_GO_BUILD_TAGS=${KUBEVIRT_GO_BUILD_TAGS} && ./hack/build-go.sh test ${WHAT}"

test: bazel-test

fuzz:
	hack/dockerized "./hack/fuzz.sh"

integ-test:
	hack/integration-test.sh

functest: build-functests
	hack/functests.sh

dump: bazel-build
	hack/dump.sh

functest-image-build: manifests build-functests
	hack/func-tests-image.sh build

functest-image-push: functest-image-build
	hack/func-tests-image.sh push

conformance:
	hack/dockerized "export KUBEVIRT_PROVIDER=${KUBEVIRT_PROVIDER} SKIP_OUTSIDE_CONN_TESTS=${SKIP_OUTSIDE_CONN_TESTS} RUN_ON_ARM64_INFRA=${RUN_ON_ARM64_INFRA} SKIP_BLOCK_STORAGE_TESTS=${SKIP_BLOCK_STORAGE_TESTS} SKIP_SNAPSHOT_STORAGE_TESTS=${SKIP_SNAPSHOT_STORAGE_TESTS} KUBEVIRT_E2E_FOCUS=${KUBEVIRT_E2E_FOCUS} DOCKER_PREFIX=${DOCKER_PREFIX} DOCKER_TAG=${DOCKER_TAG} && hack/conformance.sh"

perftest: build-functests
	hack/perftests.sh

kwok-perftest: build-functests
	hack/kwok-perftests.sh

realtime-perftest: build-functests
	hack/realtime-perftests.sh

clean:
	hack/dockerized "./hack/build-go.sh clean ${WHAT} && rm _out/* -rf"
	hack/dockerized "bazel clean --expunge"
	rm -f tools/openapispec/openapispec tools/resource-generator/resource-generator tools/manifest-templator/manifest-templator tools/vms-generator/vms-generator

distclean: clean
	hack/dockerized "rm -rf vendor/ && rm -f go.sum && GO111MODULE=on go clean -modcache"
	rm -rf vendor/

cluster-patch:
	hack/dockerized "export BUILD_ARCH=${BUILD_ARCH} && hack/bazel-fmt.sh && DOCKER_PREFIX=${DOCKER_PREFIX} DOCKER_TAG=${DOCKER_TAG} DOCKER_TAG_ALT=${DOCKER_TAG_ALT} IMAGE_PREFIX=${IMAGE_PREFIX} IMAGE_PREFIX_ALT=${IMAGE_PREFIX_ALT} KUBEVIRT_PROVIDER=${KUBEVIRT_PROVIDER} PUSH_TARGETS='virt-api virt-controller virt-handler virt-launcher' ./hack/bazel-push-images.sh"
	hack/cluster-patch.sh

deps-update-patch:
	SYNC_VENDOR=true hack/dockerized " ./hack/dep-update.sh -- -u=patch && ./hack/bazel-generate.sh"

deps-update:
	SYNC_VENDOR=true hack/dockerized " ./hack/dep-update.sh && ./hack/bazel-generate.sh"

deps-sync:
	SYNC_VENDOR=true hack/dockerized " ./hack/dep-update.sh --sync-only && ./hack/bazel-generate.sh"

rpm-deps:
	SYNC_VENDOR=true hack/dockerized "CUSTOM_REPO=${CUSTOM_REPO} SINGLE_ARCH=${SINGLE_ARCH} BASESYSTEM=${BASESYSTEM} LIBVIRT_VERSION=${LIBVIRT_VERSION} QEMU_VERSION=${QEMU_VERSION} SEABIOS_VERSION=${SEABIOS_VERSION} EDK2_VERSION=${EDK2_VERSION} LIBGUESTFS_VERSION=${LIBGUESTFS_VERSION} GUESTFSTOOLS_VERSION=${GUESTFSTOOLS_VERSION} PASST_VERSION=${PASST_VERSION} VIRTIOFSD_VERSION=${VIRTIOFSD_VERSION} SWTPM_VERSION=${SWTPM_VERSION} ./hack/rpm-deps.sh"

bump-images:
	hack/dockerized "./hack/rpm-deps.sh && ./hack/bump-distroless.sh"

verify-rpm-deps:
	SYNC_VENDOR=true hack/dockerized " ./hack/verify-rpm-deps.sh"

build-verify:
	hack/build-verify.sh

manifests:
	hack/manifests.sh

manifests-no-bazel:
	KUBEVIRT_NO_BAZEL=true hack/manifests.sh

cluster-up:
	./hack/cluster-up.sh

cluster-down:
	./kubevirtci/cluster-up/down.sh

cluster-build:
	./hack/cluster-build.sh

cluster-clean:
	./hack/cluster-clean.sh

cluster-deploy: cluster-clean
	./hack/cluster-deploy.sh

cluster-sync:
	./hack/cluster-sync.sh

builder-build:
	./hack/builder/build.sh

builder-publish:
	./hack/builder/publish.sh

olm-verify:
	hack/dockerized "./hack/olm.sh verify"

current-dir := $(realpath .)
rule-spec-dumper-executable := "rule-spec-dumper"

build-prom-spec-dumper:
	hack/dockerized "go build -o ${rule-spec-dumper-executable} ./hack/prom-rule-ci/rule-spec-dumper.go"

clean-prom-spec-dumper:
	rm -f ${rule-spec-dumper-executable}

prom-rules-verify: build-prom-spec-dumper
	./hack/prom-rule-ci/verify-rules.sh \
		"${current-dir}/${rule-spec-dumper-executable}" \
		"${current-dir}/hack/prom-rule-ci/prom-rules-tests.yaml"
	rm ${rule-spec-dumper-executable}

olm-push:
	hack/dockerized "DOCKER_TAG=${DOCKER_TAG} CSV_VERSION=${CSV_VERSION} QUAY_USERNAME=${QUAY_USERNAME} \
	    QUAY_PASSWORD=${QUAY_PASSWORD} QUAY_REPOSITORY=${QUAY_REPOSITORY} PACKAGE_NAME=${PACKAGE_NAME} ./hack/olm.sh push"

bump-kubevirtci:
	./hack/bump-kubevirtci.sh

fossa:
	hack/dockerized "FOSSA_TOKEN_FILE=${FOSSA_TOKEN_FILE} PULL_BASE_REF=${PULL_BASE_REF} CI=${CI} ./hack/fossa.sh"

format:
	./hack/dockerized "hack/bazel-fmt.sh"

fmt: format

# --- DVP fork: containerized lint/test/vendor checks ------------------------
# These targets run in a locally built image (hack/ci.Dockerfile) instead of
# the upstream builder image, which still ships Go 1.23 and golangci-lint v1.
# TODO: once the builder image is rebuilt with Go 1.25 and published, restore
# the upstream dockerized recipes (lint-test-cleanup-label, monitoringlinter,
# license-header-check) and drop the local image.
# Single unified image for every containerized target. lint/test/vendor run a
# plain docker-run in it; generate drives it through hack/dockerized (rsync +
# bazel). Built from hack/builder/Dockerfile. Locally IMAGE defaults to a
# content-hash tag (hack/image-tag.sh over the Dockerfile + its COPYed files):
# the image target reuses it while the recipe is unchanged and rebuilds only
# when it changes, so builds are cached across runs. CI overrides IMAGE with
# the registry content-tag it builds/pulls (see .gitlab-ci.yml).
IMAGE ?= 3p-kubevirt-builder:$(shell bash hack/image-tag.sh 2>/dev/null || echo local)
GOLANGCI_LINT_VERSION ?= 2.12.2
LINT_ARGS ?=

CI_UIDGID := $(shell id -u):$(shell id -g)

# Git stamp for LOCAL generate only. On the host git works natively but the
# generate container can't resolve the version itself (ownership/discovery on
# the rsynced tree, and the fork's tags), leaving bazel's workspace_status with
# an unbound KUBEVIRT_GIT_VERSION. Compute it here and forward via
# hack/dockerized so hack/version.sh uses it.
#
# Skipped under CI: there the in-container version.sh already resolves the
# stamp, and SHELL=hack/timestamps.sh (set when CI=true) would inject
# timestamps into these $(shell ...) values and corrupt the env assignments.
ifneq (${CI},true)
GEN_GIT_COMMIT := $(shell git rev-parse "HEAD^{commit}" 2>/dev/null)
GEN_GIT_VERSION := $(shell git describe --tags --match 'v[0-9]*' --abbrev=14 "$(GEN_GIT_COMMIT)^{commit}" 2>/dev/null)
GEN_GIT_TREE_STATE := $(shell test -z "$$(git status --porcelain 2>/dev/null)" && echo clean || echo dirty)
GEN_GIT_ENV := KUBEVIRT_GIT_COMMIT=$(GEN_GIT_COMMIT) KUBEVIRT_GIT_VERSION=$(GEN_GIT_VERSION) KUBEVIRT_GIT_TREE_STATE=$(GEN_GIT_TREE_STATE)
endif

# Always build/run the builder image as amd64, matching CI. On arm64 macs the
# amd64 container runs under Rosetta (uname -m = x86_64 inside), so bazel's
# rules_go finds a host toolchain and generation matches CI exactly.
# Generation is arch-independent, so amd64 is correct on every host.
BUILDER_PLATFORM := linux/amd64
BUILDER_ARCH := amd64
BUILDER_BAZEL_ARCH := x86_64

# Build the unified image locally. In CI the build:images job builds/pushes it
# to the registry under a content tag and sets IMAGE, so this is skipped.
image:
	docker image inspect $(IMAGE) >/dev/null 2>&1 || \
		{ echo "$(IMAGE)" | grep -q / && docker pull $(IMAGE) 2>/dev/null; } || \
		docker build --platform=$(BUILDER_PLATFORM) -t $(IMAGE) \
			--build-arg ARCH=$(BUILDER_ARCH) --build-arg BAZEL_ARCH=$(BUILDER_BAZEL_ARCH) \
			-f hack/builder/Dockerfile hack/builder/

# Run a command in the image as root (some suites need CAP_CHOWN), then chown
# the bind-mounted checkout back to the runner uid so a later job can git-clean
# it; the make exit code is preserved. --entrypoint bash bypasses the image's
# rsync entrypoint (Go is already on PATH via the image ENV). Single wrapper
# used by every non-generate containerized target.
define run_in_ci_image
	docker run --rm --entrypoint bash --platform=$(BUILDER_PLATFORM) \
		-v $(CURDIR):/work -w /work \
		-v 3p-kubevirt-gomod:/go/pkg/mod \
		-v 3p-kubevirt-gocache:/root/.cache \
		-e GOTOOLCHAIN=auto \
		$(IMAGE) -c "$(1); rc=\$$?; find /work -path /work/.git -prune -o -exec chown $(CI_UIDGID) {} + 2>/dev/null || true; exit \$$rc"
endef

lint: image
	$(call run_in_ci_image,bash hack/golangci-lint.sh $(LINT_ARGS))

test-unit: image
	$(call run_in_ci_image,bash hack/test-unit.sh)

vendor-verify: image
	$(call run_in_ci_image,bash hack/verify-vendor.sh)

# Update Go deps and vendor/. Pass args through to go get, e.g.
# `make update UPDATE_ARGS=-u` to upgrade.
update: image
	$(call run_in_ci_image,bash hack/update.sh $(UPDATE_ARGS))

# Full kubevirt code generation in the image, pointing hack/dockerized at it
# via KUBEVIRT_BUILDER_IMAGE instead of the upstream quay image. `task
# generate` / `task check:generate` wrap these.
#
# generate runs through hack/dockerized as root. Its rsync --delete step
# chokes on an _out left by another job (test-unit's junit/binaries), so
# remove _out up front as root; afterwards chown the tree back to the runner
# uid (preserving the make exit code) so a later job can git-clean it.
define rm_out
	docker run --rm --platform=$(BUILDER_PLATFORM) -v $(CURDIR):/work --entrypoint rm $(IMAGE) -rf /work/_out
endef
define chown_tree
	docker run --rm --platform=$(BUILDER_PLATFORM) -v $(CURDIR):/work --entrypoint bash $(IMAGE) -c "find /work -path /work/.git -prune -o -exec chown $(CI_UIDGID) {} + 2>/dev/null || true"
endef
# A crashed generate leaves the hack/dockerized bazel-server up; it holds the
# host network and blocks the next run's rsyncd. Remove it up front so a retry
# starts clean without a manual `docker rm`.
define clean_bazel_server
	cids=$$(docker ps -aq --filter name=bazel-server 2>/dev/null); [ -z "$$cids" ] || docker rm -f $$cids >/dev/null 2>&1 || true
endef

generate-local: image
	-$(call clean_bazel_server)
	$(call rm_out)
	rc=0; KUBEVIRT_BUILDER_IMAGE=$(IMAGE) $(GEN_GIT_ENV) $(MAKE) generate || rc=$$?; $(call chown_tree) || true; exit $$rc

generate-verify-local: image
	-$(call clean_bazel_server)
	$(call rm_out)
	rc=0; KUBEVIRT_BUILDER_IMAGE=$(IMAGE) $(GEN_GIT_ENV) $(MAKE) generate-verify || rc=$$?; $(call chown_tree) || true; exit $$rc

lint-metrics:
	hack/dockerized "./hack/prom-metric-linter/metrics_collector.sh > metrics.json"
	./hack/prom-metric-linter/metric_name_linter.sh --operator-name="kubevirt" --sub-operator-name="kubevirt" --metrics-file=metrics.json
	rm metrics.json

gofumpt:
	./hack/dockerized "hack/gofumpt.sh"

update-generated-api-testdata:
	./hack/update-generated-api-testdata.sh

.PHONY: \
	build-verify \
	conformance \
	go-build \
	go-test \
	go-all \
	bazel-generate \
	bazel-build \
	bazel-build-image-bundle \
	bazel-build-images \
	bazel-push-images \
	bazel-test \
	functest-image-build \
	functest-image-push \
	test \
	clean \
	distclean \
	deps-sync \
	sync \
	manifests \
	functest \
	cluster-up \
	cluster-down \
	cluster-clean \
	cluster-deploy \
	cluster-sync \
	olm-verify \
	olm-push \
	coverage \
	goveralls \
	build-functests \
	fossa \
	realtime-perftest \
	format \
	fmt \
	lint \
	image \
	test-unit \
	vendor-verify \
	update \
	generate-local \
	generate-verify-local \
	lint-metrics \
	update-generated-api-testdata \
	$(NULL)
