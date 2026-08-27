# Automated Verify-by-Probe Implementation Plan

> **For implementers:** This plan is written to be executed task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Gate the reconciler's widen/promote decisions on a live capacity probe — a successful probe confirms a `(shape, zone)` and lets the rung widen/promote onto it; a failure is inconclusive and never touches the evidence ledger.

**Architecture:** A positive-only confirmation track, separate from the evidence ledger, lives in the reconcile state. Async (Approach A): the probe VM *is* the pending probe — one tick creates it, a later tick polls/reaps it. Conservative, opt-in, bounded.

**Tech Stack:** Go (advisor), `google.golang.org/api` / `cloud.google.com/go/compute` (GCE), the existing reconcile CronJob.

## Global Constraints

- `make test` needs `PIP_INDEX_URL=https://pypi.org/simple`; Go work runs from `advisor/` (`cd advisor && go test ./...`).
- **`probe.Result`/probe outcomes must NEVER be written to `evidence.Ledger`** — permanent Plan 4 boundary. A failed probe is the *absence* of a confirmation, never a penalty.
- **Never degrade a working ladder:** widen/promote only ever *adds* probe-confirmed options; a rung keeps its current zones/shape when nothing is confirmed.
- Conservative budget constants (verbatim): `probeConfirmTTL = 30 * time.Minute`, `probesPerTick = 1`, `probesPerDay = 6`, `probeBudgetWindow = 24 * time.Hour`.
- Opt-in: reconciler probing requires a new `probe.automated` flag (default false), which also gates the instance-create IAM binding. `probe.enabled` continues to gate only the human `probe` subcommand.
- Commits: signed (`git commit -S`), emoji conventional. Commit gate (`git status` + `git log -n 3`) before each message.
- Diagrams Mermaid, never ASCII.
- Pre-existing baseline flake (out of scope): `workloads/01-queue/worker` `TestRunStopsPromptlyOnCancelWithoutLosingTasks` fails intermittently on a cancel-timing race; passes on retry. Not this plan's code.

---

## Task 1: Split the probe Compute interface for async (offline, TDD)

Separate "issue create" from "wait for running" so the reconciler can create in one tick and poll in another. The human `probe.Run` keeps its blocking one-shot behavior.

**Files:**
- Modify: `advisor/internal/probe/probe.go` (add `Status`, extend `Compute`, recompose `Run`)
- Modify: `advisor/internal/probe/fake/fake.go` (scriptable `InsertFn`/`StatusFn` + recorders)
- Test: `advisor/internal/probe/probe_test.go`

**Interfaces:**
- Produces: `type Status int` with `StatusProvisioning`, `StatusRunning`, `StatusStockout`, `StatusGone`; `Compute` interface with `InsertSpotVM(ctx, Request) error`, `VMStatus(ctx, project, zone, name string) (Status, error)`, `DeleteVM(ctx, project, zone, name string) error`. `Run(ctx, Compute, Request) Result` unchanged in behavior.

- [ ] **Step 1: Write the failing test for `Run` recomposed over the new ops.**

```go
func TestRunObtainsWhenStatusReachesRunning(t *testing.T) {
	calls := 0
	c := &fake.Compute{
		InsertFn: func(context.Context, probe.Request) error { return nil },
		StatusFn: func(context.Context, string, string, string) (probe.Status, error) {
			calls++
			if calls < 2 {
				return probe.StatusProvisioning, nil
			}
			return probe.StatusRunning, nil
		},
	}
	res := probe.Run(context.Background(), c, probe.Request{
		Project: "p", Zone: "us-central1-a", MachineType: "g2-standard-4",
		Name: "probe-x", MaxRunSeconds: 300,
	})
	if !res.Obtained {
		t.Fatalf("want obtained, got %+v", res)
	}
	if len(c.Deleted) != 1 {
		t.Fatalf("Run must always delete; deletes=%v", c.Deleted)
	}
}

func TestRunReportsStockoutWithoutObtaining(t *testing.T) {
	c := &fake.Compute{
		InsertFn: func(context.Context, probe.Request) error { return nil },
		StatusFn: func(context.Context, string, string, string) (probe.Status, error) {
			return probe.StatusStockout, nil
		},
	}
	res := probe.Run(context.Background(), c, probe.Request{
		Project: "p", Zone: "us-central1-a", MachineType: "g2-standard-4",
		Name: "probe-x", MaxRunSeconds: 300,
	})
	if res.Obtained {
		t.Fatalf("stockout must not obtain, got %+v", res)
	}
	if len(c.Deleted) != 1 {
		t.Fatalf("Run must always delete even on stockout; deletes=%v", c.Deleted)
	}
}
```

