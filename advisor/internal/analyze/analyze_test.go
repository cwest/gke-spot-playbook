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

package analyze

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/cwest/gke-spot-instance-node-pools/advisor/internal/advice"
	"github.com/cwest/gke-spot-instance-node-pools/advisor/internal/advice/fake"
	"github.com/cwest/gke-spot-instance-node-pools/advisor/internal/config"
	"github.com/cwest/gke-spot-instance-node-pools/advisor/internal/evidence"
)

func cfg() *config.Config {
	return &config.Config{
		Project:        "p",
		AllowedRegions: []string{"us-central1", "us-east4"},
		Profiles: map[string]config.Profile{
			"cpu-batch": {Kind: "cpu", MachineTypes: []string{"e2-standard-8", "n2-standard-8"}, Size: 20},
		},
		Caps: config.Caps{MaxSpotRungs: 3},
	}
}

func flat(rate float64, n int) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = rate
	}
	return out
}

func api() *fake.API {
	f := fake.New()
	f.RegionsFn = func(string) ([]string, error) {
		return []string{"us-central1", "us-east4", "europe-west1"}, nil
	}
	f.CapacityFn = func(q advice.CapacityQuery) ([]advice.CapacityResult, error) {
		if q.Region == "us-central1" {
			return []advice.CapacityResult{{
				Obtainability: 0.9, EstimatedUptimeSeconds: 3600,
				Shards: []advice.Shard{
					{Zone: "us-central1-a", MachineType: "e2-standard-8", Count: 10},
					{Zone: "us-central1-b", MachineType: "e2-standard-8", Count: 5},
					{Zone: "us-central1-b", MachineType: "n2-standard-8", Count: 5},
				},
			}}, nil
		}
		return []advice.CapacityResult{{
			Obtainability: 0.3, EstimatedUptimeSeconds: 600, // below 0.4: dropped
			Shards: []advice.Shard{{Zone: "us-east4-a", MachineType: "e2-standard-8", Count: 20}},
		}}, nil
	}
	f.HistoryFn = func(q advice.HistoryQuery) (*advice.HistoryResult, error) {
		switch q.Zone {
		case "us-central1-a":
			return &advice.HistoryResult{DailyPreemptionRates: flat(0.1, 30), LatestSpotUSDPerHour: 0.08}, nil
		case "us-central1-b":
			return &advice.HistoryResult{DailyPreemptionRates: flat(0.2, 30), LatestSpotUSDPerHour: 0.10}, nil
		default:
			return &advice.HistoryResult{}, nil // no data
		}
	}
	return f
}

func TestRun(t *testing.T) {
	a, err := Run(context.Background(), api(), cfg(), "cpu-batch", time.Unix(1753300000, 0), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Candidates) != 4 {
		t.Fatalf("want 4 candidates, got %d: %+v", len(a.Candidates), a.Candidates)
	}
	top := a.Candidates[0]
	if top.MachineType != "e2-standard-8" || top.Zone != "us-central1-a" {
		t.Errorf("top candidate = %s/%s", top.MachineType, top.Zone)
	}
	// Last candidate should be the dropped us-east4 one (obtainability 0.3)
	if !a.Candidates[3].Dropped {
		t.Errorf("us-east4 candidate should be dropped (obtainability 0.3)")
	}
	if got := a.Candidates[3].Flags; len(got) == 0 {
		t.Errorf("dropped candidate should carry flags, got none")
	}
	// cheapest kept candidate has PriceFactor 1.0
	if top.PriceFactor != 1.0 {
		t.Errorf("top PriceFactor = %v, want 1.0", top.PriceFactor)
	}
}

func TestRunUnknownProfile(t *testing.T) {
	if _, err := Run(context.Background(), api(), cfg(), "nope", time.Now(), nil); err == nil {
		t.Fatal("expected error for unknown profile")
	}
}

