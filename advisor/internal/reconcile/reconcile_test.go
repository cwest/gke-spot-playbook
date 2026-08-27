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
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/cwest/gke-spot-playbook/advisor/internal/advice"
	advicefake "github.com/cwest/gke-spot-playbook/advisor/internal/advice/fake"
	"github.com/cwest/gke-spot-playbook/advisor/internal/analyze"
	"github.com/cwest/gke-spot-playbook/advisor/internal/config"
	"github.com/cwest/gke-spot-playbook/advisor/internal/evidence"
	gcplogfake "github.com/cwest/gke-spot-playbook/advisor/internal/evidence/gcplog/fake"
	"github.com/cwest/gke-spot-playbook/advisor/internal/kube"
	kubefake "github.com/cwest/gke-spot-playbook/advisor/internal/kube/fake"
)

func now(min int) time.Time {
	return time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC).Add(time.Duration(min) * time.Minute)
}

// testAPI shards e2-standard-8 across three us-central1 zones and one
// us-east4 zone that is cheaper but slightly lower-obtainability.
func testAPI(obtainability map[string]float64, price map[string]float64) *advicefake.API {
	zones := map[string][]string{
		"us-central1": {"us-central1-a", "us-central1-b", "us-central1-c"},
		"us-east4":    {"us-east4-a", "us-east4-b"},
	}
	a := advicefake.New()
	a.RegionsFn = func(string) ([]string, error) { return []string{"us-central1", "us-east4"}, nil }
	a.CapacityFn = func(q advice.CapacityQuery) ([]advice.CapacityResult, error) {
		var shards []advice.Shard
		for _, z := range zones[q.Region] {
			for _, mt := range q.MachineTypes {
				shards = append(shards, advice.Shard{Zone: z, MachineType: mt, Count: q.Size})
			}
		}
		o, ok := obtainability[q.Region]
		if !ok {
			o = 0.9
		}
		return []advice.CapacityResult{{
			Obtainability: o, EstimatedUptimeSeconds: 7200, Shards: shards,
		}}, nil
	}
	a.HistoryFn = func(q advice.HistoryQuery) (*advice.HistoryResult, error) {
		p, ok := price[q.Region]
		if !ok {
			p = 0.10
		}
		return &advice.HistoryResult{
			DailyPreemptionRates: []float64{0.05, 0.05, 0.05},
			LatestSpotUSDPerHour: p,
		}, nil
	}
	return a
}

// zonedAPI is testAPI with per-zone obtainability inside us-central1.
//
// The hysteresis tests need it because testAPI returns a single Obtainability
// for the whole region: any uniform regional move rescales every zone equally,
// leaves the rung ordering untouched, and renders byte-identical bytes, so the
// tick short-circuits to NoOp before hysteresis is consulted at all. Only
// per-zone variation changes which zones sit on the rung — and the zone list is
// the part of the spec the fingerprint actually hashes.
//
// Zones absent from central keep testAPI's 0.9. Other regions are untouched, so
// the cross-region advisory keeps whatever behaviour testAPI gave it.
func zonedAPI(central map[string]float64) *advicefake.API {
	a := testAPI(nil, nil)
	regional := a.CapacityFn
	a.CapacityFn = func(q advice.CapacityQuery) ([]advice.CapacityResult, error) {
		if q.Region != "us-central1" {
			return regional(q)
		}
		var out []advice.CapacityResult
		for _, z := range []string{"us-central1-a", "us-central1-b", "us-central1-c"} {
			o, ok := central[z]
			if !ok {
				o = 0.9
			}
			var shards []advice.Shard
			for _, mt := range q.MachineTypes {
				shards = append(shards, advice.Shard{Zone: z, MachineType: mt, Count: q.Size})
			}
			out = append(out, advice.CapacityResult{
				Obtainability: o, EstimatedUptimeSeconds: 7200, Shards: shards,
			})
		}
		return out, nil
	}
	return a
}

// widenAPI shards e2-standard-8 across only us-central1-a and -b, while
// e2-standard-16 also covers us-central1-c. That makes us-central1-c an in-region
// universe zone e2-standard-8 was never sharded into — the exact never-sampled
// widen target an all-dry e2-standard-8 rung must fall onto.
func widenAPI() *advicefake.API {
	zones := map[string]map[string][]string{
		"us-central1": {
			"e2-standard-8":  {"us-central1-a", "us-central1-b"},
			"e2-standard-16": {"us-central1-a", "us-central1-b", "us-central1-c"},
		},
		"us-east4": {
			"e2-standard-8":  {"us-east4-a"},
			"e2-standard-16": {"us-east4-a"},
		},
	}
	a := advicefake.New()
	a.RegionsFn = func(string) ([]string, error) { return []string{"us-central1", "us-east4"}, nil }
	a.CapacityFn = func(q advice.CapacityQuery) ([]advice.CapacityResult, error) {
		var shards []advice.Shard
		for _, mt := range q.MachineTypes {
			for _, z := range zones[q.Region][mt] {
				shards = append(shards, advice.Shard{Zone: z, MachineType: mt, Count: q.Size})
			}
		}
		return []advice.CapacityResult{{
			Obtainability: 0.9, EstimatedUptimeSeconds: 7200, Shards: shards,
		}}, nil
	}
	a.HistoryFn = func(advice.HistoryQuery) (*advice.HistoryResult, error) {
		return &advice.HistoryResult{
			DailyPreemptionRates: []float64{0.05, 0.05, 0.05},
			LatestSpotUSDPerHour: 0.10,
		}, nil
	}
	return a
}

