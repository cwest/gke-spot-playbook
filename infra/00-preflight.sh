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

# Verifies the environment can run the spot demo before anything is created.
set -euo pipefail
# shellcheck disable=SC1091
source "$(dirname "${BASH_SOURCE[0]}")/lib/common.sh"
spotdemo::init

for cmd in gcloud kubectl go; do spotdemo::require "$cmd"; done

spotdemo::log "checking ADC identity can list regions on ${PROJECT}"
gcloud compute regions list --project "${PROJECT}" --limit 1 --format 'value(name)' >/dev/null

REQUIRED_APIS=(container.googleapis.com compute.googleapis.com pubsub.googleapis.com
  artifactregistry.googleapis.com cloudbuild.googleapis.com monitoring.googleapis.com
  firestore.googleapis.com storage.googleapis.com logging.googleapis.com)
enabled="$(gcloud services list --enabled --project "${PROJECT}" --format 'value(config.name)')"
for api in "${REQUIRED_APIS[@]}"; do
  if ! grep -q "^${api}$" <<<"${enabled}"; then
    spotdemo::log "enabling ${api}"
    gcloud services enable "${api}" --project "${PROJECT}"
  fi
done

if [[ -z "${REGION:-}" ]]; then
  spotdemo::log "no out/cluster-config.env yet — run the advisor first (see infra/01-cluster.sh header). Skipping version check."
  exit 0
fi

MIN_VERSION="1.35.2-gke.1842000"
DEFAULT_VERSION="$(gcloud container get-server-config --region "${REGION}" --project "${PROJECT}" \
  --format 'value(channels.filter("channel:RAPID").extract(defaultVersion).flatten())' 2>/dev/null)"
[[ -n "${DEFAULT_VERSION}" ]] || { spotdemo::log "FATAL: could not read rapid-channel default version"; exit 1; }
spotdemo::log "rapid channel default in ${REGION}: ${DEFAULT_VERSION}"
if [[ "$(printf '%s\n%s\n' "${MIN_VERSION}" "${DEFAULT_VERSION}" | sort -V | head -1)" != "${MIN_VERSION}" ]]; then
  spotdemo::log "FATAL: rapid default ${DEFAULT_VERSION} < required ${MIN_VERSION}"
  exit 1
fi

  spotdemo::log "checking L4 GPU quota in ${REGION}"
  quota_json=$(gcloud compute regions describe "${REGION}" --project "${PROJECT}" --format=json)
  if ! bad=$(python3 -c '
import json, sys
d = json.load(sys.stdin)
q = {x["metric"]: x["limit"] for x in d.get("quotas", [])}
need = {"NVIDIA_L4_GPUS": 2, "PREEMPTIBLE_NVIDIA_L4_GPUS": 2}
bad = ["%s limit %s < %s" % (m, q.get(m, 0), n) for m, n in need.items() if q.get(m, 0) < n]
print("; ".join(bad))
sys.exit(1 if bad else 0)' <<<"${quota_json}"); then
    spotdemo::log "ERROR: insufficient L4 quota in ${REGION}: ${bad} — request an increase, or pick another region"
    exit 1
  fi

spotdemo::log "preflight OK"
