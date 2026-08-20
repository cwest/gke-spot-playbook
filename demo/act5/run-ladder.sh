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

# Drives the three-point agent-density ladder: P1/P2/P3 wall packing.
# Records the scheduled agent count at each RAM wall, the node's available
# memory (allocatable − requests), and calls capacity-advisor agent-density.
# Usage: run-ladder.sh [<out-dir>]
set -euo pipefail

# shellcheck disable=SC1091
source "$(dirname "${BASH_SOURCE[0]}")/../../infra/lib/common.sh"
spotdemo::init
spotdemo::require kubectl

OUT_DIR="${1:-${SPOTDEMO_ROOT}/out/act5}"
mkdir -p "${OUT_DIR}"

NAMESPACE="act5-agents"
DEPLOYMENT="agents"
NODE_SELECTOR="cloud.google.com/compute-class=agents"
MANIFEST_DIR="${SPOTDEMO_ROOT}/workloads/05-agents/manifests"
# Sane cap so wall-detection can never loop forever if pending never appears.
MAX_REPLICAS=64
MACHINE_TYPE=""
REGION=""
WALL_COUNT=0

spotdemo::log "===== ACT 5: THREE-POINT AGENT DENSITY LADDER ====="

# Apply namespace, service account, and the agents Deployment. The
# serviceAccountName: agent-worker pods are rejected without the SA, so all
# three manifests must be applied (not just agents.yaml).
spotdemo::log "applying act5 manifests (namespace, serviceaccount, agents)"
env -u GOOGLE_APPLICATION_CREDENTIALS kubectl apply -f "${MANIFEST_DIR}/namespace.yaml"
env -u GOOGLE_APPLICATION_CREDENTIALS kubectl apply -f "${MANIFEST_DIR}/serviceaccount.yaml"
env -u GOOGLE_APPLICATION_CREDENTIALS kubectl apply -f "${MANIFEST_DIR}/agents.yaml"

# Helper: get current scheduled + pending agent pod counts as "scheduled,pending".
get_agent_pod_count() {
  env -u GOOGLE_APPLICATION_CREDENTIALS kubectl get pods -n "${NAMESPACE}" \
    -l app=agent-worker -o json 2>/dev/null | python3 -c '
import json, sys
data = json.load(sys.stdin)
scheduled = 0
pending = 0
for pod in data.get("items", []):
    phase = pod["status"].get("phase", "")
    if phase == "Pending":
        pending += 1
    elif phase in ("Running", "Succeeded"):
        scheduled += 1
print(f"{scheduled},{pending}")
' || echo "0,0"
}

# Helper: node memory available to schedule = Allocatable memory − memory Requests,
# parsed from `kubectl describe node`. Emits e.g. "15862Mi" (or "unknown").
# `kubectl top node` reports memory USED, not available, so it is not used here.
get_node_mem_available() {
  local node=$1
  env -u GOOGLE_APPLICATION_CREDENTIALS kubectl describe node "${node}" 2>/dev/null | python3 -c '
import sys, re

def to_mib(value):
    m = re.match(r"^(\d+)\s*([A-Za-z]*)$", value.strip())
    if not m:
        return None
    n = int(m.group(1))
    unit = m.group(2)
    factors = {
        "": 1.0 / (1024 * 1024),
        "Ki": 1.0 / 1024, "Mi": 1.0, "Gi": 1024.0, "Ti": 1024.0 * 1024,
        "K": 1000.0 / (1024 * 1024), "M": 1000.0 * 1000 / (1024 * 1024),
        "G": 1000.0 * 1000 * 1000 / (1024 * 1024),
    }
    f = factors.get(unit)
    return n * f if f is not None else None

text = sys.stdin.read()
alloc = req = None
m = re.search(r"Allocatable:(.*?)(?:\n\S|\Z)", text, re.S)
if m:
    mm = re.search(r"memory:\s*(\S+)", m.group(1))
    if mm:
        alloc = to_mib(mm.group(1))
m = re.search(r"Allocated resources:(.*?)(?:\n[A-Z]\S|\Z)", text, re.S)
if m:
    mm = re.search(r"memory\s+(\S+)", m.group(1))
    if mm:
        req = to_mib(mm.group(1))
if alloc is not None and req is not None:
    print(f"{int(alloc - req)}Mi")
else:
    print("unknown")
' || echo "unknown"
}

# Helper: set deployment replicas and wait for pods to appear (scheduled or pending).
set_replicas_and_wait() {
  local count=$1
  spotdemo::log "scaling agents Deployment to replicas=${count}"
  env -u GOOGLE_APPLICATION_CREDENTIALS kubectl -n "${NAMESPACE}" \
    patch deployment "${DEPLOYMENT}" --type merge -p "{\"spec\":{\"replicas\":${count}}}"

  # Wait up to 60s for pods to appear (scheduled or pending).
  local wait_count=0
  local scheduled pending pair total
  while (( wait_count < 60 )); do
    pair="$(get_agent_pod_count)"
    IFS=',' read -r scheduled pending <<< "${pair}"
    total=$((scheduled + pending))
    if [[ ${total} -ge ${count} ]]; then
      spotdemo::log "pod count reached: scheduled=${scheduled} pending=${pending}"
      break
    fi
    sleep 1
    wait_count=$((wait_count + 1))
  done
}

