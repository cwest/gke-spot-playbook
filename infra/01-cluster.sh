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

# Creates the advisor-informed GKE Standard cluster.
# Prereq: fresh advisor artifacts —
#   cd advisor && env -u GOOGLE_APPLICATION_CREDENTIALS \
#     go run ./cmd/capacity-advisor analyze --profile cpu-batch --config ../advisor.yaml --out ../out --render
set -euo pipefail
# shellcheck disable=SC1091
source "$(dirname "${BASH_SOURCE[0]}")/lib/common.sh"
spotdemo::init

CLUSTER="${CLUSTER:-spot-demo}"
[[ -n "${REGION:-}" && -n "${ZONES:-}" ]] || { spotdemo::log "run the advisor first (no cluster-config.env)"; exit 1; }

if spotdemo::exists gcloud container clusters describe "${CLUSTER}" --region "${REGION}" --project "${PROJECT}"; then
  spotdemo::log "cluster ${CLUSTER} already exists in ${REGION}"
else
  spotdemo::log "creating ${CLUSTER} in ${REGION} (zones ${ZONES})"
  gcloud container clusters create "${CLUSTER}" \
    --project "${PROJECT}" --region "${REGION}" --node-locations "${ZONES}" \
    --release-channel rapid \
    --workload-pool "${PROJECT}.svc.id.goog" \
    --num-nodes 1 --machine-type e2-standard-4 \
    --addons GcsFuseCsiDriver \
    --enable-ip-alias --quiet
fi
gcloud container clusters get-credentials "${CLUSTER}" --region "${REGION}" --project "${PROJECT}"
kubectl get nodes -o wide
