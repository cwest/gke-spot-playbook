# Cost Accounting Implementation Plan (Plan 2.5 of the demo roadmap)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give every act a machine-produced "pays for itself" readout (actual spot cost vs spot-discount counterfactual vs always-on counterfactual) and add cost controls (`priceExponent`, `maxHourlyUSDPerUnit`) to the advisor's scoring.

**Architecture:** A bash collector samples node inventory to CSV during runs. A new `capacity-advisor cost` subcommand joins the CSV with on-demand prices (Cloud Billing Catalog API, core+RAM composition) and spot prices (existing `advice.CapacityHistory`) and renders `cost-report.md`/`.json` showing the two multiplicative savings factors separately, then combined. Advisor scoring gains a configurable price exponent and a hard budget cap.

**Tech Stack:** Go 1.26 (advisor module; `google.golang.org/api/cloudbilling/v1`), bash + kubectl, existing fixtures/golden test conventions.

## Global Constraints

- Project `example-sandbox`; live commands with `GOOGLE_APPLICATION_CREDENTIALS` unset (common.sh handles it; ad-hoc commands use `env -u GOOGLE_APPLICATION_CREDENTIALS`).
- Cluster `spot-demo` (us-south1) is UP with Act 1 deployed; do not tear down; do not re-run the advisor's analyze against `out/` (zone drift) — Task 5's live run reuses existing artifacts.
- Composite formula becomes `obtainability² × uptime × preemption × price^priceExponent`; default `priceExponent: 1.0` MUST reproduce existing behavior bit-for-bit (existing tests/goldens unchanged unless stated).
- Budget cap: `maxHourlyUSDPerUnit` drops a candidate only when its price is KNOWN and exceeds the cap (flag `over-budget`); unknown price stays neutral + flagged `no-price`, never cap-dropped.
- Naming: collector CSV `out/cost-samples.csv`, header `ts,node,machine_type,lifecycle,compute_class`; lifecycle values `spot|on-demand`; cost artifacts `out/cost-report.md`, `out/cost-report.json`.
- GPU pricing is Plan 3 scope: `pricing` package must return a typed `ErrUnsupported` for non-{e2,n2,t2d} families; cost report surfaces such nodes as "unpriced" rather than failing.
- Commit style: Casey's conventional-commits-with-emoji; signed; no AI attribution trailers. Mermaid only for diagrams.

## File Structure

- `advisor/internal/config/config.go` — add `Scoring{PriceExponent float64; MaxHourlyUSDPerUnit float64}` (modify)
- `advisor/internal/score/score.go` — `Composite` gains exponent parameter (modify; all callers updated)
- `advisor/internal/analyze/analyze.go` — apply exponent + budget cap, `over-budget` flag (modify)
- `advisor/internal/pricing/pricing.go` — `Source` interface + machine-shape decomposition
- `advisor/internal/pricing/billing.go` — Cloud Billing Catalog implementation
- `advisor/internal/pricing/fake/fake.go` — scripted fake
- `advisor/internal/cost/cost.go` — CSV parse, usage aggregation, three-way comparison, report rendering
- `advisor/internal/cli/cli.go` + `cmd/capacity-advisor/main.go` — `cost` subcommand (modify)
- `demo/cost/collector.sh` — node sampler
- `demo/act1/runbook.md` — "Beat 3 — the bill" section + verified cost run (modify)
- `advisor/testdata/billing_skus.json`, `advisor/testdata/cost-samples.csv`, `advisor/testdata/golden/cost-report.md` — fixtures/goldens

---

### Task 1: Scoring config — priceExponent + budget cap

**Files:**
- Modify: `advisor/internal/config/config.go`, `advisor/internal/score/score.go`, `advisor/internal/analyze/analyze.go`
- Test: `advisor/internal/config/config_test.go`, `advisor/internal/score/score_test.go`, `advisor/internal/analyze/analyze_test.go` (extend all three)

