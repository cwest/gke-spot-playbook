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

# 09-serving-nodepool.sh — create the version-pinned gVisor GPU spot node pool
# for LLM serving with Pod snapshots (Act 6). Requires snapshot-enabled cluster
# (see infra/serving/snapshot-storage.md for the WI-first ordering).
set -euo pipefail
# shellcheck disable=SC1091
source "$(dirname "$0")/lib/common.sh"
spotdemo::init

spotdemo::require gcloud

CLUSTER="${CLUSTER:-spot-demo}"
REGION="${REGION:?REGION must be set (sourced from out/cluster-config.env)}"

# Multi-zone node locations improve spot obtainability (spike observed
# g2/L4 on-demand region-stockout while spot had capacity).
NODE_LOCATIONS="us-central1-a,us-central1-b,us-central1-c"

spotdemo::log "creating l4-gvisor-spot node pool (g2-standard-8, 1x nvidia-l4, gvisor, spot)"
if ! spotdemo::exists gcloud container node-pools describe l4-gvisor-spot \
  --cluster "${CLUSTER}" --region "${REGION}" --project "${PROJECT}"; then
  gcloud container node-pools create l4-gvisor-spot \
    --cluster "${CLUSTER}" --region "${REGION}" --project "${PROJECT}" \
    --node-locations "${NODE_LOCATIONS}" \
    --machine-type g2-standard-8 \
    --accelerator type=nvidia-l4,count=1,gpu-driver-version=latest \
    --spot --sandbox type=gvisor \
    --num-nodes 0 --enable-autoscaling --min-nodes 0 --max-nodes 2
else
  spotdemo::log "l4-gvisor-spot already exists"
fi

spotdemo::log "recording GKE version + GPU driver for restore-matching"
GKE_VERSION=$(gcloud container clusters describe "${CLUSTER}" \
  --region "${REGION}" --project "${PROJECT}" \
  --format='value(currentMasterVersion)')
echo "  GKE version: ${GKE_VERSION}"
echo "  GPU driver:  latest (580.x / CUDA 13-capable; required for vllm-openai:latest)"
echo ""
echo "Pod snapshots persist in GCS independent of node lifetime, so restore survives preemption."
