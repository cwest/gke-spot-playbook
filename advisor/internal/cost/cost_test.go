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

package cost

import (
	"context"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/cwest/gke-spot-instance-node-pools/advisor/internal/pricing"
)

var update = flag.Bool("update", false, "rewrite golden files")

const region = "us-central1"

// fakeOnDemand is a scripted pricing.Source keyed by machine type. Unknown
// machine types return ErrUnsupported, mirroring the real BillingSource so the
// unpriced-family path is exercised.
type fakeOnDemand map[string]float64

func (f fakeOnDemand) OnDemandHourlyUSD(_ context.Context, _, mt string) (float64, error) {
	if p, ok := f[mt]; ok {
		return p, nil
	}
	return 0, fmt.Errorf("%w: %s", pricing.ErrUnsupported, mt)
}

// fakeSpot is a scripted SpotSource keyed by machine type.
type fakeSpot map[string]float64

func (f fakeSpot) SpotHourlyUSD(_ context.Context, _, mt string) (float64, error) {
	if p, ok := f[mt]; ok {
		return p, nil
	}
	return 0, fmt.Errorf("no spot price for %s", mt)
}

func onDemand() fakeOnDemand {
	return fakeOnDemand{"e2-standard-4": 0.150, "t2d-standard-8": 0.338}
}

func spot() fakeSpot {
	return fakeSpot{"t2d-standard-8": 0.0554}
}

