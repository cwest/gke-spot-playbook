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

package render

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/cwest/gke-spot-instance-node-pools/advisor/internal/analyze"
)

var update = flag.Bool("update", false, "rewrite golden files")

func fixtureAnalysis(kind string) *analyze.Analysis {
	mt := map[string][2]string{
		"cpu": {"e2-standard-8", "n2-standard-8"},
		"gpu": {"g2-standard-8", "g2-standard-24"},
	}[kind]
	return &analyze.Analysis{
		GeneratedAt: time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC),
		Project:     "p", Profile: kind + "-batch", Kind: kind, Size: 20,
		AllowedRegions: []string{"us-central1"},
		Candidates: []analyze.Candidate{
			{MachineType: mt[0], Zone: "us-central1-a", Region: "us-central1",
				Obtainability: 0.9, Composite: 0.60},
			{MachineType: mt[0], Zone: "us-central1-b", Region: "us-central1",
				Obtainability: 0.9, Composite: 0.55},
			{MachineType: mt[1], Zone: "us-central1-a", Region: "us-central1",
				Obtainability: 0.8, Composite: 0.30},
			{MachineType: mt[1], Zone: "us-central1-f", Region: "us-central1",
				Obtainability: 0.3, Composite: 0, Dropped: true},
		},
	}
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

func TestComputeClassCPU(t *testing.T) {
	got, err := ComputeClass(fixtureAnalysis("cpu"), "batch-cpu", 3)
	if err != nil {
		t.Fatal(err)
	}
	checkGolden(t, "computeclass-cpu.yaml", got)
}

func TestComputeClassGPU(t *testing.T) {
	got, err := ComputeClass(fixtureAnalysis("gpu"), "batch-gpu", 3)
	if err != nil {
		t.Fatal(err)
	}
	checkGolden(t, "computeclass-gpu.yaml", got)
}

func TestComputeClassGPUEmitsAcceleratorBlock(t *testing.T) {
	got, err := ComputeClass(fixtureAnalysis("gpu"), "batch-gpu", 3)
	if err != nil {
		t.Fatal(err)
	}
	s := string(got)
	if n, want := strings.Count(s, "spot: true"), 2; n != want {
		t.Fatalf("expected %d spot rungs, got %d:\n%s", want, n, s)
	}
	// Each spot rung plus the flex rung carries a gpu block => 3 accelerator blocks.
	if n, want := strings.Count(s, "type: nvidia-l4"), 3; n != want {
		t.Errorf("expected %d nvidia-l4 gpu blocks (2 spot + 1 flex), got %d:\n%s", want, n, s)
	}
	// Per-shape counts: g2-standard-8 -> 1 L4, g2-standard-24 -> 2 L4.
	for _, want := range []string{
		"  - machineType: g2-standard-8\n    spot: true\n    gpu:\n      type: nvidia-l4\n      count: 1\n",
		"  - machineType: g2-standard-24\n    spot: true\n    gpu:\n      type: nvidia-l4\n      count: 2\n",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing spot gpu rung:\n%q\nin:\n%s", want, s)
		}
	}
}

func TestComputeClassGPUFlexRungHasAcceleratorBlock(t *testing.T) {
	got, err := ComputeClass(fixtureAnalysis("gpu"), "batch-gpu", 3)
	if err != nil {
		t.Fatal(err)
	}
	flex := "  - machineType: g2-standard-8\n    priorityScore: 1\n    gpu:\n      type: nvidia-l4\n      count: 1\n    maxRunDurationSeconds: 86400\n"
	if !strings.Contains(string(got), flex) {
		t.Errorf("flexStart rung missing gpu block:\n%s", got)
	}
}

func TestComputeClassCPUHasNoAcceleratorBlock(t *testing.T) {
	got, err := ComputeClass(fixtureAnalysis("cpu"), "batch-cpu", 3)
	if err != nil {
		t.Fatal(err)
	}
	s := string(got)
	if strings.Contains(s, "gpu:") || strings.Contains(s, "nvidia-l4") {
		t.Errorf("cpu class must not contain a gpu block:\n%s", s)
	}
}

