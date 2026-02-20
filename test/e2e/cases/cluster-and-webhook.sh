#!/usr/bin/env bash

set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" >/dev/null 2>&1 && pwd)"
source "${SCRIPT_DIR}/../lib.sh"

verify_webhook_with_timeout() {
  local timeout_seconds="${1:-15}"
  local start_ts
  start_ts="$(date +%s)"

  echo "Waiting for webhook to be available"
  while true; do
    local out
    local rc
    out="$(
      kubectl create --dry-run=server -n "${E2E_WEBHOOK_NAMESPACE}" -f- <<-'EOT' 2>&1
apiVersion: resource.k8s.io/v1
kind: ResourceClaim
metadata:
  name: webhook-test
spec:
  devices:
    requests:
    - name: npu
      exactly:
        deviceClassName: npu.rebellions.ai
EOT
    )"
    rc=$?

    if [[ ${rc} -eq 0 ]]; then
      echo "Webhook is available"
      return 0
    fi

    echo "${out}"
    if grep -qE 'namespaces ".*" not found|service ".*" not found|failed calling webhook' <<<"${out}"; then
      echo "Detected broken/misconfigured admission webhook in the cluster."
      echo "Please inspect validating/mutating webhook configurations for ResourceClaim rules."
      kubectl get validatingwebhookconfigurations,mutatingwebhookconfigurations || true
      return 1
    fi

    if (( "$(date +%s)" - start_ts >= timeout_seconds )); then
      echo "Timed out waiting for webhook readiness (${timeout_seconds}s)"
      return 1
    fi

    sleep 1
    echo "Retrying webhook"
  done
}

wait_cluster_ready
verify_webhook_with_timeout 15
