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

package density

import (
	"math"
	"testing"
)

func TestBuildComputesPerAgentAndRatios(t *testing.T) {
	r, err := Build(48.0, []Point{
		{"baseline", 12}, {"sandbox", 16}, {"lifecycle", 48},
	})
	if err != nil {
		t.Fatal(err)
	}
	// $/agent = 48 / agents
	if got := r.Points[0].USDPerAgent; math.Abs(got-4.0) > 1e-9 {
		t.Errorf("baseline $/agent = %v want 4.0", got)
	}
	// isolation ratio = 16/12, lifecycle ratio = 48/16
	if math.Abs(r.IsolationRatio-16.0/12.0) > 1e-9 || math.Abs(r.LifecycleRatio-3.0) > 1e-9 {
		t.Errorf("ratios: iso=%v life=%v", r.IsolationRatio, r.LifecycleRatio)
	}
}

func TestBuildRejectsZeroAgents(t *testing.T) {
	if _, err := Build(48.0, []Point{{"baseline", 0}}); err == nil {
		t.Error("zero agents at a point must error, not divide by zero")
	}
}
