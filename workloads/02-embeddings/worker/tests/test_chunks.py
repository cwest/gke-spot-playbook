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

from chunks import batches, chunk_id, load_chunks, shard


def test_chunk_id_stable_and_short():
    assert chunk_id("hello") == chunk_id("hello")
    assert chunk_id("hello") != chunk_id("world")
    assert len(chunk_id("hello")) == 16


def test_load_chunks_skips_blank_lines():
    lines = ['{"text": "a"}', "", '{"text": "b"}']
    chunks = load_chunks(lines)
    assert [c["text"] for c in chunks] == ["a", "b"]
    assert all(c["id"] == chunk_id(c["text"]) for c in chunks)


def test_shard_partitions_completely_and_disjointly():
    chunks = [{"id": str(i), "text": str(i)} for i in range(10)]
    shards = [shard(chunks, i, 3) for i in range(3)]
    ids = [c["id"] for s in shards for c in s]
    assert sorted(ids) == sorted(c["id"] for c in chunks)
    assert len(ids) == len(set(ids))


def test_batches_indexes_and_remainder():
    assert list(batches(list(range(5)), 2)) == [(0, [0, 1]), (1, [2, 3]), (2, [4])]
