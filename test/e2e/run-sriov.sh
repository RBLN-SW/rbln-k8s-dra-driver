#!/usr/bin/env bash
# Manual-only test for an already provisioned SR-IOV node; not a general CI case.
# Partition changes and reboots are managed outside this script.
set -euo pipefail
# shellcheck source=test/e2e/lib.sh
source "$(dirname -- "${BASH_SOURCE[0]}")/lib.sh"
: "${KUBECONFIG:?Set the test kubeconfig explicitly}"
: "${SRIOV_NODE:?Set the dedicated NPU test node}"
MODE="${MODE:-vf4}"
readonly driver_name="npu.rebellions.ai"
TEST_IMAGE="${TEST_IMAGE:-ubuntu:22.04}"
case "$MODE" in none|vf1|vf4) ;; *) echo 'MODE must be none, vf1, or vf4' >&2; exit 1;; esac
if ! command -v jq >/dev/null 2>&1; then
  echo 'jq is required for SR-IOV allocation validation' >&2
  exit 1
fi
if [[ -n "${EXPECTED_DEVICES:-}" ]]; then
  count="$(kubectl get resourceslices -o json | jq --arg driver "$driver_name" --arg node "$SRIOV_NODE" --arg mode "$MODE" '[.items[]|select(.spec.driver==$driver and .spec.pool.name==$node)|.spec.devices[]|select(.attributes.partitionMode.string==$mode)]|length')"
  [[ "$count" == "$EXPECTED_DEVICES" ]] || { echo "Expected $EXPECTED_DEVICES devices, found $count" >&2; exit 1; }
fi
ns="rbln-dra-e2e-${RANDOM}-${RANDOM}"
artifacts="$(mktemp -d)"
cleanup() {
  cleanup_namespace "$ns" >/dev/null
  rm -rf "$artifacts"
}
trap cleanup EXIT
kubectl create namespace "$ns" >/dev/null
prefix="${driver_name%%.*}"
domain="${driver_name#*.}"
if [[ "$MODE" == none ]]; then
  device_class="$driver_name"
else
  device_class="${prefix}-${MODE}.${domain}"
fi
make_pod() {
  local name="$1" claim="$2"
  cat >"$artifacts/$name.yaml" <<YAML
apiVersion: v1
kind: Pod
metadata:
  name: $name
  namespace: $ns
spec:
  terminationGracePeriodSeconds: 5
  affinity:
    nodeAffinity:
      requiredDuringSchedulingIgnoredDuringExecution:
        nodeSelectorTerms:
        - matchExpressions:
          - key: kubernetes.io/hostname
            operator: In
            values: ["$SRIOV_NODE"]
  containers:
  - name: workload
    image: $TEST_IMAGE
    command: ["sh", "-c", "trap 'exit 0' TERM; sleep 3600 & wait"]
    resources:
      claims:
      - name: npu
  resourceClaims:
  - name: npu
    resourceClaimName: $claim
YAML
  kubectl apply -f "$artifacts/$name.yaml" >/dev/null
}
for suffix in a b; do
  kubectl apply -f - >/dev/null <<YAML
apiVersion: resource.k8s.io/v1
kind: ResourceClaim
metadata:
  name: claim-$suffix
  namespace: $ns
spec:
  devices:
    requests:
    - name: npu
      exactly:
        deviceClassName: $device_class
        count: 1
        selectors:
        - cel:
            expression: 'device.attributes["$driver_name"].partitionMode == "$MODE"'
YAML
  make_pod "consumer-$suffix" "claim-$suffix"
done
kubectl wait -n "$ns" --for=condition=Ready pod/consumer-a pod/consumer-b --timeout=180s
allocation() {
  kubectl get resourceclaim -n "$ns" "claim-$1" -o json | jq -er --arg driver "$driver_name" --arg node "$SRIOV_NODE" '.status.allocation.devices.results[] | select(.driver==$driver and .pool==$node) | .device'
}
a="$(allocation a)"; b="$(allocation b)"
[[ "$a" != "$b" ]] || { echo 'Distinct claims received the same device' >&2; exit 1; }
check_workload() {
  local pod="$1" device="$2" expected_bus
  expected_bus="$(kubectl get resourceslices -o json | jq -er --arg driver "$driver_name" --arg node "$SRIOV_NODE" --arg device "$device" --arg mode "$MODE" '.items[] | select(.spec.driver==$driver and .spec.pool.name==$node) | .spec.devices[] | select(.name==$device and .attributes.partitionMode.string==$mode) | .attributes["resource.kubernetes.io/pciBusID"].string')"
  # The shared group check does not verify the allocated PCI address or reject group 0.
  if ! kubectl exec -n "$ns" "$pod" -- rbln-smi -j -g | jq -e --arg bus "$expected_bus" '.devices | length==1 and .[0].pci.bus_id==$bus and (.[0].group_id|tonumber)>0' >/dev/null; then
    echo "Expected $ns/$pod to expose only $expected_bus in a non-default RSD group" >&2
    return 1
  fi
}
check_workload consumer-a "$a"
check_workload consumer-b "$b"
kubectl delete pod -n "$ns" consumer-a --wait --timeout=90s >/dev/null
make_pod consumer-a claim-a
kubectl wait -n "$ns" --for=condition=Ready pod/consumer-a --timeout=180s
# The last consumer's exit may deallocate the claim. Validate its new
# allocation instead of assuming that it received the same VF again.
a="$(allocation a)"
b="$(allocation b)"
[[ "$a" != "$b" ]] || { echo 'Reused claim overlaps the other consumer' >&2; exit 1; }
check_workload consumer-a "$a"
check_workload consumer-b "$b"
kubectl delete namespace "$ns" --timeout=90s >/dev/null
printf 'PASS node=%s mode=%s devices=%s,%s\n' "$SRIOV_NODE" "$MODE" "$a" "$b"
