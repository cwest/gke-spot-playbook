# Act 5 — Agent Lifecycle — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A runnable Act 5 that reproduces the shape of Google's GKE Agent Sandbox benchmark (baseline → +gVisor → +suspend/resume) to prove idle-RAM reclaim (lifecycle) beats isolation as the agent-fleet cost lever.

**Architecture:** A mock idle-bursty agent (Go, real resident RAM) runs under GKE Agent Sandbox; a shell harness drives the three-point density ladder and snapshots idle agents; a small Go `agent-density` tool turns agent counts + the node's on-demand price into `$/agent` and the two lever ratios. Offline: everything but the live run. The live three-point run is a billed gate.

**Tech Stack:** Go (mock agent + density tool, reusing `advisor/internal/pricing`), GKE Agent Sandbox Pod snapshots, shell harness, kubeconform.

## Global Constraints

- `make test` needs `PIP_INDEX_URL=https://pypi.org/simple`; advisor Go work runs from `advisor/`.
- Mock agent is a **separate Go module** under `workloads/05-agents/agent/` (mirrors `workloads/01-queue/worker` — module `github.com/cwest/gke-spot-instance-node-pools/workloads/05-agents/agent`).
- The density tool lives in the **advisor module** (reuses `pricing.OnDemandHourlyUSD`).
- **Measure all three ladder points** (P1 baseline, P2 +gVisor sandbox, P3 +suspend/resume) — the isolation-vs-lifecycle claim is comparative.
- **Restore is not network-transparent** (new IP, dropped sockets): the agent must be reconnect-tolerant; the runbook states it honestly.
- Node price via `pricing.BillingSource.OnDemandHourlyUSD(ctx, region, machineType)`; `$/agent = nodeHourlyUSD / agentsPacked`.
- Commits signed (`git commit -S`), emoji conventional, NO AI/assistant attribution. Commit gate (`git status` + `git log -n 3`) before each message.
- Diagrams Mermaid, never ASCII. Subagents: `cd` into the worktree, assert `git rev-parse --show-toplevel` ends in `topic+gke-agent-lifecycle-act` before any file/git op; after committing confirm the commit is on the topic branch and `git rev-parse --short main` is `1c7c226` (unchanged).
- Task 6 (live run) is a BILLED gate — needs explicit human go-ahead (like the probe VM).
- Pre-existing baseline flake (out of scope): `workloads/01-queue/worker` cancel-timing test; passes on retry.

---

## Task 1: Mock agent core (offline, TDD)

**Files:**
- Create: `workloads/05-agents/agent/go.mod` (module path above), `workloads/05-agents/agent/main.go`, `workloads/05-agents/agent/agent.go`
- Test: `workloads/05-agents/agent/agent_test.go`

**Interfaces:**
- Produces: `type Phase int` (`Active`, `Idle`); `func phaseAt(elapsed time.Duration, active, idle time.Duration) Phase` (pure cycle logic); `func parseSizeMiB(s string) (int, error)`; a `WorkingSet` that allocates `mib` and faults every page so RSS is real; state written to a file (`--state-file`) as `active`/`idle` each transition.

- [ ] **Step 1: Write the failing cycle + size tests.**

```go
func TestPhaseAtCyclesActiveThenIdle(t *testing.T) {
	active, idle := 10*time.Second, 30*time.Second
	cases := []struct{ el time.Duration; want Phase }{
		{0, Active}, {9 * time.Second, Active}, {10 * time.Second, Idle},
		{39 * time.Second, Idle}, {40 * time.Second, Active}, // wraps
	}
	for _, c := range cases {
		if got := phaseAt(c.el, active, idle); got != c.want {
			t.Errorf("phaseAt(%v)=%v want %v", c.el, got, c.want)
		}
	}
}

func TestParseSizeMiB(t *testing.T) {
	for in, want := range map[string]int{"256": 256, "1024": 1024} {
		if got, err := parseSizeMiB(in); err != nil || got != want {
			t.Errorf("parseSizeMiB(%q)=%d,%v want %d", in, got, err, want)
		}
	}
	if _, err := parseSizeMiB("0"); err == nil {
		t.Error("zero size must error (a snapshot of nothing proves nothing)")
	}
}
```

