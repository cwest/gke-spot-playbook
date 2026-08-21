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

// Package probe answers the one question the capacity advice API cannot: will
// this zone actually give me this machine right now? It creates a single spot
// VM, records the answer, and deletes it.
//
// Probing costs money and creates real infrastructure, so it is opt-in. Every
// request must carry a max-run-duration backstop: if this process dies between
// create and delete, GCE terminates the instance on its own. A leaked GPU VM is
// the expensive failure mode, so the backstop is required rather than defaulted.
package probe

import (
	"context"
	"fmt"
	"time"
)

type Request struct {
	Project       string
	Zone          string
	MachineType   string
	Name          string
	MaxRunSeconds int64
	// Network and Subnet are the VPC to place the probe VM in. Both are full
	// resource paths/URLs (e.g. "projects/P/global/networks/N",
	// "projects/P/regions/R/subnetworks/S"). Empty Network falls back to the
	// project default network; a wrong network reads as a false stockout, so
	// callers on a custom/shared VPC must set these. Wired to config in Task 3.
	Network string
	Subnet  string
}

type Result struct {
	MachineType string `json:"machineType"`
	Zone        string `json:"zone"`
	Obtained    bool   `json:"obtained"`
	Err         string `json:"err,omitempty"`
}

// Status is where an insert has got to. It exists so the reconciler can create
// in one tick and poll in another: InsertSpotVM issues the create, VMStatus
// reports progress, and the caller decides whether to keep waiting.
type Status int

const (
	// StatusProvisioning: the instance exists but has not reached RUNNING yet.
	StatusProvisioning Status = iota
	// StatusRunning: the machine was allocated. This is a positive verdict.
	StatusRunning
	// StatusStockout: the zone refused the capacity (spot preemption/exhaustion).
	StatusStockout
	// StatusGone: no such instance — never landed, or already terminated.
	StatusGone
)

// Compute is the slice of GCE the probe needs. The insert and the wait are split
// so a caller can poll across ticks; Run below recomposes them into one blocking
// probe for human/CLI use.
type Compute interface {
	InsertSpotVM(ctx context.Context, r Request) error
	VMStatus(ctx context.Context, project, zone, name string) (Status, error)
	DeleteVM(ctx context.Context, project, zone, name string) error
}

// pollInterval is how long Run waits between VMStatus checks while an instance
// is still provisioning. Run is the blocking one-shot path; the async reconciler
// polls on its own tick cadence and does not use this.
const pollInterval = 2 * time.Second

// cleanupTimeout bounds the detached cleanup. A delete's op.Wait polls on
// gax.Backoff{Initial: 1s, Max: 1m}, so a slow delete can be truncated
// mid-backoff at this boundary. That is not a leak — the DELETE was already
// issued and the maxRunDuration backstop covers the remainder — but it does
// surface a spurious cleanup failure in the Result. Two minutes buys roughly
// one full backoff cycle past the point where a healthy delete has returned.
const cleanupTimeout = 2 * time.Minute

// Run performs one probe. It never returns an error: a stockout is the answer,
// not a failure, and the caller wants a verdict either way.
//
// Run uses two contexts on purpose. The insert and the status polling honor ctx,
// so cancelling ctx stops the probe from buying a VM and from waiting on one.
// Cleanup deliberately does not: it runs on a context detached from ctx (see
// context.WithoutCancel) and bounded by cleanupTimeout, so an insert that
// exhausted ctx still gets its instance deleted. Run may therefore block for up
// to cleanupTimeout after ctx is cancelled, and callers must not treat
// cancellation of ctx as Run having returned — under Ctrl-C it will appear to
// hang while it finishes deleting.
func Run(ctx context.Context, c Compute, r Request) Result {
	res := Result{MachineType: r.MachineType, Zone: r.Zone}
	if r.MaxRunSeconds <= 0 {
		res.Err = "refusing to probe: maxRunSeconds must be > 0 so an abandoned VM self-terminates"
		return res
	}
	if r.Name == "" {
		res.Err = "refusing to probe: name is required"
		return res
	}
	obtained, probeErr := insertAndAwait(ctx, c, r)
	// Delete unconditionally. The insert can succeed and the instance exist even
	// when the wait times out or a status read fails, and cleanup conditional on
	// success is how you leak instances. Use a fresh context so deletion succeeds
	// even if the caller's context expired during the insert or the wait.
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	delErr := c.DeleteVM(cleanupCtx, r.Project, r.Zone, r.Name)

	if probeErr != nil {
		res.Err = probeErr.Error()
		if delErr != nil {
			res.Err += fmt.Sprintf(" (cleanup also failed: %v)", delErr)
		}
		return res
	}
	res.Obtained = obtained
	if delErr != nil {
		if obtained {
			res.Err = fmt.Sprintf("obtained, but cleanup failed: %v", delErr)
		} else {
			res.Err = fmt.Sprintf("not obtained; cleanup also failed: %v", delErr)
		}
	}
	return res
}

// insertAndAwait issues the insert, then polls VMStatus until the machine is
// RUNNING (obtained), the zone refuses it (stockout/gone → not obtained), the
// caller's context ends, or the request's own max-run backstop elapses. It
// reports (false, err) only for an insert failure or a status read that errored;
// a stockout is the answer, not an error.
func insertAndAwait(ctx context.Context, c Compute, r Request) (bool, error) {
	if err := c.InsertSpotVM(ctx, r); err != nil {
		return false, err
	}
	deadline := time.Now().Add(time.Duration(r.MaxRunSeconds) * time.Second)
	for {
		st, err := c.VMStatus(ctx, r.Project, r.Zone, r.Name)
		if err != nil {
			return false, err
		}
		switch st {
		case StatusRunning:
			return true, nil
		case StatusStockout, StatusGone:
			return false, nil
		}
		// Still provisioning: wait and re-check, unless we are out of time.
		if !time.Now().Before(deadline) {
			return false, nil
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}
