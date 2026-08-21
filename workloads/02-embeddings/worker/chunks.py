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

"""Corpus chunk helpers: stable ids, sharding, batching. Pure logic, no cloud deps."""
import hashlib
import json


def chunk_id(text: str) -> str:
    return hashlib.sha256(text.encode("utf-8")).hexdigest()[:16]


def load_chunks(lines):
    """Parse corpus JSONL lines ({"text": ...}) into [{"id", "text"}, ...]."""
    chunks = []
    for line in lines:
        line = line.strip()
        if not line:
            continue
        rec = json.loads(line)
        chunks.append({"id": chunk_id(rec["text"]), "text": rec["text"]})
    return chunks


def shard(chunks, index: int, total: int):
    """Deterministic slice for one Indexed Job completion index."""
    return [c for i, c in enumerate(chunks) if i % total == index]


def batches(items, size: int):
    for i in range(0, len(items), size):
        yield i // size, items[i : i + size]
