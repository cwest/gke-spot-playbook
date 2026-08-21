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

# 09-serving-iam.sh — grant the gkenode service agent storage.admin on the
# snapshot bucket. Act 5/6 lesson: with tokenSource: federatedP4SA, the
# checkpoint image is written to GCS by service-<PROJECT_NUMBER>@gcp-sa-gkenode,
# NOT container-engine-robot, so it needs storage.admin.
set -euo pipefail
# shellcheck disable=SC1091
source "$(dirname "$0")/lib/common.sh"
spotdemo::init

spotdemo::require gcloud

: "${SNAP_BUCKET:?set SNAP_BUCKET to the snapshot GCS bucket name (no gs:// prefix)}"

spotdemo::log "deriving PROJECT_NUMBER from PROJECT=${PROJECT}"
PROJECT_NUMBER=$(gcloud projects describe "${PROJECT}" --format='value(projectNumber)')
GKENODE_SA="service-${PROJECT_NUMBER}@gcp-sa-gkenode.iam.gserviceaccount.com"

spotdemo::log "granting roles/storage.admin on gs://${SNAP_BUCKET} to ${GKENODE_SA}"
spotdemo::retry 6 10 gcloud storage buckets add-iam-policy-binding "gs://${SNAP_BUCKET}" \
  --member="serviceAccount:${GKENODE_SA}" \
  --role="roles/storage.admin" \
  --project="${PROJECT}"

spotdemo::log "snapshot bucket IAM configured"
echo "  Bucket: gs://${SNAP_BUCKET}"
echo "  Grantee: ${GKENODE_SA}"
echo "  Role: roles/storage.admin"
