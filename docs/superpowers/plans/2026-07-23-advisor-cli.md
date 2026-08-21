# Capacity Advisor CLI Implementation Plan (Plan 1 of 4)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build the `capacity-advisor` Go CLI (`analyze` + `render`) that scores spot candidates via the beta `advice.capacity`/`advice.capacityHistory` APIs and renders ComputeClass ladders, a report, and cluster config.

**Architecture:** A thin `advice.API` interface insulates everything from the beta GAPIC client. `analyze` expands `allowedRegions`, queries capacity per region (batches of ≤5 machine types) and history per candidate zone, scores candidates (`obtainability² × uptime × preemption × price`), and writes `analysis.json`. `render` turns an analysis into `computeclass-{cpu,gpu}.yaml`, `advice-report.{md,json}`, and `cluster-config.env`. Reconcile mode is **Plan 4** — not built here.

**Tech Stack:** Go 1.24+, `cloud.google.com/go/compute/apiv1beta` (AdviceClient) + `apiv1` (RegionsClient), `gopkg.in/yaml.v3`, `github.com/spf13/cobra`, `github.com/google/go-cmp` (tests only).

## Global Constraints

- Project for live runs: `example-sandbox`. Every task's automated tests run offline (fixtures/fakes); live API calls happen only in Task 9's manual smoke.
- `advice.capacityHistory` accepts only `provisioningModel: SPOT`; `advice.capacity` caps at 5 machine types per request; obtainability < 0.4 is the documented "Low" band → hard-drop.
- Uptime factors: 3600s→1.0, 600s→0.6, 60s→0.2 (missing→0.6, flagged). Composite: `obtainability² × uptime × preemption × price`.
- Max 3 spot rungs per ComputeClass; GPU class gets a `flexStart` rung and `whenUnsatisfiable: DoNotScaleUp`; CPU class gets `ScaleUpAnyway`.
- Commit style: Casey's conventional-commits-with-emoji (see `~/.claude/CLAUDE.md`); commits signed; **no AI attribution trailers**.
- Diagrams anywhere in docs: Mermaid only, never ASCII.

## File Structure

- `advisor/go.mod` — module `github.com/cwest/gke-spot-instance-node-pools/advisor`
- `advisor/cmd/capacity-advisor/main.go` — thin cobra wiring only
- `advisor/internal/cli/cli.go` — testable command runners (deps injected)
- `advisor/internal/config/config.go` — `advisor.yaml` load + defaults + validation
- `advisor/internal/regions/expand.go` — allowedRegions expansion
- `advisor/internal/machinetype/units.go` — vCPU/GPU unit counts for price normalization
- `advisor/internal/score/score.go` — factor + composite functions
- `advisor/internal/advice/types.go` — `API` interface + normalized query/result types
- `advisor/internal/advice/fake/fake.go` — scripted fake for tests
- `advisor/internal/advice/gcp/client.go` — real GAPIC-backed implementation
- `advisor/internal/analyze/analyze.go` — orchestration → `Analysis`
- `advisor/internal/render/computeclass.go`, `report.go`, `clusterconfig.go`
- `advisor/testdata/` — API JSON fixtures + golden files
- Root: `.gitignore`, `Makefile`, `advisor.yaml` (sample)

---

### Task 1: Module scaffold + config loading

**Files:**
- Create: `.gitignore`, `Makefile`, `advisor.yaml`, `advisor/go.mod`, `advisor/internal/config/config.go`
- Test: `advisor/internal/config/config_test.go`

**Interfaces:**
- Produces: `config.Load(path string) (*Config, error)`; `Config{Project string; AllowedRegions []string; Profiles map[string]Profile; Hysteresis Hysteresis; Caps Caps}`; `Profile{MachineTypes []string; Size int32; Kind string}` (`Kind` is `"cpu"` or `"gpu"`); `Hysteresis{MinScoreDelta float64; ConsecutiveTicks int}`; `Caps{MaxSpotRungs int}`.

- [ ] **Step 1: Scaffold**

```bash
cat > .gitignore <<'EOF'
.vscode/
.worktrees/
out/
advisor/capacity-advisor
EOF
cat > Makefile <<'EOF'
.PHONY: test
test:
	cd advisor && go test ./...
EOF
cat > advisor.yaml <<'EOF'
project: example-sandbox
allowedRegions: ["us"]
profiles:
  cpu-batch:
    kind: cpu
    machineTypes: [e2-standard-8, n2-standard-8, t2d-standard-8]
    size: 20
  gpu-batch:
    kind: gpu
    machineTypes: [g2-standard-4, g2-standard-8, g2-standard-24]
    size: 4
hysteresis:
  minScoreDelta: 0.15
  consecutiveTicks: 3
caps:
  maxSpotRungs: 3
EOF
mkdir -p advisor && cd advisor && go mod init github.com/cwest/gke-spot-instance-node-pools/advisor && go get gopkg.in/yaml.v3 github.com/google/go-cmp/cmp
```

- [ ] **Step 2: Write the failing test**

`advisor/internal/config/config_test.go`:

```go
package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, s string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "advisor.yaml")
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

const valid = `
project: example-sandbox
allowedRegions: ["us"]
profiles:
  cpu-batch:
    kind: cpu
    machineTypes: [e2-standard-8]
    size: 20
`

func TestLoadValidAppliesDefaults(t *testing.T) {
	c, err := Load(write(t, valid))
	if err != nil {
		t.Fatal(err)
	}
	if c.Project != "example-sandbox" {
		t.Errorf("project = %q", c.Project)
	}
	if c.Hysteresis.MinScoreDelta != 0.15 || c.Hysteresis.ConsecutiveTicks != 3 {
		t.Errorf("hysteresis defaults not applied: %+v", c.Hysteresis)
	}
	if c.Caps.MaxSpotRungs != 3 {
		t.Errorf("caps default not applied: %+v", c.Caps)
	}
}

func TestLoadRejectsBadConfigs(t *testing.T) {
	cases := map[string]string{
		"missing project":     strings.Replace(valid, "project: example-sandbox", "", 1),
		"empty allowedRegions": strings.Replace(valid, `allowedRegions: ["us"]`, "allowedRegions: []", 1),
		"empty machineTypes":  strings.Replace(valid, "machineTypes: [e2-standard-8]", "machineTypes: []", 1),
		"bad kind":            strings.Replace(valid, "kind: cpu", "kind: quantum", 1),
		"zero size":           strings.Replace(valid, "size: 20", "size: 0", 1),
	}
	for name, y := range cases {
		if _, err := Load(write(t, y)); err == nil {
			t.Errorf("%s: expected error, got nil", name)
		}
	}
}
```

- [ ] **Step 3: Run test to verify it fails**

Run: `cd advisor && go test ./internal/config/`
Expected: FAIL — `undefined: Load`

- [ ] **Step 4: Implement**

`advisor/internal/config/config.go`:

```go
// Package config loads and validates advisor.yaml.
package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

type Profile struct {
	Kind         string   `yaml:"kind"` // "cpu" or "gpu"
	MachineTypes []string `yaml:"machineTypes"`
	Size         int32    `yaml:"size"`
}

type Hysteresis struct {
	MinScoreDelta    float64 `yaml:"minScoreDelta"`
	ConsecutiveTicks int     `yaml:"consecutiveTicks"`
}

type Caps struct {
	MaxSpotRungs int `yaml:"maxSpotRungs"`
}

type Config struct {
	Project        string             `yaml:"project"`
	AllowedRegions []string           `yaml:"allowedRegions"`
	Profiles       map[string]Profile `yaml:"profiles"`
	Hysteresis     Hysteresis         `yaml:"hysteresis"`
	Caps           Caps               `yaml:"caps"`
}

func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	c := &Config{}
	if err := yaml.Unmarshal(b, c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if c.Hysteresis.MinScoreDelta == 0 {
		c.Hysteresis.MinScoreDelta = 0.15
	}
	if c.Hysteresis.ConsecutiveTicks == 0 {
		c.Hysteresis.ConsecutiveTicks = 3
	}
	if c.Caps.MaxSpotRungs == 0 {
		c.Caps.MaxSpotRungs = 3
	}
	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

func (c *Config) validate() error {
	if c.Project == "" {
		return fmt.Errorf("project is required")
	}
	if len(c.AllowedRegions) == 0 {
		return fmt.Errorf("allowedRegions must not be empty")
	}
	for name, p := range c.Profiles {
		if p.Kind != "cpu" && p.Kind != "gpu" {
			return fmt.Errorf("profile %s: kind must be cpu or gpu, got %q", name, p.Kind)
		}
		if len(p.MachineTypes) == 0 {
			return fmt.Errorf("profile %s: machineTypes must not be empty", name)
		}
		if p.Size <= 0 {
			return fmt.Errorf("profile %s: size must be > 0", name)
		}
	}
	return nil
}
```

