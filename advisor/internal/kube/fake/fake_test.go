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

package fake

import (
	"context"
	"errors"
	"testing"

	"github.com/cwest/gke-spot-instance-node-pools/advisor/internal/kube"
)

// errInjected is a sentinel error used to test error propagation in override hooks.
var errInjected = errors.New("injected error")

// The fake must satisfy the real interface; this is the point of the package.
var _ kube.Client = (*Cluster)(nil)

// Pinning the method set makes removal or renaming a deliberate, visible edit.
// The interface satisfaction check at :15 catches widening.
var _ = func(c kube.Client) {
	_ = c.PendingClassPods
	_ = c.GetState
	_ = c.PutState
	_ = c.ApplyComputeClass
	_ = c.EmitEvent
}

func TestStateRoundTrip(t *testing.T) {
	c := New()
	ctx := context.Background()
	// Absent ConfigMap returns (nil, nil)
	got, err := c.GetState(ctx, "spot-demo", "reconciler-state")
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("absent state should be (nil, nil), got %q", got)
	}
	// Non-nil zero-length state also returns (nil, nil), matching clientgo contract
	c.State["spot-demo/empty"] = []byte{}
	got, err = c.GetState(ctx, "spot-demo", "empty")
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("empty state should be (nil, nil), got %q", got)
	}
	// But nil values (truly absent from map) also return nil
	c.State["spot-demo/absent"] = nil
	got, err = c.GetState(ctx, "spot-demo", "absent")
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("nil state should be (nil, nil), got %q", got)
	}
	// Store in first namespace
	if err := c.PutState(ctx, "spot-demo", "reconciler-state", []byte(`{"a":1}`)); err != nil {
		t.Fatal(err)
	}
	got, err = c.GetState(ctx, "spot-demo", "reconciler-state")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"a":1}` {
		t.Fatalf("GetState = %q, want the stored document", got)
	}
	// Store the same name in a different namespace with different content
	if err := c.PutState(ctx, "other-ns", "reconciler-state", []byte(`{"b":2}`)); err != nil {
		t.Fatal(err)
	}
	// Verify they don't collide
	got, err = c.GetState(ctx, "spot-demo", "reconciler-state")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"a":1}` {
		t.Fatalf("GetState after cross-ns store = %q, want original document from spot-demo", got)
	}
	got, err = c.GetState(ctx, "other-ns", "reconciler-state")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"b":2}` {
		t.Fatalf("GetState in other-ns = %q, want the document stored there", got)
	}
}

func TestApplyAndEventsAreRecorded(t *testing.T) {
	c := New()
	ctx := context.Background()
	// Apply first manifest
	if err := c.ApplyComputeClass(ctx, []byte("kind: ComputeClass\n")); err != nil {
		t.Fatal(err)
	}
	// Emit first event
	if err := c.EmitEvent(ctx, "spot-demo", kube.Event{
		Reason: "LadderUpdated", Message: "m", Type: "Normal", InvolvedName: "batch-cpu",
	}); err != nil {
		t.Fatal(err)
	}
	// Apply second manifest with different content
	if err := c.ApplyComputeClass(ctx, []byte("kind: ComputeClass\nmetadata:\n  name: other\n")); err != nil {
		t.Fatal(err)
	}
	// Emit second event
	if err := c.EmitEvent(ctx, "spot-demo", kube.Event{
		Reason: "ClassCreated", Message: "m2", Type: "Normal", InvolvedName: "gpu-a100",
	}); err != nil {
		t.Fatal(err)
	}
	// Check both were recorded in order
	if len(c.Applied) != 2 || len(c.Events) != 2 {
		t.Fatalf("Applied=%d Events=%d, want 2 and 2", len(c.Applied), len(c.Events))
	}
	// Assert the actual payloads, in order
	if string(c.Applied[0]) != "kind: ComputeClass\n" {
		t.Errorf("Applied[0] = %q, want 'kind: ComputeClass\\n'", string(c.Applied[0]))
	}
	if string(c.Applied[1]) != "kind: ComputeClass\nmetadata:\n  name: other\n" {
		t.Errorf("Applied[1] = %q, want 'kind: ComputeClass\\nmetadata:\\n  name: other\\n'", string(c.Applied[1]))
	}
	if c.Events[0].Reason != "LadderUpdated" {
		t.Errorf("first event reason = %q, want LadderUpdated", c.Events[0].Reason)
	}
	if c.Events[1].Reason != "ClassCreated" {
		t.Errorf("second event reason = %q, want ClassCreated", c.Events[1].Reason)
	}
}

func TestOverridesTakePrecedence(t *testing.T) {
	c := New()
	c.PendingClassPodsFn = func(context.Context, []string) (int, error) { return 7, nil }
	n, err := c.PendingClassPods(context.Background(), []string{"batch-cpu"})
	if err != nil {
		t.Fatal(err)
	}
	if n != 7 {
		t.Fatalf("PendingClassPods = %d, want 7", n)
	}
}

func TestPendingClassPodsDefaultsToZero(t *testing.T) {
	n, err := New().PendingClassPods(context.Background(), []string{"batch-cpu"})
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("PendingClassPods = %d, want 0", n)
	}
}

func TestErrorsPropagate(t *testing.T) {
	tests := []struct {
		name string
		hook func() error
	}{
		{
			name: "PendingClassPodsFn",
			hook: func() error {
				c := New()
				c.PendingClassPodsFn = func(context.Context, []string) (int, error) {
					return 0, errInjected
				}
				_, err := c.PendingClassPods(context.Background(), []string{})
				return err
			},
		},
		{
			name: "GetStateFn",
			hook: func() error {
				c := New()
				c.GetStateFn = func(context.Context, string, string) ([]byte, error) {
					return nil, errInjected
				}
				_, err := c.GetState(context.Background(), "ns", "name")
				return err
			},
		},
		{
			name: "PutStateFn",
			hook: func() error {
				c := New()
				c.PutStateFn = func(context.Context, string, string, []byte) error {
					return errInjected
				}
				return c.PutState(context.Background(), "ns", "name", []byte{})
			},
		},
		{
			name: "ApplyComputeClassFn",
			hook: func() error {
				c := New()
				c.ApplyComputeClassFn = func(context.Context, []byte) error {
					return errInjected
				}
				return c.ApplyComputeClass(context.Background(), []byte{})
			},
		},
		{
			name: "EmitEventFn",
			hook: func() error {
				c := New()
				c.EmitEventFn = func(context.Context, string, kube.Event) error {
					return errInjected
				}
				return c.EmitEvent(context.Background(), "ns", kube.Event{})
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.hook()
			if err != errInjected {
				t.Fatalf("%s = %v, want errInjected", tt.name, err)
			}
		})
	}
}