func testCfg() *config.Config {
	return &config.Config{
		Project:        "example-sandbox",
		AllowedRegions: []string{"us-central1", "us-east4"},
		Profiles: map[string]config.Profile{
			"cpu-batch": {Kind: "cpu", MachineTypes: []string{"e2-standard-8"}, Size: 20},
		},
		Hysteresis: config.Hysteresis{MinScoreDelta: 0.15, ConsecutiveTicks: 3},
		Caps:       config.Caps{MaxSpotRungs: 3},
		Scoring:    config.Scoring{PriceExponent: 1.0},
		Evidence: config.Evidence{
			HalfLifeMinutes: 30, Floor: 0.05, DryBelow: 0.5, MaxAgeHours: 6, FastPathTicks: 1,
		},
	}
}

func deps(t *testing.T, k *kubefake.Cluster, api *advicefake.API, log LogSource, at time.Time) Deps {
	t.Helper()
	return Deps{
		API: api, Kube: k, Log: log, Cfg: testCfg(),
		Namespace: "spot-demo", StateName: "reconciler-state",
		ClusterRegion: "us-central1", Now: at,
	}
}

func TestFingerprintIgnoresComments(t *testing.T) {
	a := []byte("# Generated at 12:00\nspec:\n  x: 1\n")
	b := []byte("# Generated at 12:10\n# rung e2 score=1000\nspec:\n  x: 1\n")
	if Fingerprint(a) != Fingerprint(b) {
		t.Fatal("comment-only differences must not change the fingerprint")
	}
	c := []byte("# Generated at 12:00\nspec:\n  x: 2\n")
	if Fingerprint(a) == Fingerprint(c) {
		t.Fatal("a spec change must change the fingerprint")
	}
}

