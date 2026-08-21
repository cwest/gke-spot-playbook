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

// Package fake is a scripted pricing.Source for tests.
package fake

import (
	"context"
	"fmt"
)

// Source is a scripted pricing.Source. Prices are keyed by
// region + "/" + machineType.
type Source struct{ Prices map[string]float64 }

// New returns a Source backed by the given price table.
func New(prices map[string]float64) *Source { return &Source{Prices: prices} }

// OnDemandHourlyUSD returns the scripted price or an error if none is set.
func (s *Source) OnDemandHourlyUSD(_ context.Context, region, machineType string) (float64, error) {
	p, ok := s.Prices[region+"/"+machineType]
	if !ok {
		return 0, fmt.Errorf("fake: no price for %s/%s", region, machineType)
	}
	return p, nil
}