func TestComputeClassAllDropped(t *testing.T) {
	a := fixtureAnalysis("cpu")
	for i := range a.Candidates {
		a.Candidates[i].Dropped = true
	}
	got, err := ComputeClass(a, "batch-cpu", 3)
	if err != nil {
		t.Fatalf("all-dropped should still render: %v", err)
	}
	s := string(got)
	for _, want := range []string{
		`# WARNING: ALL candidates scored below the 0.4 obtainability band ("Low").`,
		"# Spot capacity is unlikely to be obtainable; the fallback floor will carry the load.",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing warning %q in:\n%s", want, s)
		}
	}
	if !strings.Contains(s, "# rung ") || !strings.Contains(s, "machineType:") {
		t.Errorf("expected rungs built from dropped candidates:\n%s", s)
	}
}

func TestComputeClassAllDroppedGPUFlexStaysLast(t *testing.T) {
	a := &analyze.Analysis{
		Profile: "gpu-batch", Kind: "gpu", Project: "p", AllowedRegions: []string{"us-central1"},
		Candidates: []analyze.Candidate{
			{MachineType: "g2-standard-8", Zone: "us-central1-a", Region: "us-central1",
				Obtainability: 0.3, Composite: 0, Dropped: true, Flags: []string{"low-obtainability"}},
			{MachineType: "g2-standard-4", Zone: "us-central1-b", Region: "us-central1",
				Obtainability: 0.2, Composite: 0, Dropped: true, Flags: []string{"low-obtainability"}},
		},
	}
	out, err := ComputeClass(a, "batch-gpu", 3)
	if err != nil {
		t.Fatal(err)
	}
	y := string(out)
	// Spot rungs get synthetic descending scores 3,2; flex rung keeps 1.
	for _, want := range []string{"priorityScore: 3", "priorityScore: 2", "priorityScore: 1"} {
		if !strings.Contains(y, want) {
			t.Fatalf("missing %q in:\n%s", want, y)
		}
	}
	if strings.Count(y, "priorityScore: 1\n") != 1 {
		t.Fatalf("flex rung score 1 must be unique (no spot rung may tie it):\n%s", y)
	}
}

func TestComputeClassFiltersToTargetRegion(t *testing.T) {
	// Kept candidates span four regions. The ComputeClass is applied to ONE
	// regional cluster (the region ClusterConfig chooses: region of the first
	// kept candidate, us-south1). Rung zones must be confined to that region.
	a := &analyze.Analysis{
		GeneratedAt: time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC),
		Project:     "p", Profile: "cpu-batch", Kind: "cpu", Size: 20,
		AllowedRegions: []string{"us-south1", "us-east1", "us-central1"},
		Candidates: []analyze.Candidate{
			{MachineType: "t2d-standard-8", Zone: "us-south1-b", Region: "us-south1",
				Obtainability: 0.9, Composite: 0.62},
			{MachineType: "t2d-standard-8", Zone: "us-east1-b", Region: "us-east1",
				Obtainability: 0.8, Composite: 0.50},
			{MachineType: "e2-standard-8", Zone: "us-south1-a", Region: "us-south1",
				Obtainability: 0.7, Composite: 0.30},
			{MachineType: "e2-standard-8", Zone: "us-central1-b", Region: "us-central1",
				Obtainability: 0.6, Composite: 0.28},
		},
	}
	got, err := ComputeClass(a, "batch-cpu", 3)
	if err != nil {
		t.Fatal(err)
	}
	s := string(got)
	if !strings.Contains(s, "zones: [us-south1-b]") {
		t.Errorf("rung 1 zones should be [us-south1-b] only:\n%s", s)
	}
	if !strings.Contains(s, "zones: [us-south1-a]") {
		t.Errorf("rung 2 (e2) zones should be [us-south1-a] only:\n%s", s)
	}
	for _, bad := range []string{"us-east1-b", "us-central1-b"} {
		if strings.Contains(s, bad) {
			t.Errorf("out-of-region zone %q leaked into manifest:\n%s", bad, s)
		}
	}
}

