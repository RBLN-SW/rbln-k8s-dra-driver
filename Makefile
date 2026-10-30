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
# (Makefile, tag v0.2.1) and has been
# modified by Rebellions Inc. See NOTICE for details.

include $(CURDIR)/common.mk

# Keep known command targets even when wildcard discovery behaves unexpectedly in CI.
CMDS := $(sort npu-kubelet-plugin webhook $(patsubst ./cmd/%/,%,$(sort $(dir $(wildcard ./cmd/*/)))))
CMD_TARGETS := $(patsubst %,cmd-%, $(CMDS))

CHECK_TARGETS := vet lint license-check verify-third-party-notices
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

.PHONY: test-license-tools
test-license-tools: ## Test notice generation with and without a local vendor directory.
	python3 -B -m unittest discover -s hack -p 'test_gen_third_party_notices.py' -v

.PHONY: license-check
license-check: test-license-tools ## Check that tracked Go, shell and Python sources carry a license header and that chart license files are in sync.
	@fail=0; for f in $$(git ls-files '*.go' '*.sh' '*.py' | grep -v '^vendor/'); do \
		head -25 "$$f" | grep -q 'Copyright' || { echo "missing license header: $$f"; fail=1; }; \
	done; \
	cmp -s LICENSE deployments/helm/k8s-dra-driver-npu/LICENSE || { echo "deployments/helm/k8s-dra-driver-npu/LICENSE differs from LICENSE"; fail=1; }; \
	cmp -s NOTICE deployments/helm/k8s-dra-driver-npu/NOTICE || { echo "deployments/helm/k8s-dra-driver-npu/NOTICE differs from NOTICE"; fail=1; }; \
	[ $$fail -eq 0 ] && echo "license-check: ok" || exit 1

.PHONY: third-party-notices
third-party-notices: ## Regenerate THIRD_PARTY_NOTICES from the module dependency graph.
	python3 hack/gen-third-party-notices.py --mode module .

.PHONY: verify-third-party-notices
verify-third-party-notices: ## Check that THIRD_PARTY_NOTICES matches the dependency graph.
	@tmp=$$(mktemp); trap 'rm -f "$$tmp"' EXIT; \
	python3 hack/gen-third-party-notices.py --mode module . "$$tmp" >/dev/null || { echo "verify-third-party-notices: generator failed"; exit 1; }; \
	cmp -s "$$tmp" THIRD_PARTY_NOTICES || { echo "THIRD_PARTY_NOTICES is out of date; run 'make third-party-notices'"; exit 1; }; \
	echo "verify-third-party-notices: ok"