- [ ] **Step 2: Run — expect compile failure** (`InsertSpotVM`/`VMStatus`/`Status` undefined).

Run: `cd advisor && go test ./internal/probe/... -run TestRun -v`
Expected: FAIL (undefined).

- [ ] **Step 3: Implement the interface + Status enum, and recompose `Run`.**

```go
type Status int

const (
	StatusProvisioning Status = iota
	StatusRunning
	StatusStockout
	StatusGone
)

type Compute interface {
	InsertSpotVM(ctx context.Context, r Request) error
	VMStatus(ctx context.Context, project, zone, name string) (Status, error)
	DeleteVM(ctx context.Context, project, zone, name string) error
}
```

Recompose `Run`: `InsertSpotVM`, then poll `VMStatus` (short sleep between polls) until `StatusRunning` (Obtained=true) or `StatusStockout`/`StatusGone` (Obtained=false) or `ctx`/`MaxRunSeconds` elapses; then `DeleteVM` unconditionally on the detached, `cleanupTimeout`-bounded context exactly as today. Keep the `MaxRunSeconds<=0` and empty-`Name` guards.

- [ ] **Step 4: Update the fake** (`advisor/internal/probe/fake/fake.go`): add `InsertFn func(ctx, Request) error`, `StatusFn func(ctx, project, zone, name string) (probe.Status, error)`, recorders `Inserted []probe.Request`, keep `Deleted []string`; implement `InsertSpotVM`/`VMStatus`/`DeleteVM`; keep `var _ probe.Compute = (*Compute)(nil)`. Remove the old `CreateSpotVM`/`CreateFn` (or keep `CreateFn` as an alias used only by `Run` if simpler — but prefer the clean split).

- [ ] **Step 5: Run — expect PASS.**

Run: `cd advisor && go test ./internal/probe/... -v`
Expected: PASS.

- [ ] **Step 6: Mutation check.** In `Run`, make the stockout branch set `Obtained=true`; confirm `TestRunReportsStockoutWithoutObtaining` fails; revert; confirm `git diff` empty after revert.

- [ ] **Step 7: Commit.**

```bash
git add advisor/internal/probe/probe.go advisor/internal/probe/fake/fake.go advisor/internal/probe/probe_test.go
git commit -S -m "♻️ refactor(probe): split Compute into insert/status/delete for async"
```

---

## Task 2: gce.go async ops + network fix (offline compile; live-verified in Task 7)

> **Executed together with Task 1 as one commit.** Changing `probe.Compute`
> (Task 1) orphans `gce.Client` until it implements the new methods, so the two
> must land together or `go build ./...` fails. See the SDD ledger.

**Files:**
- Modify: `advisor/internal/probe/gce/gce.go`

**Interfaces:**
- Produces: `*gce.Client` implements the new `probe.Compute` (`InsertSpotVM`, `VMStatus`, `DeleteVM`).
- Consumes: `config.Probe.Network`/`Subnet` (Task 3) — until Task 3 lands, read from `probe.Request` fields added here OR accept them via the `Client` constructor; the implementer wires whichever Task 3 exposes.

- [ ] **Step 1: Implement `InsertSpotVM`** = the current `CreateSpotVM` body **without** `op.Wait` (issue `instances.Insert`, return the immediate call error only; do not block on the operation).

