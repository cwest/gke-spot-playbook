# Act 7 — Obtainability under GPU scarcity — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Turn the existing capacity-probe machinery toward the A100 (`a2-highgpu-1g`) so the advisor proves obtainability under GPU scarcity — probe-confirmed widening and stockout-driven region failover — captured LIVE.

**Architecture:** Near-zero new subsystem. The A100 becomes a valid *candidate shape* (`machinetype` learns the `a2` GPU family; a `gpu-a100` config profile flows through `analyze` → `render`), after which the shape-agnostic probe gate (`gateWidenPromote`/`gateWiden` in `reconcile/probe.go`) probes A100 candidates for free. The only guaranteed production change is the `machinetype` map; everything else is test coverage, config/fixtures, and a LIVE runbook.

**Tech Stack:** Go (advisor, TDD-with-fakes in the existing package style), YAML config (`advisor.yaml` + a demo config), GCE spot VMs via the `probe` subcommand, GKE `spot-demo` cluster for LIVE evidence, `make test` (Go + shellcheck + kubeconform).

## Global Constraints

- **No remote.** Never `git push`/`git pull` at any step — the repo has no remote.
- **Evidence-ledger boundary (Plan 4 invariant):** a probe outcome MUST NOT write `st.Ledger`. A failed probe is the *absence* of a confirmation, never a penalty.
- **Do not re-wire the probe.** Reuse `launchProbe`/`reapProbes`/`gateWidenPromote`/`gateWiden` and the constants `probesPerDay=6`, `probesPerTick=1`, `probeConfirmTTL=30m` verbatim.
- **A100 is integral to `a2`:** probing `a2-highgpu-1g` needs **no** `GuestAccelerators` field — naming the machine type allocates the A100.
- **A100 machine:** `a2-highgpu-1g` (1×A100 40GB) only, single-GPU. Larger `a2` shapes may exist in the units map for correctness but are not exercised.
- **Region ladder:** `us-central1`, `us-east1`, `europe-west4`; ladder order is emergent from the advisor's live composite score, not hardcoded.
- **Commits:** signed (SSH, already configured); Conventional-Commits + emoji style (`✨ feat(act7): …`, `🧪 test(act7): …`, `📝 docs(act7): …`); no self-attribution/co-author trailers.
- **LIVE evidence:** every runbook number tagged `LIVE-ORGANIC`, `LIVE-FORCED`, or `PROJECTED`; re-provoke on the cluster; tear down demo infra after each run (standing consent). Use `env -u GOOGLE_APPLICATION_CREDENTIALS` for ADC on `example-sandbox`.
- **Diagrams:** Mermaid only, never ASCII.

---

### Task 1: `machinetype` learns the `a2` (A100) GPU family

**Files:**
- Modify: `advisor/internal/machinetype/units.go`
- Test: `advisor/internal/machinetype/units_test.go`

**Interfaces:**
- Consumes: nothing new.
- Produces: `machinetype.Units("a2-highgpu-1g", "gpu")` returns `(1, nil)` (and the rest of the `a2-highgpu` family), so `analyze.buildCandidate` no longer fails fatally on an A100 shape.

- [ ] **Step 1: Add failing test cases** — extend the table in `TestUnits`:

```go
{"a2-highgpu-1g", "gpu", 1, false},
{"a2-highgpu-2g", "gpu", 2, false},
{"a2-highgpu-8g", "gpu", 8, false},
{"a2-megagpu-16g", "gpu", 16, false},
```

> NOTE: `a2` shapes are only ever used with `kind: gpu` (the `gpu-a100` profile),
> so do NOT add an `a2` case to the `cpu` path and do NOT modify the existing
> vCPU parser — `"1g"` is a GPU count, not a vCPU count. The cpu branch stays as
> the original `strconv.Atoi(parts[last])`.

- [ ] **Step 2: Run test to verify it fails**

Run: `cd advisor && go test ./internal/machinetype/ -run TestUnits -v`
Expected: FAIL — `Units("a2-highgpu-1g","gpu")` returns `0, error "unknown GPU shape"`.

- [ ] **Step 3: Add the `a2` shapes to the GPU map** — in `units.go`, extend the map (A100-per-instance, mirroring the `g2` treatment):

```go
// a2 (A100 40GB) shapes -> attached NVIDIA A100 count. Unlike n1+attached GPUs,
// the A100 is integral to the a2 machine type, so a probe needs no accelerator
// field to allocate one. Only a2-highgpu-1g is exercised by Act 7; the rest are
// here for unit correctness.
var a2GPUs = map[string]float64{
	"a2-highgpu-1g": 1, "a2-highgpu-2g": 2, "a2-highgpu-4g": 4,
	"a2-highgpu-8g": 8, "a2-megagpu-16g": 16,
}
```

