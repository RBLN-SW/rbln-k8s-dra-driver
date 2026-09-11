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
RUN make PREFIX=/artifacts VERSION="$VERSION" GOARCH="$TARGETARCH" cmds

FROM ${BASE_IMAGE}

LABEL io.k8s.display-name="Rebellions NPU Resource Driver for Dynamic Resource Allocation (DRA)"
LABEL name="Rebellions NPU Resource Driver for Dynamic Resource Allocation (DRA)"
LABEL vendor="rebellions.ai"
ARG VERSION="N/A"
LABEL version=${VERSION}
ARG RELEASE="N/A"
LABEL release="N/A"
LABEL summary="Rebellions NPU DRA resource driver for Kubernetes"
LABEL description="See summary"

COPY --from=build /artifacts/npu-kubelet-plugin /usr/bin/npu-kubelet-plugin
COPY --from=build /artifacts/webhook       /usr/bin/webhook
