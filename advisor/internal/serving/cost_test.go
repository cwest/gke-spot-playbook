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

package serving

import (
	"math"
	"testing"
)

func approx(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

func TestCostPer1000_alwaysOn(t *testing.T) {
	// $1/hr node billed 24h/day, active 8h/day at 10 req/s.
	// daily requests = 10*3600*8 = 288000; daily cost = 24.
	// per-1000 = 24/288000*1000 = 0.0833333...
	s := Scenario{NodeHourlyUSD: 1, BilledHoursPerDay: 24, ActiveHoursPerDay: 8, RequestsPerSecond: 10}
	if got := CostPer1000(s); !approx(got, 24.0/288000.0*1000.0) {
		t.Fatalf("CostPer1000 = %v", got)
	}
}

func TestCostPer1000_scaleToZeroBilledOnlyWhenActive(t *testing.T) {
	// Same node/traffic, billed only the 8 active hours.
	// daily cost = 8; per-1000 = 8/288000*1000.
	s := Scenario{NodeHourlyUSD: 1, BilledHoursPerDay: 8, ActiveHoursPerDay: 8, RequestsPerSecond: 10}
	if got := CostPer1000(s); !approx(got, 8.0/288000.0*1000.0) {
		t.Fatalf("CostPer1000 = %v", got)
	}
}

func TestSavings(t *testing.T) {
	base := Scenario{NodeHourlyUSD: 1, BilledHoursPerDay: 24, ActiveHoursPerDay: 8, RequestsPerSecond: 10}
	alt := Scenario{NodeHourlyUSD: 1, BilledHoursPerDay: 8, ActiveHoursPerDay: 8, RequestsPerSecond: 10}
	if got := Savings(base, alt); !approx(got, 1.0-8.0/24.0) {
		t.Fatalf("Savings = %v", got)
	}
}

func TestCostPer1000_zeroRequestsIsSafe(t *testing.T) {
	s := Scenario{NodeHourlyUSD: 1, BilledHoursPerDay: 24, ActiveHoursPerDay: 0, RequestsPerSecond: 0}
	if got := CostPer1000(s); !math.IsInf(got, 1) {
		t.Fatalf("expected +Inf for zero throughput, got %v", got)
	}
}