Then in `Units`, after the `g2GPUs` lookup in the `kind == "gpu"` branch, also consult `a2GPUs`:

```go
	if kind == "gpu" {
		if n, ok := g2GPUs[machineType]; ok {
			return n, nil
		}
		if n, ok := a2GPUs[machineType]; ok {
			return n, nil
		}
		return 0, fmt.Errorf("unknown GPU shape %q", machineType)
	}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd advisor && go test ./internal/machinetype/ -run TestUnits -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add advisor/internal/machinetype/units.go advisor/internal/machinetype/units_test.go
git commit -S -m "✨ feat(act7): machinetype learns the a2 (A100) GPU family"
```

---

### Task 2: `analyze` produces scored candidates for an A100 profile

Proves the end-to-end pipeline (`Run` → `buildCandidate` → `machinetype.Units`) handles an `a2` GPU profile. Before Task 1 this failed fatally (`buildCandidate` returns the unknown-shape error, which `Run` propagates as fatal); this test guards that A100 is now a first-class candidate.

**Files:**
- Test: `advisor/internal/analyze/analyze_test.go`

**Interfaces:**
- Consumes: `machinetype.Units` A100 support (Task 1); `advice/fake.API`.
- Produces: confidence that `analyze.Run(..., "gpu-a100", ...)` yields kept candidates with `Units == 1`.

- [ ] **Step 1: Write the failing test** — append to `analyze_test.go`:

```go
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
```

- [ ] **Step 2: Run test to verify it passes** (Task 1 already made A100 resolvable)

Run: `cd advisor && go test ./internal/analyze/ -run TestRunScoresA100Profile -v`
Expected: PASS. (If it FAILS with an unknown-shape error, Task 1 is incomplete — fix there, do not special-case here.)

- [ ] **Step 3: Sanity-check the RED direction** — confirm the test is meaningful by temporarily commenting the `a2GPUs` lookup in `units.go`, re-running (Expected: FAIL with unknown-shape), then restoring it. (No commit of the revert.)

- [ ] **Step 4: Commit**

```bash
git add advisor/internal/analyze/analyze_test.go
git commit -S -m "🧪 test(act7): analyze scores an A100 (a2-highgpu-1g) profile end to end"
```

---

### Task 3: probe-gating covers A100 (stockout never writes the ledger; A100 earns a probe)

Extends the probe-gate coverage to a GPU/A100 shape and re-asserts the Plan-4 invariant for it. Both tests are expected to pass with **zero production change** (the gate is shape-agnostic); if either fails, apply a *targeted* fix in `reconcile/probe.go` and note it.

**Files:**
- Test: `advisor/internal/reconcile/probe_test.go`

**Interfaces:**
- Consumes: `reapProbes`, `launchProbe`, `confirmed`, `probe/fake.Compute`, `config.Probe` (all existing).
- Produces: guard that A100 probing preserves the ledger boundary and that an A100 candidate is probed.

- [ ] **Step 1: Write the failing tests** — append to `probe_test.go`:

```go
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
```

- [ ] **Step 2: Run tests**

Run: `cd advisor && go test ./internal/reconcile/ -run 'A100' -v`
Expected: PASS (gate is shape-agnostic). If FAIL, `superpowers:systematic-debugging` → targeted fix in `reconcile/probe.go`, then re-run.

- [ ] **Step 3: Commit**

```bash
git add advisor/internal/reconcile/probe_test.go
git commit -S -m "🧪 test(act7): probe-gating covers A100 (stockout keeps the ledger boundary)"
```

---

### Task 4: pin the "A100 is integral to a2 — no accelerator field" expectation

Guards the assumption the whole act rests on: `probe/gce`'s `instanceFor` allocates an A100 by naming `a2-highgpu-1g`, with no `GuestAccelerators`. A future refactor that "helpfully" adds accelerator wiring (which would then be *wrong* for a2 and break L4/g2) fails this test.

**Files:**
- Test: `advisor/internal/probe/gce/gce_test.go`

**Interfaces:**
- Consumes: `instanceFor` (package-internal) and `probe.Request`.
- Produces: a regression guard on the instance shape.

- [ ] **Step 1: Read the existing test file** to confirm `instanceFor` is reachable in-package and mirror the existing assertion style.

