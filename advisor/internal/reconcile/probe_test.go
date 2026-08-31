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
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/cwest/gke-spot-playbook/advisor/internal/advice"
	advicefake "github.com/cwest/gke-spot-playbook/advisor/internal/advice/fake"
	"github.com/cwest/gke-spot-playbook/advisor/internal/config"
	"github.com/cwest/gke-spot-playbook/advisor/internal/evidence"
	gcplogfake "github.com/cwest/gke-spot-playbook/advisor/internal/evidence/gcplog/fake"
	kubefake "github.com/cwest/gke-spot-playbook/advisor/internal/kube/fake"
	"github.com/cwest/gke-spot-playbook/advisor/internal/probe"
	"github.com/cwest/gke-spot-playbook/advisor/internal/probe/fake"
)

func newStateForTest() *State {
	return newState()
}

// A stockout reap writes NO confirmation and NEVER touches the ledger.
func TestReapStockoutLeavesLedgerUntouched(t *testing.T) {
	now := time.Unix(2_000_000, 0)
	st := newStateForTest()
	st.PendingProbes = []PendingProbe{{VMName: "probe-1", MachineType: "g2-standard-4", Zone: "us-central1-a", CreatedAt: now.Add(-1 * time.Minute)}}
	c := &fake.Compute{StatusFn: func(context.Context, string, string, string) (probe.Status, error) { return probe.StatusStockout, nil }}
	d := Deps{Probe: c, Now: now, ProbeCfg: config.Probe{Automated: true, MaxRunDurationSeconds: 300}}
	reapProbes(context.Background(), d, st)
	if len(st.ProbeConfirmations) != 0 {
		t.Fatal("stockout must not write a confirmation")
	}
	if len(st.Ledger.Latest) != 0 {
		t.Fatal("probe outcome must never write to the evidence ledger")
	}
	if len(st.PendingProbes) != 0 {
		t.Fatal("reaped pending probe must be dropped")
	}
	if len(c.Deleted) != 1 {
		t.Fatal("reaped VM must be deleted")
	}
}

func TestReapRunningWritesConfirmation(t *testing.T) {
	now := time.Unix(2_000_000, 0)
	st := newStateForTest()
	st.PendingProbes = []PendingProbe{{VMName: "probe-1", MachineType: "g2-standard-4", Zone: "us-central1-a", CreatedAt: now.Add(-1 * time.Minute)}}
	c := &fake.Compute{StatusFn: func(context.Context, string, string, string) (probe.Status, error) { return probe.StatusRunning, nil }}
	d := Deps{Probe: c, Now: now, ProbeCfg: config.Probe{Automated: true, MaxRunDurationSeconds: 300}}
	reapProbes(context.Background(), d, st)
	if !confirmed(st, "g2-standard-4", "us-central1-a", now) {
		t.Fatal("RUNNING reap must confirm")
	}
	if len(c.Deleted) != 1 {
		t.Fatal("confirmed VM must be deleted")
	}
}

func TestLaunchRespectsAutomatedBudgetAndPending(t *testing.T) {
	now := time.Unix(2_000_000, 0)
	c := &fake.Compute{}
	// automated off → never launches
	off := Deps{Probe: c, Now: now, ProbeCfg: config.Probe{Automated: false, MaxRunDurationSeconds: 300}}
	if launchProbe(context.Background(), off, newStateForTest(), "g2-standard-4", "us-central1-a") {
		t.Fatal("must not launch when probe.automated is false")
	}
	// automated on → launches, records pending, spends budget
	st := newStateForTest()
	on := Deps{Probe: c, Now: now, ProbeCfg: config.Probe{Automated: true, MaxRunDurationSeconds: 300}}
	if !launchProbe(context.Background(), on, st, "g2-standard-4", "us-central1-a") {
		t.Fatal("must launch when automated + budget")
	}
	if len(st.PendingProbes) != 1 || len(c.Inserted) != 1 {
		t.Fatalf("must record pending + insert: pending=%v inserted=%v", st.PendingProbes, c.Inserted)
	}
	// second identical candidate → already pending, no launch
	if launchProbe(context.Background(), on, st, "g2-standard-4", "us-central1-a") {
		t.Fatal("must not double-launch a pending candidate")
	}
}

