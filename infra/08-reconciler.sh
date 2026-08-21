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

# 08-reconciler.sh — deploy the in-cluster capacity reconciler: GSA + IAM,
# Workload Identity binding, RBAC, config, and the CronJob.
set -euo pipefail
# shellcheck disable=SC1091
source "$(dirname "$0")/lib/common.sh"
spotdemo::init

spotdemo::require gcloud
spotdemo::require kubectl

# Pinned, not overridable: all three manifests hardcode `namespace: spot-demo`,
# so honouring a NAMESPACE override here would bind Workload Identity and
# publish the config into one namespace while installing the workload into
# another.
NAMESPACE="spot-demo"
GSA_NAME="spot-demo-reconciler"
GSA="${GSA_NAME}@${PROJECT}.iam.gserviceaccount.com"
KSA="capacity-advisor"
REGION="${REGION:?REGION must be set (sourced from out/cluster-config.env)}"
CLUSTER="${CLUSTER:-spot-demo}"
AR_HOST="${AR_HOST:-us-docker.pkg.dev}"
IMAGE="${AR_HOST}/${PROJECT}/spot-demo/capacity-advisor:v1"
MANIFESTS="$(dirname "$0")/reconciler"
PROBE_AUTOMATED="${PROBE_AUTOMATED:-false}"

spotdemo::log "ensuring service account ${GSA}"
if ! spotdemo::exists gcloud iam service-accounts describe "${GSA}" --project "${PROJECT}"; then
  gcloud iam service-accounts create "${GSA_NAME}" \
    --project "${PROJECT}" \
    --display-name "GKE spot demo capacity reconciler"
fi

# Read-only in GCP. The reconciler queries capacity advice and reads the
# cluster-autoscaler visibility log; it never mutates a GCP resource.
for role in roles/compute.viewer roles/logging.viewer; do
  spotdemo::log "granting ${role}"
  spotdemo::retry 6 10 gcloud projects add-iam-policy-binding "${PROJECT}" \
    --member "serviceAccount:${GSA}" --role "${role}" --condition=None >/dev/null
done

# Probe automation: when enabled, the reconciler creates/deletes probe VMs.
# This role grants the instance.create, instance.get, and instance.delete
# permissions needed to Insert, Get, and Delete spot instances.
if [[ "${PROBE_AUTOMATED}" == "true" ]]; then
  spotdemo::log "granting roles/compute.instanceAdmin.v1 (probe automation enabled)"
  spotdemo::retry 6 10 gcloud projects add-iam-policy-binding "${PROJECT}" \
    --member "serviceAccount:${GSA}" --role roles/compute.instanceAdmin.v1 --condition=None >/dev/null
fi

spotdemo::log "binding Workload Identity ${NAMESPACE}/${KSA} -> ${GSA}"
spotdemo::retry 6 10 gcloud iam service-accounts add-iam-policy-binding "${GSA}" \
  --project "${PROJECT}" \
  --role roles/iam.workloadIdentityUser \
  --member "serviceAccount:${PROJECT}.svc.id.goog[${NAMESPACE}/${KSA}]" >/dev/null

kubectl get namespace "${NAMESPACE}" >/dev/null 2>&1 || kubectl create namespace "${NAMESPACE}"

spotdemo::log "publishing advisor.yaml as a ConfigMap"
kubectl -n "${NAMESPACE}" create configmap capacity-advisor-config \
  --from-file="advisor.yaml=${SPOTDEMO_ROOT}/advisor.yaml" \
  --dry-run=client -o yaml | kubectl apply --server-side -f -

spotdemo::log "applying RBAC and the CronJob"
sed -e "s|PROJECT_PLACEHOLDER_ID|${PROJECT}|g" \
    -e "s|PROJECT_PLACEHOLDER|${GSA}|g" \
    -e "s|REGION_PLACEHOLDER|${REGION}|g" \
    -e "s|CLUSTER_PLACEHOLDER|${CLUSTER}|g" \
    -e "s|IMAGE_PLACEHOLDER|${IMAGE}|g" \
    "${MANIFESTS}"/*.yaml | kubectl apply --server-side -f -

spotdemo::log "reconciler installed; first tick within 10 minutes"
echo "  dry run now:  kubectl -n ${NAMESPACE} create job --from=cronjob/capacity-advisor advisor-manual"
echo "  watch:        kubectl -n ${NAMESPACE} logs -l job-name=advisor-manual -f"