func TestRunSkipsFailingRegion(t *testing.T) {
	api := &fake.API{
		RegionsFn: func(string) ([]string, error) { return []string{"us-central1", "us-east5"}, nil },
		CapacityFn: func(q advice.CapacityQuery) ([]advice.CapacityResult, error) {
			if q.Region == "us-east5" {
				return nil, fmt.Errorf("advice.capacity us-east5/g2-standard-4: machine type not available")
			}
			return []advice.CapacityResult{{
				Obtainability:          0.9,
				EstimatedUptimeSeconds: 3600,
				Shards:                 []advice.Shard{{Zone: "us-central1-a", MachineType: q.MachineTypes[0], Count: 4}},
			}}, nil
		},
		HistoryFn: func(q advice.HistoryQuery) (*advice.HistoryResult, error) {
			return &advice.HistoryResult{DailyPreemptionRates: []float64{0.02}, LatestSpotUSDPerHour: 0.25}, nil
		},
	}
	cfg := &config.Config{
		Project:        "p",
		AllowedRegions: []string{"us-central1", "us-east5"},
		Profiles:       map[string]config.Profile{"gpu-batch": {Kind: "gpu", MachineTypes: []string{"g2-standard-4"}, Size: 4}},
		Scoring:        config.Scoring{PriceExponent: 1.0},
	}
	a, err := Run(context.Background(), api, cfg, "gpu-batch", time.Unix(0, 0), nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(a.Candidates) == 0 {
		t.Fatal("expected candidates from the healthy region")
	}
	if len(a.SkippedRegions) != 1 || a.SkippedRegions[0].Region != "us-east5" {
		t.Fatalf("SkippedRegions = %+v, want exactly us-east5", a.SkippedRegions)
	}
	if a.SkippedRegions[0].Reason == "" {
		t.Fatal("skip reason must carry the API error text")
	}
}

func TestRunErrorsWhenAllRegionsFail(t *testing.T) {
	api := &fake.API{
		RegionsFn: func(string) ([]string, error) { return []string{"us-east5", "us-west9"}, nil },
		CapacityFn: func(q advice.CapacityQuery) ([]advice.CapacityResult, error) {
			return nil, fmt.Errorf("advice.capacity %s: no capacity", q.Region)
		},
	}
	cfg := &config.Config{
		Project:        "p",
		AllowedRegions: []string{"us-east5", "us-west9"},
		Profiles:       map[string]config.Profile{"gpu-batch": {Kind: "gpu", MachineTypes: []string{"g2-standard-4"}, Size: 4}},
		Scoring:        config.Scoring{PriceExponent: 1.0},
	}
	if _, err := Run(context.Background(), api, cfg, "gpu-batch", time.Unix(0, 0), nil); err == nil {
		t.Fatal("expected error when every region fails")
	}
}

func TestRunBudgetCap(t *testing.T) {
	c := cfg()
	// The cap sits between the two priced zones so it pins a real threshold:
	// us-central1-a at 0.08/8=0.01 passes; us-central1-b at 0.10/8=0.0125 exceeds.
	c.Scoring.MaxHourlyUSDPerUnit = 0.012
	a, err := Run(context.Background(), api(), c, "cpu-batch", time.Unix(1753300000, 0), nil)
	if err != nil {
		t.Fatal(err)
	}
	var overBudget, underBudget *Candidate
	for i := range a.Candidates {
		switch {
		case a.Candidates[i].MachineType == "n2-standard-8" && a.Candidates[i].Zone == "us-central1-b":
			overBudget = &a.Candidates[i]
		case a.Candidates[i].MachineType == "e2-standard-8" && a.Candidates[i].Zone == "us-central1-a":
			underBudget = &a.Candidates[i]
		}
	}
	if overBudget == nil || underBudget == nil {
		t.Fatalf("fixture must produce both sides of the cap; got %+v", a.Candidates)
	}
	if !overBudget.Dropped {
		t.Error("over-cap candidate must be dropped")
	}
	if !hasFlag(overBudget.Flags, "over-budget") {
		t.Errorf("missing over-budget flag: %v", overBudget.Flags)
	}
	// The other direction: a candidate under the cap must survive untouched,
	// otherwise the cap is not a threshold, just a blanket drop.
	if underBudget.Dropped {
		t.Errorf("under-cap candidate must survive: %+v", underBudget)
	}
	if hasFlag(underBudget.Flags, "over-budget") {
		t.Errorf("under-cap candidate flagged over-budget: %v", underBudget.Flags)
	}
	// Candidates with unknown price (us-east4 fixture has none) must NOT be cap-dropped
	for _, cand := range a.Candidates {
		if cand.SpotHourlyUSD == 0 && hasFlag(cand.Flags, "over-budget") {
			t.Errorf("unknown-price candidate cap-dropped: %+v", cand)
		}
	}
}

func hasFlag(flags []string, want string) bool {
	for _, f := range flags {
		if f == want {
			return true
		}
	}
	return false
}

// stubEvidence marks exactly one shape+zone dry.
type stubEvidence struct{ mt, zone string }

func (s stubEvidence) Factor(mt, zone string) float64 {
	if mt == s.mt && zone == s.zone {
		return 0.05
	}
	return 1.0
}

func (s stubEvidence) Dry(mt, zone string) bool { return mt == s.mt && zone == s.zone }

func TestRunAppliesEvidenceToTheMatchingCandidate(t *testing.T) {
	api := api() // two shapes across us-central1-a and -b
	cfg := cfg()
	got, err := Run(context.Background(), api, cfg, "cpu-batch",
		time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC),
		stubEvidence{mt: "e2-standard-8", zone: "us-central1-a"})
	if err != nil {
		t.Fatal(err)
	}
	var hit, miss *Candidate
	for i := range got.Candidates {
		c := &got.Candidates[i]
		if c.MachineType == "e2-standard-8" && c.Zone == "us-central1-a" {
			hit = c
		}
		if c.MachineType == "e2-standard-8" && c.Zone == "us-central1-b" {
			miss = c
		}
	}
	if hit == nil || miss == nil {
		t.Fatalf("fixture must produce both zones; got %+v", got.Candidates)
	}
	if hit.EvidenceFactor != 0.05 || !hit.EvidenceDry {
		t.Errorf("penalized candidate = %+v, want factor 0.05 and dry", hit)
	}
	if miss.EvidenceFactor != 1.0 || miss.EvidenceDry {
		t.Errorf("untouched candidate = %+v, want factor 1.0 and not dry", miss)
	}
	if hit.Dropped {
		t.Error("evidence must not set Dropped: the renderer needs the candidate present")
	}
	if hit.Composite >= miss.Composite {
		t.Errorf("penalized composite %v should rank below %v", hit.Composite, miss.Composite)
	}
}