- [ ] **Step 2: Implement `VMStatus(ctx, project, zone, name)`** = `instances.Get`; map GCE instance `Status`: `RUNNING` → `StatusRunning`; `PROVISIONING`/`STAGING` → `StatusProvisioning`; `TERMINATED`/`STOPPING` → `StatusStockout`; NotFound → `StatusGone`. **Live-verify the exact mapping in Task 7**, especially how a spot stockout surfaces (instance may never appear, or appear then terminate with a `statusMessage`/scheduling reason). If NotFound is ambiguous between "still provisioning" and "gone", carry the pending probe until `maxRunDuration` and let the reap deadline resolve it (Task 5).

- [ ] **Step 3: Network fix.** Replace `Network: proto.String("global/networks/default")` (gce.go:50) with the configured network, and set the subnet when provided. Prefer full URLs from `config.Probe.Network`/`Subnet`; fall back to the cluster's own network if discoverable. A wrong network reads as a false stockout, so this is correctness, not polish.

- [ ] **Step 4: Keep `DeleteVM` as-is; ensure `var _ probe.Compute = (*Client)(nil)` compiles.**

Run: `cd advisor && go build ./... && go vet ./internal/probe/...`
Expected: builds clean.

- [ ] **Step 5: Commit.**

```bash
git add advisor/internal/probe/gce/gce.go
git commit -S -m "✨ feat(probe): async insert/status ops and configurable network"
```

---

## Task 3: config.Probe automated + network fields (offline, TDD)

**Files:**
- Modify: `advisor/internal/config/config.go` (`Probe` struct + defaults/validation)
- Test: `advisor/internal/config/config_test.go`

**Interfaces:**
- Produces: `config.Probe{Enabled bool; Automated bool; MaxRunDurationSeconds int; Network string; Subnet string}`.

- [ ] **Step 1: Write the failing test.**

```go
func TestProbeAutomatedDefaultsOffAndParses(t *testing.T) {
	c, err := config.Parse([]byte("project: p\nprobe:\n  automated: true\n  network: net-x\n  subnet: sub-y\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !c.Probe.Automated || c.Probe.Network != "net-x" || c.Probe.Subnet != "sub-y" {
		t.Fatalf("got %+v", c.Probe)
	}
	def, _ := config.Parse([]byte("project: p\n"))
	if def.Probe.Automated {
		t.Fatal("automated must default false")
	}
}
```

(Match the actual `config.Parse`/loader entrypoint name in `config.go`; if parsing is via a `Load(path)` only, add a small `parse([]byte)` helper or use the existing test pattern for constructing a Config.)

- [ ] **Step 2: Run — expect FAIL** (unknown fields / Automated absent).

Run: `cd advisor && go test ./internal/config/ -run TestProbeAutomated -v`
Expected: FAIL.

- [ ] **Step 3: Add the fields** to `config.Probe`: `Automated bool` (`yaml:"automated"`), `Network string` (`yaml:"network"`), `Subnet string` (`yaml:"subnet"`). No new required validation (all optional; automated defaults false).

- [ ] **Step 4: Run — expect PASS.** `cd advisor && go test ./internal/config/ -v`

- [ ] **Step 5: Commit.**

```bash
git add advisor/internal/config/config.go advisor/internal/config/config_test.go
git commit -S -m "✨ feat(config): add probe.automated, network, subnet"
```

---

## Task 4: Confirmation track — state + pure helpers (offline, TDD)

The positive-only cache, pending list, budget — and the pure functions over them. No reconcile wiring yet.

**Files:**
- Create: `advisor/internal/reconcile/probe.go`
- Modify: `advisor/internal/reconcile/reconcile.go` (State fields at `:91`; decodeState guards at `:107`)
- Test: `advisor/internal/reconcile/probe_test.go`

**Interfaces:**
- Produces: `ProbeConfirmation{MachineType, Zone string; At time.Time}`, `PendingProbe{VMName, MachineType, Zone string; CreatedAt time.Time}`, `ProbeBudget{WindowStart time.Time; Count int}`; constants `probeConfirmTTL/probesPerTick/probesPerDay/probeBudgetWindow`; helpers `confirmed(st *State, mt, zone string, now time.Time) bool`, `budgetAvailable(st *State, now time.Time) bool`, `noteProbeLaunched(st *State, now time.Time)`, `pendingFor(st *State, mt, zone string) bool`, and a stable key `probeKey(mt, zone string) string`.

