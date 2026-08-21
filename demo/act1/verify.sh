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

# The zero-loss ledger: distinct completion receipts must equal COUNT.
#
# COUNT is the number of tasks the publisher seeded (default 5000). Set COUNT=0
# to sanity-check the harness against an empty queue: 0 distinct receipts == 0
# expected is a legitimate no-loss pass, NOT a mismatch. The guard below rejects
# the dangerous "success with loss" case (distinct < COUNT) with a non-zero exit.
set -euo pipefail
# shellcheck disable=SC1091
source "$(dirname "${BASH_SOURCE[0]}")/../../infra/lib/common.sh"
spotdemo::init
COUNT="${COUNT:-5000}"

spotdemo::log "pulling completion receipts (this drains spot-demo-completions-verify)"
tmp="$(mktemp)"
trap 'rm -f "${tmp}"' EXIT
empty_streak=0
while :; do
  batch="$(spotdemo::retry 5 5 gcloud pubsub subscriptions pull spot-demo-completions-verify \
    --project "${PROJECT}" --limit 1000 --auto-ack --format 'value(message.attributes.task_id)')"
  if [[ -z "${batch}" ]]; then
    empty_streak=$((empty_streak + 1))
    [[ "${empty_streak}" -ge 2 ]] && break
    continue
  fi
  empty_streak=0
  printf '%s\n' "${batch}" >> "${tmp}"
done
distinct="$(sort -u "${tmp}" | grep -c . || true)"
total="$(grep -c . "${tmp}" || true)"
spotdemo::log "receipts: ${total} total, ${distinct} distinct (duplicates are fine: at-least-once)"
if [[ "${distinct}" -eq "${COUNT}" ]]; then
  spotdemo::log "ZERO TASKS LOST: ${distinct}/${COUNT} ✔"
else
  spotdemo::log "MISMATCH: ${distinct}/${COUNT} — check backlog before concluding loss"
  exit 1
fi
