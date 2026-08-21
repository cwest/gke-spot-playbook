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

"""Act 2 embedding worker: one Indexed Job completion index = one corpus shard."""
import json
import logging
import os

from chunks import batches, load_chunks, shard
from progress import FirestoreCollection, Progress

log = logging.getLogger("embed-worker")


def pending_batches(my_chunks, done, batch_size):
    """Yield (batch_idx, chunks) still needing work. A batch whose every chunk
    is done is skipped; a partial batch re-runs whole — double-processing is
    harmless because Firestore marks and GCS objects are idempotent upserts."""
    for idx, batch in batches(my_chunks, batch_size):
        if all(c["id"] in done for c in batch):
            continue
        yield idx, batch


def main():
    logging.basicConfig(level=logging.INFO, format="%(asctime)s %(message)s")
    project = os.environ["PROJECT"]
    bucket_name = os.environ["BUCKET"]
    corpus_object = os.environ.get("CORPUS_OBJECT", "corpus/corpus.jsonl")
    out_prefix = os.environ.get("OUT_PREFIX", "embeddings")
    model_name = os.environ.get("MODEL_NAME", "BAAI/bge-base-en-v1.5")
    model_version = os.environ["MODEL_VERSION"]
    shards = int(os.environ["SHARDS"])
    index = int(os.environ["JOB_COMPLETION_INDEX"])
    batch_size = int(os.environ.get("BATCH_SIZE", "512"))

    from google.cloud import storage

    bucket = storage.Client(project=project).bucket(bucket_name)
    corpus = bucket.blob(corpus_object).download_as_text().splitlines()
    my_chunks = shard(load_chunks(corpus), index, shards)
    log.info("shard %d/%d: %d chunks", index, shards, len(my_chunks))

    progress = Progress(FirestoreCollection(project), model_version)
    done = progress.done_ids()
    log.info("resume: %d of my chunks already done",
             sum(1 for c in my_chunks if c["id"] in done))

    from embedder import Embedder

    embedder = Embedder(model_name)
    for idx, batch in pending_batches(my_chunks, done, batch_size):
        vectors = embedder.encode([c["text"] for c in batch])
        payload = "\n".join(
            json.dumps({"id": c["id"], "embedding": v}) for c, v in zip(batch, vectors)
        )
        name = f"{out_prefix}/{model_version}/shard{index}-batch{idx}.jsonl"
        bucket.blob(name).upload_from_string(payload)
        progress.mark_done([c["id"] for c in batch])
        log.info("shard %d batch %d: %d chunks -> %s", index, idx, len(batch), name)

    bucket.blob(f"{out_prefix}/{model_version}/_SUCCESS-shard{index}").upload_from_string("")
    log.info("shard %d complete", index)


if __name__ == "__main__":
    main()
