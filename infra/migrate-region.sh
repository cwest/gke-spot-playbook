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

# migrate-region.sh — move the spot-demo cluster to another US region.
#
# The reconciler advises region moves; it never performs them. This is the
# command it prints. Images (us multi-region AR) and demo data (US multi-region
# bucket) are region-agnostic, so only the cluster and its in-cluster objects
# are recreated.
set -euo pipefail
# shellcheck disable=SC1091
source "$(dirname "$0")/lib/common.sh"

# One list, two consumers: plan() renders it and the executor runs it, so the
# printed plan cannot drift from what the migration actually does.
# 08-reconciler.sh is a forward reference (it does not exist yet), which is why
# the executor tolerates a missing step.
STEPS=(01-cluster.sh 02-computeclasses.sh 05-keda.sh 07-kueue.sh 08-reconciler.sh)

usage() {
  cat >&2 <<'EOF'
usage: migrate-region.sh <us-region> [--dry-run]

  <us-region>  target region, e.g. us-east4. Must start with "us-" so the
               multi-region registry and bucket stay local. Migrating to the
               region the cluster is already in is a no-op, so pasting the
               command twice is safe. That also means this cannot rebuild a
               deleted cluster in place — for that, run infra/01-cluster.sh.
  --dry-run    print the plan and exit without touching anything.

  PROFILE      env override picking which out/cluster-config-<profile>.env is
               designated out/cluster-config.env, i.e. which profile's region
               and zones the cluster is built from (default: cpu-batch). Every
               profile in advisor.yaml is re-rendered regardless.
EOF
  exit 2
}

# Flags are scanned across every argument rather than read out of a fixed slot:
# on a destructive script a misplaced --dry-run must never be silently ignored,
# and an unrecognized argument is fatal rather than a no-op.
TARGET=""
DRY_RUN=0
for arg in "$@"; do
  case "${arg}" in
    --dry-run) DRY_RUN=1 ;;
    -h | --help) usage ;;
    -*)
      spotdemo::log "ERROR: unknown option: ${arg}"
      usage
      ;;
    *)
      [[ -z "${TARGET}" ]] || {
        spotdemo::log "ERROR: unexpected argument: ${arg}"
        usage
      }
      TARGET="${arg}"
      ;;
  esac
done

[[ -n "${TARGET}" ]] || usage
[[ "${TARGET}" == us-* ]] || {
  spotdemo::log "ERROR: ${TARGET} is not a US region; AR and the bucket are US-scoped"
  usage
}

spotdemo::init
CLUSTER="${CLUSTER:-spot-demo}"
# PROFILE picks one thing only: which out/cluster-config-<profile>.env is
# designated out/cluster-config.env, and therefore which profile's region and
# zones 01-cluster.sh builds --node-locations from. It does NOT narrow which
# profiles get re-rendered; see the advisor loop below.
PROFILE="${PROFILE:-cpu-batch}"
FROM="${REGION:-<unset>}"
ENV_FILE="${SPOTDEMO_ROOT}/out/cluster-config.env"
PROFILE_ENV="${SPOTDEMO_ROOT}/out/cluster-config-${PROFILE}.env"
ADVISOR_CONFIG="${SPOTDEMO_ROOT}/advisor.yaml"

# Idempotent by contract: the reconciler prints `infra/migrate-region.sh
# <region>` as a literal string a human pastes, and a second paste must not
# delete the cluster the first one just built. Checked before anything else so
# the no-op costs nothing and --dry-run reports it too.
[[ "${FROM}" != "${TARGET}" ]] || {
  spotdemo::log "already in ${TARGET} — nothing to migrate"
  exit 0
}

# The advisor renders computeclass-<kind>.yaml per profile and
# 02-computeclasses.sh applies every one it finds, so re-rendering only the
# designated profile would leave the others' ComputeClasses pinned to the old
# region's zones — and those get applied to the new cluster. Re-render every
# profile advisor.yaml defines instead, read out of the file so a future
# profile is picked up without touching this script. Profile names are the
# two-space keys directly under "profiles:"; their settings are indented
# further and are skipped. Comments are dropped before anything else: a
# column-0 comment between two profiles would otherwise read as a top-level
# key, silently truncating the list and leaving a stale ComputeClass behind.
advisor_profiles() {
  awk '
    /^[ \t]*#/ { next }
    /^profiles:[ \t]*$/ { inblock = 1; next }
    inblock && /^[^ \t]/ { inblock = 0 }
    inblock && /^  [A-Za-z0-9_.-]+:[ \t]*$/ { sub(/:[ \t]*$/, ""); sub(/^  /, ""); print }
  ' "$1"
}

