#!/usr/bin/env bash

set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" >/dev/null 2>&1 && pwd)"
source "${SCRIPT_DIR}/../lib.sh"

NAMESPACE="npu-reclaim-after-release"
trap 'cleanup_namespace "${NAMESPACE}"' EXIT

echo "=== reclaim-after-release: pending pod should run after releasing resources ==="
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
  name: two-npu
spec:
  spec:
    devices:
      requests:
      - name: npus
        exactly:
          deviceClassName: npu.rebellions.ai
          count: 2
---
apiVersion: resource.k8s.io/v1
kind: ResourceClaimTemplate
metadata:
  namespace: ${NAMESPACE}
  name: one-npu
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
  name: pod-a
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
    resourceClaimTemplateName: two-npu
---
apiVersion: v1
kind: Pod
metadata:
  namespace: ${NAMESPACE}
  name: pod-b
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
    resourceClaimTemplateName: one-npu
EOF_MANIFEST

wait_pod_ready "${NAMESPACE}" pod-a
assert_rbln_smi_group "${NAMESPACE}" pod-a 2
wait_pod_phase "${NAMESPACE}" pod-b Pending 120
kubectl delete pod -n "${NAMESPACE}" pod-a --timeout=25s
wait_pod_ready "${NAMESPACE}" pod-b
assert_rbln_smi_group "${NAMESPACE}" pod-b 1