- [ ] **Step 5: Run test to verify it passes**

Run: `cd advisor && go test ./internal/config/`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add .gitignore Makefile advisor.yaml advisor/
git commit -m "✨ feat(advisor): scaffold module and advisor.yaml config loading"
```

---

### Task 2: allowedRegions expansion

**Files:**
- Create: `advisor/internal/regions/expand.go`
- Test: `advisor/internal/regions/expand_test.go`

**Interfaces:**
- Produces: `regions.Expand(allowed, all []string) ([]string, error)` — `all` is the live region list (caller fetches it); result is sorted, deduped. Entry forms: keyword (`global`, `us`, `eu`, `asia`, `northamerica`, `southamerica`, `australia`, `me`, `africa`), explicit region name, or prefix glob ending in `*`. An entry matching zero regions is an error (catches typos).

- [ ] **Step 1: Write the failing test**

`advisor/internal/regions/expand_test.go`:

```go
package regions

import (
	"testing"

	"github.com/google/go-cmp/cmp"
)

var all = []string{
	"africa-south1", "asia-east1", "asia-south1", "australia-southeast1",
	"europe-west1", "europe-west4", "me-central1", "northamerica-northeast1",
	"southamerica-east1", "us-central1", "us-east4", "us-west1",
}

func TestExpand(t *testing.T) {
	cases := []struct {
		name    string
		allowed []string
		want    []string
	}{
		{"keyword us", []string{"us"}, []string{"us-central1", "us-east4", "us-west1"}},
		{"keyword eu", []string{"eu"}, []string{"europe-west1", "europe-west4"}},
		{"global", []string{"global"}, all},
		{"explicit", []string{"us-east4"}, []string{"us-east4"}},
		{"prefix glob", []string{"europe-west*"}, []string{"europe-west1", "europe-west4"}},
		{"union dedup", []string{"us", "us-central1", "asia-east1"},
			[]string{"asia-east1", "us-central1", "us-east4", "us-west1"}},
	}
	for _, tc := range cases {
		got, err := Expand(tc.allowed, all)
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if diff := cmp.Diff(tc.want, got); diff != "" {
			t.Errorf("%s: (-want +got)\n%s", tc.name, diff)
		}
	}
}