- [ ] **Step 2: Run — expect FAIL** (undefined). `cd workloads/05-agents/agent && go test ./... -run 'TestPhaseAt|TestParseSize' -v`

- [ ] **Step 3: Implement** `Phase`/`phaseAt` (modulo over `active+idle`), `parseSizeMiB` (reject ≤ 0), and `WorkingSet` (allocate `[]byte` of `mib<<20`, write one byte per 4 KiB page in a loop so pages are resident; keep a reference so the GC can't reclaim). `main.go`: parse flags (`--working-set-mib`, `--active-seconds`, `--idle-seconds`, `--state-file`), allocate the working set once, then loop: compute `phaseAt`, on transition write the phase to `--state-file` and log a heartbeat, sleep a short tick.

- [ ] **Step 4: Run — expect PASS.** `cd workloads/05-agents/agent && go test ./... -v`

- [ ] **Step 5: Mutation.** Change `phaseAt` to always return `Active`; confirm `TestPhaseAtCyclesActiveThenIdle` fails; revert; `git diff` empty.

- [ ] **Step 6: Commit.**

```bash
git add workloads/05-agents/agent
git commit -S -m "✨ feat(act5): mock idle-bursty agent with resident working set"
```

_Note: RSS realism (pages actually resident) is verified at runtime in Task 6, not unit-tested — a unit test cannot assert kernel RSS portably._

---

## Task 2: Mock agent Dockerfile (offline)

**Files:**
- Create: `workloads/05-agents/agent/Dockerfile`

- [ ] **Step 1: Write a minimal multi-stage Dockerfile** (golang build → distroless/static runtime), mirroring `workloads/01-queue/worker/Dockerfile`'s style (read it first). Entrypoint runs the agent binary.

- [ ] **Step 2: Build locally to verify.** `docker build -t agent5:dev workloads/05-agents/agent` (or `go build ./...` if docker is unavailable in the env — note which).

- [ ] **Step 3: Commit.**

```bash
git add workloads/05-agents/agent/Dockerfile
git commit -S -m "📦 build(act5): container image for the mock agent"
```

---

## Task 3: Manifests + Agent Sandbox config (offline)

**Files:**
- Create: `workloads/05-agents/manifests/namespace.yaml`, `serviceaccount.yaml`, `agents.yaml` (the agent Deployment + RAM request), and the Agent Sandbox runtime configuration the pods reference.
- Modify: `Makefile` (`test-manifests`: add a `workloads/05-agents/manifests` kubeconform block mirroring the existing ones).

- [ ] **Step 1: Write the manifests.** Namespace `act5-agents` (labeled for cost attribution like other acts). The agent Deployment: replicas parameterizable, `resources.requests.memory` set so packing is deterministic, `nodeSelector` pinning to the Act-5 node, and the Agent Sandbox runtime opt-in per `.../how-to/agent-sandbox` (the exact field is confirmed live in Task 6 — use the documented form and note it). Header comment explains what it is and points at `demo/act5/runbook.md`.

- [ ] **Step 2: Add to the Makefile** `test-manifests` target (copy an existing `if [ -d workloads/..]` block for `workloads/05-agents/manifests`).

- [ ] **Step 3: Validate.** `kubeconform -strict -ignore-missing-schemas -summary workloads/05-agents/manifests/` — Valid/Skipped, no Invalid/Errors. Then `PIP_INDEX_URL=https://pypi.org/simple make test` (EXIT 0).

- [ ] **Step 4: Commit.**

```bash
git add workloads/05-agents/manifests Makefile
git commit -S -m "✨ feat(act5): agent-sandbox manifests + manifest test coverage"
```

---

## Task 4: Density measurement tool (offline, TDD)

**Files:**
- Create: `advisor/internal/density/density.go`, `advisor/internal/density/density_test.go`
- Modify: `advisor/internal/cli/cli.go` (+ its command registration) to add `capacity-advisor agent-density`

**Interfaces:**
- Produces: `type Point struct{ Name string; Agents int }`; `type Report struct{ NodeHourlyUSD float64; Points []PointCost; IsolationRatio, LifecycleRatio float64 }` where `PointCost{Name string; Agents int; USDPerAgent float64}`; `func Build(nodeHourlyUSD float64, points []Point) (*Report, error)`.
- Consumes: `pricing.BillingSource.OnDemandHourlyUSD` (for `nodeHourlyUSD` in the subcommand).

- [ ] **Step 1: Write the failing test.**

```go
func TestBuildComputesPerAgentAndRatios(t *testing.T) {
	r, err := density.Build(48.0, []density.Point{
		{"baseline", 12}, {"sandbox", 16}, {"lifecycle", 48},
	})
	if err != nil { t.Fatal(err) }
	// $/agent = 48 / agents
	if got := r.Points[0].USDPerAgent; math.Abs(got-4.0) > 1e-9 {
		t.Errorf("baseline $/agent = %v want 4.0", got)
	}
	// isolation ratio = 16/12, lifecycle ratio = 48/16
	if math.Abs(r.IsolationRatio-16.0/12.0) > 1e-9 || math.Abs(r.LifecycleRatio-3.0) > 1e-9 {
		t.Errorf("ratios: iso=%v life=%v", r.IsolationRatio, r.LifecycleRatio)
	}
}

func TestBuildRejectsZeroAgents(t *testing.T) {
	if _, err := density.Build(48.0, []density.Point{{"baseline", 0}}); err == nil {
		t.Error("zero agents at a point must error, not divide by zero")
	}
}
```

- [ ] **Step 2: Run — expect FAIL.** `cd advisor && go test ./internal/density/ -v`

- [ ] **Step 3: Implement `Build`** — `$/agent = nodeHourlyUSD / agents` per point (error on `agents <= 0`); `IsolationRatio = P2.Agents / P1.Agents`, `LifecycleRatio = P3.Agents / P2.Agents` (guard needs ≥3 points; error otherwise). Render helper → `agent-density-report.md`.

- [ ] **Step 4: Run — expect PASS.** `cd advisor && go test ./internal/density/ -v`

- [ ] **Step 5: Wire the subcommand** `capacity-advisor agent-density --node-machine-type n2-standard-8 --region us-central1 --p1 N --p2 N --p3 N`: fetch `nodeHourlyUSD` via `pricing`, call `density.Build`, print the report. Match the existing `cost` subcommand's construction (read `cli.go`).

- [ ] **Step 6: Mutation.** Swap `IsolationRatio`/`LifecycleRatio` numerators/denominators; confirm the ratio test fails; revert; `git diff` empty.

- [ ] **Step 7: Full suite + commit.** `PIP_INDEX_URL=https://pypi.org/simple make test` (EXIT 0).

```bash
git add advisor/internal/density advisor/internal/cli
git commit -S -m "✨ feat(act5): agent-density tool ($/agent + lever ratios)"
```

---

## Task 5: Density harness + runbook scaffold (offline)

**Files:**
- Create: `demo/act5/run-ladder.sh`, `demo/act5/runbook.md`

- [ ] **Step 1: Write `run-ladder.sh`** (match `demo/cost/collector.sh` style; shellcheck-clean; `env -u GOOGLE_APPLICATION_CREDENTIALS` noted for ad-hoc use per the runbook convention). It: applies the agents at increasing replica counts until the node hits its RAM wall (records P1/P2 agent counts + node `MemAvailable` via `kubectl top node` / `describe`), snapshots the idle agents (the Agent Sandbox pod-snapshot command — exact form confirmed in Task 6), packs more (records P3), and calls `capacity-advisor agent-density` to emit the report. Guard: never assumes a fixed count — reads actual scheduled/pending.

- [ ] **Step 2: Write `runbook.md`** with the five beats (Pack / Reclaim / Redensify / Resume-on-demand / The bill), in the honest, evidence-not-assertion voice of the other runbooks. Leave the real numbers as a clearly-marked "filled by the Task 6 live run" block. State the new-IP/socket caveat in the Resume beat. Lead-in ties to Thread 1 (this extends the same platform thinking) without displacing it.

- [ ] **Step 3: Validate shell.** `shellcheck demo/act5/run-ladder.sh` (skip if unavailable, note it); `PIP_INDEX_URL=https://pypi.org/simple make test` (EXIT 0 — test-shell covers scripts).

- [ ] **Step 4: Commit.**

```bash
git add demo/act5
git commit -S -m "✨ feat(act5): density ladder harness + runbook scaffold"
```

---

## Task 6: Live three-point run (LIVE GATE)

Reproduce the ladder on the cluster and fill the runbook with real numbers. **Requires explicit human go-ahead** (billed: a node + snapshot storage).

**Files:**
- Modify: `demo/act5/runbook.md`

- [ ] **Step 1: Get go-ahead.**
- [ ] **Step 2: Verify enablement** — GKE Agent Sandbox + Pod snapshots on a small dedicated/ephemeral cluster or node pool in `example-sandbox` (bump within RAPID 1.36.2 if needed). Record the exact enablement + snapshot command forms (these confirm the Task 3 manifest field and the Task 5 harness command).
- [ ] **Step 3: Right-size** — a modest node; tune `--working-set-mib` so ~a dozen agents hit the RAM wall. Confirm the agent's RSS is actually resident (`kubectl exec ... cat /proc/<pid>/status | grep VmRSS`), or the reclaim is meaningless.
- [ ] **Step 4: Run the ladder** (`run-ladder.sh`), capture P1/P2/P3 agent counts + node `MemAvailable` + `$/agent`; confirm the two ratios show lifecycle ≫ isolation.
- [ ] **Step 5: Resume-on-demand** — restore one suspended agent; confirm it resumes and note the new IP / dropped connection.
- [ ] **Step 6: Tear down** — delete agents, snapshots (confirm none linger: the snapshot list is empty), and the ephemeral cluster/pool.
- [ ] **Step 7: Fill the runbook Beat** with the real captured numbers, quoting the actual output; cite Google's benchmark for the headline figures and present our smaller run as the reproduction. Honest about what a small-node run does/doesn't show.
- [ ] **Step 8: Commit.**

```bash
git add demo/act5/runbook.md
git commit -S -m "📝 docs(act5): document and reproduce the density ladder live"
```

---

## Self-Review

**Spec coverage:** §5.1 mock agent → Task 1; §5.2 manifests → Task 3; §5.3 density harness+tool → Tasks 4 (tool) + 5 (harness); §5.4 runbook five beats → Task 5 (scaffold) + Task 6 (real numbers); §4 three-point ladder → Tasks 4/5/6; §6 live plan → Task 6; §7 caveats (new-IP, RSS realism, snapshot cleanup) → Tasks 1/5/6; §8 testing → Tasks 1/4 (unit + mutation), 3 (kubeconform), 6 (live acceptance). Non-goals (§9) carried: no control-plane demo, no 1.37 primitive, no real agent (Dockerfile/manifest seam allows a later swap), no spot spine.

**Placeholder scan:** the runbook's "filled by Task 6" block is an intentional live-output slot, not a code placeholder; Tasks 3/5 note the exact Agent Sandbox field/command forms are confirmed live in Task 6 (grounded discovery, not hand-waving).

**Type consistency:** `density.Point{Name,Agents}`, `density.Build(nodeHourlyUSD float64, points []Point) (*Report, error)`, `Report{NodeHourlyUSD, Points []PointCost, IsolationRatio, LifecycleRatio}`, `PointCost{Name,Agents,USDPerAgent}` used consistently in Task 4. `pricing.OnDemandHourlyUSD` signature matches `advisor/internal/pricing/billing.go`. Agent flags (`--working-set-mib`,`--active-seconds`,`--idle-seconds`,`--state-file`) consistent across Tasks 1/3/5.
