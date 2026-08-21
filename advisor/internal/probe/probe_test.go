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

package probe

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

type stub struct {
	createErr error
	status    Status
	deleteErr error
	created   []Request
	deleted   []string
}

func (s *stub) InsertSpotVM(_ context.Context, r Request) error {
	s.created = append(s.created, r)
	return s.createErr
}

func (s *stub) VMStatus(_ context.Context, _, _, _ string) (Status, error) {
	return s.status, nil
}

func (s *stub) DeleteVM(_ context.Context, _, _, name string) error {
	s.deleted = append(s.deleted, name)
	return s.deleteErr
}

type ctxStub struct {
	createErr         error
	createCtxErr      error
	deleteDeadline    time.Time
	deleteHasDeadline bool
	created           []Request
	deleted           []string
}

func (s *ctxStub) InsertSpotVM(ctx context.Context, r Request) error {
	s.createCtxErr = ctx.Err()
	s.created = append(s.created, r)
	return s.createErr
}

func (s *ctxStub) VMStatus(_ context.Context, _, _, _ string) (Status, error) {
	return StatusRunning, nil
}

func (s *ctxStub) DeleteVM(ctx context.Context, _, _, name string) error {
	s.deleteDeadline, s.deleteHasDeadline = ctx.Deadline()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	s.deleted = append(s.deleted, name)
	return nil
}

func req() Request {
	return Request{
		Project: "example-sandbox", Zone: "us-central1-a",
		MachineType: "g2-standard-4", Name: "probe-abc", MaxRunSeconds: 300,
	}
}

func TestRunReportsObtainedAndCleansUp(t *testing.T) {
	s := &stub{status: StatusRunning}
	got := Run(context.Background(), s, req())
	if !got.Obtained {
		t.Fatalf("Obtained = false, Err = %q", got.Err)
	}
	if got.MachineType != "g2-standard-4" || got.Zone != "us-central1-a" {
		t.Errorf("Result = %+v, want it to echo the request", got)
	}
	if len(s.deleted) != 1 || s.deleted[0] != "probe-abc" {
		t.Errorf("deleted = %v, want the probe VM removed", s.deleted)
	}
	// The backstop must reach the compute layer, not just pass Run's guard.
	if len(s.created) != 1 {
		t.Fatalf("created = %v, want exactly one request", s.created)
	}
	if !reflect.DeepEqual(s.created[0], req()) {
		t.Errorf("created[0] = %+v, want %+v", s.created[0], req())
	}
}

func TestRunReportsStockoutWithoutFailing(t *testing.T) {
	s := &stub{createErr: fmt.Errorf("ZONE_RESOURCE_POOL_EXHAUSTED")}
	got := Run(context.Background(), s, req())
	if got.Obtained {
		t.Fatal("Obtained = true after a create failure")
	}
	if !strings.Contains(got.Err, "ZONE_RESOURCE_POOL_EXHAUSTED") {
		t.Errorf("Err = %q, want the API reason preserved", got.Err)
	}
}

func TestRunAlwaysAttemptsDeletion(t *testing.T) {
	// A create can fail after the instance exists (e.g. a timeout waiting for
	// RUNNING). Deleting unconditionally is the only way not to leak one.
	s := &stub{createErr: fmt.Errorf("deadline exceeded")}
	Run(context.Background(), s, req())
	if len(s.deleted) != 1 {
		t.Fatalf("deleted = %v, want cleanup even after a failed create", s.deleted)
	}
}

func TestRunSurfacesADeleteFailureWithoutChangingTheVerdict(t *testing.T) {
	s := &stub{status: StatusRunning, deleteErr: fmt.Errorf("still deleting")}
	got := Run(context.Background(), s, req())
	if !got.Obtained {
		t.Error("a delete failure must not change the capacity verdict")
	}
	if !strings.Contains(got.Err, "cleanup") {
		t.Errorf("Err = %q, want the cleanup failure surfaced", got.Err)
	}
}

func TestRunRefusesARequestWithoutABackstop(t *testing.T) {
	tests := []int64{0, -1}
	for _, maxRunSeconds := range tests {
		t.Run(fmt.Sprintf("MaxRunSeconds=%d", maxRunSeconds), func(t *testing.T) {
			s := &stub{}
			r := req()
			r.MaxRunSeconds = maxRunSeconds
			got := Run(context.Background(), s, r)
			if got.Obtained {
				t.Fatal("Obtained = true for a rejected request")
			}
			if !strings.Contains(got.Err, "maxRunSeconds") {
				t.Errorf("Err = %q, want the missing backstop named", got.Err)
			}
			if len(s.created) != 0 {
				t.Fatal("no VM may be created without a max-run-duration backstop")
			}
		})
	}
}

func TestRunCleansUpEvenWhenTheContextIsAlreadyDone(t *testing.T) {
	// Two contexts, two opposite obligations. Cleanup is detached so a create
	// that exhausted the caller's context still gets its VM deleted; the create
	// is not, so Ctrl-C never buys a spot VM nobody asked for. Detaching both is
	// the tidier-looking refactor that inverts the second half, so pin it.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s := &ctxStub{createErr: fmt.Errorf("timed out waiting for RUNNING")}
	Run(ctx, s, req())
	if len(s.deleted) != 1 {
		t.Fatalf("deleted = %v, want cleanup on a context the create exhausted", s.deleted)
	}
	if len(s.created) != 1 {
		t.Fatalf("created = %v, want the create still attempted once", s.created)
	}
	if s.createCtxErr == nil {
		t.Error("create must honor the caller's cancellation; only cleanup may outlive it")
	}
	// Detached is not the same as unbounded. Run's godoc promises a ceiling;
	// without one a wedged DeleteVM hangs the caller's Ctrl-C forever.
	if !s.deleteHasDeadline {
		t.Error("cleanup context must be bounded by cleanupTimeout; unbounded, Run can hang forever")
	} else if d := time.Until(s.deleteDeadline); d <= 0 || d > cleanupTimeout {
		t.Errorf("cleanup deadline in %v, want (0, %v]", d, cleanupTimeout)
	}
}

func TestRunRefusesARequestWithoutAName(t *testing.T) {
	s := &stub{}
	r := req()
	r.Name = ""
	got := Run(context.Background(), s, r)
	if got.Obtained {
		t.Fatal("Obtained = true for a request without a name")
	}
	if !strings.Contains(got.Err, "name") {
		t.Errorf("Err = %q, want the missing name mentioned", got.Err)
	}
	if len(s.created) != 0 {
		t.Fatal("no VM may be created without a name")
	}
}