func TestExpandErrors(t *testing.T) {
	for name, allowed := range map[string][]string{
		"typo region":  {"us-centrall"},
		"dead glob":    {"mars-*"},
		"bad keyword":  {"moon"},
	} {
		if _, err := Expand(allowed, all); err == nil {
			t.Errorf("%s: expected error, got nil", name)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd advisor && go test ./internal/regions/`
Expected: FAIL — `undefined: Expand`

- [ ] **Step 3: Implement**

`advisor/internal/regions/expand.go`:

```go
// Package regions expands allowedRegions entries (keywords, explicit names,
// prefix globs) against the live region list.
package regions

import (
	"fmt"
	"sort"
	"strings"
)

var keywords = map[string][]string{
	"global":       {""},
	"us":           {"us-"},
	"eu":           {"europe-"},
	"asia":         {"asia-"},
	"northamerica": {"northamerica-"},
	"southamerica": {"southamerica-"},
	"australia":    {"australia-"},
	"me":           {"me-"},
	"africa":       {"africa-"},
}

func Expand(allowed, all []string) ([]string, error) {
	set := map[string]bool{}
	for _, entry := range allowed {
		prefixes, matched := prefixesFor(entry, all)
		if len(prefixes) == 0 && !matched {
			return nil, fmt.Errorf("allowedRegions entry %q matches no region", entry)
		}
		for _, r := range all {
			for _, p := range prefixes {
				if strings.HasPrefix(r, p) {
					set[r] = true
				}
			}
		}
		if matched {
			set[entry] = true
		}
	}
	if len(set) == 0 {
		return nil, fmt.Errorf("allowedRegions %v matched no regions", allowed)
	}
	out := make([]string, 0, len(set))
	for r := range set {
		out = append(out, r)
	}
	sort.Strings(out)
	return out, nil
}

// prefixesFor returns match prefixes for keyword/glob entries, and whether
// the entry is itself an exact region name in all.
func prefixesFor(entry string, all []string) ([]string, bool) {
	if p, ok := keywords[entry]; ok {
		return p, false
	}
	if strings.HasSuffix(entry, "*") {
		prefix := strings.TrimSuffix(entry, "*")
		for _, r := range all {
			if strings.HasPrefix(r, prefix) {
				return []string{prefix}, false
			}
		}
		return nil, false
	}
	for _, r := range all {
		if r == entry {
			return nil, true
		}
	}
	return nil, false
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd advisor && go test ./internal/regions/`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add advisor/internal/regions/
git commit -m "✨ feat(advisor): expand allowedRegions keywords, globs, and explicit regions"
```

---

### Task 3: Machine-type units + scoring

**Files:**
- Create: `advisor/internal/machinetype/units.go`, `advisor/internal/score/score.go`
- Test: `advisor/internal/machinetype/units_test.go`, `advisor/internal/score/score_test.go`

**Interfaces:**
- Produces: `machinetype.Units(machineType, kind string) (float64, error)` — vCPUs for `kind=="cpu"` (parsed from the trailing `-<n>`), GPU count for `kind=="gpu"` (g2 map).
- Produces: `score.UptimeFactor(sec int) float64`, `score.PreemptionFactor(daily []float64) float64`, `score.PriceFactor(perUnit, minPerUnit float64) float64`, `score.Composite(obtainability, uptime, preemption, price float64) (float64, bool)` — bool is `dropped` (obtainability < 0.4).

- [ ] **Step 1: Write the failing tests**

`advisor/internal/machinetype/units_test.go`:

```go
package machinetype

import "testing"

func TestUnits(t *testing.T) {
	cases := []struct {
		mt, kind string
		want     float64
		wantErr  bool
	}{
		{"e2-standard-8", "cpu", 8, false},
		{"t2d-standard-16", "cpu", 16, false},
		{"g2-standard-4", "gpu", 1, false},
		{"g2-standard-24", "gpu", 2, false},
		{"g2-standard-96", "gpu", 8, false},
		{"n2-standard-8", "gpu", 0, true},  // not a known GPU shape
		{"weird", "cpu", 0, true},
	}
	for _, tc := range cases {
		got, err := Units(tc.mt, tc.kind)
		if (err != nil) != tc.wantErr || got != tc.want {
			t.Errorf("Units(%q,%q) = %v, %v; want %v, err=%v", tc.mt, tc.kind, got, err, tc.want, tc.wantErr)
		}
	}
}
```

`advisor/internal/score/score_test.go`:

```go
package score

import (
	"math"
	"testing"
)

func almost(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestUptimeFactor(t *testing.T) {
	cases := map[int]float64{3600: 1.0, 7200: 1.0, 600: 0.6, 60: 0.2, 0: 0.6}
	for sec, want := range cases {
		if got := UptimeFactor(sec); !almost(got, want) {
			t.Errorf("UptimeFactor(%d) = %v, want %v", sec, got, want)
		}
	}
}

func TestPreemptionFactor(t *testing.T) {
	flat := make([]float64, 30)
	for i := range flat {
		flat[i] = 0.2
	}
	if got := PreemptionFactor(flat); !almost(got, 0.8) {
		t.Errorf("flat: got %v, want 0.8", got)
	}
	// Worsening: 23 quiet days then 7 bad days -> base (1-0.5)=0.5, penalized ×0.8 = 0.4
	worse := make([]float64, 30)
	for i := 23; i < 30; i++ {
		worse[i] = 0.5
	}
	if got := PreemptionFactor(worse); !almost(got, 0.4) {
		t.Errorf("worsening: got %v, want 0.4", got)
	}
	if got := PreemptionFactor(nil); !almost(got, 1.0) {
		t.Errorf("empty history: got %v, want neutral 1.0", got)
	}
}

func TestPriceFactorAndComposite(t *testing.T) {
	if got := PriceFactor(0.02, 0.01); !almost(got, 0.5) {
		t.Errorf("PriceFactor = %v, want 0.5", got)
	}
	if got := PriceFactor(0, 0.01); !almost(got, 1.0) { // missing price -> neutral
		t.Errorf("missing price: got %v, want 1.0", got)
	}
	c, dropped := Composite(0.9, 1.0, 0.8, 0.5)
	if dropped || !almost(c, 0.9*0.9*1.0*0.8*0.5) {
		t.Errorf("Composite = %v dropped=%v", c, dropped)
	}
	if _, dropped := Composite(0.3, 1, 1, 1); !dropped {
		t.Error("obtainability 0.3 must be dropped")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd advisor && go test ./internal/machinetype/ ./internal/score/`
Expected: FAIL — `undefined: Units`, `undefined: UptimeFactor`

- [ ] **Step 3: Implement**

`advisor/internal/machinetype/units.go`:

```go
// Package machinetype maps machine types to normalization units.
package machinetype

import (
	"fmt"
	"strconv"
	"strings"
)

// g2 shapes -> attached NVIDIA L4 count.
var g2GPUs = map[string]float64{
	"g2-standard-4": 1, "g2-standard-8": 1, "g2-standard-12": 1,
	"g2-standard-16": 1, "g2-standard-32": 1, "g2-standard-24": 2,
	"g2-standard-48": 4, "g2-standard-96": 8,
}

// Units returns the per-instance denominator for price normalization:
// vCPUs for cpu profiles, GPUs for gpu profiles.
func Units(machineType, kind string) (float64, error) {
	if kind == "gpu" {
		if n, ok := g2GPUs[machineType]; ok {
			return n, nil
		}
		return 0, fmt.Errorf("unknown GPU shape %q", machineType)
	}
	parts := strings.Split(machineType, "-")
	n, err := strconv.Atoi(parts[len(parts)-1])
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("cannot parse vCPUs from %q", machineType)
	}
	return float64(n), nil
}
```

`advisor/internal/score/score.go`:

```go
// Package score computes candidate factors and the composite score.
// Composite = obtainability² × uptime × preemption × price; obtainability is
// squared because capacity you cannot get has no price.
package score

const dropBelow = 0.4 // documented "Low" obtainability band

func UptimeFactor(sec int) float64 {
	switch {
	case sec >= 3600:
		return 1.0
	case sec >= 600:
		return 0.6
	case sec > 0:
		return 0.2
	default:
		return 0.6 // missing signal: neutral, caller flags it
	}
}

// PreemptionFactor is 1 - mean(last ≤7 daily rates), penalized ×0.8 when the
// recent week is >10% worse than the preceding days. Empty history is neutral.
func PreemptionFactor(daily []float64) float64 {
	if len(daily) == 0 {
		return 1.0
	}
	split := len(daily) - 7
	if split < 0 {
		split = 0
	}
	recent := mean(daily[split:])
	f := 1 - recent
	if split >= 7 { // enough history to judge a trend
		if prior := mean(daily[:split]); recent > prior*1.1 {
			f *= 0.8
		}
	}
	if f < 0 {
		return 0
	}
	return f
}

// PriceFactor normalizes to the cheapest candidate: cheapest = 1.0.
// A missing price (0) is neutral; the caller flags it in the report.
func PriceFactor(perUnit, minPerUnit float64) float64 {
	if perUnit <= 0 || minPerUnit <= 0 {
		return 1.0
	}
	return minPerUnit / perUnit
}

func Composite(obtainability, uptime, preemption, price float64) (float64, bool) {
	if obtainability < dropBelow {
		return 0, true
	}
	return obtainability * obtainability * uptime * preemption * price, false
}

func mean(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := 0.0
	for _, x := range xs {
		s += x
	}
	return s / float64(len(xs))
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd advisor && go test ./internal/machinetype/ ./internal/score/`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add advisor/internal/machinetype/ advisor/internal/score/
git commit -m "✨ feat(advisor): scoring factors and machine-type units"
```

---

### Task 4: advice.API interface, fake, and GCP client

**Files:**
- Create: `advisor/internal/advice/types.go`, `advisor/internal/advice/fake/fake.go`, `advisor/internal/advice/gcp/client.go`
- Create: `advisor/testdata/capacity_response.json`, `advisor/testdata/history_response.json`
- Test: `advisor/internal/advice/gcp/client_test.go`

**Interfaces:**
- Produces (consumed by Tasks 5, 9):

```go
package advice

type CapacityQuery struct {
	Project, Region string
	MachineTypes    []string // ≤5, caller enforces
	Size            int32
	Kind            string // "cpu" | "gpu" (for accelerator handling later; unused by GCP call)
}
type Shard struct{ Zone, MachineType string; Count int32 }
type CapacityResult struct {
	Obtainability          float64
	EstimatedUptimeSeconds int
	Shards                 []Shard
}
type HistoryQuery struct{ Project, Region, Zone, MachineType string }
type HistoryResult struct {
	DailyPreemptionRates []float64 // oldest first
	LatestSpotUSDPerHour float64
}
type API interface {
	Regions(ctx context.Context, project string) ([]string, error)
	Capacity(ctx context.Context, q CapacityQuery) ([]CapacityResult, error)
	CapacityHistory(ctx context.Context, q HistoryQuery) (*HistoryResult, error)
}
```

- Produces: `fake.New()` returning `*fake.API` with settable `RegionsFn`, `CapacityFn`, `HistoryFn` funcs.
- Produces: `gcp.New(ctx, opts ...option.ClientOption) (*gcp.Client, error)` implementing `advice.API`.

- [ ] **Step 1: Define types and fake (no test yet — pure data)**

`advisor/internal/advice/types.go`: exactly the interface block above (package `advice`, add `import "context"`).

`advisor/internal/advice/fake/fake.go`:

```go
// Package fake is a scripted advice.API for tests.
package fake

import (
	"context"
	"fmt"

	"github.com/cwest/gke-spot-instance-node-pools/advisor/internal/advice"
)

type API struct {
	RegionsFn  func(project string) ([]string, error)
	CapacityFn func(q advice.CapacityQuery) ([]advice.CapacityResult, error)
	HistoryFn  func(q advice.HistoryQuery) (*advice.HistoryResult, error)
}

func New() *API { return &API{} }

func (f *API) Regions(_ context.Context, project string) ([]string, error) {
	if f.RegionsFn == nil {
		return nil, fmt.Errorf("fake: RegionsFn not set")
	}
	return f.RegionsFn(project)
}

func (f *API) Capacity(_ context.Context, q advice.CapacityQuery) ([]advice.CapacityResult, error) {
	if f.CapacityFn == nil {
		return nil, fmt.Errorf("fake: CapacityFn not set")
	}
	return f.CapacityFn(q)
}

func (f *API) CapacityHistory(_ context.Context, q advice.HistoryQuery) (*advice.HistoryResult, error) {
	if f.HistoryFn == nil {
		return nil, fmt.Errorf("fake: HistoryFn not set")
	}
	return f.HistoryFn(q)
}
```

- [ ] **Step 2: Verify the real GAPIC surface before writing the client**

Run:

```bash
cd advisor && go get cloud.google.com/go/compute@latest google.golang.org/api@latest
go doc cloud.google.com/go/compute/apiv1beta AdviceClient
go doc cloud.google.com/go/compute/apiv1beta AdviceClient.Capacity
go doc cloud.google.com/go/compute/apiv1beta AdviceClient.CapacityHistory
go doc cloud.google.com/go/compute/apiv1beta/computepb CapacityAdviceRequest | head -40
```

Record the exact request/response type and field names. The code in Step 4 uses the names implied by the beta discovery document — **adjust to what `go doc` reports** (the `advice.API` interface must not change; only the `gcp` package adapts).

- [ ] **Step 3: Write the failing client test (httptest fixtures)**

`advisor/testdata/capacity_response.json` (from the documented example):

```json
{
  "recommendations": [
    {
      "scores": {"obtainability": 0.9, "estimatedUptime": "600s"},
      "shards": [
        {"instanceCount": 90, "machineType": "n2-standard-2", "provisioningModel": "SPOT",
         "zone": "https://www.googleapis.com/compute/beta/projects/p/zones/us-central1-a"},
        {"instanceCount": 10, "machineType": "n2-standard-4", "provisioningModel": "SPOT",
         "zone": "https://www.googleapis.com/compute/beta/projects/p/zones/us-central1-c"}
      ]
    }
  ]
}
```

`advisor/testdata/history_response.json` (from the documented example, two days + one price interval):

```json
{
  "machineType": "n2-standard-32",
  "location": "https://compute.googleapis.com/compute/beta/projects/p/zones/us-central1-a",
  "preemptionHistory": [
    {"interval": {"startTime": "2026-04-20T07:00:00Z", "endTime": "2026-04-21T07:00:00Z"}, "preemptionRate": 0.52},
    {"interval": {"startTime": "2026-04-21T07:00:00Z", "endTime": "2026-04-22T07:00:00Z"}, "preemptionRate": 0.31}
  ],
  "priceHistory": [
    {"interval": {"startTime": "2026-04-27T07:00:00Z", "endTime": "2026-05-11T07:00:00Z"},
     "listPrice": {"currencyCode": "USD", "nanos": 478720000}}
  ]
}
```

`advisor/internal/advice/gcp/client_test.go`:

```go
package gcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"google.golang.org/api/option"

	"github.com/cwest/gke-spot-instance-node-pools/advisor/internal/advice"
)

func server(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var file string
		switch {
		case strings.Contains(r.URL.Path, "/advice/capacity") && !strings.Contains(r.URL.Path, "History"):
			file = "../../../testdata/capacity_response.json"
		case strings.Contains(r.URL.Path, "/advice/capacityHistory"):
			file = "../../../testdata/history_response.json"
		default:
			http.NotFound(w, r)
			return
		}
		b, err := os.ReadFile(file)
		if err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(b)
	}))
}

