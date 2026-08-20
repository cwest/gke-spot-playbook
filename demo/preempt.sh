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

# The chaos beat: preempt one first spot node serving the target compute class.
set -euo pipefail
# shellcheck disable=SC1091
source "$(dirname "${BASH_SOURCE[0]}")/../infra/lib/common.sh"
spotdemo::init
CLASS="${CLASS:-batch-cpu}"

node="$(kubectl get nodes -l "cloud.google.com/gke-spot=true,cloud.google.com/compute-class=${CLASS}" \
  -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)"
[[ -n "${node}" ]] || { spotdemo::log "no ${CLASS} spot nodes found"; exit 1; }
zone="$(kubectl get node "${node}" -o jsonpath='{.metadata.labels.topology\.kubernetes\.io/zone}')"
spotdemo::log "simulating preemption of ${node} (${zone})"
gcloud compute instances simulate-maintenance-event "${node}" --zone "${zone}" --project "${PROJECT}"
spotdemo::log "sent — watch the queue keep draining"