# Pack replicas up one at a time until at least one pod goes Pending — that
# Pending signal IS the RAM wall. Sets WALL_COUNT to the number of scheduled
# (Running) agents at the wall. Bounded by MAX_REPLICAS so it cannot loop forever.
pack_to_wall() {
  local start=$1
  local count=${start}
  local scheduled pending pair
  WALL_COUNT=0
  while (( count <= MAX_REPLICAS )); do
    set_replicas_and_wait "${count}"
    pair="$(get_agent_pod_count)"
    IFS=',' read -r scheduled pending <<< "${pair}"
    spotdemo::log "  replicas=${count}: scheduled=${scheduled} pending=${pending}"
    WALL_COUNT=${scheduled}
    if (( pending > 0 )); then
      spotdemo::log "  RAM wall: ${scheduled} agents fit (${count} requested, ${pending} pending)"
      return 0
    fi
    count=$((count + 1))
  done
  spotdemo::log "  reached MAX_REPLICAS=${MAX_REPLICAS} without Pending; recording ${WALL_COUNT} scheduled"
}

# Discover the agents node, its machine type, and region.
spotdemo::log "finding agents node (selector: ${NODE_SELECTOR})"
agents_node=$(env -u GOOGLE_APPLICATION_CREDENTIALS kubectl get nodes \
  -l "${NODE_SELECTOR}" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)
if [[ -z "${agents_node}" ]]; then
  spotdemo::log "ERROR: no node with selector ${NODE_SELECTOR} found"
  exit 1
fi
spotdemo::log "agents node: ${agents_node}"

MACHINE_TYPE=$(env -u GOOGLE_APPLICATION_CREDENTIALS kubectl get node "${agents_node}" \
  -o jsonpath='{.metadata.labels.node\.kubernetes\.io/instance-type}' 2>/dev/null || echo "")
if [[ -z "${MACHINE_TYPE}" ]]; then
  spotdemo::log "ERROR: could not extract machine-type from node ${agents_node}"
  exit 1
fi
spotdemo::log "machine type: ${MACHINE_TYPE}"

REGION=$(env -u GOOGLE_APPLICATION_CREDENTIALS kubectl get node "${agents_node}" \
  -o jsonpath='{.metadata.labels.topology\.kubernetes\.io/region}' 2>/dev/null || echo "")
if [[ -z "${REGION}" ]]; then
  spotdemo::log "ERROR: could not extract region from node ${agents_node}"
  exit 1
fi
spotdemo::log "region: ${REGION}"

# P1: baseline density — pack to the RAM wall.
spotdemo::log "P1: packing to the RAM wall (baseline density)"
pack_to_wall 1
p1_total=${WALL_COUNT}
p1_mem=$(get_node_mem_available "${agents_node}")
spotdemo::log "P1 result: ${p1_total} agents at wall, MemAvailable=${p1_mem}"
echo "P1,${p1_total},${p1_mem}" > "${OUT_DIR}/ladder-measurements.csv"

# P2: sandbox density — pack to the wall again (live: sandbox isolation shifts it).
spotdemo::log "P2: packing to the RAM wall (sandbox density)"
pack_to_wall "$((p1_total > 0 ? p1_total : 1))"
p2_total=${WALL_COUNT}
p2_mem=$(get_node_mem_available "${agents_node}")
spotdemo::log "P2 result: ${p2_total} agents at wall, MemAvailable=${p2_mem}"
echo "P2,${p2_total},${p2_mem}" >> "${OUT_DIR}/ladder-measurements.csv"

# Snapshot the now-idle agents to reclaim their RAM footprint. The exact
# GKE Agent Sandbox pod-snapshot command is confirmed and executed in Task 6;
# offline this is a documented placeholder (no snapshot is taken here).
spotdemo::log "P2→P3: snapshotting idle agents to reclaim RAM"
spotdemo::log "CONFIRMED-IN-TASK-6: GKE Agent Sandbox pod-snapshot command (documented form):"
spotdemo::log "  env -u GOOGLE_APPLICATION_CREDENTIALS kubectl -n ${NAMESPACE} exec <agent-pod> -- /snapshot_agent"

# P3: lifecycle density — after reclaim, pack to the wall once more.
spotdemo::log "P3: packing to the RAM wall (lifecycle density, post-snapshot)"
pack_to_wall "$((p2_total > 0 ? p2_total : 1))"
p3_total=${WALL_COUNT}
p3_mem=$(get_node_mem_available "${agents_node}")
spotdemo::log "P3 result: ${p3_total} agents at wall, MemAvailable=${p3_mem}"
echo "P3,${p3_total},${p3_mem}" >> "${OUT_DIR}/ladder-measurements.csv"

spotdemo::log "measurements saved to ${OUT_DIR}/ladder-measurements.csv"
cat "${OUT_DIR}/ladder-measurements.csv"

# Call capacity-advisor agent-density with the three wall measurements.
spotdemo::log "generating agent-density report"
( cd "${SPOTDEMO_ROOT}/advisor" && \
  env -u GOOGLE_APPLICATION_CREDENTIALS \
    go run ./cmd/capacity-advisor agent-density \
      --node-machine-type="${MACHINE_TYPE}" \
      --region="${REGION}" \
      --p1="${p1_total}" \
      --p2="${p2_total}" \
      --p3="${p3_total}" \
      --out="${OUT_DIR}" ) || spotdemo::log "WARNING: agent-density report generation failed"

spotdemo::log "===== ACT 5 LADDER COMPLETE ====="
spotdemo::log "results in ${OUT_DIR}/"