- [ ] **Step 1: Write failing tests.**

```go
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
```

(`newStateForTest` = `newState()` with the new maps/slices initialized; add it or use `newState()` if it initializes them per Step 3.)

- [ ] **Step 2: Run — expect FAIL** (undefined). `cd advisor && go test ./internal/reconcile/ -run 'TestConfirmed|TestBudget' -v`

- [ ] **Step 3: Add State fields + guards + `probe.go`.**

In `reconcile.go` `State` (after `LastLogQuery`):
```go
	ProbeConfirmations map[string]ProbeConfirmation `json:"probeConfirmations,omitempty"`
	PendingProbes      []PendingProbe               `json:"pendingProbes,omitempty"`
	ProbeBudget        ProbeBudget                  `json:"probeBudget"`
```
In `newState()` initialize `ProbeConfirmations: map[string]ProbeConfirmation{}`. In `decodeState`, nil-guard `ProbeConfirmations` (mirror the `Applied`/`Pending` guards). `PendingProbes` nil slice is fine.

In new `probe.go`: the types, the four constants (verbatim from Global Constraints), `probeKey = mt + "\x00" + zone`, and the helpers. `confirmed` returns false if missing or `now.Sub(at) >= probeConfirmTTL`. `budgetAvailable` resets the window when `now.Sub(WindowStart) >= probeBudgetWindow` then checks `Count < probesPerDay`. `noteProbeLaunched` resets-if-needed then increments (and stamps `WindowStart` when starting a fresh window).

- [ ] **Step 4: Run — expect PASS.** `cd advisor && go test ./internal/reconcile/ -run 'TestConfirmed|TestBudget' -v`

- [ ] **Step 5: Mutation check.** Change `confirmed`'s `>=` to `>` (or TTL comparison direction); confirm `TestConfirmedRespectsTTL` fails; revert; `git diff` empty.

- [ ] **Step 6: Commit.**

```bash
git add advisor/internal/reconcile/probe.go advisor/internal/reconcile/reconcile.go advisor/internal/reconcile/probe_test.go
git commit -S -m "✨ feat(reconcile): add the probe-confirmation track to state"
```

---

## Task 5: Reconcile integration — reap, gate widen/promote, launch (offline, TDD)

**Files:**
- Modify: `advisor/internal/reconcile/reconcile.go` (`Deps` at `:51`; `Tick` at `:167`; the widen/promote seam)
- Modify: `advisor/internal/reconcile/probe.go` (`reapProbes`, `launchProbe`)
- Test: `advisor/internal/reconcile/probe_test.go`

**Interfaces:**
- Consumes: `probe.Compute` (Task 1), `confirmed`/`budgetAvailable`/`noteProbeLaunched`/`pendingFor` (Task 4).
- Produces: `Deps.Probe probe.Compute` and `Deps.ProbeCfg config.Probe`; `reapProbes(ctx, d Deps, st *State)`; `launchProbe(ctx, d Deps, st *State, mt, zone string) bool`.

- [ ] **Step 1: Write the async two-tick + ledger-invariant tests.**

```go
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
```

- [ ] **Step 2: Run — expect FAIL** (undefined `reapProbes`/`launchProbe`/`Deps.Probe`/`Deps.ProbeCfg`).

Run: `cd advisor && go test ./internal/reconcile/ -run 'TestReap|TestLaunch' -v`
Expected: FAIL.

- [ ] **Step 3: Implement `reapProbes` and `launchProbe` in `probe.go`.**

