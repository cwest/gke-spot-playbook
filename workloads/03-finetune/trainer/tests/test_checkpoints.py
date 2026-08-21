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

from checkpoints import latest_checkpoint


def test_none_when_missing_or_empty(tmp_path):
    assert latest_checkpoint(str(tmp_path / "nope")) is None
    assert latest_checkpoint(str(tmp_path)) is None


def test_picks_highest_step_numerically(tmp_path):
    for n in ["checkpoint-50", "checkpoint-100", "checkpoint-9"]:
        (tmp_path / n).mkdir()
    (tmp_path / "checkpoint-junk").mkdir()          # non-numeric: ignored
    (tmp_path / "checkpoint-999.txt").write_text("")  # file, not dir: ignored
    assert latest_checkpoint(str(tmp_path)) == str(tmp_path / "checkpoint-100")