func TestComputeClassEmpty(t *testing.T) {
	a := fixtureAnalysis("cpu")
	a.Candidates = nil
	if _, err := ComputeClass(a, "batch-cpu", 3); err == nil {
		t.Fatal("expected error when no candidates exist at all")
	}
}

// rungBlock returns the rendered priority block for one machine type. Rungs are
// near-identical by construction — same zones, same shape of YAML — so a
// document-wide strings.Contains is satisfied by whichever rung happens to
// match. Assertions about a specific rung must be anchored to it.
func rungBlock(t *testing.T, s, machineType string) string {
	t.Helper()
	head := "  - machineType: " + machineType + "\n"
	i := strings.Index(s, head)
	if i < 0 {
		t.Fatalf("no rung for %s in:\n%s", machineType, s)
	}
	rest := s[i+len(head):]
	for _, end := range []string{"\n  - machineType: ", "\n  whenUnsatisfiable:"} {
		if j := strings.Index(rest, end); j >= 0 {
			rest = rest[:j]
		}
	}
	return head + rest
}

// renderedZones returns every zone listed in a rendered location.zones block.
func renderedZones(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "zones: [") {
			continue
		}
		inner := strings.TrimSuffix(strings.TrimPrefix(line, "zones: ["), "]")
		if inner == "" {
			continue
		}
		out = append(out, strings.Split(inner, ", ")...)
	}
	return out
}

// evidenceFixture shards two shapes across two in-region zones each — e2 into
// -a and -b, n2 into -c and -f — so a shape dry in both of its own zones still
// has somewhere to widen to.
func evidenceFixture(dryZones ...string) *analyze.Analysis {
	dry := map[string]bool{}
	for _, z := range dryZones {
		dry[z] = true
	}
	mk := func(mt, zone string, composite float64) analyze.Candidate {
		c := analyze.Candidate{
			MachineType: mt, Zone: zone, Region: "us-central1",
			Obtainability: 0.9, Composite: composite, EvidenceFactor: 1.0,
		}
		if mt == "e2-standard-8" && dry[zone] {
			c.EvidenceDry = true
			c.EvidenceFactor = 0.05
			c.Composite = composite * 0.05
		}
		return c
	}
	cands := []analyze.Candidate{
		mk("e2-standard-8", "us-central1-a", 0.60),
		mk("e2-standard-8", "us-central1-b", 0.55),
		mk("n2-standard-8", "us-central1-c", 0.30),
		mk("n2-standard-8", "us-central1-f", 0.28),
	}
	analyze.SortCandidates(cands)
	return &analyze.Analysis{
		GeneratedAt: time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC),
		Project:     "p", Profile: "cpu-batch", Kind: "cpu", Size: 20,
		AllowedRegions: []string{"us-central1"},
		Candidates:     cands,
	}
}

func TestComputeClassDropsDryZonesFromARung(t *testing.T) {
	got, err := ComputeClass(evidenceFixture("us-central1-a"), "batch-cpu", 3)
	if err != nil {
		t.Fatal(err)
	}
	s := string(got)
	if !strings.Contains(s, "zones: [us-central1-b]") {
		t.Errorf("e2 rung should keep only its clean zone:\n%s", s)
	}
	if strings.Contains(s, "widened=evidence") {
		t.Errorf("a rung with a surviving clean zone must not widen:\n%s", s)
	}
}