`reapProbes`: for each `PendingProbe`, if `now.Sub(CreatedAt) >= MaxRunDuration` → `DeleteVM`, drop, continue. Else `VMStatus`: `StatusRunning` → write `ProbeConfirmation{At: now}`, `DeleteVM`, drop; `StatusStockout`/`StatusGone` → `DeleteVM`, drop, write nothing; `StatusProvisioning` → keep pending. On any `VMStatus`/`DeleteVM` error → log via `res.Warnings` (thread `res` in if needed) and keep or drop conservatively; **never** write to `st.Ledger`. Rebuild `st.PendingProbes` from survivors.

`launchProbe(ctx, d, st, mt, zone) bool`: return false if `!d.ProbeCfg.Automated`, or `confirmed(...)`, or `pendingFor(...)`, or `!budgetAvailable(st, d.Now)`. Otherwise build a prefixed `Request{Name: "capacity-probe-"+mt+"-"+zone+"-"+shortstamp, MaxRunSeconds: int64(d.ProbeCfg.MaxRunDurationSeconds), Project, Zone: zone, MachineType: mt, ...network from d.ProbeCfg}`, call `d.Probe.InsertSpotVM`; on success append `PendingProbe`, `noteProbeLaunched`, return true. On insert error: log, `noteProbeLaunched` (spend budget to avoid thrash), return false. Never touch the ledger.

- [ ] **Step 4: Wire into `Tick`.** Add `Probe probe.Compute` and `ProbeCfg config.Probe` to `Deps`. In `Tick`, call `reapProbes` after loading state and **before** scoring/`reconcileClass`. In the widen/promote seam, a candidate `(shape, zone)` is applied only if `confirmed(st, shape, zone, d.Now)`; for an unconfirmed widen/promote candidate, call `launchProbe(...)` and do NOT add it this tick (keep current zones/shape — never degrade). Guard the whole probe path on `d.Probe != nil && d.ProbeCfg.Automated` so existing callers/tests with no probe are unaffected.

- [ ] **Step 5: Write the two-tick flow test** (tick N launches, ladder unchanged, pending recorded; tick N+1 with fake `StatusFn` RUNNING reaps + the candidate becomes eligible). Assert the ladder does not gain the zone until the confirmation exists, and never loses its original zones.

- [ ] **Step 6: Run affected packages — expect PASS.** `cd advisor && go test ./internal/reconcile/... ./internal/probe/... -v`

- [ ] **Step 7: Mutation check.** Make the widen/promote seam apply an unconfirmed candidate (skip the `confirmed` guard); confirm the two-tick test fails (zone appears too early); revert; `git diff` empty.

- [ ] **Step 8: Full suite.** `PIP_INDEX_URL=https://pypi.org/simple make test` → EXIT 0 (ignore the known worker flake; re-run that package once if it trips).

- [ ] **Step 9: Commit.**

```bash
git add advisor/internal/reconcile/probe.go advisor/internal/reconcile/reconcile.go advisor/internal/reconcile/probe_test.go
git commit -S -m "✨ feat(reconcile): gate widen/promote on live probe confirmation"
```

---

## Task 6: Infra — bind instance IAM when probe.automated (offline)

**Files:**
- Modify: the reconciler install script/manifests under `infra/reconciler/` and/or `infra/08-reconciler.sh` (match the existing IAM-binding pattern; grep for where the reconciler SA roles are granted)

**Interfaces:** none (infra).

- [ ] **Step 1:** Find where the reconciler SA's roles are bound (grep `infra/` for the reconciler service account + `add-iam-policy-binding` / role grants). Read the existing pattern.

- [ ] **Step 2:** Add a **conditional** binding of `roles/compute.instanceAdmin.v1` (or the minimal `compute.instances.create/get/delete` custom role already used by the probe path — reuse whatever the human probe path documents) to the reconciler SA, applied **only** when probe automation is enabled (e.g., a script flag/env `PROBE_AUTOMATED=true`, or a documented manual step). Keep default-off: a normal install grants nothing new.

- [ ] **Step 3:** If manifests are schema-tested, validate: `sed ... | kubeconform -strict -ignore-missing-schemas -summary -` per the Makefile pattern. Otherwise document the binding in the reconciler section of `docs/runbook.md`.

- [ ] **Step 4: Commit.**

