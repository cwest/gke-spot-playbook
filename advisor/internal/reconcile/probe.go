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

package reconcile

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/cwest/gke-spot-instance-node-pools/advisor/internal/analyze"
	"github.com/cwest/gke-spot-instance-node-pools/advisor/internal/probe"
	"github.com/cwest/gke-spot-instance-node-pools/advisor/internal/render"
)

// Probe confirmation types and constants.

type ProbeConfirmation struct {
	MachineType string    `json:"machineType"`
	Zone        string    `json:"zone"`
	At          time.Time `json:"at"`
}

type PendingProbe struct {
	VMName      string    `json:"vmName"`
	MachineType string    `json:"machineType"`
	Zone        string    `json:"zone"`
	CreatedAt   time.Time `json:"createdAt"`
}

type ProbeBudget struct {
	WindowStart time.Time `json:"windowStart"`
	Count       int       `json:"count"`
}

const (
	probeConfirmTTL   = 30 * time.Minute
	probesPerTick     = 1
	probesPerDay      = 6
	probeBudgetWindow = 24 * time.Hour
)

// probeKey returns a stable key for (machineType, zone).
func probeKey(mt, zone string) string {
	return mt + "\x00" + zone
}

// confirmed returns true if a fresh confirmation exists for (mt, zone).
// A confirmation is stale if now.Sub(at) >= probeConfirmTTL.
func confirmed(st *State, mt, zone string, now time.Time) bool {
	pc, ok := st.ProbeConfirmations[probeKey(mt, zone)]
	if !ok {
		return false
	}
	return now.Sub(pc.At) < probeConfirmTTL
}

// budgetAvailable returns true if we can launch a probe now.
// Resets the budget window when now.Sub(WindowStart) >= probeBudgetWindow.
func budgetAvailable(st *State, now time.Time) bool {
	if now.Sub(st.ProbeBudget.WindowStart) >= probeBudgetWindow {
		st.ProbeBudget.WindowStart = now
		st.ProbeBudget.Count = 0
	}
	return st.ProbeBudget.Count < probesPerDay
}

// noteProbeLaunched resets the budget window if needed, then increments the count.
func noteProbeLaunched(st *State, now time.Time) {
	if now.Sub(st.ProbeBudget.WindowStart) >= probeBudgetWindow {
		st.ProbeBudget.WindowStart = now
		st.ProbeBudget.Count = 0
	}
	st.ProbeBudget.Count++
}

// pendingFor returns true if a pending probe exists for (mt, zone).
func pendingFor(st *State, mt, zone string) bool {
	key := probeKey(mt, zone)
	for _, pp := range st.PendingProbes {
		if probeKey(pp.MachineType, pp.Zone) == key {
			return true
		}
	}
	return false
}

// probeProject is the project the probe VMs live in. Cfg is nil in the pure
// unit tests, and the fake ignores the project anyway, so an empty string is a
// safe fallback there; in production Cfg is always set.
func probeProject(d Deps) string {
	if d.Cfg == nil {
		return ""
	}
	return d.Cfg.Project
}

// reapProbes advances every pending probe by one poll and rebuilds the pending
// list from the survivors. It is the async half of the probe lifecycle: launch
// creates a spot VM in one tick, reap reads its status in a later one.
//
// The verdict is positive-only. A RUNNING probe writes a confirmation; a
// stockout, a gone instance, or a probe that outran its backstop writes nothing
// at all. A probe outcome must NEVER reach st.Ledger — a failed probe is the
// absence of a confirmation, never an evidence penalty (the Plan 4 boundary).
//
// A transient VMStatus GET error is not a verdict: the probe stays pending and
// is retried next tick, never penalized. Every terminal branch deletes the VM;
// a delete error is tolerated because the Request's maxRunDuration backstop
// self-terminates any instance we fail to clean up.
func reapProbes(ctx context.Context, d Deps, st *State) {
	if len(st.PendingProbes) == 0 {
		return
	}
	proj := probeProject(d)
	maxRun := time.Duration(d.ProbeCfg.MaxRunDurationSeconds) * time.Second
	var kept []PendingProbe
	for _, pp := range st.PendingProbes {
		// Backstop: a probe that outran its max run is inconclusive. Delete and
		// drop it; do not confirm and do not penalize.
		if maxRun > 0 && d.Now.Sub(pp.CreatedAt) >= maxRun {
			_ = d.Probe.DeleteVM(ctx, proj, pp.Zone, pp.VMName)
			continue
		}
		status, err := d.Probe.VMStatus(ctx, proj, pp.Zone, pp.VMName)
		if err != nil {
			// Transient GET error: keep pending, retry next tick. Never a verdict.
			kept = append(kept, pp)
			continue
		}
		switch status {
		case probe.StatusRunning:
			st.ProbeConfirmations[probeKey(pp.MachineType, pp.Zone)] = ProbeConfirmation{
				MachineType: pp.MachineType, Zone: pp.Zone, At: d.Now,
			}
			_ = d.Probe.DeleteVM(ctx, proj, pp.Zone, pp.VMName)
		case probe.StatusStockout, probe.StatusGone:
			_ = d.Probe.DeleteVM(ctx, proj, pp.Zone, pp.VMName)
		default: // StatusProvisioning: not resolved yet, keep waiting.
			kept = append(kept, pp)
		}
	}
	st.PendingProbes = kept
}