func TestComputeClassWidensWhenEveryZoneIsDry(t *testing.T) {
	// Both e2 zones are dry, so the rung widens to the zones we never sharded
	// for e2 (-c and -f) rather than disappearing. Anchored to the e2 rung: n2
	// lists exactly those same two zones, so a document-wide match would be
	// satisfied by n2 alone and would never look at the rung under test.
	got, err := ComputeClass(evidenceFixture("us-central1-a", "us-central1-b"), "batch-cpu", 3)
	if err != nil {
		t.Fatal(err)
	}
	s := string(got)
	e2 := rungBlock(t, s, "e2-standard-8")
	if !strings.Contains(e2, "zones: [us-central1-c, us-central1-f]") {
		t.Errorf("e2 rung should widen to the unsampled zones:\n%s", e2)
	}
	if !strings.Contains(s, "widened=evidence") {
		t.Errorf("widened rung must be marked in the header:\n%s", s)
	}
	if strings.Contains(s, "us-central1-a") || strings.Contains(s, "us-central1-b") {
		t.Errorf("dry zones must not appear on the widened rung:\n%s", s)
	}
}

func TestComputeClassWidensOnlyWithinTheTargetRegion(t *testing.T) {
	// Widening is the only path in the renderer that puts a zone on a rung no
	// candidate for that shape ever mentioned, so it is the only new way an
	// out-of-region zone could reach location.zones — which GKE rejects on a
	// regional cluster. The widening universe must be region-filtered.
	// TestComputeClassFiltersToTargetRegion cannot see this: its fixture
	// carries no evidence, so widening never fires there.
	a := &analyze.Analysis{
		GeneratedAt: time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC),
		Project:     "p", Profile: "cpu-batch", Kind: "cpu", Size: 20,
		AllowedRegions: []string{"us-central1", "us-east1"},
		Candidates: []analyze.Candidate{
			{MachineType: "n2-standard-8", Zone: "us-central1-c", Region: "us-central1",
				Obtainability: 0.9, Composite: 0.60, EvidenceFactor: 1.0},
			{MachineType: "t2d-standard-8", Zone: "us-east1-b", Region: "us-east1",
				Obtainability: 0.9, Composite: 0.50, EvidenceFactor: 1.0},
			{MachineType: "e2-standard-8", Zone: "us-central1-a", Region: "us-central1",
				Obtainability: 0.9, Composite: 0.030, EvidenceFactor: 0.05, EvidenceDry: true},
			{MachineType: "e2-standard-8", Zone: "us-central1-b", Region: "us-central1",
				Obtainability: 0.9, Composite: 0.028, EvidenceFactor: 0.05, EvidenceDry: true},
		},
	}
	got, err := ComputeClass(a, "batch-cpu", 3)
	if err != nil {
		t.Fatal(err)
	}
	s := string(got)
	if !strings.Contains(s, "widened=evidence") {
		t.Fatalf("fixture must exercise widening or it guards nothing:\n%s", s)
	}
	zones := renderedZones(s)
	if len(zones) == 0 {
		t.Fatalf("no zones rendered at all:\n%s", s)
	}
	for _, z := range zones {
		if !strings.HasPrefix(z, "us-central1-") {
			t.Errorf("zone %q is outside the target region us-central1:\n%s", z, s)
		}
	}
}

func TestComputeClassWidensIntoAZoneOnlyADroppedCandidateSaw(t *testing.T) {
	// us-central1-f is named by nothing but a dropped candidate. It is still a
	// real in-region zone that e2 was never sharded into, so it is a valid
	// widening target: the universe is built above the Dropped filter on
	// purpose. Adding `&& !c.Dropped` there would silently shrink the escape
	// hatch, and nothing else in the suite would notice.
	a := &analyze.Analysis{
		GeneratedAt: time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC),
		Project:     "p", Profile: "cpu-batch", Kind: "cpu", Size: 20,
		AllowedRegions: []string{"us-central1"},
		Candidates: []analyze.Candidate{
			{MachineType: "n2-standard-8", Zone: "us-central1-c", Region: "us-central1",
				Obtainability: 0.9, Composite: 0.60, EvidenceFactor: 1.0},
			{MachineType: "e2-standard-8", Zone: "us-central1-a", Region: "us-central1",
				Obtainability: 0.9, Composite: 0.030, EvidenceFactor: 0.05, EvidenceDry: true},
			{MachineType: "e2-standard-8", Zone: "us-central1-b", Region: "us-central1",
				Obtainability: 0.9, Composite: 0.028, EvidenceFactor: 0.05, EvidenceDry: true},
			{MachineType: "n2d-standard-8", Zone: "us-central1-f", Region: "us-central1",
				Obtainability: 0.3, Composite: 0, Dropped: true, Flags: []string{"low-obtainability"}},
		},
	}
	got, err := ComputeClass(a, "batch-cpu", 3)
	if err != nil {
		t.Fatal(err)
	}
	e2 := rungBlock(t, string(got), "e2-standard-8")
	if !strings.Contains(e2, "zones: [us-central1-c, us-central1-f]") {
		t.Errorf("widening must consider a zone only a dropped candidate saw:\n%s", e2)
	}
}