// TestWidenPromoteGatedOnProbeConfirmation is the async two-tick flow. A zone
// the evidence ledger marked dry is held off its rung on the tick it is
// observed — the reconciler launches a probe instead of widening onto it blind —
// and rejoins the rung only on the later tick where that probe has read RUNNING
// and written a confirmation. Throughout, the working zones are never dropped:
// the probe path only ever adds capacity back.
func TestWidenPromoteGatedOnProbeConfirmation(t *testing.T) {
	ctx := context.Background()
	k := kubefake.New()
	// Every poll reads RUNNING; it only matters once a probe is actually pending.
	c := &fake.Compute{StatusFn: func(context.Context, string, string, string) (probe.Status, error) {
		return probe.StatusRunning, nil
	}}
	mk := func(log LogSource, at time.Time) Deps {
		d := deps(t, k, testAPI(nil, nil), log, at)
		d.Probe = c
		d.ProbeCfg = config.Probe{Automated: true, MaxRunDurationSeconds: 300}
		return d
	}

	// Tick 0: cold start — all three us-central1 zones on the rung.
	if _, err := Tick(ctx, mk(nil, now(0)), []string{"cpu-batch"}); err != nil {
		t.Fatal(err)
	}
	if z := rungZones(t, k.Applied[len(k.Applied)-1]); !hasZone(z, "us-central1-a") {
		t.Fatalf("cold-start zones = %v, want us-central1-a present", z)
	}

	// Tick N: us-central1-a refuses a node. It is not yet confirmed, so the gate
	// holds it off the rung and launches a probe rather than widening onto it.
	k.PendingClassPodsFn = func(context.Context, []string) (int, error) { return 1, nil }
	log := &gcplogfake.Log{RefusalsFn: func(context.Context, time.Time) ([]evidence.Observation, error) {
		return []evidence.Observation{{MachineType: "e2-standard-8", Zone: "us-central1-a", At: now(9)}}, nil
	}}
	if _, err := Tick(ctx, mk(log, now(10)), []string{"cpu-batch"}); err != nil {
		t.Fatal(err)
	}
	gated := rungZones(t, k.Applied[len(k.Applied)-1])
	if hasZone(gated, "us-central1-a") {
		t.Fatalf("tick N zones = %v, want us-central1-a held OFF the rung until a probe confirms", gated)
	}
	if !hasZone(gated, "us-central1-b") || !hasZone(gated, "us-central1-c") {
		t.Fatalf("tick N zones = %v, want the working zones kept (never degrade)", gated)
	}
	var afterN State
	if err := json.Unmarshal(k.State["spot-demo/reconciler-state"], &afterN); err != nil {
		t.Fatal(err)
	}
	if len(afterN.PendingProbes) != 1 {
		t.Fatalf("tick N pending probes = %v, want one launched for us-central1-a", afterN.PendingProbes)
	}
	if len(c.Inserted) != 1 {
		t.Fatalf("tick N inserted = %v, want one probe VM created", c.Inserted)
	}

	// Tick N+1: the probe reads RUNNING, so the reap confirms us-central1-a and
	// the gate widens the rung back onto it. Nothing new is ingested — no pending
	// pods, no log — so only the confirmation can bring the zone back, and the
	// observation is still fresh enough to be dry on its own.
	k.PendingClassPodsFn = func(context.Context, []string) (int, error) { return 0, nil }
	if _, err := Tick(ctx, mk(nil, now(12)), []string{"cpu-batch"}); err != nil {
		t.Fatal(err)
	}
	widened := rungZones(t, k.Applied[len(k.Applied)-1])
	if !hasZone(widened, "us-central1-a") {
		t.Fatalf("tick N+1 zones = %v, want us-central1-a back after the probe confirmed", widened)
	}
	if !hasZone(widened, "us-central1-b") || !hasZone(widened, "us-central1-c") {
		t.Fatalf("tick N+1 zones = %v, want the original zones never lost", widened)
	}
}

// rungZonesFor extracts the spec location.zones for one machine type's rung.
// rungZones reads only the first rung, which is not enough here: the clean shape
// outranks the crushed dry one, so the rung under test is never first.
func rungZonesFor(t *testing.T, manifest []byte, mt string) []string {
	t.Helper()
	s := string(manifest)
	head := "  - machineType: " + mt + "\n"
	i := strings.Index(s, head)
	if i < 0 {
		t.Fatalf("no rung for %s in:\n%s", mt, s)
	}
	rest := s[i+len(head):]
	for _, end := range []string{"\n  - machineType: ", "\n  whenUnsatisfiable:"} {
		if j := strings.Index(rest, end); j >= 0 {
			rest = rest[:j]
		}
	}
	_, after, ok := strings.Cut(rest, "zones: [")
	if !ok {
		t.Fatalf("rung %s has no spec zone list:\n%s", mt, rest)
	}
	inner, _, _ := strings.Cut(after, "]")
	var zones []string
	for _, z := range strings.Split(inner, ",") {
		if z = strings.TrimSpace(z); z != "" {
			zones = append(zones, z)
		}
	}
	return zones
}

