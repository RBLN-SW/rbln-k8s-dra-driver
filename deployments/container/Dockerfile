# Copyright 2023 The Kubernetes Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
#
# Modifications Copyright 2026 Rebellions Inc.
# This file is derived from github.com/kubernetes-sigs/dra-example-driver
# (deployments/container/Dockerfile, tag v0.2.1) and has been
# modified by Rebellions Inc. See NOTICE for details.

ARG GOLANG_VERSION=undefined
ARG BASE_IMAGE=undefined
ARG VERSION="N/A"
FROM --platform=$BUILDPLATFORM golang:${GOLANG_VERSION} AS build

# Re-declared inside the stage so the build arg reaches make; without it the
# binaries are stamped with no version and the startup log cannot say which
# build is running.
ARG VERSION
# Set by buildx per target platform. Cross-compiling on the build host keeps the
# Go toolchain off QEMU when the image is built for arm64.
ARG TARGETARCH

WORKDIR /build
COPY . .

RUN mkdir /artifacts
# CI checkouts may borrow Git objects from a mirror outside the build context.
# VERSION still reaches main.version through the Makefile's linker flags.
RUN GOFLAGS=-buildvcs=false make PREFIX=/artifacts VERSION="$VERSION" GOARCH="$TARGETARCH" cmds

FROM ${BASE_IMAGE}

# docker.io/ubuntu:22.04 is republished on Canonical's cadence, so a package fix
# can be in the archive for weeks before --pull sees it. Apply pending updates
# at build time so a base-image CVE is closed by the next rebuild.
RUN apt-get update \
    && DEBIAN_FRONTEND=noninteractive apt-get upgrade -y \
    && rm -rf /var/lib/apt/lists/*

LABEL io.k8s.display-name="Rebellions NPU Resource Driver for Dynamic Resource Allocation (DRA)"
LABEL name="Rebellions NPU Resource Driver for Dynamic Resource Allocation (DRA)"
LABEL vendor="rebellions.ai"
ARG VERSION="N/A"
LABEL version=${VERSION}
ARG RELEASE="N/A"
LABEL release="N/A"
LABEL summary="Rebellions NPU DRA resource driver for Kubernetes"
LABEL description="See summary"
LABEL maintainer="Rebellions sw_devops@rebellions.ai"
LABEL org.opencontainers.image.licenses="Apache-2.0"
LABEL org.opencontainers.image.source="https://github.com/RBLN-SW/rbln-k8s-dra-driver"
LABEL org.opencontainers.image.vendor="Rebellions Inc."

COPY --from=build /artifacts/npu-kubelet-plugin /usr/bin/npu-kubelet-plugin
COPY --from=build /artifacts/webhook       /usr/bin/webhook
COPY LICENSE NOTICE THIRD_PARTY_NOTICES /licenses/