func TestComputeClassRendersZonesSortedAndDeduped(t *testing.T) {
	// Zones land on a rung in composite order, and Task 7 now appends from
	// three sources (candidates, the widening universe, the pinned fallback),
	// so a repeated (machineType, zone) pair is easy to produce. dedup only
	// collapses ADJACENT repeats, so it is load-bearing on the sort in front of
	// it: [f a f] deduped without sorting keeps both f's, and a duplicate zone
	// in location.zones is not a manifest we want to ship.
	a := &analyze.Analysis{
		GeneratedAt: time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC),
		Project:     "p", Profile: "cpu-batch", Kind: "cpu", Size: 20,
		AllowedRegions: []string{"us-central1"},
		Candidates: []analyze.Candidate{
			{MachineType: "e2-standard-8", Zone: "us-central1-f", Region: "us-central1",
				Obtainability: 0.9, Composite: 0.60, EvidenceFactor: 1.0},
			{MachineType: "e2-standard-8", Zone: "us-central1-a", Region: "us-central1",
				Obtainability: 0.9, Composite: 0.55, EvidenceFactor: 1.0},
			{MachineType: "e2-standard-8", Zone: "us-central1-f", Region: "us-central1",
				Obtainability: 0.8, Composite: 0.50, EvidenceFactor: 1.0},
		},
	}
	got, err := ComputeClass(a, "batch-cpu", 3)
	if err != nil {
		t.Fatal(err)
	}
	e2 := rungBlock(t, string(got), "e2-standard-8")
	if !strings.Contains(e2, "zones: [us-central1-a, us-central1-f]") {
		t.Errorf("zones must render sorted and deduplicated:\n%s", e2)
	}
}

func TestComputeClassPromotesTheCleanShape(t *testing.T) {
	// With e2 crushed by evidence, n2 outranks it and must take the top rung.
	got, err := ComputeClass(evidenceFixture("us-central1-a", "us-central1-b"), "batch-cpu", 3)
	if err != nil {
		t.Fatal(err)
	}
	s := string(got)
	first := strings.Index(s, "- machineType: n2-standard-8")
	second := strings.Index(s, "- machineType: e2-standard-8")
	if first < 0 || second < 0 {
		t.Fatalf("both shapes should render:\n%s", s)
	}
	if first > second {
		t.Errorf("clean n2 shape should be promoted above the dry e2 shape:\n%s", s)
	}
}

