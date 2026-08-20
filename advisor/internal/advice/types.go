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

// Package advice defines the capacity-advice API boundary consumed by the
// analyze orchestration and CLI. Implementations back this with either the
// real Compute Engine beta advice endpoints or a scripted fake for tests.
package advice

import "context"

type CapacityQuery struct {
	Project, Region string
	MachineTypes    []string // ≤5, caller enforces
	Size            int32
	Kind            string // "cpu" | "gpu" (for accelerator handling later; unused by GCP call)
}
type Shard struct {
	Zone, MachineType string
	Count             int32
}
type CapacityResult struct {
	Obtainability          float64
	EstimatedUptimeSeconds int
	Shards                 []Shard
}
type HistoryQuery struct{ Project, Region, Zone, MachineType string }
type HistoryResult struct {
	DailyPreemptionRates []float64 // oldest first
	LatestSpotUSDPerHour float64
}
type API interface {
	Regions(ctx context.Context, project string) ([]string, error)
	Capacity(ctx context.Context, q CapacityQuery) ([]CapacityResult, error)
	CapacityHistory(ctx context.Context, q HistoryQuery) (*HistoryResult, error)
}
