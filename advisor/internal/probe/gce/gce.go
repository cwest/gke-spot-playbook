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

// Package gce implements probe.Compute against the Compute Engine API.
package gce

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	compute "cloud.google.com/go/compute/apiv1"
	computepb "cloud.google.com/go/compute/apiv1/computepb"
	"google.golang.org/api/googleapi"
	"google.golang.org/protobuf/proto"

	"github.com/cwest/gke-spot-instance-node-pools/advisor/internal/probe"
)

// bootImage is a minimal image; the probe never runs a workload, it only asks
// whether the machine can be allocated at all.
const bootImage = "projects/debian-cloud/global/images/family/debian-12"

// defaultNetwork is the fallback when a Request carries no Network. A wrong
// network reads as a false stockout, so a caller on a custom/shared VPC must set
// Request.Network rather than rely on this.
const defaultNetwork = "global/networks/default"

type Client struct {
	instances *compute.InstancesClient
}

func New(ctx context.Context) (*Client, error) {
	c, err := compute.NewInstancesRESTClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("compute client: %w", err)
	}
	return &Client{instances: c}, nil
}

func (c *Client) Close() error { return c.instances.Close() }

func networkInterface(r probe.Request) *computepb.NetworkInterface {
	network := r.Network
	if network == "" {
		network = defaultNetwork
	}
	ni := &computepb.NetworkInterface{Network: proto.String(network)}
	// Subnetwork is required on custom-mode VPCs and ignored on auto-mode; set it
	// only when the caller supplied one so auto-mode/default keeps working.
	if r.Subnet != "" {
		ni.Subnetwork = proto.String(r.Subnet)
	}
	return ni
}

func instanceFor(r probe.Request) *computepb.Instance {
	return &computepb.Instance{
		Name: proto.String(r.Name),
		MachineType: proto.String(fmt.Sprintf("zones/%s/machineTypes/%s",
			r.Zone, r.MachineType)),
		Disks: []*computepb.AttachedDisk{{
			Boot:       proto.Bool(true),
			AutoDelete: proto.Bool(true),
			InitializeParams: &computepb.AttachedDiskInitializeParams{
				SourceImage: proto.String(bootImage),
				DiskSizeGb:  proto.Int64(20),
			},
		}},
		NetworkInterfaces: []*computepb.NetworkInterface{networkInterface(r)},
		Scheduling: &computepb.Scheduling{
			ProvisioningModel:         proto.String("SPOT"),
			InstanceTerminationAction: proto.String("DELETE"),
			// The backstop: GCE deletes the instance itself if we never do.
			MaxRunDuration: &computepb.Duration{Seconds: proto.Int64(r.MaxRunSeconds)},
		},
		Labels: map[string]string{"purpose": "capacity-probe"},
	}
}

// InsertSpotVM issues the instance insert and returns as soon as the call is
// accepted. It does not block on the operation: reaching RUNNING (or not) is
// observed separately via VMStatus, so a caller can create in one tick and poll
// in another. A synchronous caller composes the two (see probe.Run).
func (c *Client) InsertSpotVM(ctx context.Context, r probe.Request) error {
	if r.MaxRunSeconds <= 0 {
		return fmt.Errorf("refusing to insert %s in %s: maxRunSeconds must be > 0", r.MachineType, r.Zone)
	}
	inst := instanceFor(r)
	_, err := c.instances.Insert(ctx, &computepb.InsertInstanceRequest{
		Project: r.Project, Zone: r.Zone, InstanceResource: inst,
	})
	if err != nil {
		return fmt.Errorf("insert %s in %s: %w", r.MachineType, r.Zone, err)
	}
	return nil
}

// VMStatus maps the live instance state onto probe.Status.
//
//   - RUNNING              → StatusRunning   (allocated; positive verdict)
//   - PROVISIONING/STAGING → StatusProvisioning
//   - TERMINATED/STOPPING  → StatusStockout  (see note below)
//   - not found (404)      → StatusGone
//
// The stockout mapping is the uncertain one, verified live in Task 7. A spot
// stockout can surface two ways: the insert never yields an instance (404 here →
// StatusGone), or an instance appears and is then preempted/terminated. We treat
// TERMINATED/STOPPING as StatusStockout, but a preemption after a genuine
// RUNNING would look identical; only the reap deadline (Task 5) plus the
// statusMessage disambiguate for certain. Any unrecognised status is reported as
// StatusProvisioning so a caller keeps waiting rather than falsely concluding.
func (c *Client) VMStatus(ctx context.Context, project, zone, name string) (probe.Status, error) {
	inst, err := c.instances.Get(ctx, &computepb.GetInstanceRequest{
		Project: project, Zone: zone, Instance: name,
	})
	if err != nil {
		if isNotFound(err) {
			return probe.StatusGone, nil
		}
		return probe.StatusProvisioning, fmt.Errorf("status of %s in %s: %w", name, zone, err)
	}
	switch inst.GetStatus() {
	case "RUNNING":
		return probe.StatusRunning, nil
	case "PROVISIONING", "STAGING":
		return probe.StatusProvisioning, nil
	case "TERMINATED", "STOPPING", "STOPPED", "SUSPENDING", "SUSPENDED":
		return probe.StatusStockout, nil
	default:
		return probe.StatusProvisioning, nil
	}
}

func (c *Client) DeleteVM(ctx context.Context, project, zone, name string) error {
	op, err := c.instances.Delete(ctx, &computepb.DeleteInstanceRequest{
		Project: project, Zone: zone, Instance: name,
	})
	if err != nil {
		// Nothing to delete is success: the create may never have landed.
		if isNotFound(err) {
			return nil
		}
		return fmt.Errorf("delete %s: %w", name, err)
	}
	if err := op.Wait(ctx); err != nil && !isNotFound(err) {
		return fmt.Errorf("delete %s: %w", name, err)
	}
	return nil
}

func isNotFound(err error) bool {
	var gerr *googleapi.Error
	return errors.As(err, &gerr) && gerr.Code == http.StatusNotFound
}

var _ probe.Compute = (*Client)(nil)