// widenDeps shards e2-standard-8 into only us-central1-a and n2-standard-8 into
// -b and -c, so the in-region universe is {a,b,c} but e2 was never sharded into
// -b or -c. When -a goes dry, e2 is all-dry and its only widen targets are the
// never-sampled -b and -c — exactly the never-sharded seam this test drives.
func widenDeps(t *testing.T, k *kubefake.Cluster, log LogSource, at time.Time, c *fake.Compute) Deps {
	t.Helper()
	place := map[string][]string{
		"e2-standard-8": {"us-central1-a"},
		"n2-standard-8": {"us-central1-b", "us-central1-c"},
	}
	a := advicefake.New()
	a.RegionsFn = func(string) ([]string, error) { return []string{"us-central1"}, nil }
	a.CapacityFn = func(q advice.CapacityQuery) ([]advice.CapacityResult, error) {
		var shards []advice.Shard
		for _, mt := range q.MachineTypes {
			for _, z := range place[mt] {
				shards = append(shards, advice.Shard{Zone: z, MachineType: mt, Count: q.Size})
			}
		}
		return []advice.CapacityResult{{Obtainability: 0.9, EstimatedUptimeSeconds: 7200, Shards: shards}}, nil
	}
	a.HistoryFn = func(advice.HistoryQuery) (*advice.HistoryResult, error) {
		return &advice.HistoryResult{DailyPreemptionRates: []float64{0.05, 0.05, 0.05}, LatestSpotUSDPerHour: 0.10}, nil
	}
	cfg := testCfg()
	cfg.AllowedRegions = []string{"us-central1"}
	cfg.Profiles = map[string]config.Profile{
		"cpu-batch": {Kind: "cpu", MachineTypes: []string{"e2-standard-8", "n2-standard-8"}, Size: 20},
	}
	return Deps{
		API: a, Kube: k, Log: log, Cfg: cfg,
		Namespace: "spot-demo", StateName: "reconciler-state",
		ClusterRegion: "us-central1", Now: at,
		Probe: c, ProbeCfg: config.Probe{Automated: true, MaxRunDurationSeconds: 300},
	}
}

// TestNeverShardedWidenGatedOnProbeConfirmation drives the render-seam gate end
// to end. A rung whose only sharded zone goes dry has nothing left, so it would
// widen to the never-sampled zones — but the reconciler holds it off and probes
// them first, and it widens onto one only after a probe reads RUNNING there.
// Throughout, the rung never degrades to a zoneless priority.
func TestNeverShardedWidenGatedOnProbeConfirmation(t *testing.T) {
	ctx := context.Background()
	k := kubefake.New()
	// Only the -b widen target reads RUNNING; -a (the dry sharded zone) stays
	// stocked out, so it never rejoins and the widen path is what brings capacity
	// back.
	c := &fake.Compute{StatusFn: func(_ context.Context, _, zone, _ string) (probe.Status, error) {
		if zone == "us-central1-b" {
			return probe.StatusRunning, nil
		}
		return probe.StatusStockout, nil
	}}

	// Tick 0: cold start — e2 sits on its only sharded zone -a.
	if _, err := Tick(ctx, widenDeps(t, k, nil, now(0), c), []string{"cpu-batch"}); err != nil {
		t.Fatal(err)
	}
	if z := rungZonesFor(t, k.Applied[len(k.Applied)-1], "e2-standard-8"); !hasZone(z, "us-central1-a") {
		t.Fatalf("cold-start e2 zones = %v, want us-central1-a", z)
	}

	// Tick N: -a refuses a node. e2 is now all-dry; the never-sampled widen
	// targets -b/-c are unconfirmed, so the rung must NOT widen onto them (it
	// keeps its pinned -a, never zoneless) and a probe is launched for a target.
	k.PendingClassPodsFn = func(context.Context, []string) (int, error) { return 1, nil }
	log := &gcplogfake.Log{RefusalsFn: func(context.Context, time.Time) ([]evidence.Observation, error) {
		return []evidence.Observation{{MachineType: "e2-standard-8", Zone: "us-central1-a", At: now(9)}}, nil
	}}
	if _, err := Tick(ctx, widenDeps(t, k, log, now(10), c), []string{"cpu-batch"}); err != nil {
		t.Fatal(err)
	}
	gated := rungZonesFor(t, k.Applied[len(k.Applied)-1], "e2-standard-8")
	if hasZone(gated, "us-central1-b") || hasZone(gated, "us-central1-c") {
		t.Fatalf("tick N e2 zones = %v, want NO widen onto unconfirmed -b/-c", gated)
	}
	if !hasZone(gated, "us-central1-a") {
		t.Fatalf("tick N e2 zones = %v, want the pinned -a kept (never degrade to zoneless)", gated)
	}
	var afterN State
	if err := json.Unmarshal(k.State["spot-demo/reconciler-state"], &afterN); err != nil {
		t.Fatal(err)
	}
	if !pendingFor(&afterN, "e2-standard-8", "us-central1-b") {
		t.Fatalf("tick N pending = %v, want a probe launched for the widen target -b", afterN.PendingProbes)
	}

	// Tick N+1: the -b probe reads RUNNING, so the reap confirms it and the render
	// gate widens e2 onto -b only. -a stays off (stocked out, still dry) and -c
	// stays off (never confirmed).
	k.PendingClassPodsFn = func(context.Context, []string) (int, error) { return 0, nil }
	if _, err := Tick(ctx, widenDeps(t, k, nil, now(12), c), []string{"cpu-batch"}); err != nil {
		t.Fatal(err)
	}
	widened := rungZonesFor(t, k.Applied[len(k.Applied)-1], "e2-standard-8")
	if !hasZone(widened, "us-central1-b") {
		t.Fatalf("tick N+1 e2 zones = %v, want widened onto the confirmed -b", widened)
	}
	if hasZone(widened, "us-central1-a") || hasZone(widened, "us-central1-c") {
		t.Fatalf("tick N+1 e2 zones = %v, want ONLY the confirmed -b (dry -a and unconfirmed -c held off)", widened)
	}
}