Run: `sed -n '1,40p' advisor/internal/probe/gce/gce_test.go`

- [ ] **Step 2: Write the failing test** — add to `gce_test.go` (same `package gce`):

```go
func TestInstanceForA100NeedsNoAccelerator(t *testing.T) {
	inst := instanceFor(probe.Request{
		Project: "p", Zone: "us-central1-a", MachineType: "a2-highgpu-1g",
		Name: "probe-a100", MaxRunSeconds: 300,
	})
	if got := inst.GetMachineType(); got != "zones/us-central1-a/machineTypes/a2-highgpu-1g" {
		t.Errorf("machineType = %q, want the a2-highgpu-1g zonal URL", got)
	}
	// The A100 is integral to a2: no GuestAccelerators must be set, or the insert
	// would double-specify the GPU and fail.
	if len(inst.GetGuestAccelerators()) != 0 {
		t.Errorf("a2 probe must set no GuestAccelerators, got %v", inst.GetGuestAccelerators())
	}
	if inst.GetScheduling().GetProvisioningModel() != "SPOT" {
		t.Errorf("probe must be SPOT, got %q", inst.GetScheduling().GetProvisioningModel())
	}
}
```

- [ ] **Step 3: Run test**

Run: `cd advisor && go test ./internal/probe/gce/ -run TestInstanceForA100NeedsNoAccelerator -v`
Expected: PASS (no accelerator field exists today). If the test cannot see `instanceFor`, confirm the file declares `package gce` (it does) — `instanceFor` is package-internal and visible.

- [ ] **Step 4: Commit**

```bash
git add advisor/internal/probe/gce/gce_test.go
git commit -S -m "🧪 test(act7): pin a2/A100 probe needs no GuestAccelerators"
```

---

### Task 5: add the `gpu-a100` profile and the demo A100-ladder config

Makes A100 a first-class profile in the committed `advisor.yaml`, and adds a demo-tailored config with the full three-region ladder + automated probing for the LIVE run. `allowedRegions` is global, so the cross-region (incl. `europe-west4`) ladder lives in the demo config, not the reconciler default.

**Files:**
- Modify: `advisor.yaml`
- Create: `demo/act7/advisor-a100.yaml`
- Test: `advisor/internal/config/config_test.go`

**Interfaces:**
- Consumes: `config.Load` (existing).
- Produces: a loadable `gpu-a100` profile and a validated demo ladder config.

- [ ] **Step 1: Write the failing test** — append to `config_test.go` (loads the real files by repo-relative path from the `config` package dir):

```go
func TestDemoA100ConfigLoads(t *testing.T) {
	c, err := Load("../../../demo/act7/advisor-a100.yaml")
	if err != nil {
		t.Fatalf("demo A100 config must load: %v", err)
	}
	p, ok := c.Profiles["gpu-a100"]
	if !ok {
		t.Fatal("demo config must define the gpu-a100 profile")
	}
	if p.Kind != "gpu" || len(p.MachineTypes) == 0 || p.MachineTypes[0] != "a2-highgpu-1g" {
		t.Fatalf("gpu-a100 profile = %+v, want kind gpu with a2-highgpu-1g", p)
	}
	want := map[string]bool{"us-central1": false, "us-east1": false, "europe-west4": false}
	for _, r := range c.AllowedRegions {
		if _, ok := want[r]; ok {
			want[r] = true
		}
	}
	for r, seen := range want {
		if !seen {
			t.Errorf("A100 ladder missing region %q (allowedRegions=%v)", r, c.AllowedRegions)
		}
	}
	if !c.Probe.Automated {
		t.Error("demo A100 config must enable automated probing (the act's whole point)")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd advisor && go test ./internal/config/ -run TestDemoA100ConfigLoads -v`
Expected: FAIL — `demo/act7/advisor-a100.yaml` does not exist.

- [ ] **Step 3: Create the demo config** — `demo/act7/advisor-a100.yaml`:

```yaml
# Act 7 — obtainability under GPU scarcity. A100 (a2-highgpu-1g) across a
# three-region ladder with automated live probing. The ladder ORDER is emergent
# from the advisor's composite score (which weights spot $/unit); this file only
# declares the candidate regions. NETWORK/SUBNET must match the reconciler's VPC
# or a wrong network reads as a false stockout. Fill in the full resource paths
# for the live run.
project: example-sandbox
allowedRegions: ["us-central1", "us-east1", "europe-west4"]
profiles:
  gpu-a100:
    kind: gpu
    machineTypes: [a2-highgpu-1g]
    size: 1
scoring:
  priceExponent: 1.0
  maxHourlyUSDPerUnit: 0 # 0 = disabled
evidence:
  halfLifeMinutes: 30
  floor: 0.05
  dryBelow: 0.5
  maxAgeHours: 6
  fastPathTicks: 1
probe:
  enabled: true
  automated: true
  maxRunDurationSeconds: 300
  # network: projects/example-sandbox/global/networks/default
  # subnet:  projects/example-sandbox/regions/us-central1/subnetworks/default
```

