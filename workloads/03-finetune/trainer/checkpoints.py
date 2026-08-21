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

"""Checkpoint discovery for resume-after-preemption. Pure logic, unit-tested."""
import os
import re

_CKPT_RE = re.compile(r"^checkpoint-(\d+)$")


def latest_checkpoint(output_dir: str):
    """Return the highest-step checkpoint-N directory under output_dir, or None."""
    if not os.path.isdir(output_dir):
        return None
    best_step, best_path = -1, None
    for name in os.listdir(output_dir):
        m = _CKPT_RE.match(name)
        path = os.path.join(output_dir, name)
        if m and os.path.isdir(path) and int(m.group(1)) > best_step:
            best_step, best_path = int(m.group(1)), path
    return best_path
