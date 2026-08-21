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

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cwest/gke-spot-instance-node-pools/advisor/internal/advice"
	"github.com/cwest/gke-spot-instance-node-pools/advisor/internal/advice/fake"
	"github.com/cwest/gke-spot-instance-node-pools/advisor/internal/analyze"
	"github.com/cwest/gke-spot-instance-node-pools/advisor/internal/cost"
	"github.com/cwest/gke-spot-instance-node-pools/advisor/internal/density"
	pricingfake "github.com/cwest/gke-spot-instance-node-pools/advisor/internal/pricing/fake"
)

// TestRenderDensityMarkdownPrecision guards against rounding sub-cent $/agent
// values away to $0.00. A cheap node over many agents ($0.19/50 = $0.0038) is a
// real operating point in the Act 5 lifecycle ladder.
func TestRenderDensityMarkdownPrecision(t *testing.T) {
	rep, err := density.Build(0.19, []density.Point{
		{Name: "p1", Agents: 11},
		{Name: "p2", Agents: 16},
		{Name: "p3", Agents: 50},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	md := string(renderDensityMarkdown(rep))
	if !strings.Contains(md, "$0.0038") {
		t.Errorf("p3 $/agent rounded away; want $0.0038 in report, got:\n%s", md)
	}
}

// fakeSpot is a scripted cost.SpotSource keyed by machine type.
type fakeSpot map[string]float64

func (f fakeSpot) SpotHourlyUSD(_ context.Context, _, mt string) (float64, error) {
	if p, ok := f[mt]; ok {
		return p, nil
	}
	return 0, fmt.Errorf("no spot price for %s", mt)
}

func TestRunCostWritesArtifacts(t *testing.T) {
	dir := t.TempDir()
	samples := filepath.Join(dir, "cost-samples.csv")
	if err := os.WriteFile(samples, []byte(
		"ts,node,machine_type,lifecycle,compute_class\n"+
			"2026-07-24T06:00:00Z,default-1,e2-standard-4,on-demand,-\n"+
			"2026-07-24T06:00:30Z,default-1,e2-standard-4,on-demand,-\n"+
			"2026-07-24T06:00:30Z,spot-a,t2d-standard-8,spot,batch-cpu\n"+
			"2026-07-24T06:01:00Z,default-1,e2-standard-4,on-demand,-\n"+
			"2026-07-24T06:01:00Z,spot-a,t2d-standard-8,spot,batch-cpu\n"+
			"2026-07-24T06:01:00Z,spot-b,t2d-standard-8,spot,batch-cpu\n",
	), 0o644); err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	onDemand := pricingfake.New(map[string]float64{
		"us-central1/e2-standard-4":  0.150,
		"us-central1/t2d-standard-8": 0.338,
	})
	err := RunCost(context.Background(), CostOpts{
		SamplesPath: samples, Region: "us-central1", OutDir: out,
		Interval: 30 * time.Second, OnDemand: onDemand,
		Spot: fakeSpot{"t2d-standard-8": 0.0554}, Now: time.Unix(1753336800, 0).UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"cost-report.md", "cost-report.json"} {
		if _, err := os.Stat(filepath.Join(out, f)); err != nil {
			t.Errorf("missing artifact %s: %v", f, err)
		}
	}
	b, err := os.ReadFile(filepath.Join(out, "cost-report.json"))
	if err != nil {
		t.Fatal(err)
	}
	var rep cost.Report
	if err := json.Unmarshal(b, &rep); err != nil {
		t.Fatalf("cost-report.json does not round-trip: %v", err)
	}
	if rep.CombinedSavingsPct <= 0 {
		t.Errorf("CombinedSavingsPct = %v, want > 0", rep.CombinedSavingsPct)
	}
}

func TestRunCostRejectsBadInterval(t *testing.T) {
	dir := t.TempDir()
	samples := filepath.Join(dir, "cost-samples.csv")
	if err := os.WriteFile(samples, []byte(
		"ts,node,machine_type,lifecycle,compute_class\n"+
			"2026-07-24T06:00:00Z,default-1,e2-standard-4,on-demand,-\n",
	), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []time.Duration{0, -15 * time.Second} {
		err := RunCost(context.Background(), CostOpts{
			SamplesPath: samples, Region: "us-central1", OutDir: t.TempDir(),
			Interval: bad, OnDemand: pricingfake.New(map[string]float64{"us-central1/e2-standard-4": 0.150}),
			Spot: fakeSpot{}, Now: time.Unix(1753336800, 0).UTC(),
		})
		if err == nil {
			t.Errorf("RunCost must reject interval %v", bad)
		}
	}
}

func testAPI() *fake.API {
	f := fake.New()
	f.RegionsFn = func(string) ([]string, error) { return []string{"us-central1"}, nil }
	f.CapacityFn = func(q advice.CapacityQuery) ([]advice.CapacityResult, error) {
		return []advice.CapacityResult{{
			Obtainability: 0.9, EstimatedUptimeSeconds: 3600,
			Shards: []advice.Shard{{Zone: "us-central1-a", MachineType: q.MachineTypes[0], Count: q.Size}},
		}}, nil
	}
	f.HistoryFn = func(advice.HistoryQuery) (*advice.HistoryResult, error) {
		return &advice.HistoryResult{DailyPreemptionRates: []float64{0.1}, LatestSpotUSDPerHour: 0.08}, nil
	}
	return f
}

func writeConfig(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "advisor.yaml")
	os.WriteFile(p, []byte(`
project: p
allowedRegions: ["us-central1"]
profiles:
  cpu-batch:
    kind: cpu
    machineTypes: [e2-standard-8]
    size: 20
`), 0o644)
	return p
}

func TestRunAnalyzeWithRender(t *testing.T) {
	out := t.TempDir()
	err := RunAnalyze(context.Background(), testAPI(), AnalyzeOpts{
		ConfigPath: writeConfig(t), Profile: "cpu-batch", OutDir: out,
		Render: true, Now: time.Unix(1753300000, 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{
		"analysis-cpu-batch.json", "computeclass-cpu.yaml",
		"advice-report-cpu-batch.md", "advice-report-cpu-batch.json", "cluster-config-cpu-batch.env",
	} {
		if _, err := os.Stat(filepath.Join(out, f)); err != nil {
			t.Errorf("missing artifact %s: %v", f, err)
		}
	}
}

func TestRunRenderFromSavedAnalysis(t *testing.T) {
	out := t.TempDir()
	opts := AnalyzeOpts{ConfigPath: writeConfig(t), Profile: "cpu-batch",
		OutDir: out, Render: false, Now: time.Unix(1753300000, 0)}
	if err := RunAnalyze(context.Background(), testAPI(), opts); err != nil {
		t.Fatal(err)
	}
	out2 := t.TempDir()
	if err := RunRender(filepath.Join(out, "analysis-cpu-batch.json"), out2, 3); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(out2, "computeclass-cpu.yaml")); err != nil {
		t.Error(err)
	}
}

// A user-tweaked analysis JSON may have candidates in any order. RunRender must
// re-sort kept-first-by-composite so the top rung is the strongest shape.
func TestRunRenderResortsMisorderedCandidates(t *testing.T) {
	a := &analyze.Analysis{
		GeneratedAt: time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC),
		Project:     "p", Profile: "cpu-batch", Kind: "cpu", Size: 20,
		AllowedRegions: []string{"us-central1"},
		Candidates: []analyze.Candidate{
			{MachineType: "z-dropped", Zone: "us-central1-f", Region: "us-central1",
				Obtainability: 0.3, Composite: 0, Dropped: true},
			{MachineType: "n2-standard-8", Zone: "us-central1-a", Region: "us-central1",
				Obtainability: 0.8, Composite: 0.30},
			{MachineType: "e2-standard-8", Zone: "us-central1-a", Region: "us-central1",
				Obtainability: 0.9, Composite: 0.60},
		},
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "analysis.json")
	b, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	if err := RunRender(path, out, 3); err != nil {
		t.Fatal(err)
	}
	cc, err := os.ReadFile(filepath.Join(out, "computeclass-cpu.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(cc)
	first := strings.Index(s, "- machineType:")
	if first < 0 {
		t.Fatalf("no rungs rendered:\n%s", s)
	}
	if !strings.HasPrefix(strings.TrimSpace(s[first:]), "- machineType: e2-standard-8") {
		t.Errorf("first rung is not the highest-composite machine type:\n%s", s[first:])
	}
	if !strings.Contains(s, "priorityScore: 1000") {
		t.Errorf("expected top rung priorityScore 1000:\n%s", s)
	}
}

func TestRunAnalyzeRegionsOverride(t *testing.T) {
	var queried []string
	api := &fake.API{
		RegionsFn: func(string) ([]string, error) {
			return []string{"us-central1", "us-east4", "us-south1"}, nil
		},
		CapacityFn: func(q advice.CapacityQuery) ([]advice.CapacityResult, error) {
			queried = append(queried, q.Region)
			return []advice.CapacityResult{{Obtainability: 0.9, EstimatedUptimeSeconds: 3600,
				Shards: []advice.Shard{{Zone: q.Region + "-a", MachineType: q.MachineTypes[0], Count: 4}}}}, nil
		},
		HistoryFn: func(advice.HistoryQuery) (*advice.HistoryResult, error) {
			return &advice.HistoryResult{DailyPreemptionRates: []float64{0.01}, LatestSpotUSDPerHour: 0.2}, nil
		},
	}
	dir := t.TempDir()
	opts := AnalyzeOpts{ConfigPath: writeConfig(t), Profile: "cpu-batch",
		OutDir: dir, Render: false, Regions: []string{"us-east4"}, Now: time.Unix(0, 0)}
	if err := RunAnalyze(context.Background(), api, opts); err != nil {
		t.Fatalf("RunAnalyze: %v", err)
	}
	if len(queried) == 0 {
		t.Fatal("no capacity queries recorded; --regions override was not exercised")
	}
	for _, r := range queried {
		if r != "us-east4" {
			t.Fatalf("queried %s; --regions must restrict analysis to us-east4", r)
		}
	}
}

func TestRunServingCost_printsSavings(t *testing.T) {
	var out bytes.Buffer
	err := RunServingCost(0.70, 8, 10, &out)
	if err != nil {
		t.Fatal(err)
	}
	s := out.String()
	if !strings.Contains(s, "always-on") || !strings.Contains(s, "scale-to-zero") || !strings.Contains(s, "savings") {
		t.Fatalf("missing expected sections: %q", s)
	}
}