- [ ] **Step 4: Add the `gpu-a100` profile to the committed `advisor.yaml`** — under `profiles:`, after `gpu-batch`:

```yaml
  gpu-a100:
    kind: gpu
    machineTypes: [a2-highgpu-1g]
    size: 1
```

- [ ] **Step 5: Run the test to verify it passes**

Run: `cd advisor && go test ./internal/config/ -run TestDemoA100ConfigLoads -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add advisor.yaml demo/act7/advisor-a100.yaml advisor/internal/config/config_test.go
git commit -S -m "✨ feat(act7): gpu-a100 profile + demo A100 three-region ladder config"
```

---

### Task 6: Act 7 runbook skeleton

Creates the LIVE evidence artifact with the beats and tag legend, mirroring `demo/act6/runbook.md`. Numbers are `PROJECTED` placeholders now; Task 7 replaces them with `LIVE-*` values.

**Files:**
- Create: `demo/act7/runbook.md`
- Reference (read first): `demo/act6/runbook.md`

**Interfaces:**
- Consumes: nothing.
- Produces: the runbook Task 7 fills in.

- [ ] **Step 1: Read `demo/act6/runbook.md`** to match its beat/tag structure.

Run: `sed -n '1,60p' demo/act6/runbook.md`

- [ ] **Step 2: Write `demo/act7/runbook.md`** with: a header + tag legend (`LIVE-ORGANIC` / `LIVE-FORCED` / `PROJECTED`); a prerequisites section (A100 spot quota check); and four beat sections matching the spec §6 (quota check, obtainable probe, stockout→failover, ledger-boundary assertion), each with a `PROJECTED` result line and the exact command to run. Include the control-flow Mermaid diagram from the spec §5. Keep every not-yet-run number tagged `PROJECTED`.

- [ ] **Step 3: Confirm the only diagram is a ```mermaid``` block** (no ASCII), per the diagram constraint.

- [ ] **Step 4: Commit**

```bash
git add demo/act7/runbook.md
git commit -S -m "📝 docs(act7): runbook skeleton (obtainability beats, PROJECTED)"
```

---

### Task 7: LIVE validation on the cluster (evidence, tagged) — HARD GATE

Not a TDD code task: this runs real infra on `example-sandbox` and records LIVE evidence. Tear down after. Use `env -u GOOGLE_APPLICATION_CREDENTIALS` for every `gcloud`/`kubectl`. Build the advisor binary first (`cd advisor && go build -o /tmp/capacity-advisor ./cmd/capacity-advisor`).

**Files:**
- Modify: `demo/act7/runbook.md` (fill LIVE numbers)

**Interfaces:**
- Consumes: the `probe` subcommand (`probe --config <cfg> --machine-type a2-highgpu-1g --zone <zone>`), the `reconcile` subcommand, the demo config from Task 5.
- Produces: the finished LIVE runbook.

- [ ] **Step 1: A100 spot quota check (prerequisite).** Record spot/preemptible A100 quota per ladder region:

```bash
env -u GOOGLE_APPLICATION_CREDENTIALS gcloud compute regions describe us-central1 \
  --project example-sandbox --format="value(quotas)" | tr ';' '\n' | grep -i -A1 -E 'A100|PREEMPTIBLE'
```

If quota is zero in every ladder region, note it explicitly in the runbook: the obtainable-probe beat (Step 2) is skipped and only the forced-failover beat (Step 3) runs. Do not silently skip.

- [ ] **Step 2: LIVE obtainable probe (opportunistic → `LIVE-ORGANIC`).** Fill the demo config's `network`/`subnet` to match the reconciler VPC, then:

```bash
env -u GOOGLE_APPLICATION_CREDENTIALS /tmp/capacity-advisor probe \
  --config demo/act7/advisor-a100.yaml --machine-type a2-highgpu-1g --zone us-central1-a
```

Record `Obtained: true/false` and elapsed time. Confirm the VM self-deletes (the command deletes it; `maxRunDuration` is the backstop). Tag the result `LIVE-ORGANIC`.

