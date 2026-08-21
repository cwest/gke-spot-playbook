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

# Structural checks on the reconciler manifests and install script.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
fails=0

check() {
  local desc="$1" file="$2" pattern="$3"
  if grep -qE -- "${pattern}" "${ROOT}/${file}"; then
    printf 'ok   %s\n' "${desc}"
  else
    printf 'FAIL %s (%s !~ %s)\n' "${desc}" "${file}" "${pattern}"
    fails=$((fails+1))
  fi
}

refute() {
  local desc="$1" file="$2" pattern="$3"
  if grep -qE -- "${pattern}" "${ROOT}/${file}"; then
    printf 'FAIL %s (%s still matches %s)\n' "${desc}" "${file}" "${pattern}"
    fails=$((fails+1))
  else
    printf 'ok   %s\n' "${desc}"
  fi
}

M="${ROOT}/infra/reconciler"
S="${ROOT}/infra/08-reconciler.sh"

# Basic structure
check "install script has a bash shebang" "infra/08-reconciler.sh" "^#!/usr/bin/env bash"
if [ ! -x "${S}" ]; then
  printf 'FAIL install script is executable (file not executable)\n'
  fails=$((fails+1))
fi
refute "namespace is not overridable from the environment" "infra/08-reconciler.sh" 'NAMESPACE="\$\{NAMESPACE:-'

# 04-build.sh produces the image this CronJob runs; a default-timeout build
# fails at the end of a long provisioning run.
if sed -n '/spotdemo::log "building capacity-advisor image"/,/^$/p' "${ROOT}/infra/04-build.sh" | grep -qE -- '--timeout=1800'; then
  printf 'ok   advisor image build sets an explicit timeout\n'
else
  printf 'FAIL advisor image build sets an explicit timeout\n'
  fails=$((fails+1))
fi

# GCP IAM scoping
check "grants only read roles in GCP" "infra/08-reconciler.sh" "roles/compute\.viewer"
refute "does not grant compute.admin" "infra/08-reconciler.sh" "roles/compute\.admin"
refute "does not grant editor" "infra/08-reconciler.sh" "roles/editor"
refute "does not grant owner" "infra/08-reconciler.sh" "roles/owner"
refute "does not grant container.admin" "infra/08-reconciler.sh" "roles/container\.admin"
check "binds workload identity" "infra/08-reconciler.sh" "workloadIdentityUser"
# A binding issued against a just-created GSA can 404 until it propagates; the
# sibling scripts (03-pubsub.sh, 06-gpu-data.sh) all retry for this reason.
check "retries IAM bindings against a fresh service account" "infra/08-reconciler.sh" \
  "spotdemo::retry 6 10 gcloud iam service-accounts add-iam-policy-binding"
check "retries the project role bindings" "infra/08-reconciler.sh" \
  "spotdemo::retry 6 10 gcloud projects add-iam-policy-binding"

# Sed substitution ordering (PROJECT_PLACEHOLDER_ID before PROJECT_PLACEHOLDER)
id_line=$(grep -n "PROJECT_PLACEHOLDER_ID" "${S}" | head -1 | cut -d: -f1)
gsa_line=$(grep -n "s|PROJECT_PLACEHOLDER|" "${S}" | head -1 | cut -d: -f1)
if [ "${id_line}" -lt "${gsa_line}" ]; then
  printf 'ok   substitutes PROJECT_PLACEHOLDER_ID before PROJECT_PLACEHOLDER\n'
else
  printf 'FAIL substitutes PROJECT_PLACEHOLDER_ID before PROJECT_PLACEHOLDER (ordering reversed)\n'
  fails=$((fails+1))
fi