func TestRunRecordsEvidenceOnOverBudgetCandidates(t *testing.T) {
	// A candidate the budget cap drops still has to carry real evidence
	// fields: Task 7 reads EvidenceDry and EvidenceFactor across the whole
	// candidate set — dropped ones included — to decide where to widen. A
	// zero factor there is indistinguishable from "crushed to nothing" but
	// actually means "never computed".
	c := cfg()
	c.Scoring.MaxHourlyUSDPerUnit = 0.012 // us-central1-b at 0.10/8=0.0125 exceeds
	got, err := Run(context.Background(), api(), c, "cpu-batch",
		time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC),
		stubEvidence{mt: "n2-standard-8", zone: "us-central1-b"})
	if err != nil {
		t.Fatal(err)
	}
	var dropped *Candidate
	for i := range got.Candidates {
		if got.Candidates[i].MachineType == "n2-standard-8" && got.Candidates[i].Zone == "us-central1-b" {
			dropped = &got.Candidates[i]
		}
	}
	if dropped == nil {
		t.Fatalf("fixture must produce n2/us-central1-b; got %+v", got.Candidates)
	}
	if !dropped.Dropped || !hasFlag(dropped.Flags, "over-budget") {
		t.Fatalf("precondition: candidate must be cap-dropped, got %+v", dropped)
	}
	if dropped.EvidenceFactor != 0.05 {
		t.Errorf("EvidenceFactor = %v, want 0.05 (not the Go zero value)", dropped.EvidenceFactor)
	}
	if !dropped.EvidenceDry {
		t.Error("EvidenceDry must be computed even for a cap-dropped candidate")
	}
	if !hasFlag(dropped.Flags, "evidence-dry") {
		t.Errorf("missing evidence-dry flag: %v", dropped.Flags)
	}
}

