FROM quay.io/centos/centos:stream9

ARG ARCH
ARG BAZEL_ARCH

ENV BAZEL_VERSION=6.5.0
ENV GO_VERSION=1.25.10
ENV GOLANGCI_LINT_VERSION=v2.12.2

ENV KUBEVIRT_CREATE_BAZELRCS=false

# Install packages
RUN dnf install -y dnf-plugins-core && \
    dnf config-manager --enable crb && \
    dnf install -y --setopt=install_weak_deps=False \
        libvirt-devel \
        pkgconf-pkg-config \
        cpio \
        patch \
        make \
        git \
        sudo \
        gcc \
        gcc-c++ \
        glibc-static \
        libstdc++-static \
        glibc-devel \
        findutils \
        rsync-daemon \
        rsync \
        qemu-img \
        protobuf-compiler \
        python3 \
        python3-devel \
        jq \
        wget \
        diffutils && \
    dnf clean -y all

# Avoids the need to install sssd-client by disabling lookups
COPY nsswitch.conf /etc/nsswitch.conf

# Necessary for Bazel to find Python inside the container
#
# https://github.com/bazelbuild/bazel/issues/8665
# https://github.com/bazelbuild/bazel/issues/11554
RUN ln -s /usr/bin/python3 /usr/bin/python

# Install Go straight from go.dev. This replaces the upstream gimme setup,
# which fetches a github script that then downloads Go -- two flaky network
# hops that hang/fail under emulation.
RUN curl -sL "https://go.dev/dl/go${GO_VERSION}.linux-${ARCH}.tar.gz" | tar -C /usr/local -xz

ENV GOPATH="/go" GOBIN="/usr/bin" GO111MODULE="on" PATH="/usr/local/go/bin:${PATH}"

# Install the code generators invoked by hack/generate.sh + hack/gen-proto.sh
# and goimports (used to format generated code).
RUN set -x && \
    mkdir -p /go && \
    go install -v golang.org/x/tools/cmd/goimports@latest && \
    go install -v k8s.io/code-generator/cmd/conversion-gen@v0.32.5  && \
    go install -v k8s.io/code-generator/cmd/deepcopy-gen@v0.32.5 && \
    go install -v k8s.io/code-generator/cmd/defaulter-gen@v0.32.5  && \
    go install -v k8s.io/kube-openapi/cmd/openapi-gen@v0.0.0-20260330154417-16be699c7b31 && \
    go install -v github.com/golang/protobuf/protoc-gen-go@1643683 && \
    go install -v k8s.io/code-generator/cmd/client-gen@v0.32.5 && \
    go install -v sigs.k8s.io/controller-tools/cmd/controller-gen@v0.20.1 && \
    go clean -cache -modcache

# mockgen v0.6.0 (v0.5.1 pulls an x/tools that fails to build on Go 1.25).
# TODO: the vendored go.uber.org/mock is still v0.5.1; bump it and regenerate
# the mocks so check:generate produces a zero diff.
RUN set -x && \
    go install -v go.uber.org/mock/mockgen@v0.6.0 && \
    go install -v github.com/rmohr/go-swagger-utils/swagger-doc@v0.0.0-20181110145135-878f7deb1d17 && \
    go clean -cache -modcache

# Build golangci-lint from source: the prebuilt-binary install.sh fails its
# checksum verification behind the CI proxy.
RUN set -x && \
    go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@${GOLANGCI_LINT_VERSION} && \
    go clean -cache -modcache

COPY rsyncd.conf /etc/rsyncd.conf

COPY entrypoint.sh /entrypoint.sh

COPY create_bazel_cache_rcs.sh /create_bazel_cache_rcs.sh

RUN if test "${ARCH}" != "s390x"; then \
       curl -L -o /usr/bin/bazel https://github.com/bazelbuild/bazel/releases/download/${BAZEL_VERSION}/bazel-${BAZEL_VERSION}-linux-${BAZEL_ARCH} && \
       chmod u+x /usr/bin/bazel; \
    fi

# Add /root/go/src/kubevirt.io/kubevirt and mark it as a safe directory for git
RUN mkdir -p /root/go/src/kubevirt.io/kubevirt && git config --global --add safe.directory /root/go/src/kubevirt.io/kubevirt

ENTRYPOINT [ "/entrypoint.sh" ]