- [ ] **Step 3: Stockout → failover (opportunistic + forced fallback — HARD GATE on one).** Try a genuine stockout across the ladder (repeat Step 2 per zone/region). If A100 is obtainable everywhere, force the path deterministically — a bogus zone (e.g. `--zone us-central1-z`) or a quota-0 region — and capture `StatusStockout`/`Gone`. Then drive a reconcile tick with the demo config and assert the dry/stocked-out zone is held off the rung and failover routes to the next-cheapest region:

```bash
env -u GOOGLE_APPLICATION_CREDENTIALS /tmp/capacity-advisor reconcile \
  --config demo/act7/advisor-a100.yaml --profiles gpu-a100 \
  --cluster-region us-central1 --dry-run
```

Tag `LIVE-ORGANIC` if the stockout was genuine, else `LIVE-FORCED`.

- [ ] **Step 4: Ledger-boundary assertion (re-provoke live).** After the stockout tick, dump the reconciler state ConfigMap and confirm the probe wrote **no** ledger entry for the stocked-out A100 shape (the Plan-4 invariant, re-provoked per the standing evidence rule):

```bash
env -u GOOGLE_APPLICATION_CREDENTIALS kubectl -n spot-demo get configmap \
  capacity-advisor-state -o jsonpath='{.data}' | grep -i ledger || echo "no ledger entry (expected)"
```

- [ ] **Step 5: Fill the runbook** — replace every `PROJECTED` line touched above with the captured value and its `LIVE-ORGANIC`/`LIVE-FORCED` tag; leave any un-run beat explicitly `PROJECTED` with the reason.

- [ ] **Step 6: Tear down** all Act 7 demo infra (probe VMs are already gone; remove any node pool/cluster resources spun up for the run) per standing consent. Confirm no A100 VM remains:

```bash
env -u GOOGLE_APPLICATION_CREDENTIALS gcloud compute instances list \
  --project example-sandbox --filter="labels.purpose=capacity-probe" --format="value(name)"
```

- [ ] **Step 7: Commit**

```bash
git add demo/act7/runbook.md
git commit -S -m "📝 docs(act7): live obtainability evidence (LIVE-ORGANIC/LIVE-FORCED)"
```

---

## Final verification & review (workflow Steps 6–7)

- [ ] Run the full suite from the worktree root: `make test` — expect exit 0 (Go + shellcheck + all kubeconform).
- [ ] Invoke `superpowers:verification-before-completion` — confirm every §6 spec step is green and every runbook claim is `LIVE-*` or explicitly `PROJECTED`.
- [ ] Invoke `superpowers:requesting-code-review` — per-task + whole-branch review (as Acts 5/6 did); address findings.
- [ ] Merge is a clean fast-forward from the **MAIN checkout** (worktree-isolation rail blocks merging from the worktree session). Leave `/tmp/act7-merge-handoff.md`.

## Self-review (plan vs spec)

- **Spec §1 (goal: probe-confirmed widening + stockout failover):** Tasks 3 (gate covers A100), 7 Steps 2–3 (LIVE). ✓
- **Spec §3 (A100 as ladder rung):** Tasks 1, 2, 5. ✓
- **Spec §3 (probe-gating extended to A100):** Task 3. ✓
- **Spec §3 (region failover across the ladder):** Task 5 (config), Task 7 Step 3 (LIVE). ✓
- **Spec §3 (LIVE runbook, tagged):** Tasks 6, 7. ✓
- **Spec §2/§4 (A100 integral to a2 — no accelerator):** Task 4. ✓
- **Spec §6.1 (unit/static HARD GATE):** Tasks 1–5 + final `make test`. ✓
- **Spec §6.2 (quota check):** Task 7 Step 1. ✓
- **Spec §2 (ledger boundary):** Task 3 + Task 7 Step 4. ✓
- **Non-goals (no serving, no re-wiring, single-GPU):** honored — no `workloads/` changes, gate reused verbatim, only `a2-highgpu-1g` exercised. ✓
- **Placeholder scan:** every code step has real code; the only intentional `PROJECTED` placeholders are LIVE numbers filled by Task 7. ✓
- **Type consistency:** `machinetype.Units(mt, kind) (float64, error)`, `probe.Status`, `fake.Compute{StatusFn, Inserted, Deleted}`, `config.Probe{Automated, MaxRunDurationSeconds}`, `Deps{Probe, Now, ProbeCfg}` all match the current source. ✓
