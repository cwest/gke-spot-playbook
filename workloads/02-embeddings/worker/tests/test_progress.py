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

from main import pending_batches
from progress import Progress


class FakeCollection:
    def __init__(self):
        self.docs = set()

    def stream_ids(self, model_version):
        return [d for d in self.docs if d.endswith(f":{model_version}")]

    def set_many(self, doc_ids):
        self.docs.update(doc_ids)


def test_mark_done_is_idempotent_and_versioned():
    col = FakeCollection()
    p1 = Progress(col, "v1")
    p1.mark_done(["abc", "def"])
    p1.mark_done(["abc"])  # double-processing is harmless
    assert p1.done_ids() == {"abc", "def"}
    assert Progress(col, "v2").done_ids() == set()  # new model version starts fresh


def test_pending_batches_skips_fully_done_batches_only():
    chunks = [{"id": f"c{i}", "text": str(i)} for i in range(6)]
    done = {"c0", "c1", "c2"}  # batch0 fully done; batch1 partial; batch2 untouched
    out = list(pending_batches(chunks, done, 2))
    assert [idx for idx, _ in out] == [1, 2]
