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

// Package fake is a scripted advice.API for tests.
package fake

import (
	"context"
	"fmt"

	"github.com/cwest/gke-spot-instance-node-pools/advisor/internal/advice"
)

type API struct {
	RegionsFn  func(project string) ([]string, error)
	CapacityFn func(q advice.CapacityQuery) ([]advice.CapacityResult, error)
	HistoryFn  func(q advice.HistoryQuery) (*advice.HistoryResult, error)
}

func New() *API { return &API{} }

func (f *API) Regions(_ context.Context, project string) ([]string, error) {
	if f.RegionsFn == nil {
		return nil, fmt.Errorf("fake: RegionsFn not set")
	}
	return f.RegionsFn(project)
}

func (f *API) Capacity(_ context.Context, q advice.CapacityQuery) ([]advice.CapacityResult, error) {
	if f.CapacityFn == nil {
		return nil, fmt.Errorf("fake: CapacityFn not set")
	}
	return f.CapacityFn(q)
}

func (f *API) CapacityHistory(_ context.Context, q advice.HistoryQuery) (*advice.HistoryResult, error) {
	if f.HistoryFn == nil {
		return nil, fmt.Errorf("fake: HistoryFn not set")
	}
	return f.HistoryFn(q)
}
