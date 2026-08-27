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

// Package fake is a scriptable probe.Compute for tests and dry runs.
package fake

import (
	"context"

	"github.com/cwest/gke-spot-playbook/advisor/internal/probe"
)

type Compute struct {
	InsertFn func(ctx context.Context, r probe.Request) error
	StatusFn func(ctx context.Context, project, zone, name string) (probe.Status, error)
	DeleteFn func(ctx context.Context, project, zone, name string) error

	Inserted []probe.Request
	Deleted  []string
}

func (c *Compute) InsertSpotVM(ctx context.Context, r probe.Request) error {
	c.Inserted = append(c.Inserted, r)
	if c.InsertFn != nil {
		return c.InsertFn(ctx, r)
	}
	return nil
}

func (c *Compute) VMStatus(ctx context.Context, project, zone, name string) (probe.Status, error) {
	if c.StatusFn != nil {
		return c.StatusFn(ctx, project, zone, name)
	}
	// A bare fake models the happy path: the machine was allocated.
	return probe.StatusRunning, nil
}

func (c *Compute) DeleteVM(ctx context.Context, project, zone, name string) error {
	c.Deleted = append(c.Deleted, name)
	if c.DeleteFn != nil {
		return c.DeleteFn(ctx, project, zone, name)
	}
	return nil
}

var _ probe.Compute = (*Compute)(nil)