**Interfaces:**
- Consumes: existing `Config`, `score.Composite`, `analyze.Run`.
- Produces: `config.Scoring{PriceExponent float64 \`yaml:"priceExponent"\`; MaxHourlyUSDPerUnit float64 \`yaml:"maxHourlyUSDPerUnit"\`}` on `Config.Scoring` (default PriceExponent 1.0 when 0; MaxHourlyUSDPerUnit 0 = disabled; validation: PriceExponent must be > 0 after defaulting, MaxHourlyUSDPerUnit must be >= 0). New signature `score.Composite(obtainability, uptime, preemption, price, priceExponent float64) (float64, bool)`. `analyze.Run` drops candidates whose known `SpotHourlyUSD/Units` exceeds the cap with flag `over-budget` (Dropped=true, Composite 0).

- [ ] **Step 1: Write the failing tests**

Append to `advisor/internal/config/config_test.go`:

```go
func TestScoringDefaultsAndValidation(t *testing.T) {
	c, err := Load(write(t, valid))
	if err != nil {
		t.Fatal(err)
	}
	if c.Scoring.PriceExponent != 1.0 {
		t.Errorf("PriceExponent default = %v, want 1.0", c.Scoring.PriceExponent)
	}
	if c.Scoring.MaxHourlyUSDPerUnit != 0 {
		t.Errorf("MaxHourlyUSDPerUnit default = %v, want 0 (disabled)", c.Scoring.MaxHourlyUSDPerUnit)
	}
	withScoring := valid + `
scoring:
  priceExponent: 2.5
  maxHourlyUSDPerUnit: 0.02
`
	c, err = Load(write(t, withScoring))
	if err != nil {
		t.Fatal(err)
	}
	if c.Scoring.PriceExponent != 2.5 || c.Scoring.MaxHourlyUSDPerUnit != 0.02 {
		t.Errorf("scoring not loaded: %+v", c.Scoring)
	}
	if _, err := Load(write(t, valid+"\nscoring:\n  priceExponent: -1\n")); err == nil {
		t.Error("negative priceExponent must be rejected")
	}
	if _, err := Load(write(t, valid+"\nscoring:\n  maxHourlyUSDPerUnit: -0.01\n")); err == nil {
		t.Error("negative maxHourlyUSDPerUnit must be rejected")
	}
}
```

Append to `advisor/internal/score/score_test.go`:

```go
func TestCompositePriceExponent(t *testing.T) {
	base, _ := Composite(0.9, 1.0, 1.0, 0.5, 1.0)
	squared, _ := Composite(0.9, 1.0, 1.0, 0.5, 2.0)
	if !almost(squared, base*0.5) {
		t.Errorf("exponent 2: got %v, want %v", squared, base*0.5)
	}
	neutral, _ := Composite(0.9, 1.0, 1.0, 1.0, 3.0)
	if !almost(neutral, 0.9*0.9) {
		t.Errorf("price 1.0 must be exponent-invariant: %v", neutral)
	}
}
```

Append to `advisor/internal/analyze/analyze_test.go`:

```go
func TestRunBudgetCap(t *testing.T) {
	c := cfg()
	c.Scoring.MaxHourlyUSDPerUnit = 0.012 // e2 at 0.08/8=0.01 passes; n2 at 0.12/8=0.015 exceeds
	a, err := Run(context.Background(), api(), c, "cpu-batch", time.Unix(1753300000, 0))
	if err != nil {
		t.Fatal(err)
	}
	var overBudget *Candidate
	for i := range a.Candidates {
		if a.Candidates[i].MachineType == "n2-standard-8" && a.Candidates[i].Zone == "us-central1-b" {
			overBudget = &a.Candidates[i]
		}
	}
	if overBudget == nil {
		t.Fatal("n2 candidate missing")
	}
	if !overBudget.Dropped {
		t.Error("over-cap candidate must be dropped")
	}
	found := false
	for _, f := range overBudget.Flags {
		if f == "over-budget" {
			found = true
		}
	}
	if !found {
		t.Errorf("missing over-budget flag: %v", overBudget.Flags)
	}
	// Candidates with unknown price (us-east4 fixture has none) must NOT be cap-dropped
	for _, cand := range a.Candidates {
		if cand.SpotHourlyUSD == 0 {
			for _, f := range cand.Flags {
				if f == "over-budget" {
					t.Errorf("unknown-price candidate cap-dropped: %+v", cand)
				}
			}
		}
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd advisor && go test ./internal/config/ ./internal/score/ ./internal/analyze/`
Expected: FAIL — `c.Scoring undefined`, `too many arguments to Composite`

- [ ] **Step 3: Implement**

