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

# Asserts the image/registry path is region-independent so migrate-region.sh
# can move the cluster without rebuilding images.
set -euo pipefail
root="$(cd "$(dirname "$0")/../.." && pwd)"
fail=0

check() {
  local desc="$1" file="$2" pattern="$3"
  if grep -qE -- "${pattern}" "${root}/${file}"; then
    printf 'ok   %s\n' "${desc}"
  else
    printf 'FAIL %s (%s !~ %s)\n' "${desc}" "${file}" "${pattern}"
    fail=1
  fi
}

refute() {
  local desc="$1" file="$2" pattern="$3"
  if grep -qE -- "${pattern}" "${root}/${file}"; then
    printf 'FAIL %s (%s still matches %s)\n' "${desc}" "${file}" "${pattern}"
    fail=1
  else
    printf 'ok   %s\n' "${desc}"
  fi
}

# Tree variant of refute: no manifest or doc under the given directories may
# match. Scoped by extension so vendored deps (workloads/*/.venv, created by
# make test-python) cannot make the result depend on local build state, and
# guarded on -d so a mistyped directory fails loudly instead of passing on
# grep's exit 2.
refute_tree() {
  local desc="$1" pattern="$2"
  shift 2
  local dirs=() d
  for d in "$@"; do
    if [[ ! -d "${root}/${d}" ]]; then
      printf 'FAIL %s (no such directory: %s)\n' "${desc}" "${d}"
      fail=1
      return
    fi
    dirs+=("${root}/${d}")
  done
  if grep -rqE --include='*.yaml' --include='*.yml' --include='*.md' \
    -- "${pattern}" "${dirs[@]}"; then
    printf 'FAIL %s (%s still matches %s)\n' "${desc}" "$*" "${pattern}"
    fail=1
  else
    printf 'ok   %s\n' "${desc}"
  fi
}

check 'AR repo created in the us multi-region' infra/03-pubsub.sh '--location us'
refute 'AR repo not pinned to REGION' infra/03-pubsub.sh 'repositories create spot-demo.*\$\{REGION\}'
check 'images pushed to us-docker.pkg.dev' infra/04-build.sh 'us-docker\.pkg\.dev'
check 'teardown deletes the us AR repo' infra/90-teardown.sh '--location us'
check 'teardown still removes a legacy region-pinned repo' infra/90-teardown.sh '--location +"?\$\{REGION\}'
check 'logging API required' infra/00-preflight.sh 'logging\.googleapis\.com'
refute_tree 'no REGION-docker placeholder left in manifests or runbooks' 'REGION-docker' workloads demo
check 'bucket created in the US multi-region' infra/06-gpu-data.sh '--location US'
check 'non-US bucket warns instead of silently persisting' infra/06-gpu-data.sh 'WARNING: .*is not the US multi-region'

exit "${fail}"
