// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package machinetype

import "testing"

func TestUnits(t *testing.T) {
	cases := []struct {
		mt, kind string
		want     float64
		wantErr  bool
	}{
		{"e2-standard-8", "cpu", 8, false},
		{"t2d-standard-16", "cpu", 16, false},
		{"g2-standard-4", "gpu", 1, false},
		{"g2-standard-24", "gpu", 2, false},
		{"g2-standard-96", "gpu", 8, false},
		{"a2-highgpu-1g", "gpu", 1, false},
		{"a2-highgpu-2g", "gpu", 2, false},
		{"a2-highgpu-8g", "gpu", 8, false},
		{"a2-megagpu-16g", "gpu", 16, false},
		{"n2-standard-8", "gpu", 0, true},  // not a known GPU shape
		{"weird", "cpu", 0, true},
	}
	for _, tc := range cases {
		got, err := Units(tc.mt, tc.kind)
		if (err != nil) != tc.wantErr || got != tc.want {
			t.Errorf("Units(%q,%q) = %v, %v; want %v, err=%v", tc.mt, tc.kind, got, err, tc.want, tc.wantErr)
		}
	}
}