[[ -f "${ADVISOR_CONFIG}" ]] || {
  spotdemo::log "no ${ADVISOR_CONFIG} to read profiles from"
  exit 1
}
PROFILES=()
while IFS= read -r line; do
  PROFILES+=("${line}")
done < <(advisor_profiles "${ADVISOR_CONFIG}")
((${#PROFILES[@]})) || {
  spotdemo::log "no profiles defined in ${ADVISOR_CONFIG}"
  exit 1
}

plan() {
  local n=0 step profile
  printf 'migrate %s: %s -> %s\n\n' "${CLUSTER}" "${FROM}" "${TARGET}"
  printf '  %d. delete cluster %s in %s\n' "$((++n))" "${CLUSTER}" "${FROM}"
  printf '  %d. re-run the advisor for every advisor.yaml profile, pinned to %s:\n' \
    "$((++n))" "${TARGET}"
  for profile in "${PROFILES[@]}"; do
    printf '       analyze --profile %s --regions %s\n' "${profile}" "${TARGET}"
  done
  printf '  %d. designate out/cluster-config.env from out/cluster-config-%s.env\n' \
    "$((++n))" "${PROFILE}"
  for step in "${STEPS[@]}"; do
    printf '  %d. re-run infra/%s\n' "$((++n))" "${step}"
  done
  printf '\n  unchanged: us-docker.pkg.dev images, gs://%s-spot-demo (US multi-region)\n' \
    "${PROJECT}"
}

if ((DRY_RUN)); then
  plan
  spotdemo::log "DRY RUN — nothing was changed"
  exit 0
fi

spotdemo::require gcloud
spotdemo::require kubectl
spotdemo::require go
plan >&2
read -r -p "proceed? this deletes the ${FROM} cluster [y/N] " ans
[[ "${ans}" == "y" || "${ans}" == "Y" ]] || {
  spotdemo::log "aborted"
  exit 1
}

# Teardown runs first and ENV_FILE is not touched until it is done: the delete
# resolves the old region out of that file (see demo/act2/runbook.md Beat 0.5).
if [[ "${FROM}" != "<unset>" ]]; then
  if spotdemo::exists gcloud container clusters describe "${CLUSTER}" \
    --region "${FROM}" --project "${PROJECT}"; then
    spotdemo::log "deleting ${CLUSTER} in ${FROM}"
    gcloud container clusters delete "${CLUSTER}" \
      --region "${FROM}" --project "${PROJECT}" --quiet
  else
    # Re-runnable after a half-finished migration: a cluster that is already
    # gone is the state teardown was trying to reach.
    spotdemo::log "no ${CLUSTER} in ${FROM} — nothing to tear down"
  fi
fi

# Regenerate the config instead of hand-editing REGION into it: 01-cluster.sh
# hard-requires ZONES too, and only the advisor knows the target's zones.
# --regions pins the analysis to the migration target. Every profile is
# re-rendered so no out/computeclass-*.yaml is left holding old-region zones.
# spotdemo::init has already unset GOOGLE_APPLICATION_CREDENTIALS for this
# process and its children.
mkdir -p "${SPOTDEMO_ROOT}/out"
for profile in "${PROFILES[@]}"; do
  spotdemo::log "running the advisor for ${profile} pinned to ${TARGET}"
  (
    cd "${SPOTDEMO_ROOT}/advisor" &&
      go run ./cmd/capacity-advisor analyze \
        --profile "${profile}" --regions "${TARGET}" \
        --config ../advisor.yaml --out ../out --render
  )
done
[[ -f "${PROFILE_ENV}" ]] || {
  spotdemo::log "advisor wrote no ${PROFILE_ENV}"
  exit 1
}
grep -q "^REGION=${TARGET}\$" "${PROFILE_ENV}" || {
  spotdemo::log "advisor did not pin REGION=${TARGET} in ${PROFILE_ENV}"
  exit 1
}

spotdemo::log "designating ${ENV_FILE} for ${TARGET}"
cp "${PROFILE_ENV}" "${ENV_FILE}"

for step in "${STEPS[@]}"; do
  if [[ -x "${SPOTDEMO_ROOT}/infra/${step}" ]]; then
    spotdemo::log "running ${step}"
    "${SPOTDEMO_ROOT}/infra/${step}"
  else
    spotdemo::log "skipping ${step} (not present)"
  fi
done

spotdemo::log "migration to ${TARGET} complete"