func TestTickColdStartAppliesImmediately(t *testing.T) {
	k := kubefake.New()
	got, err := Tick(context.Background(),
		deps(t, k, testAPI(nil, nil), nil, now(0)), []string{"cpu-batch"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Applied) != 1 || got.Applied[0] != "batch-cpu" {
		t.Fatalf("Applied = %v, want [batch-cpu]; warnings: %v", got.Applied, got.Warnings)
	}
	if len(k.Applied) != 1 {
		t.Fatalf("cluster saw %d applies, want 1", len(k.Applied))
	}
	if !strings.Contains(string(k.Applied[0]), "name: batch-cpu") {
		t.Errorf("applied the wrong manifest:\n%s", k.Applied[0])
	}
	if len(k.State) != 1 {
		t.Fatal("state was not persisted")
	}
}

func TestTickSecondRunIsANoOp(t *testing.T) {
	k := kubefake.New()
	ctx := context.Background()
	if _, err := Tick(ctx, deps(t, k, testAPI(nil, nil), nil, now(0)), []string{"cpu-batch"}); err != nil {
		t.Fatal(err)
	}
	got, err := Tick(ctx, deps(t, k, testAPI(nil, nil), nil, now(10)), []string{"cpu-batch"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.NoOp) != 1 {
		t.Fatalf("NoOp = %v, want [batch-cpu]", got.NoOp)
	}
	if len(k.Applied) != 1 {
		t.Fatalf("cluster saw %d applies, want the ladder untouched at 1", len(k.Applied))
	}
}

func TestTickSkipsTheLogQueryWhenNothingIsPending(t *testing.T) {
	k := kubefake.New() // PendingClassPods defaults to 0
	log := &gcplogfake.Log{RefusalsFn: func(context.Context, time.Time) ([]evidence.Observation, error) {
		t.Fatal("log must not be queried when no pod is waiting for capacity")
		return nil, nil
	}}
	if _, err := Tick(context.Background(),
		deps(t, k, testAPI(nil, nil), log, now(0)), []string{"cpu-batch"}); err != nil {
		t.Fatal(err)
	}
}

func TestTickIngestsEvidenceWhenPodsAreWaiting(t *testing.T) {
	k := kubefake.New()
	k.PendingClassPodsFn = func(context.Context, []string) (int, error) { return 2, nil }
	log := &gcplogfake.Log{RefusalsFn: func(context.Context, time.Time) ([]evidence.Observation, error) {
		return []evidence.Observation{{
			MachineType: "e2-standard-8", Zone: "us-central1-a",
			Reason: "no.scale.up.nap.pod.zonal.resources.exceeded", At: now(-1),
		}}, nil
	}}
	got, err := Tick(context.Background(),
		deps(t, k, testAPI(nil, nil), log, now(0)), []string{"cpu-batch"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Observations != 1 {
		t.Fatalf("Observations = %d, want 1", got.Observations)
	}
	if len(k.Applied) != 1 {
		t.Fatalf("cluster saw %d applies, want 1", len(k.Applied))
	}
	zones := rungZones(t, k.Applied[0])
	if hasZone(zones, "us-central1-a") {
		t.Errorf("the dry zone (us-central1-a) should be gone from the top rung")
	}
	if len(zones) == 0 {
		t.Errorf("the top rung has no zones left; widening must have left some")
	}
}

func TestObservationsCountsDistinctKeys(t *testing.T) {
	k := kubefake.New()
	k.PendingClassPodsFn = func(context.Context, []string) (int, error) { return 2, nil }
	log := &gcplogfake.Log{RefusalsFn: func(context.Context, time.Time) ([]evidence.Observation, error) {
		return []evidence.Observation{
			{MachineType: "e2-standard-8", Zone: "us-central1-a",
				Reason: "no.scale.up.nap.pod.zonal.resources.exceeded", At: now(-1)},
			{MachineType: "e2-standard-8", Zone: "us-central1-a",
				Reason: "no.scale.up.nap.capacity.pool.exhausted", At: now(-2)},
		}, nil
	}}
	got, err := Tick(context.Background(),
		deps(t, k, testAPI(nil, nil), log, now(0)), []string{"cpu-batch"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Observations != 1 {
		t.Fatalf("Observations = %d, want 1 (deduplicating same shape/zone pair)", got.Observations)
	}
}

func TestEvidenceDrivenChangeTakesTheFastPath(t *testing.T) {
	ctx := context.Background()
	k := kubefake.New()
	// Cold start with no evidence.
	if _, err := Tick(ctx, deps(t, k, testAPI(nil, nil), nil, now(0)), []string{"cpu-batch"}); err != nil {
		t.Fatal(err)
	}
	// Now a failure lands. consecutiveTicks is 3, but fastPathTicks is 1.
	k.PendingClassPodsFn = func(context.Context, []string) (int, error) { return 1, nil }
	log := &gcplogfake.Log{RefusalsFn: func(context.Context, time.Time) ([]evidence.Observation, error) {
		return []evidence.Observation{{
			MachineType: "e2-standard-8", Zone: "us-central1-a", At: now(9),
		}}, nil
	}}
	got, err := Tick(ctx, deps(t, k, testAPI(nil, nil), log, now(10)), []string{"cpu-batch"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Applied) != 1 {
		t.Fatalf("Applied = %v (waiting %v, warnings %v), want the fast path to apply on tick 1",
			got.Applied, got.Waiting, got.Warnings)
	}
	if len(k.Events) == 0 || k.Events[len(k.Events)-1].Reason != "LadderEvidenceUpdate" {
		t.Errorf("events = %+v, want a LadderEvidenceUpdate", k.Events)
	}
}

func TestEventsGoToTheDefaultNamespace(t *testing.T) {
	// Every event this reconciler emits is about a ComputeClass, which is
	// cluster-scoped, so its involvedObject.namespace is empty. The API server
	// only accepts such an Event in "default"; anywhere else it rejects the
	// create with
	//
	//   involvedObject.namespace: Invalid value: "": does not match event.namespace
	//
	// Verified against the live cluster on 2026-08-02: an identical event was
	// accepted in "default" and rejected in "spot-demo", including in the
	// new-style eventTime form. Emitting into d.Namespace (the pod's namespace,
	// where the state ConfigMap lives) therefore drops every event on the floor
	// — the tick still succeeds because emit failures only warn, so the loss is
	// silent.
	ctx := context.Background()
	k := kubefake.New()
	d := deps(t, k, testAPI(nil, nil), nil, now(0))
	if d.Namespace == "default" {
		t.Fatal("this test is vacuous unless deps() uses a non-default namespace")
	}
	got, err := Tick(ctx, d, []string{"cpu-batch"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Applied) == 0 {
		t.Fatalf("nothing applied, so nothing emitted an event: %+v", got)
	}
	if len(k.EventNS) == 0 {
		t.Fatal("no events emitted at all")
	}
	for i, ns := range k.EventNS {
		if ns != "default" {
			t.Errorf("event %d emitted into %q, want \"default\"", i, ns)
		}
	}
}

func TestApplyFailureLeavesTheLiveLadderRecorded(t *testing.T) {
	ctx := context.Background()
	k := kubefake.New()
	k.ApplyComputeClassFn = func(context.Context, []byte) error {
		return fmt.Errorf("apiserver said no")
	}
	got, err := Tick(ctx, deps(t, k, testAPI(nil, nil), nil, now(0)), []string{"cpu-batch"})
	if err != nil {
		t.Fatal("a failed apply must not fail the tick")
	}
	if len(got.Applied) != 0 {
		t.Fatalf("Applied = %v, want nothing recorded", got.Applied)
	}
	if len(got.Warnings) != 1 {
		t.Fatalf("Warnings = %v, want the apply error surfaced", got.Warnings)
	}
	// The next tick must retry rather than believing the ladder is live.
	k.ApplyComputeClassFn = nil
	got, err = Tick(ctx, deps(t, k, testAPI(nil, nil), nil, now(10)), []string{"cpu-batch"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Applied) != 1 {
		t.Fatalf("retry Applied = %v, want the ladder to land", got.Applied)
	}
}

func TestAnalyzeFailureIsReportedNotFatal(t *testing.T) {
	api := advicefake.New()
	api.RegionsFn = func(string) ([]string, error) { return nil, fmt.Errorf("quota exhausted") }
	k := kubefake.New()
	got, err := Tick(context.Background(),
		deps(t, k, api, nil, now(0)), []string{"cpu-batch"})
	if err != nil {
		t.Fatalf("an advice failure must not fail the tick: %v", err)
	}
	if len(k.Applied) != 0 {
		t.Fatal("nothing may be applied when the analysis failed")
	}
	if len(got.Warnings) != 1 || !strings.Contains(got.Warnings[0], "quota exhausted") {
		t.Fatalf("Warnings = %v, want the advice error surfaced", got.Warnings)
	}
}

func TestDryRunAppliesNothingAndPersistsNothing(t *testing.T) {
	k := kubefake.New()
	d := deps(t, k, testAPI(nil, nil), nil, now(0))
	d.DryRun = true
	got, err := Tick(context.Background(), d, []string{"cpu-batch"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Would) != 1 {
		t.Fatalf("Would = %v, want [batch-cpu]", got.Would)
	}
	if len(k.Applied) != 0 || len(k.State) != 0 {
		t.Fatalf("dry run touched the cluster: applies=%d state=%d", len(k.Applied), len(k.State))
	}
}

func TestAdviseRegionPrefersTheCheapestQualifier(t *testing.T) {
	cands := []analyze.Candidate{
		{Region: "us-central1", Composite: 0.50, SpotHourlyUSD: 0.40, Units: 8},
		{Region: "us-west1", Composite: 0.80, SpotHourlyUSD: 0.48, Units: 8},  // best score
		{Region: "us-east4", Composite: 0.70, SpotHourlyUSD: 0.24, Units: 8},  // cheapest qualifier
		{Region: "us-south1", Composite: 0.52, SpotHourlyUSD: 0.08, Units: 8}, // cheap but under the bar
	}
	got := adviseRegion(cands, "us-central1", 0.15)
	if !strings.Contains(got, "us-east4") {
		t.Fatalf("advisory = %q, want the cheapest qualifying region", got)
	}
	if !strings.Contains(got, "infra/migrate-region.sh us-east4") {
		t.Errorf("advisory must include a runnable command: %q", got)
	}
}

func TestAdviseRegionSilentWhenHomeIsFine(t *testing.T) {
	cands := []analyze.Candidate{
		{Region: "us-central1", Composite: 0.80, SpotHourlyUSD: 0.40, Units: 8},
		{Region: "us-east4", Composite: 0.85, SpotHourlyUSD: 0.24, Units: 8}, // only +6%
	}
	if got := adviseRegion(cands, "us-central1", 0.15); got != "" {
		t.Fatalf("advisory = %q, want silence below the delta bar", got)
	}
}

func TestAdvisoryEventIsEmittedOnlyOnChange(t *testing.T) {
	ctx := context.Background()
	k := kubefake.New()
	// us-east4 is much better, so every tick produces the same advisory.
	api := testAPI(map[string]float64{"us-central1": 0.45, "us-east4": 0.99},
		map[string]float64{"us-east4": 0.05})
	for _, at := range []int{0, 10, 20} {
		if _, err := Tick(ctx, deps(t, k, api, nil, now(at)), []string{"cpu-batch"}); err != nil {
			t.Fatal(err)
		}
	}
	n := 0
	for _, e := range k.Events {
		if e.Reason == "RegionAdvisory" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("emitted %d RegionAdvisory events across 3 ticks, want 1", n)
	}
}

func TestTickPinsTheLogQueryWindow(t *testing.T) {
	ctx := context.Background()
	k := kubefake.New()
	k.PendingClassPodsFn = func(context.Context, []string) (int, error) { return 1, nil }
	log1 := &gcplogfake.Log{RefusalsFn: func(_ context.Context, since time.Time) ([]evidence.Observation, error) {
		return []evidence.Observation{{
			MachineType: "e2-standard-8", Zone: "us-central1-a", At: now(-1),
		}}, nil
	}}
	if _, err := Tick(ctx, deps(t, k, testAPI(nil, nil), log1, now(0)), []string{"cpu-batch"}); err != nil {
		t.Fatal(err)
	}
	log2 := &gcplogfake.Log{RefusalsFn: func(_ context.Context, since time.Time) ([]evidence.Observation, error) {
		return []evidence.Observation{{
			MachineType: "e2-standard-8", Zone: "us-central1-a", At: now(5),
		}}, nil
	}}
	if _, err := Tick(ctx, deps(t, k, testAPI(nil, nil), log2, now(10)), []string{"cpu-batch"}); err != nil {
		t.Fatal(err)
	}
	// The second tick's Since should equal the first tick's Now (not the full MaxAge window).
	if log2.Since != now(0) {
		t.Fatalf("second tick's Since = %v, want %v (the first tick's Now)", log2.Since, now(0))
	}
}

// rungZones returns the zone list GKE would act on: the contents of the first
// `zones: [...]` in the rendered spec. The header comments spell it `zones=[…]`,
// so this cannot be satisfied by a comment.
func rungZones(t *testing.T, manifest []byte) []string {
	t.Helper()
	_, after, ok := strings.Cut(string(manifest), "zones: [")
	if !ok {
		t.Fatalf("manifest has no spec zone list:\n%s", manifest)
	}
	inner, _, ok := strings.Cut(after, "]")
	if !ok {
		t.Fatalf("manifest has an unterminated spec zone list:\n%s", manifest)
	}
	var zones []string
	for _, z := range strings.Split(inner, ",") {
		if z = strings.TrimSpace(z); z != "" {
			zones = append(zones, z)
		}
	}
	return zones
}

func hasZone(zones []string, want string) bool {
	for _, z := range zones {
		if z == want {
			return true
		}
	}
	return false
}

// TestEvidenceWidensBackWhenDecayed walks a full narrow-then-widen cycle and
// asserts on the zones on the rung, because that is the thing that changes.
//
// The timing: one observation crushes the factor to Floor and it recovers as
// 0.05 + 0.95(1 − 0.5^(age/30min)), crossing DryBelow (0.5) at
// age = 30·log₂(0.95/0.5) = 27.78 minutes. So an observation at now(9) is dry
// on the now(10) tick and wet again on the now(40) tick, whose 31-minute-old
// observation is still far short of the 6h prune horizon.
//
// The widen-back tick ingests nothing — no pending pods, no log — so the only
// thing that can restore the zone is the persisted ledger plus the live
// ladder's Evidence flag putting the change back on the fast path. Without
// that flag the change is score-driven, and dropping a zone barely moves the
// top composite, so it would sit in Waiting behind three ticks and a delta bar
// it cannot clear.
func TestEvidenceWidensBackWhenDecayed(t *testing.T) {
	ctx := context.Background()
	k := kubefake.New()

	// 1. Cold start: every sharded zone is on the rung.
	if _, err := Tick(ctx, deps(t, k, testAPI(nil, nil), nil, now(0)), []string{"cpu-batch"}); err != nil {
		t.Fatal(err)
	}
	if len(k.Applied) != 1 {
		t.Fatalf("cold start applied %d manifests, want 1", len(k.Applied))
	}
	if z := rungZones(t, k.Applied[0]); !hasZone(z, "us-central1-a") {
		t.Fatalf("cold-start rung zones = %v, want us-central1-a on the rung", z)
	}

	// 2. us-central1-a refuses a node, so it leaves the rung.
	k.PendingClassPodsFn = func(context.Context, []string) (int, error) { return 1, nil }
	log1 := &gcplogfake.Log{RefusalsFn: func(context.Context, time.Time) ([]evidence.Observation, error) {
		return []evidence.Observation{{MachineType: "e2-standard-8", Zone: "us-central1-a", At: now(9)}}, nil
	}}
	if _, err := Tick(ctx, deps(t, k, testAPI(nil, nil), log1, now(10)), []string{"cpu-batch"}); err != nil {
		t.Fatal(err)
	}
	if len(k.Applied) != 2 {
		t.Fatalf("after the narrowing tick the cluster saw %d applies, want 2", len(k.Applied))
	}
	narrowed := rungZones(t, k.Applied[1])
	if hasZone(narrowed, "us-central1-a") {
		t.Fatalf("narrowed rung zones = %v, want us-central1-a off the rung", narrowed)
	}
	if !hasZone(narrowed, "us-central1-b") {
		t.Fatalf("narrowed rung zones = %v, want the surviving in-region zones kept", narrowed)
	}

	// 3. Past the decay point, ingesting nothing at all: the zone comes back.
	k.PendingClassPodsFn = func(context.Context, []string) (int, error) { return 0, nil }
	got, err := Tick(ctx, deps(t, k, testAPI(nil, nil), nil, now(40)), []string{"cpu-batch"})
	if err != nil {
		t.Fatal(err)
	}
	widened := rungZones(t, k.Applied[len(k.Applied)-1])
	if !hasZone(widened, "us-central1-a") {
		t.Fatalf("rung zones after decay = %v, want us-central1-a back on the rung "+
			"(Applied %v, Waiting %v, NoOp %v, warnings %v)",
			widened, got.Applied, got.Waiting, got.NoOp, got.Warnings)
	}

	// 4. The widen itself rode the sticky flag, but nothing about the ladder it
	//    wrote is evidence-driven any more, so the flag must clear. If the widen
	//    persisted the sticky value instead, a class that was narrowed once
	//    would skip both the three-tick wait and the delta bar for the life of
	//    the CronJob — every estimate wobble churning node pools, silently.
	//
	//    This is asserted on the persisted field rather than on a later tick's
	//    Waiting because this fixture cannot express a sub-bar score change: the
	//    profile has one machine type, so the rendered spec has one rung whose
	//    priorityScore is normalized against itself and is therefore always
	//    1000. Any score-only shift renders identical bytes and lands in NoOp,
	//    which proves nothing about hysteresis.
	var persisted State
	if err := json.Unmarshal(k.State["spot-demo/reconciler-state"], &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.Applied["batch-cpu"].Evidence {
		t.Errorf("persisted Applied[batch-cpu].Evidence = true after the widen, "+
			"want false: the widened ladder has no evidence behind it and must not "+
			"keep skipping hysteresis (state: %s)", k.State["spot-demo/reconciler-state"])
	}
}

// wireLedgerKey is the persisted form of the ledger's machine-type/zone key.
// evidence.Ledger joins the two with a NUL byte and encoding/json escapes it,
// so the ConfigMap carries the six characters \u0000 rather than a literal NUL.
// evidence.TestLedgerRoundTripsThroughJSON already pins these bytes at the
// producer; this is a deliberate second anchor at the consumer, so a separator
// change fails where the ConfigMap is actually written too. The coverage that
// is genuinely new here is the second half of the test below: that a ledger
// decoded back out of the ConfigMap still steers the ladder.
const wireLedgerKey = `"e2-standard-8\u0000us-central1-a":`

// TestPersistedLedgerKeepsItsKeyOnTheWire guards both halves of the round
// trip: the exact bytes written to the ConfigMap, and that what comes back out
// still steers the ladder. Asserting only that the document mentions the
// machine type and the zone would pass for any separator, including one that
// re-keys the map so every lookup misses.
func TestPersistedLedgerKeepsItsKeyOnTheWire(t *testing.T) {
	ctx := context.Background()
	k := kubefake.New()

	if _, err := Tick(ctx, deps(t, k, testAPI(nil, nil), nil, now(0)), []string{"cpu-batch"}); err != nil {
		t.Fatal(err)
	}

	// The observation lands but the apply fails, so the ledger is persisted
	// while the live ladder stays as the cold start left it. That is what
	// forces the next tick to re-render and re-apply from state alone.
	k.PendingClassPodsFn = func(context.Context, []string) (int, error) { return 1, nil }
	k.ApplyComputeClassFn = func(context.Context, []byte) error { return fmt.Errorf("apiserver said no") }
	log := &gcplogfake.Log{RefusalsFn: func(context.Context, time.Time) ([]evidence.Observation, error) {
		return []evidence.Observation{{MachineType: "e2-standard-8", Zone: "us-central1-a", At: now(9)}}, nil
	}}
	if _, err := Tick(ctx, deps(t, k, testAPI(nil, nil), log, now(10)), []string{"cpu-batch"}); err != nil {
		t.Fatal(err)
	}

	doc := string(k.State["spot-demo/reconciler-state"])
	if !strings.Contains(doc, wireLedgerKey) {
		t.Fatalf("persisted state does not carry %s as a ledger key:\n%s", wireLedgerKey, doc)
	}

	// Nothing is re-ingested on this tick: no pending pods, no log. The dry
	// zone can only stay off the rung if the decoded ledger is consulted.
	k.ApplyComputeClassFn = nil
	k.PendingClassPodsFn = func(context.Context, []string) (int, error) { return 0, nil }
	got, err := Tick(ctx, deps(t, k, testAPI(nil, nil), nil, now(15)), []string{"cpu-batch"})
	if err != nil {
		t.Fatal(err)
	}
	zones := rungZones(t, k.Applied[len(k.Applied)-1])
	if hasZone(zones, "us-central1-a") {
		t.Fatalf("rung zones = %v, want the dry zone still off the rung from persisted evidence "+
			"(Applied %v, NoOp %v, warnings %v)", zones, got.Applied, got.NoOp, got.Warnings)
	}
}

// TestScoreDeltaBarEnforced pins Hysteresis.MinScoreDelta, not the NoOp path in
// front of it. Reaching worthIt at all needs a ladder that renders differently
// from the live one — otherwise the fingerprint matches and reconcileClass
// returns NoOp before the bar is ever consulted — while the top score moves by
// less than MinScoreDelta.
//
// So: us-central1-c collapses to 0.30, under score's 0.4 drop band, and leaves
// the rung; us-central1-a ticks up 0.90 → 0.92. The composite is
// obtainability² × 0.95 (uptime 1.0, price normalized to 1.0, no evidence), so
// the top moves 0.770 → 0.804, +4.5%, comfortably under the 15% bar, while the
// spec's zone list drops from three zones to two.
//
// Every tick therefore lands in Waiting, which is the assertion that matters:
// it is the one outcome the NoOp path cannot produce, so the test cannot pass
// by nothing having happened.
func TestScoreDeltaBarEnforced(t *testing.T) {
	ctx := context.Background()
	k := kubefake.New()

	if _, err := Tick(ctx, deps(t, k, testAPI(nil, nil), nil, now(0)), []string{"cpu-batch"}); err != nil {
		t.Fatal(err)
	}

	api := zonedAPI(map[string]float64{"us-central1-a": 0.92, "us-central1-c": 0.30})
	// The first two ticks are held by the consecutive-tick counter; from the
	// third on, the counter is satisfied and only the delta bar holds the
	// change back.
	for i, at := range []int{10, 20, 30, 40} {
		got, err := Tick(ctx, deps(t, k, api, nil, now(at)), []string{"cpu-batch"})
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Waiting) != 1 || got.Waiting[0] != "batch-cpu" {
			t.Fatalf("tick %d: Waiting = %v, want [batch-cpu] held below the delta bar "+
				"(Applied %v, NoOp %v, warnings %v)",
				i+2, got.Waiting, got.Applied, got.NoOp, got.Warnings)
		}
	}
	if len(k.Applied) != 1 {
		t.Fatalf("cluster saw %d applies, want 1: a sub-bar score change must not churn the ladder",
			len(k.Applied))
	}
}

// TestScoreDrivenChangeWaitsForConsecutiveTicks pins
// Hysteresis.ConsecutiveTicks: two ticks held back, the third lands.
//
// The shift has to be in-region, because Tick narrows the analysis to the
// cluster's own region before rendering, and it has to clear the delta bar, so
// that the tick counter is the only thing holding it. us-central1-c collapses
// to 0.30 and leaves the rung while the two survivors fall 0.90 → 0.60, taking
// the top composite from 0.770 to 0.342 — a 56% drop, well past the 15% bar.
//
// ConsecutiveTicks is 3, but the cold-start path forces required = 1, so the
// shift only starts counting once a ladder already exists.
func TestScoreDrivenChangeWaitsForConsecutiveTicks(t *testing.T) {
	ctx := context.Background()
	k := kubefake.New()
	if _, err := Tick(ctx, deps(t, k, testAPI(nil, nil), nil, now(0)), []string{"cpu-batch"}); err != nil {
		t.Fatal(err)
	}
	shifted := zonedAPI(map[string]float64{
		"us-central1-a": 0.60, "us-central1-b": 0.60, "us-central1-c": 0.30,
	})
	for i, at := range []int{10, 20} {
		got, err := Tick(ctx, deps(t, k, shifted, nil, now(at)), []string{"cpu-batch"})
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Waiting) != 1 {
			t.Fatalf("tick %d: Waiting = %v, want the change held back "+
				"(Applied %v, NoOp %v, warnings %v)",
				i+2, got.Waiting, got.Applied, got.NoOp, got.Warnings)
		}
	}
	got, err := Tick(ctx, deps(t, k, shifted, nil, now(30)), []string{"cpu-batch"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Applied) != 1 {
		t.Fatalf("third tick: Applied = %v (warnings %v), want the change to land",
			got.Applied, got.Warnings)
	}
}

func TestMultiProfileFailureHandling(t *testing.T) {
	cfg := testCfg()
	cfg.Profiles["mem-batch"] = config.Profile{Kind: "mem", MachineTypes: []string{"e2-standard-16"}, Size: 10}

	k := kubefake.New()
	callCount := 0
	k.ApplyComputeClassFn = func(context.Context, []byte) error {
		callCount++
		if callCount == 2 {
			return fmt.Errorf("error")
		}
		return nil
	}

	d := Deps{API: testAPI(nil, nil), Kube: k, Cfg: cfg, Namespace: "spot-demo", StateName: "reconciler-state", ClusterRegion: "us-central1", Now: now(0)}
	got, _ := Tick(context.Background(), d, []string{"cpu-batch", "mem-batch"})
	if len(got.Applied) != 1 || got.Applied[0] != "batch-cpu" {
		t.Fatalf("Applied = %v", got.Applied)
	}
	if len(got.Warnings) == 0 {
		t.Fatal("want warning on second profile failure")
	}
	if !strings.Contains(got.Warnings[0], "batch-mem") {
		t.Fatalf("Warnings[0] = %q, want it to contain batch-mem", got.Warnings[0])
	}
}

func TestTickRendersOnlyTheClusterRegion(t *testing.T) {
	ctx := context.Background()
	k := kubefake.New()
	api := testAPI(map[string]float64{"us-central1": 0.5, "us-east4": 0.95}, nil)
	d := deps(t, k, api, nil, now(0))
	d.ClusterRegion = "us-central1"
	got, err := Tick(ctx, d, []string{"cpu-batch"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Applied) != 1 {
		t.Fatalf("Applied = %v, want [batch-cpu]", got.Applied)
	}
	zones := rungZones(t, k.Applied[0])
	for _, z := range zones {
		if !strings.HasPrefix(z, "us-central1-") {
			t.Errorf("zone %q does not have us-central1- prefix; all rendered zones should be in cluster region", z)
		}
	}
}

func TestTickStillAdvisesTheBetterRegion(t *testing.T) {
	ctx := context.Background()
	k := kubefake.New()
	api := testAPI(map[string]float64{"us-central1": 0.5, "us-east4": 0.95}, nil)
	d := deps(t, k, api, nil, now(0))
	d.ClusterRegion = "us-central1"
	got, err := Tick(ctx, d, []string{"cpu-batch"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Advisory == "" {
		t.Fatal("Advisory is empty; should advise the better region")
	}
	if !strings.Contains(got.Advisory, "us-east4") {
		t.Errorf("Advisory = %q, want it to mention us-east4", got.Advisory)
	}
}

func TestTickSkipsUnknownProfileAndReconcilesTheRest(t *testing.T) {
	ctx := context.Background()
	cfg := testCfg()
	cfg.Profiles["gpu-batch"] = config.Profile{Kind: "gpu", MachineTypes: []string{"g2-standard-4"}, Size: 5}
	k := kubefake.New()
	d := Deps{
		API: testAPI(nil, nil), Kube: k, Cfg: cfg,
		Namespace: "spot-demo", StateName: "reconciler-state",
		ClusterRegion: "us-central1", Now: now(0),
	}
	got, err := Tick(ctx, d, []string{"nope-batch", "cpu-batch"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Applied) != 1 || got.Applied[0] != "batch-cpu" {
		t.Fatalf("Applied = %v, want [batch-cpu]", got.Applied)
	}
	if len(got.Warnings) != 1 {
		t.Fatalf("Warnings = %v, want exactly one", got.Warnings)
	}
	if !strings.Contains(got.Warnings[0], "nope-batch") {
		t.Errorf("Warnings[0] = %q, want it to name nope-batch", got.Warnings[0])
	}
}

func TestTickFailsWhenNoProfileResolves(t *testing.T) {
	ctx := context.Background()
	cfg := testCfg()
	k := kubefake.New()
	d := Deps{
		API: testAPI(nil, nil), Kube: k, Cfg: cfg,
		Namespace: "spot-demo", StateName: "reconciler-state",
		ClusterRegion: "us-central1", Now: now(0),
	}
	_, err := Tick(ctx, d, []string{"nope-batch"})
	if err == nil {
		t.Fatal("expected error when no profile resolves")
	}
}

func TestAllDroppedTickLeavesTheLadderAlone(t *testing.T) {
	ctx := context.Background()
	k := kubefake.New()
	// First tick: apply with a good score
	if _, err := Tick(ctx, deps(t, k, testAPI(nil, nil), nil, now(0)), []string{"cpu-batch"}); err != nil {
		t.Fatal(err)
	}
	if len(k.Applied) != 1 {
		t.Fatalf("first tick applied %d manifests, want 1", len(k.Applied))
	}
	// Second tick: all candidates dropped (very low obtainability)
	api := testAPI(map[string]float64{"us-central1": 0.3, "us-east4": 0.2}, nil)
	got, err := Tick(ctx, deps(t, k, api, nil, now(10)), []string{"cpu-batch"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Applied) != 0 {
		t.Fatalf("Applied = %v, want no applies when all candidates dropped", got.Applied)
	}
	if len(got.Warnings) == 0 {
		t.Fatal("expected a warning naming the class")
	}
	if !strings.Contains(got.Warnings[0], "batch-cpu") {
		t.Errorf("Warnings[0] = %q, want it to name batch-cpu", got.Warnings[0])
	}
	// Verify the live ladder was not overwritten
	if len(k.Applied) != 1 {
		t.Fatalf("cluster saw %d applies after the all-dropped tick, want 1 (ladder untouched)", len(k.Applied))
	}
}

func TestAdvisoryRetriesAfterAFailedEmit(t *testing.T) {
	ctx := context.Background()
	k := kubefake.New()
	// Use the same fixture that produces a non-empty advisory
	api := testAPI(map[string]float64{"us-central1": 0.5, "us-east4": 0.95}, nil)

	// Fail selectively on RegionAdvisory events, not other events like LadderRescored
	advisoryAttempts := 0
	k.EmitEventFn = func(_ context.Context, _ string, ev kube.Event) error {
		if ev.Reason == "RegionAdvisory" {
			advisoryAttempts++
			if advisoryAttempts == 1 {
				// First advisory emit fails
				return fmt.Errorf("transient failure")
			}
		}
		// All other events and retry advisory succeed and record
		k.Events = append(k.Events, ev)
		return nil
	}

	d1 := deps(t, k, api, nil, now(0))
	res1, err := Tick(ctx, d1, []string{"cpu-batch"})
	if err != nil {
		t.Fatal(err)
	}
	if res1.Advisory == "" {
		t.Fatal("fixture produced no advisory; the test cannot exercise the retry path")
	}

	// After tick 1: advisory emit failed, so it should not be recorded
	advisoryEventsAfterTick1 := 0
	for _, e := range k.Events {
		if e.Reason == "RegionAdvisory" {
			advisoryEventsAfterTick1++
		}
	}
	if advisoryEventsAfterTick1 != 0 {
		t.Errorf("after tick 1: got %d RegionAdvisory events, want 0 (first emit failed)", advisoryEventsAfterTick1)
	}

	// After tick 1: warning should mention the failed advisory event
	warningText := strings.Join(res1.Warnings, " ")
	if !strings.Contains(warningText, "advisory event") {
		t.Errorf("after tick 1: got warnings %q, want substring 'advisory event'", warningText)
	}

	// Tick 2: Same advisory conditions, advisory emit should now succeed on retry
	d2 := deps(t, k, api, nil, now(10))
	res2, err := Tick(ctx, d2, []string{"cpu-batch"})
	if err != nil {
		t.Fatal(err)
	}

	// After tick 2: exactly one RegionAdvisory event recorded
	advisoryEventsAfterTick2 := 0
	for _, e := range k.Events {
		if e.Reason == "RegionAdvisory" {
			advisoryEventsAfterTick2++
		}
	}
	if advisoryEventsAfterTick2 != 1 {
		t.Errorf("after tick 2: got %d RegionAdvisory events, want 1 (retry succeeded)", advisoryEventsAfterTick2)
	}

	// After tick 2: exactly two advisory emit attempts (one failed, one succeeded)
	if advisoryAttempts != 2 {
		t.Errorf("advisory emit attempted %d times, want 2 (first failed, second succeeded)", advisoryAttempts)
	}

	// After tick 2: advisory should be set (means the emit was recorded)
	if res2.Advisory == "" {
		t.Error("after tick 2: advisory should be set (emit succeeded)")
	}
}

func TestWidenBackEmitsRecoveryNotFailure(t *testing.T) {
	ctx := context.Background()
	k := kubefake.New()

	// 1. Cold start: every sharded zone is on the rung.
	if _, err := Tick(ctx, deps(t, k, testAPI(nil, nil), nil, now(0)), []string{"cpu-batch"}); err != nil {
		t.Fatal(err)
	}

	// 2. us-central1-a refuses a node, so it leaves the rung.
	k.PendingClassPodsFn = func(context.Context, []string) (int, error) { return 1, nil }
	log1 := &gcplogfake.Log{RefusalsFn: func(context.Context, time.Time) ([]evidence.Observation, error) {
		return []evidence.Observation{{MachineType: "e2-standard-8", Zone: "us-central1-a", At: now(9)}}, nil
	}}
	if _, err := Tick(ctx, deps(t, k, testAPI(nil, nil), log1, now(10)), []string{"cpu-batch"}); err != nil {
		t.Fatal(err)
	}

	// 3. Past the decay point: the zone comes back.
	k.PendingClassPodsFn = func(context.Context, []string) (int, error) { return 0, nil }
	if _, err := Tick(ctx, deps(t, k, testAPI(nil, nil), nil, now(40)), []string{"cpu-batch"}); err != nil {
		t.Fatal(err)
	}

	// Check the last event emitted
	found := false
	for _, e := range k.Events {
		if e.Reason == "LadderEvidenceRecovered" {
			found = true
			if strings.Contains(e.Message, "observed provisioning failures") {
				t.Errorf("recovery event should not mention observed provisioning failures; got: %q", e.Message)
			}
			if !strings.Contains(e.Message, "previously failing capacity recovered") {
				t.Errorf("recovery event should mention previously failing capacity; got: %q", e.Message)
			}
			break
		}
	}
	if !found {
		t.Errorf("no LadderEvidenceRecovered event found; events: %+v", k.Events)
	}
}

// TestDefaultOffWidensOntoUnsampledZone pins the pre-branch widening behavior for
// the DEFAULT configuration: probe automation OFF (d.Probe nil, ProbeCfg.Automated
// false). With automation off there are never any probe confirmations, so gating
// the render seam on confirmed() would suppress every never-sharded widen and
// silently collapse an all-dry rung to its pinned (known-dry) zones. The reconciler
// must instead reproduce the old render.ComputeClass widening: an all-dry rung
// falls onto the in-region zones it was never sharded into.
//
// Fixture: e2-standard-8 is sharded only into us-central1-a and -b; e2-standard-16
// also covers -c, so -c is an in-region universe zone e2-standard-8 was never
// sharded into. Evidence then makes both -a and -b dry for e2-standard-8, so its
// rung must widen onto -c — not fall back to the exhausted (still-dry) -a/-b.
func TestDefaultOffWidensOntoUnsampledZone(t *testing.T) {
	ctx := context.Background()
	k := kubefake.New()
	cfg := testCfg()
	cfg.Profiles["cpu-batch"] = config.Profile{
		Kind: "cpu", MachineTypes: []string{"e2-standard-8", "e2-standard-16"}, Size: 20,
	}
	mkDeps := func(log LogSource, at time.Time) Deps {
		return Deps{
			API: widenAPI(), Kube: k, Log: log, Cfg: cfg,
			Namespace: "spot-demo", StateName: "reconciler-state",
			ClusterRegion: "us-central1", Now: at,
			// Probe stays nil and ProbeCfg zero: this is the default-off path.
		}
	}
	if probeAutomated(mkDeps(nil, now(0))) {
		t.Fatal("this test is vacuous unless probe automation is off")
	}

	// 1. Cold start: e2-standard-8 sits on its two sharded zones.
	if _, err := Tick(ctx, mkDeps(nil, now(0)), []string{"cpu-batch"}); err != nil {
		t.Fatal(err)
	}
	if len(k.Applied) != 1 {
		t.Fatalf("cold start applied %d manifests, want 1", len(k.Applied))
	}
	cold := rungZonesFor(t, k.Applied[0], "e2-standard-8")
	if !hasZone(cold, "us-central1-a") || !hasZone(cold, "us-central1-b") {
		t.Fatalf("cold-start e2-standard-8 rung = %v, want its sharded zones a and b", cold)
	}

	// 2. Both sharded zones refuse a node: the whole rung goes dry.
	k.PendingClassPodsFn = func(context.Context, []string) (int, error) { return 2, nil }
	log := &gcplogfake.Log{RefusalsFn: func(context.Context, time.Time) ([]evidence.Observation, error) {
		return []evidence.Observation{
			{MachineType: "e2-standard-8", Zone: "us-central1-a", At: now(9)},
			{MachineType: "e2-standard-8", Zone: "us-central1-b", At: now(9)},
		}, nil
	}}
	got, err := Tick(ctx, mkDeps(log, now(10)), []string{"cpu-batch"})
	if err != nil {
		t.Fatal(err)
	}
	if len(k.Applied) != 2 {
		t.Fatalf("after the all-dry tick the cluster saw %d applies, want 2 "+
			"(Applied %v, Waiting %v, warnings %v)", len(k.Applied), got.Applied, got.Waiting, got.Warnings)
	}
	widened := rungZonesFor(t, k.Applied[1], "e2-standard-8")
	if !hasZone(widened, "us-central1-c") {
		t.Fatalf("all-dry e2-standard-8 rung = %v, want it widened onto the never-sharded "+
			"universe zone us-central1-c (default-off widening must match render.ComputeClass)", widened)
	}
	if hasZone(widened, "us-central1-a") || hasZone(widened, "us-central1-b") {
		t.Fatalf("all-dry e2-standard-8 rung = %v, want the dry sharded zones gone, not an "+
			"exhausted fallback to the pinned zones", widened)
	}
}
