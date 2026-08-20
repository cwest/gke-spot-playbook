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

"""Build the Act 2 corpus locally: ~50k paragraph chunks of Simple English
Wikipedia -> corpus.jsonl, then upload with:
  gcloud storage cp corpus.jsonl gs://<project>-spot-demo/corpus/corpus.jsonl

Usage: python load_corpus.py [--max-chunks 50000] [--out corpus.jsonl]
Requires: pip install datasets==3.0.0
"""
import argparse
import json


def paragraphs(article_text, min_chars=200, max_chars=2000):
    for para in article_text.split("\n"):
        para = para.strip()
        if len(para) >= min_chars:
            yield para[:max_chars]


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--max-chunks", type=int, default=50_000)
    ap.add_argument("--out", default="corpus.jsonl")
    args = ap.parse_args()

    from datasets import load_dataset

    ds = load_dataset("wikimedia/wikipedia", "20231101.simple", split="train")
    n = 0
    with open(args.out, "w", encoding="utf-8") as f:
        for article in ds:
            for para in paragraphs(article["text"]):
                f.write(json.dumps({"text": para, "title": article["title"]}) + "\n")
                n += 1
                if n >= args.max_chunks:
                    print(f"wrote {n} chunks to {args.out}")
                    return
    print(f"wrote {n} chunks to {args.out} (corpus exhausted)")


if __name__ == "__main__":
    main()
