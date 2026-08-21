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

# Creates Pub/Sub resources, service accounts, IAM and the image repo for Act 1.
set -euo pipefail
# shellcheck disable=SC1091
source "$(dirname "${BASH_SOURCE[0]}")/lib/common.sh"
spotdemo::init
[[ -n "${REGION:-}" ]] || { spotdemo::log "REGION unset — run the advisor"; exit 1; }

ensure_topic() {
  spotdemo::exists gcloud pubsub topics describe "$1" --project "${PROJECT}" ||
    gcloud pubsub topics create "$1" --project "${PROJECT}"
}
ensure_topic spot-demo-tasks
ensure_topic spot-demo-completions

spotdemo::exists gcloud pubsub subscriptions describe spot-demo-tasks-sub --project "${PROJECT}" ||
  gcloud pubsub subscriptions create spot-demo-tasks-sub --topic spot-demo-tasks \
    --ack-deadline 60 --project "${PROJECT}"
spotdemo::exists gcloud pubsub subscriptions describe spot-demo-completions-verify --project "${PROJECT}" ||
  gcloud pubsub subscriptions create spot-demo-completions-verify --topic spot-demo-completions \
    --ack-deadline 30 --project "${PROJECT}"

ensure_gsa() {
  spotdemo::exists gcloud iam service-accounts describe "$1@${PROJECT}.iam.gserviceaccount.com" --project "${PROJECT}" ||
    gcloud iam service-accounts create "$1" --project "${PROJECT}" --display-name "$2"
}
ensure_gsa spot-demo-worker "Spot demo Act1 worker"
ensure_gsa spot-demo-keda "Spot demo KEDA scaler"

WORKER_GSA="spot-demo-worker@${PROJECT}.iam.gserviceaccount.com"
KEDA_GSA="spot-demo-keda@${PROJECT}.iam.gserviceaccount.com"

spotdemo::retry 6 10 gcloud pubsub subscriptions add-iam-policy-binding spot-demo-tasks-sub \
  --member "serviceAccount:${WORKER_GSA}" --role roles/pubsub.subscriber --project "${PROJECT}" >/dev/null
spotdemo::retry 6 10 gcloud pubsub topics add-iam-policy-binding spot-demo-completions \
  --member "serviceAccount:${WORKER_GSA}" --role roles/pubsub.publisher --project "${PROJECT}" >/dev/null
for role in roles/monitoring.viewer roles/pubsub.viewer; do
  spotdemo::retry 6 10 gcloud projects add-iam-policy-binding "${PROJECT}" \
    --member "serviceAccount:${KEDA_GSA}" --role "${role}" --condition None >/dev/null
done

# Workload Identity: KSA -> GSA
spotdemo::retry 6 10 gcloud iam service-accounts add-iam-policy-binding "${WORKER_GSA}" \
  --member "serviceAccount:${PROJECT}.svc.id.goog[act1-queue/queue-worker]" \
  --role roles/iam.workloadIdentityUser --project "${PROJECT}" >/dev/null
spotdemo::retry 6 10 gcloud iam service-accounts add-iam-policy-binding "${KEDA_GSA}" \
  --member "serviceAccount:${PROJECT}.svc.id.goog[keda/keda-operator]" \
  --role roles/iam.workloadIdentityUser --project "${PROJECT}" >/dev/null

# The repo lives in the `us` multi-region, not ${REGION}: migrate-region.sh moves
# the cluster between US regions and must not have to rebuild or re-push images.
spotdemo::exists gcloud artifacts repositories describe spot-demo --location us --project "${PROJECT}" ||
  gcloud artifacts repositories create spot-demo --repository-format docker \
    --location us --project "${PROJECT}"
spotdemo::log "pubsub/iam/registry ready"