// launchProbe issues one asynchronous capacity probe for (mt, zone) and records
// it as pending for a later reap. It returns true only when it actually created
// a VM.
//
// It never launches when automation is off, when the pair is already confirmed
// (nothing to learn), when a probe is already pending for it, or when the daily
// budget is spent. An insert failure still spends the budget: retrying a broken
// insert every tick would thrash. launchProbe never touches st.Ledger.
func launchProbe(ctx context.Context, d Deps, st *State, mt, zone string) bool {
	if !d.ProbeCfg.Automated {
		return false
	}
	if confirmed(st, mt, zone, d.Now) {
		return false
	}
	if pendingFor(st, mt, zone) {
		return false
	}
	if !budgetAvailable(st, d.Now) {
		return false
	}
	name := fmt.Sprintf("capacity-probe-%s-%s-%d", mt, zone, d.Now.Unix())
	req := probe.Request{
		Project:       probeProject(d),
		Zone:          zone,
		MachineType:   mt,
		Name:          name,
		MaxRunSeconds: int64(d.ProbeCfg.MaxRunDurationSeconds),
		Network:       d.ProbeCfg.Network,
		Subnet:        d.ProbeCfg.Subnet,
	}
	if err := d.Probe.InsertSpotVM(ctx, req); err != nil {
		// Spend the budget so a broken insert cannot thrash; record nothing to
		// reap, since no VM exists.
		noteProbeLaunched(st, d.Now)
		return false
	}
	st.PendingProbes = append(st.PendingProbes, PendingProbe{
		VMName: name, MachineType: mt, Zone: zone, CreatedAt: d.Now,
	})
	noteProbeLaunched(st, d.Now)
	return true
}

// gateWidenPromote is the probe gate on the reconciler's widen/promote seam. A
// zone the evidence ledger marked dry is normally dropped from its rung; this
// gate lets a live probe overrule that. A fresh confirmation clears the dry flag
// so the rung widens/promotes back onto the zone; an unconfirmed dry zone stays
// off and earns a probe (bounded by the per-tick and daily budgets) so a later
// tick can decide with evidence.
//
// The gate only ever ADDS a confirmed zone back; it never removes a working one,
// so it cannot degrade the live ladder. It mutates the in-region analysis in
// place, which is safe because inRegion hands back value copies of the
// candidates.
func gateWidenPromote(ctx context.Context, d Deps, st *State, a *analyze.Analysis) {
	launched := 0
	for i := range a.Candidates {
		c := &a.Candidates[i]
		if !c.EvidenceDry {
			continue
		}
		if confirmed(st, c.MachineType, c.Zone, d.Now) {
			c.EvidenceDry = false
			continue
		}
		if launched < probesPerTick && launchProbe(ctx, d, st, c.MachineType, c.Zone) {
			launched++
		}
	}
}

// gateWiden is the probe gate on the reconciler's never-sharded widening seam.
// When every zone a shape was sharded into goes evidence-dry, buildRungs would
// widen the rung to the in-region zones the advice API never sampled — but
// ComputeClassGated now widens onto such a zone only once a live probe has
// confirmed it. gateWiden launches those confirming probes: for every all-dry
// shape it walks the shape's widen targets (render.WidenTargets) and probes each
// one that is neither confirmed nor already pending, bounded by probesPerTick
// this tick and the shared daily budget.
//
// A shape with any clean in-region zone keeps that zone and never widens, so it
// is skipped. gateWiden never mutates the analysis or the ledger; the render gate
// alone decides the widen, and a failed probe is simply the absence of a
// confirmation. It is a sibling of gateWidenPromote, which recovers dry *sharded*
// zones; the two seams cover disjoint zone sets and each launches up to
// probesPerTick, so exploring never-sampled zones is not starved behind
// recovering sampled ones. Run it after gateWidenPromote: a sharded zone a probe
// just recovered makes its shape no longer all-dry, and gateWiden then skips it.
func gateWiden(ctx context.Context, d Deps, st *State, a *analyze.Analysis, region string) {
	clean := map[string]bool{}
	for _, c := range a.Candidates {
		if c.Region == region && !c.EvidenceDry {
			clean[c.MachineType] = true
		}
	}
	wt := render.WidenTargets(a, region)
	mts := make([]string, 0, len(wt))
	for mt := range wt {
		mts = append(mts, mt)
	}
	sort.Strings(mts)
	launched := 0
	for _, mt := range mts {
		if clean[mt] {
			continue
		}
		for _, z := range wt[mt] {
			if launched >= probesPerTick {
				return
			}
			if confirmed(st, mt, z, d.Now) || pendingFor(st, mt, z) {
				continue
			}
			if launchProbe(ctx, d, st, mt, z) {
				launched++
			}
		}
	}
}