func TestComputeClassNeverEmitsAZonelessRung(t *testing.T) {
	// Widening is deliberately PER SHAPE: n2 failing in us-central1-c says
	// nothing about e2 there. Exhaustion therefore needs a shape whose own dry
	// zones cover the entire in-region universe — here e2 alone, dry in both
	// the zones the analysis saw. There is nowhere left to widen to, so the
	// ladder must degrade to its original zones rather than emit an invalid
	// zoneless priority.
	a := evidenceFixture("us-central1-a", "us-central1-b")
	var only []analyze.Candidate
	for _, c := range a.Candidates {
		if c.MachineType == "e2-standard-8" {
			only = append(only, c)
		}
	}
	a.Candidates = only
	got, err := ComputeClass(a, "batch-cpu", 3)
	if err != nil {
		t.Fatal(err)
	}
	s := string(got)
	if strings.Contains(s, "zones: []") {
		t.Errorf("no rung may be zoneless:\n%s", s)
	}
	if !strings.Contains(s, "exhausted=all-zones-dry") {
		t.Errorf("exhausted rungs must be marked:\n%s", s)
	}
	// Widened and Exhausted are mutually exclusive by construction. Claiming
	// both would tell the operator the rung was rebuilt from fresh zones AND
	// had nowhere to go, and would stamp widened=evidence on zones we know are
	// dry — worse than saying nothing.
	if strings.Contains(s, "widened=evidence") {
		t.Errorf("an exhausted rung must not also claim to be widened:\n%s", s)
	}
}

func TestComputeClassWidenedGolden(t *testing.T) {
	got, err := ComputeClass(evidenceFixture("us-central1-a", "us-central1-b"), "batch-cpu", 3)
	if err != nil {
		t.Fatal(err)
	}
	checkGolden(t, "computeclass-cpu-widened.yaml", got)
}

func findRung(t *testing.T, rungs []rung, mt string) rung {
	t.Helper()
	for _, r := range rungs {
		if r.MachineType == mt {
			return r
		}
	}
	t.Fatalf("no rung for %s in %v", mt, rungs)
	return rung{}
}

// TestBuildRungsWidensOnlyToConfirmedZones is the render-seam probe gate: a rung
// whose only sharded zone is evidence-dry widens ONLY to the never-sampled
// universe zones a live probe has confirmed. us-central1-b and -f are both in the
// universe (a second shape put them there) and neither was ever sharded for g2,
// so both are widen candidates; only -b is confirmed, so -f must stay off. With
// nothing confirmed the rung never degrades: it keeps its pinned zone and is
// marked exhausted, never zoneless.
func TestBuildRungsWidensOnlyToConfirmedZones(t *testing.T) {
	a := &analyze.Analysis{Candidates: []analyze.Candidate{
		{MachineType: "g2-standard-4", Zone: "us-central1-a", Region: "us-central1", Composite: 0.3, EvidenceDry: true},
		// A different shape puts -b and -f in the in-region universe; g2 was
		// never sharded into either, so both are legitimate widen targets.
		{MachineType: "n2-standard-8", Zone: "us-central1-b", Region: "us-central1", Composite: 0.5},
		{MachineType: "n2-standard-8", Zone: "us-central1-f", Region: "us-central1", Composite: 0.5},
	}}

	confirmed := func(mt, z string) bool { return mt == "g2-standard-4" && z == "us-central1-b" }
	g2 := findRung(t, buildRungs(a, 5, false, "us-central1", confirmed), "g2-standard-4")
	if got, want := g2.Zones, []string{"us-central1-b"}; !cmp.Equal(got, want) {
		t.Errorf("widened zones = %v, want %v (confirmed -b only; unconfirmed -f held off)", got, want)
	}
	if !g2.Widened || g2.Exhausted {
		t.Errorf("rung should be widened (not exhausted): widened=%v exhausted=%v", g2.Widened, g2.Exhausted)
	}

	// Never degrade: with nothing confirmed the rung keeps its pinned zone and is
	// marked exhausted rather than emitting a zoneless priority.
	none := func(string, string) bool { return false }
	g2 = findRung(t, buildRungs(a, 5, false, "us-central1", none), "g2-standard-4")
	if got, want := g2.Zones, []string{"us-central1-a"}; !cmp.Equal(got, want) {
		t.Errorf("unconfirmed rung zones = %v, want pinned %v", got, want)
	}
	if g2.Widened || !g2.Exhausted {
		t.Errorf("rung with nothing confirmed must be exhausted, not widened: widened=%v exhausted=%v", g2.Widened, g2.Exhausted)
	}
}
