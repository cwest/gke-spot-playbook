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

package pricing

import (
	"errors"
	"testing"
)

func TestParseShape(t *testing.T) {
	cases := []struct {
		mt     string
		family string
		vcpus  float64
		memGB  float64
		err    bool
	}{
		{"t2d-standard-8", "t2d", 8, 32, false},
		{"e2-standard-16", "e2", 16, 64, false},
		{"n2-standard-4", "n2", 4, 16, false},
		{"n1-standard-8", "", 0, 0, true},
		{"weird", "", 0, 0, true},
	}
	for _, tc := range cases {
		s, err := ParseShape(tc.mt)
		if tc.err {
			if !errors.Is(err, ErrUnsupported) {
				t.Errorf("%s: want ErrUnsupported, got %v", tc.mt, err)
			}
			continue
		}
		if err != nil || s.Family != tc.family || s.VCPUs != tc.vcpus || s.MemoryGB != tc.memGB {
			t.Errorf("ParseShape(%s) = %+v, %v", tc.mt, s, err)
		}
	}
}

func TestParseShapeG2(t *testing.T) {
	cases := []struct {
		mt             string
		vcpu, gb, gpus float64
	}{
		{"g2-standard-4", 4, 16, 1},
		{"g2-standard-8", 8, 32, 1},
		{"g2-standard-24", 24, 96, 2},
		{"g2-standard-96", 96, 384, 8},
	}
	for _, c := range cases {
		s, err := ParseShape(c.mt)
		if err != nil {
			t.Fatalf("ParseShape(%s): %v", c.mt, err)
		}
		if s.Family != "g2" || s.VCPUs != c.vcpu || s.MemoryGB != c.gb || s.GPUs != c.gpus {
			t.Fatalf("ParseShape(%s) = %+v, want vcpu=%v gb=%v gpus=%v", c.mt, s, c.vcpu, c.gb, c.gpus)
		}
	}
}

func TestParseShapeUnknownG2ShapeUnsupported(t *testing.T) {
	if _, err := ParseShape("g2-standard-5"); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("want ErrUnsupported for unknown g2 shape, got %v", err)
	}
}

func TestParseShapeCPUFamiliesHaveZeroGPUs(t *testing.T) {
	s, err := ParseShape("n2-standard-8")
	if err != nil || s.GPUs != 0 {
		t.Fatalf("n2 shape GPUs = %v (err %v), want 0", s.GPUs, err)
	}
}
