#!/usr/bin/env bash

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

set -euo pipefail

function helm_uninstall_if_exists {
  local release="$1"
  local namespace="$2"

  if helm status "${release}" -n "${namespace}" >/dev/null 2>&1; then
    helm uninstall "${release}" -n "${namespace}" --wait
  else
    echo "Skipping missing release ${namespace}/${release}"
  fi
}

function delete_namespace_if_exists {
  local namespace="$1"

  if kubectl get namespace "${namespace}" >/dev/null 2>&1; then
    kubectl delete namespace "${namespace}" --ignore-not-found=true --timeout=120s
  else
    echo "Skipping missing namespace ${namespace}"
  fi
}

helm_uninstall_if_exists k8s-dra-driver-npu k8s-dra-driver-npu

for ns in \
  npu-two-pods-single-npu \
  npu-single-pod-double-npu \
  npu-three-pods-contention \
  npu-multi-container-shared-claim \
  npu-reclaim-after-release \
  npu-shared-resourceclaim-two-pods; do
  delete_namespace_if_exists "${ns}"
done

# Cleanup namespaces created during setup.
for ns in k8s-dra-driver-npu; do
  delete_namespace_if_exists "${ns}"
done