`config.go`: add to `Config`:

```go
type Scoring struct {
	PriceExponent        float64 `yaml:"priceExponent"`
	MaxHourlyUSDPerUnit  float64 `yaml:"maxHourlyUSDPerUnit"`
}
```

Field `Scoring Scoring \`yaml:"scoring"\``; in `Load` defaults: `if c.Scoring.PriceExponent == 0 { c.Scoring.PriceExponent = 1.0 }`; in `validate`: reject `PriceExponent <= 0` (post-default this only catches negatives — note: a YAML `priceExponent: 0` becomes the 1.0 default by design, mirroring the hysteresis fields) and `MaxHourlyUSDPerUnit < 0`.

`score.go`: `func Composite(obtainability, uptime, preemption, price, priceExponent float64) (float64, bool)` — `return obtainability * obtainability * uptime * preemption * math.Pow(price, priceExponent), false` (keep the drop check first; add `"math"` import).

`analyze.go` in `finalize` (which needs the `*config.Scoring`): change signature to `finalize(cands []Candidate, sc config.Scoring)` (caller passes `cfg.Scoring`); before computing composite:

```go
perUnit := 0.0
if c.SpotHourlyUSD > 0 {
	perUnit = c.SpotHourlyUSD / c.Units
}
if sc.MaxHourlyUSDPerUnit > 0 && perUnit > 0 && perUnit > sc.MaxHourlyUSDPerUnit {
	c.Dropped = true
	c.Composite = 0
	c.Flags = append(c.Flags, "over-budget")
	continue
}
c.PriceFactor = score.PriceFactor(perUnit, minPerUnit)
c.Composite, c.Dropped = score.Composite(c.Obtainability, c.UptimeFactor, c.PreemptionFactor, c.PriceFactor, sc.PriceExponent)
```

(Restructure the existing loop accordingly; compute UptimeFactor/PreemptionFactor before the cap check so dropped candidates still show factors in the report.)

- [ ] **Step 4: Run the full advisor suite**

Run: `cd advisor && go vet ./... && go test ./...`
Expected: ALL PASS — including existing golden tests unchanged (default exponent 1.0 reproduces old numbers exactly).

- [ ] **Step 5: Commit**

```bash
git add advisor/ && git commit -m "✨ feat(advisor): configurable price exponent and hourly budget cap"
```

---

### Task 2: pricing package — shapes + Billing Catalog source

**Files:**
- Create: `advisor/internal/pricing/pricing.go`, `advisor/internal/pricing/billing.go`, `advisor/internal/pricing/fake/fake.go`
- Create: `advisor/testdata/billing_skus.json`
- Test: `advisor/internal/pricing/pricing_test.go`, `advisor/internal/pricing/billing_test.go`

**Interfaces:**
- Produces (consumed by Tasks 3-4):

```go
package pricing

var ErrUnsupported = errors.New("pricing: unsupported machine family")

type Shape struct{ Family string; VCPUs, MemoryGB float64 }
// ParseShape("t2d-standard-8") -> {Family:"t2d", VCPUs:8, MemoryGB:32}; e2/n2/t2d standard = 4 GB/vCPU.
// Other families (incl. g2) -> ErrUnsupported.
func ParseShape(machineType string) (Shape, error)

type Source interface {
	// OnDemandHourlyUSD returns the list price for the machine type in region.
	OnDemandHourlyUSD(ctx context.Context, region, machineType string) (float64, error)
}

func NewBillingSource(ctx context.Context, opts ...option.ClientOption) (*BillingSource, error) // implements Source
```

- `fake.New(map[string]float64)` keyed `region/machineType` implements `Source`.

- [ ] **Step 1: Write the failing shape tests**

`advisor/internal/pricing/pricing_test.go`:

