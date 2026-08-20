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

"""Chunk progress keyed by chunk-hash × model-version (idempotent upserts).

Progress takes any object with the tiny collection interface below, so unit
tests run against an in-memory fake and never import cloud SDKs.
"""


class Progress:
    """collection interface: stream_ids(model_version) -> iterable[str];
    set_many(doc_ids: list[str]) -> None (idempotent)."""

    def __init__(self, collection, model_version: str):
        self._col = collection
        self._version = model_version

    def done_ids(self) -> set:
        done = set()
        for doc_id in self._col.stream_ids(self._version):
            cid, _, ver = doc_id.rpartition(":")
            if ver == self._version:
                done.add(cid)
        return done

    def mark_done(self, chunk_ids) -> None:
        self._col.set_many([f"{c}:{self._version}" for c in chunk_ids])


class FirestoreCollection:
    """Real adapter; firestore is imported lazily (worker runtime only)."""

    def __init__(self, project: str, name: str = "embedding-progress"):
        from google.cloud import firestore

        self._db = firestore.Client(project=project)
        self._name = name

    def stream_ids(self, model_version: str):
        from google.cloud.firestore_v1.base_query import FieldFilter

        q = self._db.collection(self._name).where(
            filter=FieldFilter("model_version", "==", model_version)
        )
        for doc in q.stream():
            yield doc.id

    def set_many(self, doc_ids):
        batch = self._db.batch()
        pending = 0
        for doc_id in doc_ids:
            ref = self._db.collection(self._name).document(doc_id)
            batch.set(ref, {"model_version": doc_id.rpartition(":")[2]})
            pending += 1
            if pending == 400:  # stay under Firestore's 500-write batch cap
                batch.commit()
                batch = self._db.batch()
                pending = 0
        if pending:
            batch.commit()
