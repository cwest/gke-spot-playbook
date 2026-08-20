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

# Applies advisor-rendered ComputeClasses. batch-cpu is required; batch-gpu
# is validated server-side now (Plan 3 consumes it) and applied if present.
#
# The batch-gpu dry-run exists to confirm the flexStart rung schema (a question
# deferred from Plan 1). The gpu profile scores into whatever region has GPU
# capacity, which may differ from the cluster region; GKE Warden then rejects
# the manifest for out-of-cluster zones. That locality rejection is expected
# and is reconciled in Plan 3 — it is NOT a schema failure, so we surface it as
# a warning. Any other rejection (a genuine schema violation) is fatal.
set -euo pipefail
# shellcheck disable=SC1091
source "$(dirname "${BASH_SOURCE[0]}")/lib/common.sh"
spotdemo::init

CPU_CLASS="${SPOTDEMO_ROOT}/out/computeclass-cpu.yaml"
GPU_CLASS="${SPOTDEMO_ROOT}/out/computeclass-gpu.yaml"

[[ -f "${CPU_CLASS}" ]] || { spotdemo::log "missing ${CPU_CLASS} — run the advisor"; exit 1; }
kubectl apply --server-side -f "${CPU_CLASS}"
kubectl get computeclass batch-cpu -o yaml | head -40

if [[ -f "${GPU_CLASS}" ]]; then
  spotdemo::log "applying batch-gpu ComputeClass"
  kubectl apply --server-side -f "${GPU_CLASS}"
fi