```go
package pricing

import (
	"errors"
	"testing"
)

func TestParseShape(t *testing.T) {
	cases := []struct {
		mt     string
		family string
		vcpus  float64
		memGB  float64
		err    bool
	}{
		{"t2d-standard-8", "t2d", 8, 32, false},
		{"e2-standard-16", "e2", 16, 64, false},
		{"n2-standard-4", "n2", 4, 16, false},
		{"g2-standard-8", "", 0, 0, true},  // GPU pricing is Plan 3
		{"n1-standard-8", "", 0, 0, true},
		{"weird", "", 0, 0, true},
	}
	for _, tc := range cases {
		s, err := ParseShape(tc.mt)
		if tc.err {
			if !errors.Is(err, ErrUnsupported) {
				t.Errorf("%s: want ErrUnsupported, got %v", tc.mt, err)
			}
			continue
		}
		if err != nil || s.Family != tc.family || s.VCPUs != tc.vcpus || s.MemoryGB != tc.memGB {
			t.Errorf("ParseShape(%s) = %+v, %v", tc.mt, s, err)
		}
	}
}
```

- [ ] **Step 2: Verify the Billing Catalog surface, capture a fixture**

The Compute Engine service ID in the Catalog API is `services/6F81-5844-456A`. Explore the real SKU shapes once (live, read-only) to pin the description/serviceRegions filtering:

```bash
env -u GOOGLE_APPLICATION_CREDENTIALS gcloud services enable cloudbilling.googleapis.com --project example-sandbox
env -u GOOGLE_APPLICATION_CREDENTIALS curl -s -H "Authorization: Bearer $(gcloud auth application-default print-access-token)" \
  "https://cloudbilling.googleapis.com/v1/services/6F81-5844-456A/skus?pageSize=5000" > /tmp/skus_page1.json
python3 - <<'EOF'
import json
d = json.load(open('/tmp/skus_page1.json'))
for s in d.get('skus', []):
    desc = s['description']
    if 'us-south1' in s.get('serviceRegions', []) and ('T2D' in desc or 'E2 ' in desc):
        print(desc, '|', s['category']['usageType'])
EOF
```

Record: exact descriptions for `T2D AMD Instance Core running in ...` / `T2D AMD Instance Ram ...` with usageType `OnDemand` (also note the `Preemptible`/`Spot` variants exist — we exclude them), and whether pagination is needed to reach them (follow `nextPageToken` if page 1 lacks T2D for us-south1). Build `advisor/testdata/billing_skus.json` as a MINIMAL catalog page: 4 SKUs (T2D core OnDemand, T2D ram OnDemand, T2D core Preemptible — to prove exclusion — and E2 core OnDemand) for us-south1 with realistic `pricingInfo[0].pricingExpression.tieredRates[0].unitPrice` (`units`,`nanos`) values copied from the live output, preserving the real JSON structure.

- [ ] **Step 3: Write the failing billing test (fixture-driven httptest)**

`advisor/internal/pricing/billing_test.go`:

```go
package pricing

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"google.golang.org/api/option"
)

func billingServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := os.ReadFile("../../testdata/billing_skus.json")
		if err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(b)
	}))
}

func TestOnDemandHourlyUSDComposesCoreAndRam(t *testing.T) {
	srv := billingServer(t)
	defer srv.Close()
	src, err := NewBillingSource(context.Background(),
		option.WithEndpoint(srv.URL), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	got, err := src.OnDemandHourlyUSD(context.Background(), "us-south1", "t2d-standard-8")
	if err != nil {
		t.Fatal(err)
	}
	// expected = 8*corePrice + 32*ramPrice from the fixture values —
	// compute the constant when building the fixture and assert exactly.
	want := expectedT2D8FromFixture // defined in this test file next to the fixture values
	if diff := got - want; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("t2d-standard-8 = %v, want %v", got, want)
	}
}

func TestOnDemandUnsupportedFamily(t *testing.T) {
	srv := billingServer(t)
	defer srv.Close()
	src, _ := NewBillingSource(context.Background(),
		option.WithEndpoint(srv.URL), option.WithoutAuthentication())
	if _, err := src.OnDemandHourlyUSD(context.Background(), "us-south1", "g2-standard-8"); err == nil {
		t.Error("want error for unsupported family")
	}
}
```

(Define `expectedT2D8FromFixture` as a `const` with the hand-computed value once fixture prices are pinned in Step 2; a comment shows the arithmetic.)

- [ ] **Step 4: Implement**

`pricing.go`:

