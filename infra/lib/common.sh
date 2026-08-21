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

# Shared helpers for spot-demo infra scripts. Source this; do not execute.
set -euo pipefail

SPOTDEMO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

spotdemo::log() { printf '>>> %s\n' "$*" >&2; }

spotdemo::require() {
  command -v "$1" >/dev/null 2>&1 || { spotdemo::log "missing required command: $1"; exit 1; }
}

# Run a probe command silently; usable in conditionals without set -e exits.
spotdemo::exists() { "$@" >/dev/null 2>&1; }

# Retry a command up to N times with a fixed delay; for eventually-consistent
# APIs (fresh service accounts take a few seconds to be bindable).
spotdemo::retry() {
  local attempts="$1" delay="$2"; shift 2
  local i
  for ((i = 1; i <= attempts; i++)); do
    if "$@"; then return 0; fi
    spotdemo::log "attempt ${i}/${attempts} failed: $* — retrying in ${delay}s"
    sleep "${delay}"
  done
  spotdemo::log "giving up after ${attempts} attempts: $*"
  return 1
}

spotdemo::init() {
  # A stale GOOGLE_APPLICATION_CREDENTIALS shadows ADC logins (see repo docs).
  if [[ -n "${GOOGLE_APPLICATION_CREDENTIALS:-}" ]]; then
    spotdemo::log "unsetting GOOGLE_APPLICATION_CREDENTIALS=${GOOGLE_APPLICATION_CREDENTIALS} for this run"
    unset GOOGLE_APPLICATION_CREDENTIALS
  fi
  PROJECT="${PROJECT:-example-sandbox}"
  local env_file="${SPOTDEMO_ROOT}/out/cluster-config.env"
  if [[ -f "${env_file}" ]]; then
    # shellcheck source=/dev/null
    source "${env_file}"
    spotdemo::log "loaded ${env_file}: REGION=${REGION:-<unset>} ZONES=${ZONES:-<unset>}"
  fi
  export PROJECT
}
