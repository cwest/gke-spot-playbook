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

# Builds and pushes worker images via Cloud Build.
set -euo pipefail
# shellcheck disable=SC1091
source "$(dirname "${BASH_SOURCE[0]}")/lib/common.sh"
spotdemo::init
[[ -n "${REGION:-}" ]] || { spotdemo::log "REGION unset — run the advisor"; exit 1; }

# One hostname for every US region. Task 11 adds another image build and reuses
# this variable; do not re-inline the literal.
AR_HOST="${AR_HOST:-us-docker.pkg.dev}"

for spec in \
  "workloads/01-queue/worker:queue-worker" \
  "workloads/02-embeddings/worker:embed-worker" \
  "workloads/03-finetune/trainer:tune-worker"; do
  dir="${spec%%:*}"; name="${spec#*:}"
  [[ -d "${SPOTDEMO_ROOT}/${dir}" ]] || continue
  IMAGE="${AR_HOST}/${PROJECT}/spot-demo/${name}:v1"
  spotdemo::log "building ${IMAGE}"
  gcloud builds submit "${SPOTDEMO_ROOT}/${dir}" --tag "${IMAGE}" \
    --project "${PROJECT}" --timeout=1800
done

spotdemo::log "building capacity-advisor image"
gcloud builds submit "${SPOTDEMO_ROOT}/advisor" \
  --project "${PROJECT}" \
  --timeout=1800 \
  --tag "${AR_HOST}/${PROJECT}/spot-demo/capacity-advisor:v1"
