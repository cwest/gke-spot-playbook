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

import "fmt"

// Point represents a named measurement point with agent count.
type Point struct {
	Name   string
	Agents int
}

// PointCost represents a point with its computed cost per agent.
type PointCost struct {
	Name        string
	Agents      int
	USDPerAgent float64
}

// Report contains the density analysis results.
type Report struct {
	NodeHourlyUSD  float64
	Points         []PointCost
	IsolationRatio float64
	LifecycleRatio float64
}

// Build computes the density report from node hourly cost and measurement points.
// Errors if any point has agents <= 0 or if fewer than 3 points are provided.
func Build(nodeHourlyUSD float64, points []Point) (*Report, error) {
	// Validate: at least 3 points for ratios
	if len(points) < 3 {
		return nil, fmt.Errorf("need at least 3 points for ratios, got %d", len(points))
	}

	// Compute per-agent cost for each point and validate agents > 0
	pointCosts := make([]PointCost, len(points))
	for i, p := range points {
		if p.Agents <= 0 {
			return nil, fmt.Errorf("point %q has agents=%d, need > 0", p.Name, p.Agents)
		}
		pointCosts[i] = PointCost{
			Name:        p.Name,
			Agents:      p.Agents,
			USDPerAgent: nodeHourlyUSD / float64(p.Agents),
		}
	}

	// Compute ratios: P2.Agents / P1.Agents, P3.Agents / P2.Agents
	isolationRatio := float64(points[1].Agents) / float64(points[0].Agents)
	lifecycleRatio := float64(points[2].Agents) / float64(points[1].Agents)

	return &Report{
		NodeHourlyUSD:  nodeHourlyUSD,
		Points:         pointCosts,
		IsolationRatio: isolationRatio,
		LifecycleRatio: lifecycleRatio,
	}, nil
}
