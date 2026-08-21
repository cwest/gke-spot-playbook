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

# Live split view: spot nodes, Act 1 worker replicas, queue backlog.
#
# Backlog note: `gcloud monitoring time-series list` (used by the original
# brief) is NOT present in the installed SDK — it is absent from the GA, beta,
# AND alpha tracks in gcloud 576.0.0 ("Invalid choice: 'time-series'"). Rather
# than depend on a drifting/removed CLI (and the GNU-vs-macOS `date -v` split it
# dragged in), we read the SAME signal KEDA reads: the gcp-pubsub scaler exposes
# subscription num_undelivered_messages as the external metric on its managed
# HPA, whose TARGETS column shows `<current>/<target> (avg)`. This uses only
# kubectl (already required), is portable across OSes, and stays in lockstep
# with the scaling decisions the demo is showcasing.
set -euo pipefail
# shellcheck disable=SC1091
source "$(dirname "${BASH_SOURCE[0]}")/../infra/lib/common.sh"
spotdemo::init
spotdemo::require watch

# Each section guards its own lookup so a not-yet-created resource renders a
# friendly line instead of blanking the pane. `watch` re-runs this every 5s.
watch -n 5 "
echo '=== NODES (compute-class / spot) ==='
kubectl get nodes -L cloud.google.com/compute-class,cloud.google.com/gke-spot --no-headers 2>/dev/null || echo 'no nodes'
echo
echo '=== ACT 1 WORKERS (KEDA-driven replicas) ==='
kubectl -n act1-queue get deploy queue-worker --no-headers 2>/dev/null || echo 'deployment not found'
echo
echo '=== BACKLOG (KEDA num_undelivered_messages / target, avg) ==='
kubectl -n act1-queue get hpa keda-hpa-queue-worker --no-headers 2>/dev/null || echo 'no HPA yet (ScaledObject inactive at 0)'
"
