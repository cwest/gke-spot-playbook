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

# Destroys everything the spot demo created. Idempotent; safe when partial.
set -euo pipefail
# shellcheck disable=SC1091
source "$(dirname "${BASH_SOURCE[0]}")/lib/common.sh"
spotdemo::init
CLUSTER="${CLUSTER:-spot-demo}"

for sub in spot-demo-tasks-sub spot-demo-completions-verify; do
  spotdemo::exists gcloud pubsub subscriptions describe "${sub}" --project "${PROJECT}" &&
    gcloud pubsub subscriptions delete "${sub}" --project "${PROJECT}" --quiet
done
for topic in spot-demo-tasks spot-demo-completions; do
  spotdemo::exists gcloud pubsub topics describe "${topic}" --project "${PROJECT}" &&
    gcloud pubsub topics delete "${topic}" --project "${PROJECT}" --quiet
done
KEDA_GSA="spot-demo-keda@${PROJECT}.iam.gserviceaccount.com"
for role in roles/monitoring.viewer roles/pubsub.viewer; do
  gcloud projects remove-iam-policy-binding "${PROJECT}" \
    --member "serviceAccount:${KEDA_GSA}" --role "${role}" --condition None --quiet >/dev/null 2>&1 || true
done
for gsa in spot-demo-worker spot-demo-keda; do
  spotdemo::exists gcloud iam service-accounts describe "${gsa}@${PROJECT}.iam.gserviceaccount.com" --project "${PROJECT}" &&
    gcloud iam service-accounts delete "${gsa}@${PROJECT}.iam.gserviceaccount.com" --project "${PROJECT}" --quiet
done
spotdemo::exists gcloud artifacts repositories describe spot-demo --location us --project "${PROJECT}" &&
  gcloud artifacts repositories delete spot-demo --location us --project "${PROJECT}" --quiet

# Legacy: stacks built before the multi-region move have a ${REGION}-pinned repo.
# Teardown must still remove it or it is orphaned and silently billed.
if [[ -n "${REGION:-}" ]] && spotdemo::exists gcloud artifacts repositories describe spot-demo \
    --location "${REGION}" --project "${PROJECT}"; then
  spotdemo::log "removing legacy ${REGION}-pinned artifact registry repo"
  gcloud artifacts repositories delete spot-demo --location "${REGION}" --project "${PROJECT}" --quiet
fi
if [[ -n "${REGION:-}" ]] && spotdemo::exists gcloud container clusters describe "${CLUSTER}" --region "${REGION}" --project "${PROJECT}"; then
  spotdemo::log "deleting cluster ${CLUSTER} (${REGION})"
  gcloud container clusters delete "${CLUSTER}" --region "${REGION}" --project "${PROJECT}" --quiet
fi
# GPU acts data plane (Plan 3)
gcloud projects remove-iam-policy-binding "${PROJECT}" \
  --member "serviceAccount:spot-demo-embed@${PROJECT}.iam.gserviceaccount.com" \
  --role roles/datastore.user --condition=None --quiet >/dev/null 2>&1 || true
for gsa in spot-demo-embed spot-demo-tune; do
  gcloud iam service-accounts delete "${gsa}@${PROJECT}.iam.gserviceaccount.com" \
    --project "${PROJECT}" --quiet 2>/dev/null || true
done
gcloud storage rm -r "gs://${PROJECT}-spot-demo" 2>/dev/null || true
# The Firestore (default) DB stays: deleting a project's default DB is invasive
# and an empty DB costs nothing. Progress docs live in collection
# 'embedding-progress'; delete them manually if desired.
spotdemo::log "removing the reconciler service account"
GSA="spot-demo-reconciler@${PROJECT}.iam.gserviceaccount.com"
if spotdemo::exists gcloud iam service-accounts describe "${GSA}" --project "${PROJECT}"; then
  for role in roles/compute.viewer roles/logging.viewer; do
    gcloud projects remove-iam-policy-binding "${PROJECT}" \
      --member "serviceAccount:${GSA}" --role "${role}" --condition=None >/dev/null || true
  done
  gcloud iam service-accounts delete "${GSA}" --project "${PROJECT}" --quiet || true
fi
# Later tasks append resource deletions ABOVE this line.
spotdemo::log "teardown complete"
