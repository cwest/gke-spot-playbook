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

from train import format_example


def test_format_example_prompt_shape():
    ex = {"question": "How many?", "context": "CREATE TABLE t(a int)",
          "answer": "SELECT COUNT(a) FROM t"}
    out = format_example(ex)
    assert out.startswith("question: How many?")
    assert "context: CREATE TABLE t(a int)" in out
    assert out.endswith("answer: SELECT COUNT(a) FROM t")