func newClient(t *testing.T, url string) *Client {
	t.Helper()
	c, err := New(context.Background(),
		option.WithEndpoint(url), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestCapacityNormalizesResponse(t *testing.T) {
	srv := server(t)
	defer srv.Close()
	got, err := newClient(t, srv.URL).Capacity(context.Background(), advice.CapacityQuery{
		Project: "p", Region: "us-central1",
		MachineTypes: []string{"n2-standard-2", "n2-standard-4"}, Size: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Obtainability != 0.9 || got[0].EstimatedUptimeSeconds != 600 {
		t.Fatalf("scores: %+v", got)
	}
	if len(got[0].Shards) != 2 || got[0].Shards[0].Zone != "us-central1-a" || got[0].Shards[0].Count != 90 {
		t.Fatalf("shards: %+v", got[0].Shards)
	}
}

func TestCapacityHistoryNormalizesResponse(t *testing.T) {
	srv := server(t)
	defer srv.Close()
	got, err := newClient(t, srv.URL).CapacityHistory(context.Background(), advice.HistoryQuery{
		Project: "p", Region: "us-central1", Zone: "us-central1-a", MachineType: "n2-standard-32",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []float64{0.52, 0.31}
	if len(got.DailyPreemptionRates) != 2 || got.DailyPreemptionRates[0] != want[0] {
		t.Fatalf("rates: %+v", got.DailyPreemptionRates)
	}
	if got.LatestSpotUSDPerHour != 0.47872 {
		t.Fatalf("price: %v", got.LatestSpotUSDPerHour)
	}
}
```

- [ ] **Step 4: Run test to verify it fails, then implement the client**

Run: `cd advisor && go test ./internal/advice/...`
Expected: FAIL — `undefined: New`

`advisor/internal/advice/gcp/client.go` (adjust GAPIC names per Step 2 findings):

```go
// Package gcp implements advice.API against the Compute Engine beta advice
// endpoints and the v1 regions list.
package gcp

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	computebeta "cloud.google.com/go/compute/apiv1beta"
	"cloud.google.com/go/compute/apiv1beta/computepb"
	compute "cloud.google.com/go/compute/apiv1"
	computev1pb "cloud.google.com/go/compute/apiv1/computepb"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/protobuf/proto"

	"github.com/cwest/gke-spot-instance-node-pools/advisor/internal/advice"
)

type Client struct {
	adv     *computebeta.AdviceClient
	regions *compute.RegionsClient
}

func New(ctx context.Context, opts ...option.ClientOption) (*Client, error) {
	adv, err := computebeta.NewAdviceRESTClient(ctx, opts...)
	if err != nil {
		return nil, err
	}
	rc, err := compute.NewRegionsRESTClient(ctx, opts...)
	if err != nil {
		return nil, err
	}
	return &Client{adv: adv, regions: rc}, nil
}

func (c *Client) Regions(ctx context.Context, project string) ([]string, error) {
	it := c.regions.List(ctx, &computev1pb.ListRegionsRequest{Project: project})
	var out []string
	for {
		r, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return nil, err
		}
		out = append(out, r.GetName())
	}
	return out, nil
}

func (c *Client) Capacity(ctx context.Context, q advice.CapacityQuery) ([]advice.CapacityResult, error) {
	sel := &computepb.CapacityAdviceRequestInstanceFlexibilityPolicyInstanceSelection{
		MachineTypes: q.MachineTypes,
	}
	req := &computepb.CapacityAdviceRequest{
		Project: q.Project,
		Region:  q.Region,
		CapacityAdviceRequestResource: &computepb.CapacityAdviceRequestResource{
			InstanceProperties: &computepb.CapacityAdviceRequestInstanceProperties{
				Scheduling: &computepb.CapacityAdviceRequestInstancePropertiesScheduling{
					ProvisioningModel: proto.String("SPOT"),
				},
			},
			InstanceFlexibilityPolicy: &computepb.CapacityAdviceRequestInstanceFlexibilityPolicy{
				InstanceSelections: map[string]*computepb.CapacityAdviceRequestInstanceFlexibilityPolicyInstanceSelection{
					"selection-1": sel,
				},
			},
			Size: proto.Int32(q.Size),
			DistributionPolicy: &computepb.CapacityAdviceRequestDistributionPolicy{
				TargetShape: proto.String("ANY"),
			},
		},
	}
	resp, err := c.adv.Capacity(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("advice.capacity %s/%s: %w", q.Region, strings.Join(q.MachineTypes, ","), err)
	}
	var out []advice.CapacityResult
	for _, rec := range resp.GetRecommendations() {
		r := advice.CapacityResult{
			Obtainability:          rec.GetScores().GetObtainability(),
			EstimatedUptimeSeconds: parseSeconds(rec.GetScores().GetEstimatedUptime()),
		}
		for _, s := range rec.GetShards() {
			r.Shards = append(r.Shards, advice.Shard{
				Zone:        lastSegment(s.GetZone()),
				MachineType: s.GetMachineType(),
				Count:       s.GetInstanceCount(),
			})
		}
		out = append(out, r)
	}
	return out, nil
}

func (c *Client) CapacityHistory(ctx context.Context, q advice.HistoryQuery) (*advice.HistoryResult, error) {
	req := &computepb.CapacityHistoryRequest{
		Project: q.Project,
		Region:  q.Region,
		CapacityHistoryRequestResource: &computepb.CapacityHistoryRequestResource{
			InstanceProperties: &computepb.CapacityHistoryRequestInstanceProperties{
				MachineType: proto.String(q.MachineType),
				Scheduling: &computepb.CapacityHistoryRequestInstancePropertiesScheduling{
					ProvisioningModel: proto.String("SPOT"),
				},
			},
			LocationPolicy: &computepb.CapacityHistoryRequestLocationPolicy{
				Location: proto.String("zones/" + q.Zone),
			},
			Types: []string{"PREEMPTION", "PRICE"},
		},
	}
	resp, err := c.adv.CapacityHistory(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("advice.capacityHistory %s/%s: %w", q.Zone, q.MachineType, err)
	}
	out := &advice.HistoryResult{}
	for _, p := range resp.GetPreemptionHistory() {
		out.DailyPreemptionRates = append(out.DailyPreemptionRates, p.GetPreemptionRate())
	}
	if ph := resp.GetPriceHistory(); len(ph) > 0 {
		last := ph[len(ph)-1]
		out.LatestSpotUSDPerHour = float64(last.GetListPrice().GetUnits()) +
			float64(last.GetListPrice().GetNanos())/1e9
	}
	return out, nil
}

func parseSeconds(d string) int {
	n, err := strconv.Atoi(strings.TrimSuffix(d, "s"))
	if err != nil {
		return 0
	}
	return n
}

func lastSegment(url string) string {
	parts := strings.Split(url, "/")
	return parts[len(parts)-1]
}
```

**Note:** the `computepb` type names above follow the discovery document's schema names; Step 2's `go doc` output is authoritative. If the GAPIC wraps differ (e.g., resource field named differently, `Types` being an enum slice), adapt this file only.

- [ ] **Step 5: Run tests to verify they pass**

Run: `cd advisor && go test ./internal/advice/...`
Expected: PASS (both normalization tests)

- [ ] **Step 6: Commit**

```bash
git add advisor/internal/advice/ advisor/testdata/
git commit -m "✨ feat(advisor): advice.API interface with fake and beta GAPIC client"
```

---

### Task 5: analyze orchestration

**Files:**
- Create: `advisor/internal/analyze/analyze.go`
- Test: `advisor/internal/analyze/analyze_test.go`

**Interfaces:**
- Consumes: `advice.API` (Task 4), `regions.Expand` (Task 2), `score.*` (Task 3), `machinetype.Units` (Task 3), `config.Config`/`Profile` (Task 1).
- Produces (consumed by Tasks 6–8):

```go
package analyze

type Candidate struct {
	MachineType, Zone, Region string
	Obtainability             float64
	EstimatedUptimeSeconds    int
	Daily                     []float64
	SpotHourlyUSD             float64
	Units                     float64
	UptimeFactor, PreemptionFactor, PriceFactor, Composite float64
	Dropped                   bool
	Flags                     []string // "no-history", "no-price", "low-obtainability"
}
type Analysis struct {
	GeneratedAt    time.Time
	Project        string
	Profile        string
	Kind           string // "cpu" | "gpu"
	Size           int32
	AllowedRegions []string // expanded
	Candidates     []Candidate // sorted: kept (composite desc), then dropped
}
func Run(ctx context.Context, api advice.API, cfg *config.Config, profileName string, now time.Time) (*Analysis, error)
```

- [ ] **Step 1: Write the failing test**

`advisor/internal/analyze/analyze_test.go`:

```go
package analyze

import (
	"context"
	"testing"
	"time"

	"github.com/cwest/gke-spot-instance-node-pools/advisor/internal/advice"
	"github.com/cwest/gke-spot-instance-node-pools/advisor/internal/advice/fake"
	"github.com/cwest/gke-spot-instance-node-pools/advisor/internal/config"
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
					{Zone: "us-central1-a", MachineType: "e2-standard-8", Count: 15},
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
			return &advice.HistoryResult{DailyPreemptionRates: flat(0.3, 30), LatestSpotUSDPerHour: 0.12}, nil
		default:
			return &advice.HistoryResult{}, nil // no data
		}
	}
	return f
}

func TestRun(t *testing.T) {
	a, err := Run(context.Background(), api(), cfg(), "cpu-batch", time.Unix(1753300000, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Candidates) != 3 {
		t.Fatalf("want 3 candidates, got %d: %+v", len(a.Candidates), a.Candidates)
	}
	top := a.Candidates[0]
	if top.MachineType != "e2-standard-8" || top.Zone != "us-central1-a" {
		t.Errorf("top candidate = %s/%s", top.MachineType, top.Zone)
	}
	if !a.Candidates[2].Dropped {
		t.Errorf("us-east4 candidate should be dropped (obtainability 0.3)")
	}
	if got := a.Candidates[2].Flags; len(got) == 0 {
		t.Errorf("dropped candidate should carry flags, got none")
	}
	// cheapest kept candidate has PriceFactor 1.0
	if top.PriceFactor != 1.0 {
		t.Errorf("top PriceFactor = %v, want 1.0", top.PriceFactor)
	}
}

func TestRunUnknownProfile(t *testing.T) {
	if _, err := Run(context.Background(), api(), cfg(), "nope", time.Now()); err == nil {
		t.Fatal("expected error for unknown profile")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd advisor && go test ./internal/analyze/`
Expected: FAIL — `undefined: Run`

- [ ] **Step 3: Implement**

`advisor/internal/analyze/analyze.go`:

```go
// Package analyze orchestrates advice queries into a scored Analysis.
package analyze

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/cwest/gke-spot-instance-node-pools/advisor/internal/advice"
	"github.com/cwest/gke-spot-instance-node-pools/advisor/internal/config"
	"github.com/cwest/gke-spot-instance-node-pools/advisor/internal/machinetype"
	"github.com/cwest/gke-spot-instance-node-pools/advisor/internal/regions"
	"github.com/cwest/gke-spot-instance-node-pools/advisor/internal/score"
)

type Candidate struct {
	MachineType, Zone, Region string
	Obtainability             float64
	EstimatedUptimeSeconds    int
	Daily                     []float64
	SpotHourlyUSD             float64
	Units                     float64
	UptimeFactor              float64
	PreemptionFactor          float64
	PriceFactor               float64
	Composite                 float64
	Dropped                   bool
	Flags                     []string
}

type Analysis struct {
	GeneratedAt    time.Time
	Project        string
	Profile        string
	Kind           string
	Size           int32
	AllowedRegions []string
	Candidates     []Candidate
}

const capacityBatchSize = 5 // documented API cap on machine types per request

func Run(ctx context.Context, api advice.API, cfg *config.Config, profileName string, now time.Time) (*Analysis, error) {
	profile, ok := cfg.Profiles[profileName]
	if !ok {
		return nil, fmt.Errorf("unknown profile %q", profileName)
	}
	all, err := api.Regions(ctx, cfg.Project)
	if err != nil {
		return nil, fmt.Errorf("list regions: %w", err)
	}
	allowed, err := regions.Expand(cfg.AllowedRegions, all)
	if err != nil {
		return nil, err
	}

	var cands []Candidate
	for _, region := range allowed {
		for _, batch := range chunk(profile.MachineTypes, capacityBatchSize) {
			results, err := api.Capacity(ctx, advice.CapacityQuery{
				Project: cfg.Project, Region: region,
				MachineTypes: batch, Size: profile.Size, Kind: profile.Kind,
			})
			if err != nil {
				return nil, err
			}
			for _, res := range results {
				for _, shard := range res.Shards {
					c, err := buildCandidate(ctx, api, cfg.Project, region, profile, res, shard)
					if err != nil {
						return nil, err
					}
					cands = append(cands, c)
				}
			}
		}
	}
	finalize(cands)
	return &Analysis{
		GeneratedAt: now, Project: cfg.Project, Profile: profileName,
		Kind: profile.Kind, Size: profile.Size,
		AllowedRegions: allowed, Candidates: cands,
	}, nil
}

func buildCandidate(ctx context.Context, api advice.API, project, region string,
	profile config.Profile, res advice.CapacityResult, shard advice.Shard) (Candidate, error) {

	c := Candidate{
		MachineType: shard.MachineType, Zone: shard.Zone, Region: region,
		Obtainability:          res.Obtainability,
		EstimatedUptimeSeconds: res.EstimatedUptimeSeconds,
	}
	units, err := machinetype.Units(shard.MachineType, profile.Kind)
	if err != nil {
		return c, err
	}
	c.Units = units

	hist, err := api.CapacityHistory(ctx, advice.HistoryQuery{
		Project: project, Region: region, Zone: shard.Zone, MachineType: shard.MachineType,
	})
	if err != nil {
		return c, err
	}
	c.Daily = hist.DailyPreemptionRates
	c.SpotHourlyUSD = hist.LatestSpotUSDPerHour
	if len(c.Daily) == 0 {
		c.Flags = append(c.Flags, "no-history")
	}
	if c.SpotHourlyUSD <= 0 {
		c.Flags = append(c.Flags, "no-price")
	}
	if res.EstimatedUptimeSeconds == 0 {
		c.Flags = append(c.Flags, "no-uptime")
	}
	return c, nil
}

// finalize computes cross-candidate price normalization and composites,
// then sorts: kept candidates by composite desc, dropped candidates last.
func finalize(cands []Candidate) {
	minPerUnit := 0.0
	for _, c := range cands {
		if c.SpotHourlyUSD > 0 {
			p := c.SpotHourlyUSD / c.Units
			if minPerUnit == 0 || p < minPerUnit {
				minPerUnit = p
			}
		}
	}
	for i := range cands {
		c := &cands[i]
		c.UptimeFactor = score.UptimeFactor(c.EstimatedUptimeSeconds)
		c.PreemptionFactor = score.PreemptionFactor(c.Daily)
		perUnit := 0.0
		if c.SpotHourlyUSD > 0 {
			perUnit = c.SpotHourlyUSD / c.Units
		}
		c.PriceFactor = score.PriceFactor(perUnit, minPerUnit)
		c.Composite, c.Dropped = score.Composite(c.Obtainability, c.UptimeFactor, c.PreemptionFactor, c.PriceFactor)
		if c.Dropped {
			c.Flags = append(c.Flags, "low-obtainability")
		}
	}
	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].Dropped != cands[j].Dropped {
			return !cands[i].Dropped
		}
		return cands[i].Composite > cands[j].Composite
	})
}

func chunk(xs []string, n int) [][]string {
	var out [][]string
	for len(xs) > n {
		out = append(out, xs[:n])
		xs = xs[n:]
	}
	return append(out, xs)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd advisor && go test ./internal/analyze/`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add advisor/internal/analyze/
git commit -m "✨ feat(advisor): analyze orchestration with batching and cross-candidate scoring"
```

---

### Task 6: render ComputeClass YAML (golden)

**Files:**
- Create: `advisor/internal/render/computeclass.go`
- Create: `advisor/testdata/golden/computeclass-cpu.yaml`, `advisor/testdata/golden/computeclass-gpu.yaml`
- Test: `advisor/internal/render/computeclass_test.go`

**Interfaces:**
- Consumes: `analyze.Analysis`, `analyze.Candidate` (Task 5), `config.Caps` (Task 1).
- Produces: `render.ComputeClass(a *analyze.Analysis, className string, maxRungs int) ([]byte, error)` — CPU (`a.Kind=="cpu"`): spot rungs + `whenUnsatisfiable: ScaleUpAnyway`. GPU: spot rungs + flexStart rung + `whenUnsatisfiable: DoNotScaleUp`. Rung = machine type (first-ranked order), zones = union of that type's kept candidates' zones, `priorityScore = max(1, round(1000 × composite / bestComposite))`.

- [ ] **Step 1: Write the failing golden test**

`advisor/internal/render/computeclass_test.go`:

```go
package render

import (
	"flag"
	"os"
	"path/filepath"
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

func TestComputeClassAllDropped(t *testing.T) {
	a := fixtureAnalysis("cpu")
	for i := range a.Candidates {
		a.Candidates[i].Dropped = true
	}
	if _, err := ComputeClass(a, "batch-cpu", 3); err == nil {
		t.Fatal("expected error when no candidates survive")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd advisor && go test ./internal/render/`
Expected: FAIL — `undefined: ComputeClass`

- [ ] **Step 3: Implement**

`advisor/internal/render/computeclass.go`:

```go
// Package render turns an Analysis into deployable artifacts.
package render

import (
	"bytes"
	"fmt"
	"math"
	"sort"
	"text/template"

	"github.com/cwest/gke-spot-instance-node-pools/advisor/internal/analyze"
)

type rung struct {
	MachineType string
	Zones       []string
	Score       int
	Composite   float64
}

var ccTmpl = template.Must(template.New("cc").Parse(`# Generated by capacity-advisor at {{.GeneratedAt}} — do not edit by hand.
# Profile: {{.Profile}} | project: {{.Project}} | allowed regions: {{.Regions}}
{{- range .Rungs}}
# rung {{.MachineType}} score={{.Score}} composite={{printf "%.3f" .Composite}} zones={{.Zones}}
{{- end}}
apiVersion: cloud.google.com/v1
kind: ComputeClass
metadata:
  name: {{.Name}}
spec:
  nodePoolAutoCreation:
    enabled: true
  activeMigration:
    optimizeRulePriority: true
  priorities:
{{- range .Rungs}}
  - machineType: {{.MachineType}}
    spot: true
    priorityScore: {{.Score}}
    location:
      zones: [{{range $i, $z := .Zones}}{{if $i}}, {{end}}{{$z}}{{end}}]
{{- end}}
{{- if .FlexRung}}
  - machineType: {{.FlexRung}}
    maxRunDurationSeconds: 86400
    flexStart:
      enabled: true
      nodeRecycling:
        leadTimeSeconds: 3600
{{- end}}
  whenUnsatisfiable: {{.WhenUnsatisfiable}}
`))

func ComputeClass(a *analyze.Analysis, className string, maxRungs int) ([]byte, error) {
	rungs := buildRungs(a, maxRungs)
	if len(rungs) == 0 {
		return nil, fmt.Errorf("no candidates survived scoring; refusing to render %s", className)
	}
	data := struct {
		GeneratedAt, Profile, Project, Regions, Name, WhenUnsatisfiable, FlexRung string
		Rungs                                                                     []rung
	}{
		GeneratedAt: a.GeneratedAt.Format("2006-01-02T15:04:05Z"),
		Profile:     a.Profile, Project: a.Project,
		Regions: fmt.Sprint(a.AllowedRegions),
		Name:    className, Rungs: rungs,
		WhenUnsatisfiable: "ScaleUpAnyway",
	}
	if a.Kind == "gpu" {
		data.WhenUnsatisfiable = "DoNotScaleUp"
		data.FlexRung = rungs[0].MachineType // flex-start fallback on the best shape
	}
	var buf bytes.Buffer
	if err := ccTmpl.Execute(&buf, data); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// buildRungs groups kept candidates by machine type (ranked order preserved),
// unioning zones, scoring proportionally to the best composite.
func buildRungs(a *analyze.Analysis, maxRungs int) []rung {
	var order []string
	byType := map[string]*rung{}
	best := 0.0
	for _, c := range a.Candidates {
		if c.Dropped {
			continue
		}
		if best == 0 {
			best = c.Composite
		}
		r, ok := byType[c.MachineType]
		if !ok {
			if len(order) == maxRungs {
				continue
			}
			r = &rung{MachineType: c.MachineType, Composite: c.Composite}
			byType[c.MachineType] = r
			order = append(order, c.MachineType)
		}
		r.Zones = append(r.Zones, c.Zone)
	}
	var out []rung
	for _, mt := range order {
		r := byType[mt]
		sort.Strings(r.Zones)
		r.Zones = dedup(r.Zones)
		if best > 0 {
			r.Score = int(math.Max(1, math.Round(1000*r.Composite/best)))
		} else {
			r.Score = 1
		}
		out = append(out, *r)
	}
	return out
}

func dedup(xs []string) []string {
	out := xs[:0]
	for i, x := range xs {
		if i == 0 || x != xs[i-1] {
			out = append(out, x)
		}
	}
	return out
}
```

- [ ] **Step 4: Generate goldens, inspect, then verify pass**

```bash
cd advisor && mkdir -p testdata/golden && go test ./internal/render/ -update && cat testdata/golden/computeclass-gpu.yaml
```

Inspect by eye: 2 spot rungs (scores 1000, 500), zones unioned/sorted, GPU file has the flexStart rung + `DoNotScaleUp`, CPU file has `ScaleUpAnyway` and no flexStart.

Run: `go test ./internal/render/`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add advisor/internal/render/ advisor/testdata/golden/
git commit -m "✨ feat(advisor): render ComputeClass ladders from analysis"
```

---

### Task 7: render report (markdown + JSON, golden)

**Files:**
- Create: `advisor/internal/render/report.go`
- Create: `advisor/testdata/golden/advice-report.md`
- Test: `advisor/internal/render/report_test.go`

**Interfaces:**
- Consumes: `analyze.Analysis` (Task 5).
- Produces: `render.ReportMarkdown(a *analyze.Analysis) []byte`, `render.ReportJSON(a *analyze.Analysis) ([]byte, error)` (indented `json.Marshal` of the Analysis), `render.Sparkline(rates []float64) string` (▁▂▃▄▅▆▇█ over 0–max scale, empty string for no data).

- [ ] **Step 1: Write the failing test**

`advisor/internal/render/report_test.go`:

```go
package render

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSparkline(t *testing.T) {
	if got := Sparkline(nil); got != "" {
		t.Errorf("empty: %q", got)
	}
	got := Sparkline([]float64{0, 0.25, 0.5, 1.0})
	if len([]rune(got)) != 4 || !strings.HasSuffix(got, "█") || []rune(got)[0] != '▁' {
		t.Errorf("Sparkline = %q", got)
	}
}

func TestReportMarkdownGolden(t *testing.T) {
	a := fixtureAnalysis("cpu")
	a.Candidates[0].Daily = []float64{0.1, 0.2, 0.4}
	a.Candidates[0].SpotHourlyUSD = 0.08
	a.Candidates[0].Flags = []string{"no-uptime"}
	checkGolden(t, "advice-report.md", ReportMarkdown(a))
}

func TestReportJSONRoundTrips(t *testing.T) {
	b, err := ReportJSON(fixtureAnalysis("cpu"))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if m["Profile"] != "cpu-batch" {
		t.Errorf("Profile = %v", m["Profile"])
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd advisor && go test ./internal/render/`
Expected: FAIL — `undefined: Sparkline`

- [ ] **Step 3: Implement**

`advisor/internal/render/report.go`:

```go
package render

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/cwest/gke-spot-instance-node-pools/advisor/internal/analyze"
)

var sparks = []rune("▁▂▃▄▅▆▇█")

// Sparkline renders daily preemption rates scaled 0..max.
func Sparkline(rates []float64) string {
	if len(rates) == 0 {
		return ""
	}
	max := 0.0
	for _, r := range rates {
		if r > max {
			max = r
		}
	}
	out := make([]rune, len(rates))
	for i, r := range rates {
		idx := 0
		if max > 0 {
			idx = int(r / max * float64(len(sparks)-1))
		}
		out[i] = sparks[idx]
	}
	return string(out)
}

func ReportMarkdown(a *analyze.Analysis) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "# Capacity advice — %s\n\n", a.Profile)
	fmt.Fprintf(&b, "Generated %s | project `%s` | size %d | regions %v\n\n",
		a.GeneratedAt.Format("2006-01-02T15:04:05Z"), a.Project, a.Size, a.AllowedRegions)
	fmt.Fprintf(&b, "Scores are advisory (beta API), not capacity guarantees.\n\n")
	b.WriteString("| # | machine type | zone | obtainability | uptime | preemption (30d) | spot $/hr | composite | flags |\n")
	b.WriteString("|---|---|---|---|---|---|---|---|---|\n")
	for i, c := range a.Candidates {
		mark := ""
		if c.Dropped {
			mark = " ~~dropped~~"
		}
		fmt.Fprintf(&b, "| %d | %s | %s | %.2f | %ds | %s | %.4f | %.3f%s | %s |\n",
			i+1, c.MachineType, c.Zone, c.Obtainability, c.EstimatedUptimeSeconds,
			Sparkline(c.Daily), c.SpotHourlyUSD, c.Composite, mark, flagsOrDash(c.Flags))
	}
	return b.Bytes()
}

func flagsOrDash(f []string) string {
	if len(f) == 0 {
		return "—"
	}
	return fmt.Sprint(f)
}

func ReportJSON(a *analyze.Analysis) ([]byte, error) {
	return json.MarshalIndent(a, "", "  ")
}
```

- [ ] **Step 4: Generate golden, inspect, verify pass**

```bash
cd advisor && go test ./internal/render/ -update && cat testdata/golden/advice-report.md
go test ./internal/render/
```

Expected: PASS; the markdown table shows the sparkline `▁▂█` on row 1 and `~~dropped~~` on the last row.

- [ ] **Step 5: Commit**

```bash
git add advisor/internal/render/ advisor/testdata/golden/
git commit -m "✨ feat(advisor): markdown and JSON advice reports with preemption sparklines"
```

---

### Task 8: render cluster-config.env

**Files:**
- Create: `advisor/internal/render/clusterconfig.go`
- Test: `advisor/internal/render/clusterconfig_test.go`

**Interfaces:**
- Consumes: `analyze.Analysis` (Task 5).
- Produces: `render.ClusterConfig(a *analyze.Analysis) ([]byte, error)` — env file with `PROJECT`, `REGION` (region of the top kept candidate), `ZONES` (comma-joined zones of kept candidates in that region, sorted, deduped). Error if no kept candidates.

- [ ] **Step 1: Write the failing test**

`advisor/internal/render/clusterconfig_test.go`:

```go
package render

import (
	"strings"
	"testing"
)

func TestClusterConfig(t *testing.T) {
	got, err := ClusterConfig(fixtureAnalysis("cpu"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(got)
	for _, want := range []string{"PROJECT=p\n", "REGION=us-central1\n", "ZONES=us-central1-a,us-central1-b\n"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in:\n%s", want, s)
		}
	}
}

func TestClusterConfigAllDropped(t *testing.T) {
	a := fixtureAnalysis("cpu")
	for i := range a.Candidates {
		a.Candidates[i].Dropped = true
	}
	if _, err := ClusterConfig(a); err == nil {
		t.Fatal("expected error")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd advisor && go test ./internal/render/`
Expected: FAIL — `undefined: ClusterConfig`

- [ ] **Step 3: Implement**

`advisor/internal/render/clusterconfig.go`:

```go
package render

import (
	"bytes"
	"fmt"
	"sort"

	"github.com/cwest/gke-spot-instance-node-pools/advisor/internal/analyze"
)

// ClusterConfig emits the env file infra scripts source to create the cluster
// in the advisor-chosen region.
func ClusterConfig(a *analyze.Analysis) ([]byte, error) {
	region := ""
	zoneSet := map[string]bool{}
	for _, c := range a.Candidates {
		if c.Dropped {
			continue
		}
		if region == "" {
			region = c.Region
		}
		if c.Region == region {
			zoneSet[c.Zone] = true
		}
	}
	if region == "" {
		return nil, fmt.Errorf("no kept candidates; cannot choose a region")
	}
	zones := make([]string, 0, len(zoneSet))
	for z := range zoneSet {
		zones = append(zones, z)
	}
	sort.Strings(zones)
	var b bytes.Buffer
	fmt.Fprintf(&b, "# Generated by capacity-advisor at %s\n", a.GeneratedAt.Format("2006-01-02T15:04:05Z"))
	fmt.Fprintf(&b, "PROJECT=%s\n", a.Project)
	fmt.Fprintf(&b, "REGION=%s\n", region)
	var buf bytes.Buffer
	for i, z := range zones {
		if i > 0 {
			buf.WriteString(",")
		}
		buf.WriteString(z)
	}
	fmt.Fprintf(&b, "ZONES=%s\n", buf.String())
	return b.Bytes(), nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd advisor && go test ./internal/render/`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add advisor/internal/render/
git commit -m "✨ feat(advisor): render cluster-config.env from top candidates"
```

---

### Task 9: CLI wiring + README + live smoke

**Files:**
- Create: `advisor/internal/cli/cli.go`, `advisor/cmd/capacity-advisor/main.go`, `advisor/README.md`
- Test: `advisor/internal/cli/cli_test.go`

**Interfaces:**
- Consumes: everything above.
- Produces: `cli.RunAnalyze(ctx, api advice.API, opts cli.AnalyzeOpts) error` with `AnalyzeOpts{ConfigPath, Profile, OutDir string, Render bool, Now time.Time}` — writes `analysis-<profile>.json` and, with `Render`, `computeclass-<cpu|gpu>.yaml` + `advice-report.{md,json}` + `cluster-config.env` into `OutDir`. `cli.RunRender(analysisPath, outDir string, maxRungs int) error` re-renders from a saved analysis. `main.go` wires cobra commands `analyze` (flags: `--config`, `--profile`, `--out`, `--render`) and `render` (flags: `--analysis`, `--out`) to these, constructing `gcp.New` only in `main`.

- [ ] **Step 1: Write the failing test**

`advisor/internal/cli/cli_test.go`:

```go
package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cwest/gke-spot-instance-node-pools/advisor/internal/advice"
	"github.com/cwest/gke-spot-instance-node-pools/advisor/internal/advice/fake"
)

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
		"advice-report.md", "advice-report.json", "cluster-config.env",
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd advisor && go test ./internal/cli/`
Expected: FAIL — `undefined: RunAnalyze`

- [ ] **Step 3: Implement**

`advisor/internal/cli/cli.go`:

```go
// Package cli holds testable command implementations; main.go only wires flags.
package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/cwest/gke-spot-instance-node-pools/advisor/internal/advice"
	"github.com/cwest/gke-spot-instance-node-pools/advisor/internal/analyze"
	"github.com/cwest/gke-spot-instance-node-pools/advisor/internal/config"
	"github.com/cwest/gke-spot-instance-node-pools/advisor/internal/render"
)

type AnalyzeOpts struct {
	ConfigPath string
	Profile    string
	OutDir     string
	Render     bool
	Now        time.Time
}

func RunAnalyze(ctx context.Context, api advice.API, opts AnalyzeOpts) error {
	cfg, err := config.Load(opts.ConfigPath)
	if err != nil {
		return err
	}
	a, err := analyze.Run(ctx, api, cfg, opts.Profile, opts.Now)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(opts.OutDir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(opts.OutDir, fmt.Sprintf("analysis-%s.json", opts.Profile))
	if err := os.WriteFile(path, b, 0o644); err != nil {
		return err
	}
	if !opts.Render {
		return nil
	}
	return renderAll(a, opts.OutDir, cfg.Caps.MaxSpotRungs)
}

func RunRender(analysisPath, outDir string, maxRungs int) error {
	b, err := os.ReadFile(analysisPath)
	if err != nil {
		return err
	}
	a := &analyze.Analysis{}
	if err := json.Unmarshal(b, a); err != nil {
		return fmt.Errorf("parse %s: %w", analysisPath, err)
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	return renderAll(a, outDir, maxRungs)
}

func renderAll(a *analyze.Analysis, outDir string, maxRungs int) error {
	cc, err := render.ComputeClass(a, "batch-"+a.Kind, maxRungs)
	if err != nil {
		return err
	}
	env, err := render.ClusterConfig(a)
	if err != nil {
		return err
	}
	rj, err := render.ReportJSON(a)
	if err != nil {
		return err
	}
	files := map[string][]byte{
		fmt.Sprintf("computeclass-%s.yaml", a.Kind): cc,
		"advice-report.md":   render.ReportMarkdown(a),
		"advice-report.json": rj,
		"cluster-config.env": env,
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(outDir, name), content, 0o644); err != nil {
			return err
		}
	}
	return nil
}
```

`advisor/cmd/capacity-advisor/main.go`:

```go
// capacity-advisor scores spot candidates via the GCE capacity advice APIs
// and renders GKE ComputeClass ladders.
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/cwest/gke-spot-instance-node-pools/advisor/internal/advice/gcp"
	"github.com/cwest/gke-spot-instance-node-pools/advisor/internal/cli"
)

func main() {
	root := &cobra.Command{Use: "capacity-advisor", SilenceUsage: true}

	var aOpts cli.AnalyzeOpts
	analyzeCmd := &cobra.Command{
		Use:   "analyze",
		Short: "Query the advice APIs and score spot candidates",
		RunE: func(cmd *cobra.Command, _ []string) error {
			api, err := gcp.New(cmd.Context())
			if err != nil {
				return err
			}
			aOpts.Now = time.Now().UTC()
			return cli.RunAnalyze(cmd.Context(), api, aOpts)
		},
	}
	analyzeCmd.Flags().StringVar(&aOpts.ConfigPath, "config", "advisor.yaml", "path to advisor.yaml")
	analyzeCmd.Flags().StringVar(&aOpts.Profile, "profile", "", "profile name (required)")
	analyzeCmd.Flags().StringVar(&aOpts.OutDir, "out", "out", "output directory")
	analyzeCmd.Flags().BoolVar(&aOpts.Render, "render", false, "also render artifacts")
	analyzeCmd.MarkFlagRequired("profile")

	var analysisPath, outDir string
	var maxRungs int
	renderCmd := &cobra.Command{
		Use:   "render",
		Short: "Render artifacts from a saved analysis",
		RunE: func(*cobra.Command, []string) error {
			return cli.RunRender(analysisPath, outDir, maxRungs)
		},
	}
	renderCmd.Flags().StringVar(&analysisPath, "analysis", "", "analysis JSON path (required)")
	renderCmd.Flags().StringVar(&outDir, "out", "out", "output directory")
	renderCmd.Flags().IntVar(&maxRungs, "max-rungs", 3, "max spot rungs")
	renderCmd.MarkFlagRequired("analysis")

	root.AddCommand(analyzeCmd, renderCmd)
	if err := root.ExecuteContext(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
```

Also: `cd advisor && go get github.com/spf13/cobra`.

`advisor/README.md` — write these sections (short prose, real commands): what the tool does (one paragraph), prerequisites (`roles/compute.viewer`, Compute API enabled, gcloud auth ADC), the two commands with the exact flags above, sample output listing, and a note that scores are advisory beta data.

- [ ] **Step 4: Run all tests**

Run: `cd advisor && go vet ./... && go test ./...`
Expected: all packages PASS

- [ ] **Step 5: Live smoke (manual, uses example-sandbox)**

```bash
cd advisor && go run ./cmd/capacity-advisor analyze --profile cpu-batch --config ../advisor.yaml --out ../out --render
cat ../out/advice-report.md
# Cross-check one candidate against gcloud:
gcloud beta compute advice capacity --project=example-sandbox --region="$(grep REGION ../out/cluster-config.env | cut -d= -f2)" \
  --provisioning-model=SPOT --size=20 \
  --instance-selection-machine-types=e2-standard-8 --target-distribution-shape=any
```

Expected: report ranks real candidates; gcloud obtainability matches the report's for the same machine type/region (same order of magnitude — data is live and may drift between calls). If the beta API rejects or reshapes a request, fix the `gcp` package (only) and re-run Task 4 tests.

- [ ] **Step 6: Commit**

```bash
git add advisor/
git commit -m "✨ feat(advisor): capacity-advisor CLI with analyze and render commands"
```

---

## Self-Review Notes

- **Spec coverage (Plan 1 scope):** config/params §4 (Task 1), allowedRegions expansion §4 (Task 2), scoring §4 (Task 3), API access §4/§9 (Task 4), batching + analysis §4 (Task 5), ComputeClass mapping §4 (Task 6), report §4/§6 (Task 7), cluster-config §3 (Task 8), CLI + smoke §4/§8 (Task 9). Reconcile mode, hysteresis *use* (config fields land here, logic in Plan 4), infra, and workloads are Plans 2–4 by design.
- **Node caps in rendered YAML:** deliberately deferred — the exact ComputeClass field for max nodes must be verified against the live CRD schema in Plan 2; Plan 1 renders without caps.
- **GAPIC name risk:** isolated to `internal/advice/gcp` behind the `advice.API` interface; Task 4 Step 2 verifies names before code is written; fixtures pin JSON behavior regardless.