```go
// Package pricing resolves machine-type list prices.
package pricing

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

var ErrUnsupported = errors.New("pricing: unsupported machine family")

type Shape struct {
	Family   string
	VCPUs    float64
	MemoryGB float64
}

// gbPerVCPU for the standard shapes this demo prices. GPU families are
// priced in Plan 3 (GPU SKUs are separate line items).
var gbPerVCPU = map[string]float64{"e2": 4, "n2": 4, "t2d": 4}

func ParseShape(machineType string) (Shape, error) {
	parts := strings.Split(machineType, "-")
	if len(parts) != 3 || parts[1] != "standard" {
		return Shape{}, fmt.Errorf("%w: %s", ErrUnsupported, machineType)
	}
	ratio, ok := gbPerVCPU[parts[0]]
	if !ok {
		return Shape{}, fmt.Errorf("%w: %s", ErrUnsupported, machineType)
	}
	n, err := strconv.Atoi(parts[2])
	if err != nil || n <= 0 {
		return Shape{}, fmt.Errorf("%w: %s", ErrUnsupported, machineType)
	}
	return Shape{Family: parts[0], VCPUs: float64(n), MemoryGB: float64(n) * ratio}, nil
}

type Source interface {
	OnDemandHourlyUSD(ctx context.Context, region, machineType string) (float64, error)
}
```

