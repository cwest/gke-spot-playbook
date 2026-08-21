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

# progress.sh — Act 2 progress: batch files written + shards complete.
set -euo pipefail
# shellcheck disable=SC1091
source "$(dirname "$0")/../../infra/lib/common.sh"
spotdemo::init
MODEL_VERSION="${MODEL_VERSION:-bge-base-en-v1.5-1}"
SHARDS="${SHARDS:-8}"
prefix="gs://${PROJECT}-spot-demo/embeddings/${MODEL_VERSION}"
total=$(gcloud storage ls "${prefix}/**" 2>/dev/null | wc -l | tr -d ' ') || total=0
done_shards=$(gcloud storage ls "${prefix}/_SUCCESS-shard*" 2>/dev/null | wc -l | tr -d ' ') || done_shards=0
echo "batches written: $((total - done_shards))  shards complete: ${done_shards}/${SHARDS}"
