#!/usr/bin/env bash

set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" >/dev/null 2>&1 && pwd)"
source "${SCRIPT_DIR}/../lib.sh"

NAMESPACE="npu-two-pods-single-npu"
trap 'cleanup_namespace "${NAMESPACE}"' EXIT

echo "=== two-pods-single-npu: two pods each claim one NPU ==="
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
  name: single-npu
spec:
  spec:
    devices:
      requests:
      - name: npu
        exactly:
          deviceClassName: npu.rebellions.ai
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
      - name: npu
  resourceClaims:
  - name: npu
    resourceClaimTemplateName: single-npu
---
apiVersion: v1
kind: Pod
metadata:
  namespace: ${NAMESPACE}
  name: pod1
spec:
  containers:
  - name: ctr0
    image: ubuntu:22.04
    command: ["bash", "-c"]
    args: ["export; trap 'exit 0' TERM; sleep 9999 & wait"]
    resources:
      claims:
      - name: npu
  resourceClaims:
  - name: npu
    resourceClaimTemplateName: single-npu
EOF_MANIFEST

wait_pod_ready "${NAMESPACE}" pod0
wait_pod_ready "${NAMESPACE}" pod1
assert_running_pod_count "${NAMESPACE}" 2
assert_rbln_smi_group "${NAMESPACE}" pod0 1
assert_rbln_smi_group "${NAMESPACE}" pod1 1