func TestRunWithNilEvidenceIsNeutral(t *testing.T) {
	api := api()
	cfg := cfg()
	got, err := Run(context.Background(), api, cfg, "cpu-batch",
		time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range got.Candidates {
		if c.EvidenceFactor != 1.0 || c.EvidenceDry {
			t.Fatalf("nil evidence must be neutral, got %+v", c)
		}
	}
}

func TestLedgerSourceAdaptsALedger(t *testing.T) {
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	l := evidence.NewLedger()
	l.Add(evidence.Observation{MachineType: "e2-standard-8", Zone: "us-central1-a", At: now})
	src := LedgerSource(l, now, evidence.Params{
		HalfLife: 30 * time.Minute, Floor: 0.05, DryBelow: 0.5, MaxAge: 6 * time.Hour,
	})
	if got := src.Factor("e2-standard-8", "us-central1-a"); got != 0.05 {
		t.Errorf("Factor = %v, want 0.05", got)
	}
	if !src.Dry("e2-standard-8", "us-central1-a") {
		t.Error("want dry")
	}
	if src.Dry("e2-standard-8", "us-central1-b") {
		t.Error("sibling zone must not be dry")
	}
	// Dry must consult p.DryBelow, not a threshold baked into the adapter.
	// Task 9 feeds DryBelow from config, so the same 0.05 factor has to read
	// as dry under one threshold and not dry under another.
	lenient := LedgerSource(l, now, evidence.Params{
		HalfLife: 30 * time.Minute, Floor: 0.05, DryBelow: 0.01, MaxAge: 6 * time.Hour,
	})
	if lenient.Dry("e2-standard-8", "us-central1-a") {
		t.Error("DryBelow 0.01 must not call a 0.05 factor dry")
	}
}

func TestRunScoresA100Profile(t *testing.T) {
	cfg := &config.Config{
		Project:        "p",
		AllowedRegions: []string{"us-central1", "us-east1", "europe-west4"},
		Profiles: map[string]config.Profile{
			"gpu-a100": {Kind: "gpu", MachineTypes: []string{"a2-highgpu-1g"}, Size: 1},
		},
		Caps: config.Caps{MaxSpotRungs: 3},
	}
	f := fake.New()
	f.RegionsFn = func(string) ([]string, error) {
		return []string{"us-central1", "us-east1", "europe-west4"}, nil
	}
	f.CapacityFn = func(q advice.CapacityQuery) ([]advice.CapacityResult, error) {
		return []advice.CapacityResult{{
			Obtainability: 0.9, EstimatedUptimeSeconds: 7200,
			Shards: []advice.Shard{{Zone: q.Region + "-a", MachineType: "a2-highgpu-1g", Count: q.Size}},
		}}, nil
	}
	f.HistoryFn = func(advice.HistoryQuery) (*advice.HistoryResult, error) {
		return &advice.HistoryResult{DailyPreemptionRates: flat(0.05, 7), LatestSpotUSDPerHour: 1.90}, nil
	}
	a, err := Run(context.Background(), f, cfg, "gpu-a100", time.Unix(1753300000, 0), nil)
	if err != nil {
		t.Fatalf("A100 profile must analyze without error: %v", err)
	}
	if len(a.Candidates) != 3 {
		t.Fatalf("want one A100 candidate per region (3), got %d: %+v", len(a.Candidates), a.Candidates)
	}
	for _, c := range a.Candidates {
		if c.MachineType != "a2-highgpu-1g" || c.Units != 1 {
			t.Errorf("candidate %s/%s Units=%v, want a2-highgpu-1g Units=1", c.MachineType, c.Zone, c.Units)
		}
		if c.Dropped {
			t.Errorf("candidate %s/%s dropped unexpectedly (obtainability 0.9): flags=%v", c.MachineType, c.Zone, c.Flags)
		}
	}
}
