#!/usr/bin/env bash
# Copyright 2026 Google LLC
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

# 07-kueue.sh — install Kueue (pinned) and the demo's cluster-scoped queue objects.
set -euo pipefail
# shellcheck disable=SC1091
source "$(dirname "$0")/lib/common.sh"
spotdemo::init

KUEUE_VERSION="${KUEUE_VERSION:-v0.19.0}"

spotdemo::log "installing Kueue ${KUEUE_VERSION}"
kubectl apply --server-side -f \
  "https://github.com/kubernetes-sigs/kueue/releases/download/${KUEUE_VERSION}/manifests.yaml"
kubectl -n kueue-system rollout status deployment/kueue-controller-manager --timeout=300s

# Kueue v0.19 removed the enable field and defaults waitForPodsReady ON; under a
# capacity stall this evicts Pending pods every ~5 min and starves node
# auto-provisioning of pod pressure (observed live 2026-07-25). Neutralize via
# very long timeout; feature-gate alternative (DisableWaitForPodsReady=true)
# would need a deployment patch and is rejected for simplicity.
spotdemo::log "neutralizing default-on waitForPodsReady via timeout"
cfg=$(kubectl -n kueue-system get configmap kueue-manager-config \
  -o jsonpath='{.data.controller_manager_config\.yaml}')
[[ -n "${cfg}" ]] || { spotdemo::log "ERROR: kueue-manager-config key came back empty"; exit 1; }
if ! grep -q '^waitForPodsReady:' <<<"${cfg}"; then
  tmp=$(mktemp)
  trap 'rm -f "${tmp}"' EXIT
  printf '%s\nwaitForPodsReady:\n  timeout: 9999h\n' "${cfg}" > "${tmp}"
  kubectl -n kueue-system create configmap kueue-manager-config \
    --from-file=controller_manager_config.yaml="${tmp}" \
    --dry-run=client -o yaml | kubectl apply --server-side --force-conflicts -f -
  kubectl -n kueue-system rollout restart deployment kueue-controller-manager
  kubectl -n kueue-system rollout status deployment kueue-controller-manager --timeout=180s
fi

# CRDs must be Established before the queue objects apply cleanly.
kubectl wait --for=condition=Established crd/clusterqueues.kueue.x-k8s.io --timeout=120s
kubectl apply --server-side -f "${SPOTDEMO_ROOT}/infra/kueue/resources.yaml"
spotdemo::log "kueue ready: $(kubectl get clusterqueue gpu-cq -o name)"