func parseFixture(t *testing.T) ([]Usage, float64, map[string]int) {
	t.Helper()
	f, err := os.Open(filepath.Join("..", "..", "testdata", "cost-samples.csv"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	usages, window, peak, _, err := ParseSamples(f, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return usages, window, peak
}

func findUsage(t *testing.T, usages []Usage, mt, life string) Usage {
	t.Helper()
	for _, u := range usages {
		if u.MachineType == mt && u.Lifecycle == life {
			return u
		}
	}
	t.Fatalf("usage %s/%s not found in %+v", mt, life, usages)
	return Usage{}
}

func TestParseSamples(t *testing.T) {
	usages, window, _ := parseFixture(t)
	// window = (last − first) + interval = 120s + 30s = 150s
	if window != 150 {
		t.Errorf("window = %v, want 150", window)
	}
	// spot t2d: 12 rows × 30s = 360 node-seconds; 4 distinct nodes; peak 4 concurrent
	spot := findUsage(t, usages, "t2d-standard-8", "spot")
	if spot.NodeSeconds != 360 || spot.Nodes != 4 || spot.PeakNodes != 4 {
		t.Errorf("t2d/spot = %+v, want NodeSeconds 360 Nodes 4 PeakNodes 4", spot)
	}
	// on-demand e2: 5 rows × 30s = 150 node-seconds; 1 node; peak 1
	e2 := findUsage(t, usages, "e2-standard-4", "on-demand")
	if e2.NodeSeconds != 150 || e2.Nodes != 1 || e2.PeakNodes != 1 {
		t.Errorf("e2/on-demand = %+v, want NodeSeconds 150 Nodes 1 PeakNodes 1", e2)
	}
}

func TestParseSamplesWarnsOnIntervalMismatch(t *testing.T) {
	// The fixture's distinct timestamps are spaced 30s apart. Parsing with a
	// 15s interval that disagrees with that modal delta must surface a
	// non-fatal warning; parsing with the matching 30s interval must not.
	open := func() *os.File {
		f, err := os.Open(filepath.Join("..", "..", "testdata", "cost-samples.csv"))
		if err != nil {
			t.Fatal(err)
		}
		return f
	}

	fMismatch := open()
	defer fMismatch.Close()
	_, _, _, warnings, err := ParseSamples(fMismatch, 15*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) == 0 {
		t.Fatalf("expected an interval-mismatch warning for 15s interval, got none")
	}

	fMatch := open()
	defer fMatch.Close()
	_, _, _, warnings, err = ParseSamples(fMatch, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 0 {
		t.Errorf("expected no warnings for matching 30s interval, got %v", warnings)
	}
}

func approx(t *testing.T, name string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-6 {
		t.Errorf("%s = %.10f, want %.10f", name, got, want)
	}
}

func TestBuildReportMath(t *testing.T) {
	usages, window, peak := parseFixture(t)
	now := time.Date(2026, 7, 24, 6, 5, 0, 0, time.UTC)
	rep, err := Build(context.Background(), usages, window, peak, region, onDemand(), spot(), now)
	if err != nil {
		t.Fatal(err)
	}
	// fake prices: on-demand e2-standard-4 = 0.150, t2d-standard-8 = 0.338 $/hr;
	// spot t2d-standard-8 = 0.0554 $/hr.
	// spot usage 360 node-seconds; e2 usage 150 node-seconds; window 150s.
	// ActualUSD      = 360/3600×0.0554 + 150/3600×0.150       = 0.011790
	// SpotAtOnDemand = 360/3600×0.338  + 150/3600×0.150       = 0.040050
	// AlwaysOn       = (4×0.338 + 1×0.150) × 150/3600         = 0.06258333…
	// AlwaysOnDaily  = 0.06258333 × 86400/150                 = 36.048
	approx(t, "ActualUSD", rep.ActualUSD, 0.011790)
	approx(t, "SpotAtOnDemandUSD", rep.SpotAtOnDemandUSD, 0.040050)
	approx(t, "AlwaysOnUSD", rep.AlwaysOnUSD, 0.0625833333)
	approx(t, "AlwaysOnDailyUSD", rep.AlwaysOnDailyUSD, 36.048)
	// SpotDiscountPct    = 1 − 0.011790/0.040050    = 0.70561798
	// DutyCyclePct       = 1 − 0.040050/0.06258333  = 0.36005326
	// CombinedSavingsPct = 1 − 0.011790/0.06258333  = 0.81161119
	approx(t, "SpotDiscountPct", rep.SpotDiscountPct, 0.70561798)
	approx(t, "DutyCyclePct", rep.DutyCyclePct, 0.36005326)
	approx(t, "CombinedSavingsPct", rep.CombinedSavingsPct, 0.81161119)
	if len(rep.Unpriced) != 0 {
		t.Errorf("Unpriced = %v, want none", rep.Unpriced)
	}
}

// TestAlwaysOnMixedLifecycleConcurrency locks the rule that AlwaysOnUSD sizes
// the on-demand counterfactual by peak CONCURRENT nodes summed across BOTH
// lifecycles per machine type (per-timestamp sum over lifecycles, then max over
// timestamps) — not the max of the per-lifecycle peaks.
//
// t2d-standard-8 runs spot AND on-demand at the same three timestamps:
//
//	ts        spot  on-demand  concurrent
//	00:00:00   2       1          3
//	00:00:30   3       2          5   <- true peak
//	00:01:00   3       1          4
//
// True cross-lifecycle PeakConcurrent = 5. The old max-of-per-lifecycle logic
// would use max(spotPeak 3, onDemandPeak 2) = 3, understating the pool.
//
// window = (00:01:00 − 00:00:00) + 30s interval = 90s; on-demand t2d = 0.338.
// AlwaysOnUSD = 5 × 0.338 × 90/3600 = 0.04225 (correct).
// Old behavior would yield 3 × 0.338 × 90/3600 = 0.02535 (wrong).
func TestAlwaysOnMixedLifecycleConcurrency(t *testing.T) {
	csvData := strings.Join([]string{
		"ts,node,machine_type,lifecycle,compute_class",
		"2026-07-24T00:00:00Z,s1,t2d-standard-8,spot,",
		"2026-07-24T00:00:00Z,s2,t2d-standard-8,spot,",
		"2026-07-24T00:00:00Z,d1,t2d-standard-8,on-demand,",
		"2026-07-24T00:00:30Z,s1,t2d-standard-8,spot,",
		"2026-07-24T00:00:30Z,s2,t2d-standard-8,spot,",
		"2026-07-24T00:00:30Z,s3,t2d-standard-8,spot,",
		"2026-07-24T00:00:30Z,d1,t2d-standard-8,on-demand,",
		"2026-07-24T00:00:30Z,d2,t2d-standard-8,on-demand,",
		"2026-07-24T00:01:00Z,s1,t2d-standard-8,spot,",
		"2026-07-24T00:01:00Z,s2,t2d-standard-8,spot,",
		"2026-07-24T00:01:00Z,s3,t2d-standard-8,spot,",
		"2026-07-24T00:01:00Z,d1,t2d-standard-8,on-demand,",
		"",
	}, "\n")

	usages, window, peak, _, err := ParseSamples(strings.NewReader(csvData), 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if window != 90 {
		t.Errorf("window = %v, want 90", window)
	}
	if peak["t2d-standard-8"] != 5 {
		t.Errorf("peak concurrent t2d-standard-8 = %d, want 5", peak["t2d-standard-8"])
	}

	rep, err := Build(context.Background(), usages, window, peak, region, onDemand(), spot(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	// AlwaysOn sized to the 5-node cross-lifecycle peak, not the 3-node
	// per-lifecycle max: 5 × 0.338 × 90/3600 = 0.04225.
	approx(t, "AlwaysOnUSD", rep.AlwaysOnUSD, 0.04225)
}

func TestBuildUnpricedFamily(t *testing.T) {
	usages, window, peak := parseFixture(t)
	// A GPU family the demo still does not price (g2 is priced now; a2 is not);
	// on-demand lookup returns ErrUnsupported so it must be reported and excluded
	// from all totals.
	usages = append(usages, Usage{
		MachineType: "a2-highgpu-1g", Lifecycle: "spot",
		NodeSeconds: 300, Nodes: 2, PeakNodes: 2,
	})
	peak["a2-highgpu-1g"] = 2
	rep, err := Build(context.Background(), usages, window, peak, region, onDemand(), spot(), time.Now())
	if err != nil {
		t.Fatalf("unpriced family must not error: %v", err)
	}
	if diff := cmp.Diff([]string{"a2-highgpu-1g"}, rep.Unpriced); diff != "" {
		t.Errorf("Unpriced mismatch (-want +got):\n%s", diff)
	}
	// Totals must match the priced-only report — g2 excluded entirely.
	approx(t, "ActualUSD", rep.ActualUSD, 0.011790)
	approx(t, "AlwaysOnUSD", rep.AlwaysOnUSD, 0.0625833333)
}

func checkGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	p := filepath.Join("..", "..", "testdata", "golden", name)
	if *update {
		if err := os.WriteFile(p, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read golden (run with -update to create): %v", err)
	}
	if diff := cmp.Diff(string(want), string(got)); diff != "" {
		t.Errorf("%s mismatch (-want +got):\n%s", name, diff)
	}
}

func TestRenderMarkdownGolden(t *testing.T) {
	usages, window, peak := parseFixture(t)
	now := time.Date(2026, 7, 24, 6, 5, 0, 0, time.UTC)
	rep, err := Build(context.Background(), usages, window, peak, region, onDemand(), spot(), now)
	if err != nil {
		t.Fatal(err)
	}
	checkGolden(t, "cost-report.md", RenderMarkdown(rep))
}