```bash
git add infra/ docs/
git commit -S -m "✨ feat(infra): bind probe instance IAM to the reconciler when automated"
```

---

## Task 7: Runbook Beat + live demonstration (LIVE GATE)

Demonstrate an automated probe confirming a zone on demand — this IS live-demonstrable (unlike a stockout). **Requires explicit go-ahead** (creates a real spot VM; needs the Task 6 IAM binding).

**Files:**
- Modify: `demo/act4/runbook.md`

- [ ] **Step 1: Get go-ahead.**
- [ ] **Step 2:** Enable `probe.automated: true` (+ network/subnet) in the deployed config; apply the Task 6 IAM binding.
- [ ] **Step 3:** Verify the async lifecycle against real GCE — confirm the `VMStatus` mapping (esp. how a spot stockout actually surfaces: instance TERMINATED vs. never-created), the network fix (VM joins the cluster network), and the two-tick create→reap→confirm flow. Capture the state ConfigMap showing a `probeConfirmation` written and the ledger untouched.
- [ ] **Step 4:** Force a widen/promote scenario (e.g., an evidence-dry rung) and show the ladder widening only after the probe confirms — and NOT degrading when it doesn't.
- [ ] **Step 5:** Tear down probe VMs; confirm none linger (prefix-scan + `gcloud compute instances list --filter 'name~^capacity-probe-'`).
- [ ] **Step 6:** Write the Beat into `runbook.md` with the real captured output, and update Act 4's "hand deviations" note that verify-by-probe is now automated (Act 3's widening/promotion mechanism). Honestly note what remains manual/opportunistic.
- [ ] **Step 7: Commit.**

```bash
git add demo/act4/runbook.md
git commit -S -m "📝 docs(act4): document and verify automated verify-by-probe"
```

---

## Task 8: Gate the render-seam never-sharded widening on confirmation (offline, TDD)

Task 5 gated only the EvidenceDry zone-recovery path. This closes design §5.5 #1: `buildRungs` widens an all-evidence-dry rung to `universe` zones (computeclass.go:229-235) off the stale prior, with no probe. Gate that widening on a confirmation, and have the reconciler launch probes for unconfirmed widen targets. (§5.5 #2 — promoting a shape the advice never sharded, e.g. Act 3's `g2-standard-8` — is NOT in scope: `buildRungs` only ranks existing candidates, so it would require synthesizing candidates; documented as future work.)

**Files:**
- Modify: `advisor/internal/render/computeclass.go` (`buildRungs` + a `widenTargets` helper)
- Modify: `advisor/internal/reconcile/reconcile.go` / `probe.go` (launch probes for unconfirmed widen targets)
- Test: `advisor/internal/render/computeclass_test.go`, `advisor/internal/reconcile/probe_test.go`

**Interfaces:**
- Consumes: `confirmed(st, mt, zone, now)` / `launchProbe` (Task 4/5).
- Produces: `func widenTargets(a *analyze.Analysis, region string) map[string][]string` (shape → the in-region universe zones it would widen to, i.e. not its own sharded zones); `buildRungs` gains a `confirmedZone func(mt, zone string) bool` parameter and only widens to a universe zone when `confirmedZone(mt, z)` is true (in addition to the existing `!dry[mt][z]`).

- [ ] **Step 1: Write the failing render test.** A rung whose sharded zones are all EvidenceDry widens ONLY to universe zones for which `confirmedZone` returns true; an unconfirmed universe zone is NOT added (and if none are confirmed, the rung stays `Exhausted` with its pinned zones — never degraded).

```go
func TestBuildRungsWidensOnlyToConfirmedZones(t *testing.T) {
	a := &analyze.Analysis{Candidates: []analyze.Candidate{
		{MachineType: "g2-standard-4", Zone: "us-central1-a", Region: "us-central1", Composite: 0.3, EvidenceDry: true},
		{MachineType: "g2-standard-4", Zone: "us-central1-b", Region: "us-central1", Composite: 0.3}, // universe zone, other shape absent
	}}
	confirmed := func(mt, z string) bool { return mt == "g2-standard-4" && z == "us-central1-b" }
	rungs := buildRungs(a, 5, false, "us-central1", confirmed)
	// the -a shard is dry; -b is a confirmed universe zone → widen onto -b only
	// (assert the g2-standard-4 rung's zones == ["us-central1-b"], Widened=true)
}
```

