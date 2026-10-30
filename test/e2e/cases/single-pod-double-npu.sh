#!/usr/bin/env bash

# Copyright 2026 Rebellions Inc.
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

set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" >/dev/null 2>&1 && pwd)"
source "${SCRIPT_DIR}/../lib.sh"

NAMESPACE="npu-single-pod-double-npu"
trap 'cleanup_namespace "${NAMESPACE}"' EXIT

echo "=== single-pod-double-npu: one pod claims two NPUs ==="
kubectl apply -f - <<EOF_MANIFEST
apiVersion: v1
kind: Namespace
metadata:
  name: ${NAMESPACE}
---
apiVersion: resource.k8s.io/v1
kind: ResourceClaimTemplate
metadata:
  namespace: ${NAMESPACE}
  name: double-npu
spec:
  spec:
    devices:
      requests:
      - name: npus
        exactly:
          deviceClassName: npu.rebellions.ai
          count: 2
---
apiVersion: v1
kind: Pod
metadata:
  namespace: ${NAMESPACE}
  name: pod0
spec:
  containers:
  - name: ctr0
    image: ubuntu:22.04
    command: ["bash", "-c"]
    args: ["export; trap 'exit 0' TERM; sleep 9999 & wait"]
    resources:
      claims:
      - name: npus
  resourceClaims:
  - name: npus
    resourceClaimTemplateName: double-npu
EOF_MANIFEST

wait_pod_ready "${NAMESPACE}" pod0
assert_running_pod_count "${NAMESPACE}" 1
assert_rbln_smi_group "${NAMESPACE}" pod0 2