`billing.go`: `NewBillingSource` wraps `cloudbilling.NewService(ctx, opts...)`; `OnDemandHourlyUSD`:
1. `ParseShape`; propagate `ErrUnsupported`.
2. List SKUs for `services/6F81-5844-456A` (follow `nextPageToken`; cache the filtered result per region+family in the struct so repeated calls don't re-list).
3. Select SKUs where: `category.resourceFamily == "Compute"`, `category.usageType == "OnDemand"`, `serviceRegions` contains the region, description contains the family marker (`"T2D AMD Instance Core"` / `"T2D AMD Instance Ram"` — build the marker map per family from Step 2's recorded descriptions: e2 → `"E2 Instance Core"` / `"E2 Instance Ram"`, n2 → `"N2 Instance Core"` / `"N2 Instance Ram"`), and description does NOT contain `"Sole Tenancy"`, `"Custom"`, `"Commitment"`.
4. Price from `pricingInfo[0].pricingExpression.tieredRates[0].unitPrice`: `float64(units) + float64(nanos)/1e9`.
5. Return `shape.VCPUs*corePrice + shape.MemoryGB*ramPrice`; error naming which SKU (core/ram) was not found.

`fake/fake.go`:

```go
// Package fake is a scripted pricing.Source for tests.
package fake

import (
	"context"
	"fmt"
)

type Source struct{ Prices map[string]float64 } // key: region + "/" + machineType

func New(prices map[string]float64) *Source { return &Source{Prices: prices} }

func (s *Source) OnDemandHourlyUSD(_ context.Context, region, machineType string) (float64, error) {
	p, ok := s.Prices[region+"/"+machineType]
	if !ok {
		return 0, fmt.Errorf("fake: no price for %s/%s", region, machineType)
	}
	return p, nil
}
```

Dependency: `cd advisor && go get google.golang.org/api/cloudbilling/v1`.

- [ ] **Step 5: Run tests, commit**

Run: `cd advisor && go vet ./... && go test ./internal/pricing/...` → PASS; full `go test ./...` → PASS.

```bash
git add advisor/ && git commit -m "✨ feat(advisor): billing-catalog pricing source with shape decomposition"
```

---

### Task 3: collector script

**Files:**
- Create: `demo/cost/collector.sh`

**Interfaces:**
- Produces: CSV at `out/cost-samples.csv` (or `$1` if given), header `ts,node,machine_type,lifecycle,compute_class`; `ts` RFC3339 UTC; `lifecycle` `spot` when node label `cloud.google.com/gke-spot=true` else `on-demand`; `compute_class` from label `cloud.google.com/compute-class` or `-`; sample interval `COLLECT_INTERVAL` (default 30s); stops cleanly on SIGINT/SIGTERM.

- [ ] **Step 1: Write `demo/cost/collector.sh`**

```bash
#!/usr/bin/env bash
# Samples cluster node inventory to CSV for the cost report.
# Usage: collector.sh [output.csv]   (Ctrl-C to stop)
set -euo pipefail
# shellcheck disable=SC1091
source "$(dirname "${BASH_SOURCE[0]}")/../../infra/lib/common.sh"
spotdemo::init
spotdemo::require kubectl

OUT="${1:-${SPOTDEMO_ROOT}/out/cost-samples.csv}"
INTERVAL="${COLLECT_INTERVAL:-30}"
mkdir -p "$(dirname "${OUT}")"
[[ -s "${OUT}" ]] || echo "ts,node,machine_type,lifecycle,compute_class" > "${OUT}"

running=1
trap 'running=0' INT TERM
spotdemo::log "collecting to ${OUT} every ${INTERVAL}s (Ctrl-C to stop)"
while (( running )); do
  ts="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  kubectl get nodes -o json 2>/dev/null | python3 -c '
import json, sys
ts = sys.argv[1]
for n in json.load(sys.stdin).get("items", []):
    labels = n["metadata"].get("labels", {})
    mt = labels.get("node.kubernetes.io/instance-type", "unknown")
    life = "spot" if labels.get("cloud.google.com/gke-spot") == "true" else "on-demand"
    cc = labels.get("cloud.google.com/compute-class", "-")
    print(f"{ts},{n[\"metadata\"][\"name\"]},{mt},{life},{cc}")
' "${ts}" >> "${OUT}" || spotdemo::log "sample at ${ts} failed (kubectl error) — continuing"
  for _ in $(seq "${INTERVAL}"); do (( running )) || break; sleep 1; done
done
spotdemo::log "collector stopped; $(( $(wc -l < "${OUT}") - 1 )) samples in ${OUT}"
```

- [ ] **Step 2: Verify**

`shellcheck demo/cost/collector.sh` → clean; `chmod +x demo/cost/collector.sh`.
Live (idle cluster, 1 default node): `COLLECT_INTERVAL=5 timeout 12 bash demo/cost/collector.sh /tmp/cc-test.csv || true; cat /tmp/cc-test.csv` → header + 2-3 rows: `...,e2-standard-4,on-demand,-`.

- [ ] **Step 3: Commit**

```bash
git add demo/cost/ && git commit -m "✨ feat(demo): node inventory collector for cost accounting"
```

---

### Task 4: cost engine + `capacity-advisor cost` subcommand

**Files:**
- Create: `advisor/internal/cost/cost.go`
- Create: `advisor/testdata/cost-samples.csv`, `advisor/testdata/golden/cost-report.md`
- Modify: `advisor/internal/cli/cli.go`, `advisor/cmd/capacity-advisor/main.go`
- Test: `advisor/internal/cost/cost_test.go`, `advisor/internal/cli/cli_test.go` (extend)

**Interfaces:**
- Consumes: `pricing.Source` (Task 2), `advice.API.CapacityHistory` (spot $ per machine type+zone — regional pricing; pass the cluster region's any-zone: use zone = region+"-a"? NO — samples don't carry zones. Decision locked: spot price lookup uses `HistoryQuery{Region: region, Zone: "", MachineType: mt}` — `advice.CapacityHistory` with empty Zone means region-aggregated per the API's locationPolicy-optional behavior; the gcp client's `LocationPolicy` block must be omitted when `q.Zone == ""` — small modify in `advisor/internal/advice/gcp/client.go` guarded by a client test with a no-location fixture assertion).
- Produces:

```go
package cost

type Usage struct {
	MachineType string
	Lifecycle   string // "spot" | "on-demand"
	NodeSeconds float64
	Nodes       int     // distinct nodes observed
	PeakNodes   int     // max concurrent in any sample
}
type Report struct {
	GeneratedAt        time.Time
	Region             string
	WindowSeconds      float64
	Usages             []Usage
	ActualUSD          float64            // Σ nodeSeconds × actual rate
	SpotAtOnDemandUSD  float64            // same node-seconds, on-demand rates
	AlwaysOnUSD        float64            // peak-sized on-demand pool × window
	AlwaysOnDailyUSD   float64            // extrapolated 24h
	SpotDiscountPct    float64            // 1 - Actual/SpotAtOnDemand
	DutyCyclePct       float64            // 1 - SpotAtOnDemand/AlwaysOn
	CombinedSavingsPct float64            // 1 - Actual/AlwaysOn
	Unpriced           []string           // machine types we couldn't price
}
func ParseSamples(r io.Reader, interval time.Duration) ([]Usage, float64, error) // usages, windowSeconds
func Build(ctx context.Context, usages []Usage, window float64, region string, onDemand pricing.Source, spot SpotSource, now time.Time) (*Report, error)
type SpotSource interface{ SpotHourlyUSD(ctx context.Context, region, machineType string) (float64, error) }
func RenderMarkdown(rep *Report) []byte
func RenderJSON(rep *Report) ([]byte, error)
```

- `cli.RunCost(ctx, opts CostOpts) error` with `CostOpts{SamplesPath, Region, OutDir string, Interval time.Duration, OnDemand pricing.Source, Spot cost.SpotSource, Now time.Time}`; `main.go` gains `cost` command (flags `--samples` default `out/cost-samples.csv`, `--region` required, `--out` default `out`, `--interval` default `30s`) constructing the real sources (BillingSource + an adapter over `gcp.Client.CapacityHistory` picking `LatestSpotUSDPerHour`).
- Node-seconds accounting rule (locked): each sample row = `interval` seconds of that node's life; `NodeSeconds = rows × interval`; `WindowSeconds = (lastTs − firstTs) + interval`; `PeakNodes` = max rows sharing one `ts` per (machineType, lifecycle); `AlwaysOnUSD = Σ over machine types (peak concurrent nodes across BOTH lifecycles × on-demand rate × window)` — the standard-way counterfactual provisions peak capacity on-demand, always on.

- [ ] **Step 1: Build the test fixture**

`advisor/testdata/cost-samples.csv` — hand-written, interval 30s, window 120s (5 ts values), telling this story: 1 on-demand e2-standard-4 present in all 5 samples (the default pool); t2d-standard-8 spot nodes ramping 0→2→4→4→2 across the 5 timestamps (12 spot rows total). Exact rows enumerated in the fixture (ts values `2026-07-24T06:00:00Z` + 30s steps; node names `default-1`, `spot-a..d`).

- [ ] **Step 2: Write the failing tests**

`advisor/internal/cost/cost_test.go` asserts (constants hand-computed from the fixture + fake prices; show arithmetic in comments):

```go
// fake prices: on-demand e2-standard-4 = 0.150, t2d-standard-8 = 0.338 $/hr
// spot t2d-standard-8 = 0.0554 $/hr (e2 spot unused: default pool is on-demand)
// spot usage: 12 rows × 30s = 360 node-seconds; e2: 5 × 30s = 150
// ActualUSD      = 360/3600×0.0554 + 150/3600×0.150            = 0.011790
// SpotAtOnDemand = 360/3600×0.338  + 150/3600×0.150            = 0.040050
// AlwaysOn: peaks — t2d 4, e2 1; window = 150s
//                = (4×0.338 + 1×0.150) × 150/3600              = 0.062583…
```

Tests: `TestParseSamples` (usages: t2d/spot NodeSeconds 360, Nodes 4, PeakNodes 4; e2/on-demand 150/1/1; window 150), `TestBuildReportMath` (the three USD totals and the three pct fields to 1e-6), `TestBuildUnpricedFamily` (add a `g2-standard-8,spot` row variant in-memory: report gains `Unpriced: ["g2-standard-8"]`, totals exclude it, no error), `TestRenderMarkdownGolden` (golden `cost-report.md` — shows the two factors separately then combined; `checkGolden` helper pattern from the render package, local copy since packages differ), plus in `cli_test.go`: `TestRunCostWritesArtifacts` using fakes end-to-end (cost-report.md + .json exist, JSON round-trips with CombinedSavingsPct > 0).

- [ ] **Step 3: Run tests to verify they fail**

Run: `cd advisor && go test ./internal/cost/ ./internal/cli/`
Expected: FAIL — `undefined: ParseSamples` etc.

- [ ] **Step 4: Implement**

`cost.go` per the Interfaces block. Parsing: `encoding/csv`, skip header, reject unknown lifecycle values, rows keyed by (machineType, lifecycle); Build: resolve rates (on-demand for every machine type; spot rate only for lifecycles=="spot" rows; on-demand-lifecycle rows use on-demand rate in ActualUSD too), `errors.Is(err, pricing.ErrUnsupported)` → append machine type to `Unpriced` once and skip its rows from all totals; percentages guarded against zero denominators. RenderMarkdown layout:

```markdown
# Cost report — {region}

Generated {ts} | observation window {m}m{s}s

## What this run actually cost
| machine type | lifecycle | node-hours | rate $/hr | cost |
...
**Actual: ${ActualUSD}**

## Factor 1 — the spot discount
Same node-hours at on-demand list: ${SpotAtOnDemandUSD} → spot saved {SpotDiscountPct}%

## Factor 2 — the duty cycle
Always-on peak-sized on-demand pool for this window: ${AlwaysOnUSD}
(≈ ${AlwaysOnDailyUSD}/day if left running) → elasticity saved {DutyCyclePct}%

## Combined
{SpotDiscountPct}% × duty cycle → **{CombinedSavingsPct}% vs the standard way**
{unpriced warning if any}
```

`gcp` client modify: in `CapacityHistory`, only set `LocationPolicy` when `q.Zone != ""`; add fixture-driven test asserting the request body omits locationPolicy for empty zone (assert via httptest handler capturing the request body). SpotSource adapter in `main.go`:

```go
type spotViaAdvice struct{ api advice.API }

func (s spotViaAdvice) SpotHourlyUSD(ctx context.Context, region, machineType string) (float64, error) {
	h, err := s.api.CapacityHistory(ctx, advice.HistoryQuery{Project: project, Region: region, MachineType: machineType})
	if err != nil {
		return 0, err
	}
	if h.LatestSpotUSDPerHour <= 0 {
		return 0, fmt.Errorf("no spot price for %s in %s", machineType, region)
	}
	return h.LatestSpotUSDPerHour, nil
}
```

(place the adapter in `internal/cli` so `main.go` stays wiring-only; `project` threaded via CostOpts — add `Project string` to CostOpts.)

- [ ] **Step 5: Run full suite, generate golden, commit**

```bash
cd advisor && go test ./internal/cost/ -update && go vet ./... && go test ./...
git add advisor/ && git commit -m "✨ feat(advisor): cost subcommand with spot-discount and duty-cycle factors"
```

---

### Task 5: Act 1 "the bill" — runbook beat + verified live cost run

**Files:**
- Modify: `demo/act1/runbook.md`

**Interfaces:** consumes everything above; produces recorded cost evidence.

- [ ] **Step 1: Add "Beat 3 — the bill" section** to the runbook (before "Verified run"): start `demo/cost/collector.sh` in the background before publishing, `kill` it after the drain, then:

```bash
cd advisor && env -u GOOGLE_APPLICATION_CREDENTIALS go run ./cmd/capacity-advisor cost \
  --region us-south1 --samples ../out/cost-samples.csv --out ../out && cat ../out/cost-report.md
```

- [ ] **Step 2: Live verified cost run** (cluster is up, Act 1 deployed): collector start → publisher with `--count 2000` (smaller re-run; same mechanics) → wait for drain + scale-down toward 0 → collector stop → cost subcommand. Expected: report shows t2d spot node-hours at ~$0.0554/hr, both factors, combined savings >95% for the burst. If the Billing Catalog SKU filtering misbehaves live, fix the marker map in `billing.go` (fixtures updated to match reality), keep tests green, document.

- [ ] **Step 3: Append the real `cost-report.md` content** into the runbook's "Verified run" area under a `#### Cost readout (2000-task re-run)` heading, with one sentence tying the two incentives together (cost + the advisor having picked this hardware for obtainability).

- [ ] **Step 4: Commit**

```bash
git add demo/act1/runbook.md && git commit -m "🧪 test(act1): record verified live cost readout"
```

---

## Self-Review Notes

- **Spec coverage:** §6 cost accounting (Tasks 3-5), advisor cost controls (Task 1), pricing sources exactly as spec names them (Task 2: Billing Catalog for on-demand; capacityHistory for spot — Task 4 adapter), "the bill" beat (Task 5). Billing-export non-goal respected.
- **Locked decisions the implementer must not re-open:** node-seconds = rows × interval; AlwaysOn = peak concurrent × on-demand × window; unknown-price never cap-dropped; g2 pricing deferred with typed ErrUnsupported.
- **Type consistency:** `CostOpts{SamplesPath, Region, Project, OutDir, Interval, OnDemand, Spot, Now}`; `SpotSource` lives in `cost` package, adapter in `cli`; collector CSV header matches `ParseSamples` expectations verbatim.
