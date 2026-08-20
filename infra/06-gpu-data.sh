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

# 06-gpu-data.sh — Firestore DB, demo GCS bucket, and workload GSAs for the GPU acts.
set -euo pipefail
# shellcheck disable=SC1091
source "$(dirname "$0")/lib/common.sh"
spotdemo::init

BUCKET="gs://${PROJECT}-spot-demo"

# Firestore (native mode). One (default) DB per project; describe-first.
if ! gcloud firestore databases describe --database='(default)' --project "${PROJECT}" >/dev/null 2>&1; then
  spotdemo::log "creating Firestore (default) database (nam5, native mode)"
  gcloud firestore databases create --database='(default)' --location=nam5 \
    --type=firestore-native --project "${PROJECT}" --quiet
fi

# multi-region demo bucket: corpus/ + embeddings/ (Act 2), checkpoints/ (Act 3).
# One describe serves both the existence guard and the location check.
loc=$(gcloud storage buckets describe "${BUCKET}" --project "${PROJECT}" \
  --format='value(location)' 2>/dev/null || true)
if [[ -z "${loc}" ]]; then
  spotdemo::log "creating ${BUCKET} in the US multi-region"
  gcloud storage buckets create "${BUCKET}" --project "${PROJECT}" \
    --location US --uniform-bucket-level-access --quiet
else
  # A bucket created before Plan 4 is pinned to one region. We never delete demo
  # data automatically; warn so the operator can migrate deliberately.
  if [[ "${loc}" != "US" ]]; then
    spotdemo::log "WARNING: ${BUCKET} is not the US multi-region (${loc})."
    spotdemo::log "WARNING: after a region move it will serve cross-region. See docs/runbook.md."
  fi
fi

# Workload GSAs + Workload Identity bindings (gsa:namespace:ksa).
for spec in \
  "spot-demo-embed:act2-embeddings:embed-worker" \
  "spot-demo-tune:act3-finetune:tune-worker"; do
  gsa="${spec%%:*}"; rest="${spec#*:}"; ns="${rest%%:*}"; ksa="${rest#*:}"
  email="${gsa}@${PROJECT}.iam.gserviceaccount.com"
  if ! gcloud iam service-accounts describe "${email}" --project "${PROJECT}" >/dev/null 2>&1; then
    spotdemo::log "creating GSA ${gsa}"
    gcloud iam service-accounts create "${gsa}" --project "${PROJECT}" --quiet
  fi
  spotdemo::retry 6 10 gcloud storage buckets add-iam-policy-binding "${BUCKET}" \
    --member "serviceAccount:${email}" --role roles/storage.objectAdmin --quiet >/dev/null
  spotdemo::retry 6 10 gcloud iam service-accounts add-iam-policy-binding "${email}" \
    --project "${PROJECT}" --role roles/iam.workloadIdentityUser \
    --member "serviceAccount:${PROJECT}.svc.id.goog[${ns}/${ksa}]" --quiet >/dev/null
done

# Firestore access for the Act 2 progress table.
spotdemo::retry 6 10 gcloud projects add-iam-policy-binding "${PROJECT}" \
  --member "serviceAccount:spot-demo-embed@${PROJECT}.iam.gserviceaccount.com" \
  --role roles/datastore.user --condition=None --quiet >/dev/null

spotdemo::log "gpu data plane ready"