func TestConfirmedRespectsTTL(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	st := newStateForTest()
	st.ProbeConfirmations[probeKey("g2-standard-4", "us-central1-a")] =
		ProbeConfirmation{MachineType: "g2-standard-4", Zone: "us-central1-a", At: now.Add(-29 * time.Minute)}
	if !confirmed(st, "g2-standard-4", "us-central1-a", now) {
		t.Fatal("29m-old confirmation must be fresh")
	}
	if confirmed(st, "g2-standard-4", "us-central1-a", now.Add(2*time.Minute)) {
		t.Fatal("31m-old confirmation must be stale")
	}
}

func TestBudgetCapAndWindowReset(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	st := newStateForTest()
	for i := 0; i < probesPerDay; i++ {
		if !budgetAvailable(st, now) {
			t.Fatalf("probe %d should be allowed", i)
		}
		noteProbeLaunched(st, now)
	}
	if budgetAvailable(st, now) {
		t.Fatal("over daily cap must be denied")
	}
	if !budgetAvailable(st, now.Add(probeBudgetWindow+time.Second)) {
		t.Fatal("window reset must re-allow")
	}
}

// An A100 stockout reap writes NO confirmation and NEVER touches the ledger —
// the Plan-4 invariant, re-asserted for a GPU/A100 shape (Act 7).
func TestReapA100StockoutLeavesLedgerUntouched(t *testing.T) {
	now := time.Unix(2_000_000, 0)
	st := newStateForTest()
	st.PendingProbes = []PendingProbe{{VMName: "probe-a100", MachineType: "a2-highgpu-1g", Zone: "us-central1-a", CreatedAt: now.Add(-1 * time.Minute)}}
	c := &fake.Compute{StatusFn: func(context.Context, string, string, string) (probe.Status, error) { return probe.StatusStockout, nil }}
	d := Deps{Probe: c, Now: now, ProbeCfg: config.Probe{Automated: true, MaxRunDurationSeconds: 300}}
	reapProbes(context.Background(), d, st)
	if len(st.ProbeConfirmations) != 0 {
		t.Fatal("A100 stockout must not write a confirmation")
	}
	if len(st.Ledger.Latest) != 0 {
		t.Fatal("A100 probe outcome must never write to the evidence ledger")
	}
	if len(c.Deleted) != 1 {
		t.Fatal("reaped A100 probe VM must be deleted")
	}
}

// An evidence-dry A100 candidate earns a probe when automation is on.
func TestLaunchProbesA100Candidate(t *testing.T) {
	now := time.Unix(2_000_000, 0)
	c := &fake.Compute{}
	st := newStateForTest()
	on := Deps{Probe: c, Now: now, ProbeCfg: config.Probe{Automated: true, MaxRunDurationSeconds: 300}}
	if !launchProbe(context.Background(), on, st, "a2-highgpu-1g", "us-central1-a") {
		t.Fatal("must launch a probe for an A100 candidate")
	}
	if len(st.PendingProbes) != 1 || len(c.Inserted) != 1 {
		t.Fatalf("must record pending + insert one A100 probe: pending=%v inserted=%v", st.PendingProbes, c.Inserted)
	}
	if st.PendingProbes[0].MachineType != "a2-highgpu-1g" {
		t.Fatalf("pending probe machine type = %q, want a2-highgpu-1g", st.PendingProbes[0].MachineType)
	}
}
