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

package gce

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	compute "cloud.google.com/go/compute/apiv1"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"

	"github.com/cwest/gke-spot-playbook/advisor/internal/probe"
)

// newTestClient builds a Client whose Compute Engine calls terminate at a local
// httptest server. option.WithEndpoint redirects every request — the instances
// client hands the same endpoint and HTTP client to the zone-operations client
// it polls with, so operation polls stay local too. option.WithoutAuthentication
// is what keeps this offline rather than merely redirected: it sets
// DialSettings.NoAuth, which takes the "do nothing" branch of the auth switch in
// google.golang.org/api/transport/http.newTransport (and, on the new auth
// library path, DisableAuthentication in cloud.google.com/go/auth's
// httptransport). Neither branch reaches credentials.DetectDefault, so nothing
// reads ADC or calls the GCE metadata server.
func newTestClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	ic, err := compute.NewInstancesRESTClient(context.Background(),
		option.WithEndpoint(srv.URL), option.WithoutAuthentication())
	if err != nil {
		t.Fatalf("NewInstancesRESTClient against %s: %v", srv.URL, err)
	}
	t.Cleanup(func() { _ = ic.Close() })
	return &Client{instances: ic}
}

func TestInstanceFor(t *testing.T) {
	r := probe.Request{
		Project:       "example-sandbox",
		Zone:          "us-central1-a",
		MachineType:   "g2-standard-4",
		Name:          "probe-abc",
		MaxRunSeconds: 300,
	}
	inst := instanceFor(r)

	if inst.GetName() != "probe-abc" {
		t.Errorf("Name = %s, want probe-abc", inst.GetName())
	}

	expectedMachineType := "zones/us-central1-a/machineTypes/g2-standard-4"
	if inst.GetMachineType() != expectedMachineType {
		t.Errorf("MachineType = %s, want %s", inst.GetMachineType(), expectedMachineType)
	}

	if inst.Scheduling.GetProvisioningModel() != "SPOT" {
		t.Errorf("ProvisioningModel = %s, want SPOT", inst.Scheduling.GetProvisioningModel())
	}

	if inst.Scheduling.GetInstanceTerminationAction() != "DELETE" {
		t.Errorf("InstanceTerminationAction = %s, want DELETE", inst.Scheduling.GetInstanceTerminationAction())
	}

	// The backstop must be carried verbatim, whatever the request says. A
	// conditional MaxRunDuration would drop it exactly when it is missing;
	// InsertSpotVM's guard is what stops those values reaching GCE at all.
	for _, want := range []int64{300, 0, -1} {
		r := r
		r.MaxRunSeconds = want
		d := instanceFor(r).GetScheduling().GetMaxRunDuration()
		if d == nil {
			t.Errorf("MaxRunSeconds=%d: MaxRunDuration is nil, want it always set", want)
			continue
		}
		if d.GetSeconds() != want {
			t.Errorf("MaxRunSeconds=%d: MaxRunDuration.Seconds = %d, want %d", want, d.GetSeconds(), want)
		}
	}
}

func TestInsertSpotVMRefusesARequestWithoutABackstop(t *testing.T) {
	// Task 11 holds a *Client directly, so this guard — not probe.Run's — is what
	// stands between a bug and a VM that never self-terminates. The server exists
	// to prove the guard returns before any request is issued.
	var calls atomic.Int64
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "the guard should have refused before this", http.StatusInternalServerError)
	})

	for _, s := range []int64{0, -1} {
		err := c.InsertSpotVM(context.Background(), probe.Request{
			Project: "example-sandbox", Zone: "us-central1-a",
			MachineType: "g2-standard-4", Name: "probe-abc", MaxRunSeconds: s,
		})
		if err == nil || !strings.Contains(err.Error(), "maxRunSeconds") {
			t.Errorf("MaxRunSeconds=%d: err = %v, want the backstop refusal", s, err)
		}
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("%d request(s) reached Compute Engine, want 0: the guard must refuse before the insert", n)
	}
}

func TestDeleteVMTreatsAnAlreadyGoneInstanceAsSuccess(t *testing.T) {
	// The 404 can arrive on the operation rather than the DELETE itself: the
	// call is accepted, and the failure surfaces when the operation completes.
	// Operation.Wait reports that as an error, so DeleteVM has to recognise it
	// or a create that never landed turns into a reported cleanup failure.
	var polled atomic.Bool
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodDelete {
			fmt.Fprint(w, `{"name":"op-1","status":"RUNNING"}`)
			return
		}
		polled.Store(true)
		fmt.Fprint(w, `{"name":"op-1","status":"DONE","httpErrorStatusCode":404,`+
			`"httpErrorMessage":"NOT FOUND"}`)
	})

	if err := c.DeleteVM(context.Background(), "example-sandbox", "us-central1-a", "probe-abc"); err != nil {
		t.Errorf("DeleteVM = %v, want nil: deleting an already-gone instance is success", err)
	}
	if !polled.Load() {
		t.Error("the operation was never polled; this test did not exercise the op.Wait path")
	}
}

func TestIsNotFound(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil error", nil, false},
		{"404 googleapi.Error", &googleapi.Error{Code: http.StatusNotFound}, true},
		{"403 googleapi.Error", &googleapi.Error{Code: http.StatusForbidden}, false},
		{"wrapped 404 googleapi.Error", fmt.Errorf("delete x: %w", &googleapi.Error{Code: http.StatusNotFound}), true},
		{"unrelated error", fmt.Errorf("deadline exceeded"), false},
		{"false positive: contains 404 in message", fmt.Errorf("Error 403: Limit: 404.0"), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isNotFound(tt.err)
			if got != tt.want {
				t.Errorf("isNotFound(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestInstanceForA100NeedsNoAccelerator(t *testing.T) {
	inst := instanceFor(probe.Request{
		Project: "p", Zone: "us-central1-a", MachineType: "a2-highgpu-1g",
		Name: "probe-a100", MaxRunSeconds: 300,
	})
	if got := inst.GetMachineType(); got != "zones/us-central1-a/machineTypes/a2-highgpu-1g" {
		t.Errorf("machineType = %q, want the a2-highgpu-1g zonal URL", got)
	}
	// The A100 is integral to a2: no GuestAccelerators must be set, or the insert
	// would double-specify the GPU and fail.
	if len(inst.GetGuestAccelerators()) != 0 {
		t.Errorf("a2 probe must set no GuestAccelerators, got %v", inst.GetGuestAccelerators())
	}
	if inst.GetScheduling().GetProvisioningModel() != "SPOT" {
		t.Errorf("probe must be SPOT, got %q", inst.GetScheduling().GetProvisioningModel())
	}
}