# No placeholders survive
if ! sed -e 's|PROJECT_PLACEHOLDER_ID|p|g' \
         -e 's|PROJECT_PLACEHOLDER|g@p.iam.gserviceaccount.com|g' \
         -e 's|REGION_PLACEHOLDER|r|g' \
         -e 's|CLUSTER_PLACEHOLDER|c|g' \
         -e 's|IMAGE_PLACEHOLDER|i|g' \
         "${M}"/*.yaml | grep -qE 'PLACEHOLDER'; then
  printf 'ok   no placeholder survives substitution dry run\n'
else
  printf 'FAIL no placeholder survives substitution dry run\n'
  fails=$((fails+1))
fi

# CronJob concurrency policy
check "cronjob forbids concurrent ticks" "infra/reconciler/cronjob.yaml" "concurrencyPolicy: Forbid"
refute "cronjob does not allow Replace" "infra/reconciler/cronjob.yaml" "concurrencyPolicy: Replace"

# Node selector for stability
check "reconciler does not run on spot" "infra/reconciler/cronjob.yaml" "gke-provisioning: standard"
refute "reconciler not scheduled on spot" "infra/reconciler/cronjob.yaml" "gke-provisioning: spot"

# Security context
check "root filesystem is read-only" "infra/reconciler/cronjob.yaml" "readOnlyRootFilesystem: true"

# RBAC scoping
check "state configmap access is confined by name" "infra/reconciler/rbac.yaml" "capacity-advisor-state"

# ClusterRole: no cluster-wide secret or configmap access
cluster_role=$(sed -n '/kind: ClusterRole/,/^---$/p' "${M}/rbac.yaml" | sed '$d')
if printf "%s" "${cluster_role}" | grep -qE 'configmaps|secrets'; then
  printf 'FAIL ClusterRole does not access configmaps or secrets\n'
  fails=$((fails+1))
else
  printf 'ok   ClusterRole does not access configmaps or secrets\n'
fi

# Whole file: no secrets access
refute "no cluster-wide secret access" "infra/reconciler/rbac.yaml" "^  resources:\s+\[.*secrets"

# ClusterRole verbs: no update on computeclasses
cluster_role_update=$(sed -n '/kind: ClusterRole/,/^---$/p' "${M}/rbac.yaml" | sed '$d')
if printf "%s" "${cluster_role_update}" | grep -q '"update"'; then
  printf 'FAIL ClusterRole does not grant update on computeclasses\n'
  fails=$((fails+1))
else
  printf 'ok   ClusterRole does not grant update on computeclasses\n'
fi

# Role: configmaps get/update/patch rule has resourceNames
if grep -A 5 'resources:.*configmaps' "${M}/rbac.yaml" | grep -q 'resourceNames:'; then
  printf 'ok   Role configmaps rule is confined by resourceNames\n'
else
  printf 'FAIL Role configmaps rule is confined by resourceNames\n'
  fails=$((fails+1))
fi

# Environment variable names match main.go expectations
for env in POD_NAMESPACE PROJECT_ID CLUSTER_REGION CLUSTER_NAME STATE_NAME; do
  if grep -q "name: ${env}" "${M}/cronjob.yaml"; then
    printf 'ok   CronJob has env var %s\n' "${env}"
  else
    printf 'FAIL CronJob has env var %s\n' "${env}"
    fails=$((fails+1))
  fi
done

# The args block is the whole reason the CronJob does anything. Without these
# checks, deleting it leaves the suite green and the CronJob running the bare
# root command every ten minutes.
check "CronJob invokes the reconcile subcommand" "infra/reconciler/cronjob.yaml" "^ +- reconcile$"
check "CronJob points at the mounted config" "infra/reconciler/cronjob.yaml" "^ +- --config=/etc/advisor/advisor\.yaml$"
check "CronJob passes a profile list" "infra/reconciler/cronjob.yaml" "^ +- --profiles=.+$"

# RBAC: no wildcards in verbs, apiGroups, or resources (flow or block style)
if grep -qE '(verbs|apiGroups|resources):.*\*' "${M}/rbac.yaml" || \
   grep -qE '^[[:space:]]*-[[:space:]]*["'"'"']?\*["'"'"']?[[:space:]]*$' "${M}/rbac.yaml"; then
  printf 'FAIL no wildcards in verbs, apiGroups, or resources\n'
  fails=$((fails+1))
else
  printf 'ok   no wildcards in verbs, apiGroups, or resources\n'
fi

# CronJob: uses correct service account
check "CronJob uses correct service account" "infra/reconciler/cronjob.yaml" "serviceAccountName: capacity-advisor$"

# Install script: grants logging.viewer role
check "grants logging.viewer role" "infra/08-reconciler.sh" "roles/logging\.viewer"

# All YAML files start with ---
for yaml in "${M}"/*.yaml; do
  basename=$(basename "${yaml}")
  if [ "$(head -1 "${yaml}")" = "---" ]; then
    printf 'ok   %s starts with ---\n' "${basename}"
  else
    printf 'FAIL %s starts with --- (actual: %s)\n' "${basename}" "$(head -1 "${yaml}")"
    fails=$((fails+1))
  fi
done

[ "${fails}" -eq 0 ] || exit 1
printf 'all reconciler manifest checks passed\n'