- [ ] **Step 2: Run — FAIL** (buildRungs signature/behavior). `cd advisor && go test ./internal/render/ -run TestBuildRungsWidensOnlyToConfirmedZones -v`

- [ ] **Step 3: Implement.** Extract `widenTargets` (the `universe`-minus-own-sharded-zones computation already inline in buildRungs). Add the `confirmedZone` param; in the widen loop (computeclass.go:230), add a zone only when `!dry[mt][z] && confirmedZone(mt, z)`. Update all `buildRungs` call sites: the reconciler passes `func(mt,z){ return confirmed(st,mt,z,d.Now) }`; the one-shot CLI/advise path (no probe state) passes a `func(...) bool { return true }` so non-reconcile rendering is unchanged (a probe gate only makes sense with reconcile state).

- [ ] **Step 4: Wire probe launches for unconfirmed widen targets.** In the reconciler, before/around rendering, for each all-dry rung use `widenTargets` to get its universe zones; for each `(mt, z)` that is `!confirmed` and not pending, `launchProbe` (bounded by `probesPerTick`/budget). Extend `gateWidenPromote` or add a sibling.

- [ ] **Step 5: Reconcile two-tick test.** An all-dry rung with no confirmations does NOT widen (stays exhausted/pinned) and launches a probe; after a RUNNING reap confirms a universe zone, the next tick widens onto exactly that zone. Assert never-degrade (original zones retained when nothing confirmed).

- [ ] **Step 6: Run + full suite.** `cd advisor && go test ./internal/render/... ./internal/reconcile/...` then `PIP_INDEX_URL=https://pypi.org/simple make test` (EXIT 0; known worker flake).

- [ ] **Step 7: Mutation.** Drop the `confirmedZone(mt,z)` guard from the widen loop; the render test must fail (an unconfirmed zone gets widened onto); revert; `git diff` empty.

- [ ] **Step 8: Commit.**

```bash
git add advisor/internal/render/computeclass.go advisor/internal/render/computeclass_test.go advisor/internal/reconcile/
git commit -S -m "✨ feat(reconcile): gate never-sharded rung widening on live probe"
```

---

## Self-Review

**Spec coverage:** §5.1 confirmation type → Task 4. §5.2 config → Task 3. §5.3 state → Task 4. §5.4 async lifecycle → Tasks 1/2 (ops) + Task 5 (reap/launch). §5.5 widen/promote gate → Task 5. §5.6 budget constants → Task 4. §5.7 probe refactor → Task 1. §5.8 gce network → Task 2. §5.9 IAM → Task 6. §5.10 flow → Tasks 4/5. §6 error handling → Task 5 Step 3. §7 testing → Tasks 1/4/5 (incl. the ledger-invariant test + mutations). §8 sequencing → task order. Runbook Beat → Task 7.

**Placeholder scan:** No TBD/TODO. Live-verification items (Task 2 status mapping, Task 7) are genuine live steps, not deferred code. Task 3's note to match the actual `config.Parse` entrypoint and Task 6's grep-for-the-pattern are grounded discovery steps, not vague hand-waves.

**Type consistency:** `probe.Compute{InsertSpotVM, VMStatus, DeleteVM}`, `probe.Status{Provisioning|Running|Stockout|Gone}`, `ProbeConfirmation{MachineType,Zone,At}`, `PendingProbe{VMName,MachineType,Zone,CreatedAt}`, `ProbeBudget{WindowStart,Count}`, `Deps.Probe`/`Deps.ProbeCfg`, and the four constants are used identically across Tasks 1/4/5. `confirmed/budgetAvailable/noteProbeLaunched/pendingFor/probeKey` signatures match between Task 4 (defined) and Task 5 (consumed).
