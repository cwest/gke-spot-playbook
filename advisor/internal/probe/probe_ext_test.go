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

package probe_test

// These tests exercise Run over a scriptable probe.Compute. They live in the
// external test package because they use probe/fake, and fake imports probe: an
// internal (package probe) test importing fake would be a build cycle. The
// unexported-symbol tests that need cleanupTimeout stay in probe_test.go.

import (
	"context"
	"testing"

	"github.com/cwest/gke-spot-instance-node-pools/advisor/internal/probe"
	"github.com/cwest/gke-spot-instance-node-pools/advisor/internal/probe/fake"
)

func TestRunObtainsWhenStatusReachesRunning(t *testing.T) {
	calls := 0
	c := &fake.Compute{
		InsertFn: func(context.Context, probe.Request) error { return nil },
		StatusFn: func(context.Context, string, string, string) (probe.Status, error) {
			calls++
			if calls < 2 {
				return probe.StatusProvisioning, nil
			}
			return probe.StatusRunning, nil
		},
	}
	res := probe.Run(context.Background(), c, probe.Request{
		Project: "p", Zone: "us-central1-a", MachineType: "g2-standard-4",
		Name: "probe-x", MaxRunSeconds: 300,
	})
	if !res.Obtained {
		t.Fatalf("want obtained, got %+v", res)
	}
	if len(c.Deleted) != 1 {
		t.Fatalf("Run must always delete; deletes=%v", c.Deleted)
	}
}

func TestRunReportsStockoutWithoutObtaining(t *testing.T) {
	c := &fake.Compute{
		InsertFn: func(context.Context, probe.Request) error { return nil },
		StatusFn: func(context.Context, string, string, string) (probe.Status, error) {
			return probe.StatusStockout, nil
		},
	}
	res := probe.Run(context.Background(), c, probe.Request{
		Project: "p", Zone: "us-central1-a", MachineType: "g2-standard-4",
		Name: "probe-x", MaxRunSeconds: 300,
	})
	if res.Obtained {
		t.Fatalf("stockout must not obtain, got %+v", res)
	}
	if len(c.Deleted) != 1 {
		t.Fatalf("Run must always delete even on stockout; deletes=%v", c.Deleted)
	}
}
