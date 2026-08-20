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

# Installs KEDA (pinned) and binds its operator to the scaler GSA via WI.
set -euo pipefail
# shellcheck disable=SC1091
source "$(dirname "${BASH_SOURCE[0]}")/lib/common.sh"
spotdemo::init

# Pinned at implementation time from:
#   gh api repos/kedacore/keda/releases/latest --jq .tag_name
# Override with KEDA_VERSION=... to bump; re-runs are reproducible by default.
KEDA_VERSION="${KEDA_VERSION:-v2.20.1}"
spotdemo::log "installing KEDA ${KEDA_VERSION}"
kubectl apply --server-side -f \
  "https://github.com/kedacore/keda/releases/download/${KEDA_VERSION}/keda-${KEDA_VERSION#v}.yaml"
kubectl -n keda annotate serviceaccount keda-operator \
  "iam.gke.io/gcp-service-account=spot-demo-keda@${PROJECT}.iam.gserviceaccount.com" --overwrite
kubectl -n keda rollout status deploy/keda-operator --timeout 180s
