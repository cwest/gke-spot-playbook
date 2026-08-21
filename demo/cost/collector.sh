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

# Samples cluster node inventory to CSV for the cost report.
# Usage: collector.sh [output.csv]   (Ctrl-C to stop)
set -euo pipefail
# shellcheck disable=SC1091
source "$(dirname "${BASH_SOURCE[0]}")/../../infra/lib/common.sh"
spotdemo::init
spotdemo::require kubectl

OUT="${1:-${SPOTDEMO_ROOT}/out/cost-samples.csv}"
INTERVAL="${COLLECT_INTERVAL:-30}"
mkdir -p "$(dirname "${OUT}")"
[[ -s "${OUT}" ]] || echo "ts,node,machine_type,lifecycle,compute_class" > "${OUT}"

running=1
trap 'running=0' INT TERM
spotdemo::log "collecting to ${OUT} every ${INTERVAL}s (Ctrl-C to stop)"
while (( running )); do
  ts="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  kubectl get nodes -o json 2>/dev/null | python3 -c '
import json, sys
ts = sys.argv[1]
for n in json.load(sys.stdin).get("items", []):
    labels = n["metadata"].get("labels", {})
    mt = labels.get("node.kubernetes.io/instance-type", "unknown")
    life = "spot" if labels.get("cloud.google.com/gke-spot") == "true" else "on-demand"
    cc = labels.get("cloud.google.com/compute-class", "-")
    print(f"{ts},{n['\''metadata'\'']['\''name'\'']},{mt},{life},{cc}")
' "${ts}" >> "${OUT}" || spotdemo::log "sample at ${ts} failed (kubectl error) — continuing"
  for _ in $(seq "${INTERVAL}"); do (( running )) || break; sleep 1; done
done
spotdemo::log "collector stopped; $(( $(wc -l < "${OUT}") - 1 )) samples in ${OUT}"
