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

include $(CURDIR)/common.mk

# Keep known command targets even when wildcard discovery behaves unexpectedly in CI.
CMDS := $(sort npu-kubelet-plugin webhook $(patsubst ./cmd/%/,%,$(sort $(dir $(wildcard ./cmd/*/)))))
CMD_TARGETS := $(patsubst %,cmd-%, $(CMDS))

CHECK_TARGETS := vet lint
MAKE_TARGETS := binaries build build-image check vendor fmt test cmds $(CHECK_TARGETS)

TARGETS := $(MAKE_TARGETS) $(CMD_TARGETS)

.PHONY: $(TARGETS)

GOOS ?= linux
# Empty means the host architecture. The container build passes buildx's
# TARGETARCH here so arm64 binaries are cross-compiled instead of built under QEMU.
GOARCH ?=

binaries: cmds
ifneq ($(PREFIX),)
cmd-%: COMMAND_BUILD_OPTIONS = -o $(PREFIX)/$(*)
endif
cmds: $(CMD_TARGETS)
# Only stamp main.version when VERSION is set, so an unset VERSION leaves the
# binary's own "devel" default instead of overwriting it with an empty string.
LDFLAGS := -s -w $(if $(VERSION),-X main.version=$(VERSION))

$(CMD_TARGETS): cmd-%:
	CGO_LDFLAGS_ALLOW='-Wl,--unresolved-symbols=ignore-in-object-files' GOOS=$(GOOS) GOARCH=$(GOARCH) \
		go build -ldflags "$(LDFLAGS)" $(COMMAND_BUILD_OPTIONS) $(MODULE)/cmd/$(*)

build:
	GOOS=$(GOOS) GOARCH=$(GOARCH) go build ./...

# Entry point of the cloud-component-release-kit rc build:
#   make build-image IMAGE_NAME=<registry>/k8s-dra-driver-npu VERSION=<tag> \
#        BUILD_MULTI_PLATFORM=true PUSH_ON_BUILD=true
# Command-line variables reach the sub-make through MAKEFLAGS.
build-image:
	$(MAKE) -f $(CURDIR)/deployments/container/Makefile build

all: check test binaries
check: $(CHECK_TARGETS)

# Update the vendor folder
vendor:
	go mod vendor

# Apply go fmt to the codebase
fmt:
	go list -f '{{.Dir}}' $(MODULE)/... \
		| xargs gofmt -s -l -w

lint:
	golangci-lint run ./...

vet:
	go vet $(MODULE)/...


COVERAGE_FILE := coverage.out
test: build cmds
	go test -v -coverprofile=$(COVERAGE_FILE) $(MODULE)/...

# Release runner contract. Registry is a host/project prefix; Make builds only RC images.
.PHONY: release-artifact
release-artifact:
ifeq ($(ARTIFACT_KIND),image)
	@set -eu; \
	test -n "$(RELEASE_RESULT)"; \
	sha=$$(jq -er '.sourceSha | strings' "$(RELEASE_INPUT)"); \
	test "$$sha" = "$$(git rev-parse HEAD)" || { echo "release source SHA differs from checkout" >&2; exit 1; }; \
	repo=$$(NAME="$(ARTIFACT_NAME)" yq -er '.artifacts[] | select(.kind == "image" and .name == strenv(NAME)) | .repository' .piper/release.yaml); \
	version=$$(jq -er 'select(.schemaVersion == 1 and .phase == "rc") | .version | strings | select(test("^v[0-9]+\\.[0-9]+\\.[0-9]+-rc[0-9]+$$"))' "$(RELEASE_INPUT)"); \
	platforms=$$(yq -er '.imagePlatforms | join(",") | select(length > 0)' .piper/release.yaml); \
	registry=$$(jq -er '.targetRegistry | strings | select(length > 0)' "$(RELEASE_INPUT)"); \
	$(MAKE) build-image IMAGE_NAME="$$registry/$$repo" IMAGE_TAG="$$version" VERSION="$$version" PLATFORM="$$platforms" BUILD_MULTI_PLATFORM=true PUSH_ON_BUILD=true; \
	ref="$$registry/$$repo:$$version"; \
	digest=$$(regctl image digest "$$ref"); \
	jq -ne --arg name "$(ARTIFACT_NAME)" --arg ref "$$ref" --arg digest "$$digest" \
	  'select($$digest | test("^sha256:[a-f0-9]{64}$$")) | {schemaVersion:1, name:$$name, kind:"image", output:{type:"registry", ref:$$ref, digest:$$digest}}' > "$(RELEASE_RESULT)"
else
	@echo "Unsupported release artifact: $(ARTIFACT_KIND)/$(ARTIFACT_NAME)" >&2; exit 1
endif
