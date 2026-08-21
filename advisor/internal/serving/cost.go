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

// Package serving computes the dollar case for scale-to-zero LLM serving:
// billing only for hours a replica actually runs, amortized over the requests
// served in the active window.
package serving

import "math"

type Scenario struct {
	Name              string
	NodeHourlyUSD     float64 // effective hourly node price (spot or on-demand)
	BilledHoursPerDay float64 // hours/day a replica is billed (24 always-on; active-only for scale-to-zero)
	ActiveHoursPerDay float64 // hours/day actually serving traffic
	RequestsPerSecond float64 // sustained throughput while active
}

// CostPer1000 returns USD per 1000 requests. +Inf when no requests are served.
func CostPer1000(s Scenario) float64 {
	dailyRequests := s.RequestsPerSecond * 3600 * s.ActiveHoursPerDay
	if dailyRequests == 0 {
		return math.Inf(1)
	}
	dailyCost := s.NodeHourlyUSD * s.BilledHoursPerDay
	return dailyCost / dailyRequests * 1000
}

// Savings is the fractional reduction in CostPer1000 of alt versus base.
func Savings(base, alt Scenario) float64 {
	b := CostPer1000(base)
	return (b - CostPer1000(alt)) / b
}
