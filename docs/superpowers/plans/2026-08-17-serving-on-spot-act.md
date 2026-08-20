# Serving on Spot (Act 6) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Serve a small LLM (vLLM) on a single spot L4, scale it to zero when idle, and wake it fast via a GKE GPU Pod-snapshot warm restore instead of a cold model load — proving the lifecycle lever on serving.

**Architecture:** A single-replica vLLM Deployment runs under GKE Sandbox (gVisor) on a spot L4 node pool with Pod snapshots enabled. KEDA's HTTP add-on scales the Deployment 0↔1 and buffers the first request; the 0→1 path restores a warm PodSnapshot (VRAM re-injected via `cuda-checkpoint`) so the model is already resident. The same restore path recovers the service after a spot preemption. Cost is quantified with a small pure-Go helper reusing the advisor's pricing.

**Tech Stack:** GKE (Standard, GKE Sandbox/gVisor, GPU Pod snapshots), NVIDIA L4 / `g2-standard-8`, vLLM, `Qwen/Qwen2.5-3B-Instruct` (ungated), KEDA + KEDA HTTP add-on, Go 1.x (advisor), `kubeconform`, GCS.

## Global Constraints

- **Model:** `Qwen/Qwen2.5-3B-Instruct` (ungated, ~6 GB bf16 — fits 24 GB L4 with KV headroom, keeps the checkpoint small). Copied verbatim into every manifest/runbook that names a model.
- **GPU / machine:** 1× `nvidia-l4` on `g2-standard-8` (32 GB host RAM ≥ serialized VRAM overhead). Region `us-central1` (advisor preflight already checks L4 quota there).
- **Runtime:** `runtimeClassName: gvisor` on every snapshotted pod. Pod snapshots require GKE Sandbox; **E2 and MIG are unsupported**.
- **Version pinning:** snapshot and restore nodes MUST share machine series + GPU architecture + gVisor kernel + GPU driver version. The node pool definition and runbook must record the exact GKE version and driver.
- **Repo conventions:** manifests validated with `kubeconform -strict -ignore-missing-schemas`; Go code TDD-first with fakes, matching `advisor/internal/*` package style; commits use the repo's emoji + Conventional Commits style; no self-attribution in commits.
- **Honesty rule:** every number recorded in the runbook is tagged `LIVE` or `PROJECTED`.
- **No remote:** the repository has no git remote — never push or pull.

---

## File Structure

- `demo/act6/spike/` — throwaway feasibility prototype (notes + ad-hoc manifests); NOT the committed product path.
- `infra/09-serving-nodepool.sh` — creates the version-pinned gVisor GPU spot node pool with Pod snapshots enabled.
- `infra/serving/snapshot-storage.md` + `infra/09-serving-iam.sh` — GCS snapshot bucket + storage config + the `gkenode` service-agent IAM grant (Act 5 repro debt, GPU-extended).
- `workloads/06-serve/manifests/` — `namespace.yaml`, `serviceaccount.yaml`, `deployment.yaml` (vLLM), `service.yaml`, `podsnapshot.yaml` (StorageConfig/Policy/Trigger), `httpscaledobject.yaml` (KEDA HTTP).
- `advisor/internal/serving/cost.go` + `cost_test.go` — pure-Go $/1000-requests comparison.
- `advisor/internal/cli/cli.go` (+ test) — a `serving-cost` subcommand wiring the helper to real pricing.
- `demo/act6/runbook.md` — dated beats, LIVE/PROJECTED tags.

---

## Task 1: Feasibility spike (HARD GATE)

**Purpose:** Answer, cheaply and on live infra, whether a GPU Pod snapshot/restore of a warm vLLM pod resumes the model from VRAM and is meaningfully faster than a cold model load. If not, STOP and rescope to "honest cold start" (KEDA scale-to-zero without snapshot). This uses throwaway/manual infra; nothing here is the committed product.

**Files:**
- Create: `demo/act6/spike/README.md` (procedure + measured results)
- Create: `demo/act6/spike/vllm-pod.yaml` (ad-hoc single-pod manifest for the spike)

**Interfaces:**
- Consumes: nothing.
- Produces: a GO/NO-GO decision and two measured numbers — `cold_load_seconds` and `restore_seconds` — recorded in `demo/act6/spike/README.md`. Task 7 cites these; Tasks 2–6 are contingent on GO.

- [ ] **Step 1: Invoke the prototype skill.** This spike is exactly a throwaway logic prototype; use `superpowers:prototype` to run it, keeping the code out of the product path.

- [ ] **Step 2: Stand up minimal snapshot-capable infra (manual, ephemeral).**

```bash
# A snapshot-enabled Standard cluster with a gVisor L4 spot pool. Ephemeral —
# torn down at the end of the spike. Substitute PROJECT/REGION as configured.
gcloud container clusters create act6-spike \
  --project example-sandbox --region us-central1 \
  --release-channel rapid --enable-pod-snapshots
gcloud container node-pools create l4-gvisor-spot \
  --cluster act6-spike --region us-central1 --project example-sandbox \
  --machine-type g2-standard-8 --accelerator type=nvidia-l4,count=1 \
  --spot --sandbox type=gvisor --num-nodes 1
```

- [ ] **Step 3: Run vLLM warm, measure cold load.** Apply `demo/act6/spike/vllm-pod.yaml` (vLLM serving `Qwen/Qwen2.5-3B-Instruct`, `runtimeClassName: gvisor`, 1× L4). Time from pod start to first successful `/v1/completions` 200. Record as `cold_load_seconds`.

- [ ] **Step 4: Snapshot the warm pod, then restore.** Trigger a Pod snapshot (per the GKE Pod-snapshot how-to), delete the pod, then restore it. Time from restore trigger to first successful `/v1/completions` 200. Record as `restore_seconds`. Confirm via vLLM logs that NO model (re)load occurred on restore (weights already resident).

- [ ] **Step 5: Record the verdict.** Write both numbers and a GO/NO-GO to `demo/act6/spike/README.md`. GO if `restore_seconds` is materially below `cold_load_seconds` AND no reload happened. Tag both numbers `LIVE`.

- [ ] **Step 6: Tear down the ephemeral cluster.**

```bash
gcloud container clusters delete act6-spike --region us-central1 --project example-sandbox --quiet
```

- [ ] **Step 7: Commit the spike record.**

```bash
git add demo/act6/spike/
git commit -m "🧪 test(act6): feasibility spike — GPU snapshot restore vs cold load"
```

- [ ] **GATE:** If NO-GO, stop and return to brainstorming to rescope the act. Do not start Task 2.

---

## Task 2: Cost model — $/1000 requests (pure Go, TDD)

**Purpose:** The dollar story is a pure function, testable without any cloud. Build it first so later live beats just feed it real numbers.

**Files:**
- Create: `advisor/internal/serving/cost.go`
- Test: `advisor/internal/serving/cost_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `type Scenario struct { Name string; NodeHourlyUSD float64; BilledHoursPerDay float64; ActiveHoursPerDay float64; RequestsPerSecond float64 }`
  - `func CostPer1000(s Scenario) float64` — USD per 1000 requests.
  - `func Savings(base, alt Scenario) float64` — fractional reduction `(base−alt)/base` of `CostPer1000`.

- [ ] **Step 1: Write the failing test.**

```go
package serving

import (
	"math"
	"testing"
)

func approx(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

func TestCostPer1000_alwaysOn(t *testing.T) {
	// $1/hr node billed 24h/day, active 8h/day at 10 req/s.
	// daily requests = 10*3600*8 = 288000; daily cost = 24.
	// per-1000 = 24/288000*1000 = 0.0833333...
	s := Scenario{NodeHourlyUSD: 1, BilledHoursPerDay: 24, ActiveHoursPerDay: 8, RequestsPerSecond: 10}
	if got := CostPer1000(s); !approx(got, 24.0/288000.0*1000.0) {
		t.Fatalf("CostPer1000 = %v", got)
	}
}

func TestCostPer1000_scaleToZeroBilledOnlyWhenActive(t *testing.T) {
	// Same node/traffic, billed only the 8 active hours.
	// daily cost = 8; per-1000 = 8/288000*1000.
	s := Scenario{NodeHourlyUSD: 1, BilledHoursPerDay: 8, ActiveHoursPerDay: 8, RequestsPerSecond: 10}
	if got := CostPer1000(s); !approx(got, 8.0/288000.0*1000.0) {
		t.Fatalf("CostPer1000 = %v", got)
	}
}

func TestSavings(t *testing.T) {
	base := Scenario{NodeHourlyUSD: 1, BilledHoursPerDay: 24, ActiveHoursPerDay: 8, RequestsPerSecond: 10}
	alt := Scenario{NodeHourlyUSD: 1, BilledHoursPerDay: 8, ActiveHoursPerDay: 8, RequestsPerSecond: 10}
	if got := Savings(base, alt); !approx(got, 1.0-8.0/24.0) {
		t.Fatalf("Savings = %v", got)
	}
}

func TestCostPer1000_zeroRequestsIsSafe(t *testing.T) {
	s := Scenario{NodeHourlyUSD: 1, BilledHoursPerDay: 24, ActiveHoursPerDay: 0, RequestsPerSecond: 0}
	if got := CostPer1000(s); !math.IsInf(got, 1) {
		t.Fatalf("expected +Inf for zero throughput, got %v", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails.**

Run: `cd advisor && go test ./internal/serving/ -run TestCost -v`
Expected: FAIL (package/functions undefined).

- [ ] **Step 3: Write minimal implementation.**

```go
// Package serving computes the dollar case for scale-to-zero LLM serving:
// billing only for hours a replica actually runs, amortized over the requests
// served in the active window.
package serving

import "math"

type Scenario struct {
	Name              string
	NodeHourlyUSD     float64 // effective hourly node price (spot or on-demand)
	BilledHoursPerDay float64 // hours/day a replica is billed (24 always-on; active-only for scale-to-zero)
	ActiveHoursPerDay float64 // hours/day actually serving traffic
	RequestsPerSecond float64 // sustained throughput while active
}

// CostPer1000 returns USD per 1000 requests. +Inf when no requests are served.
func CostPer1000(s Scenario) float64 {
	dailyRequests := s.RequestsPerSecond * 3600 * s.ActiveHoursPerDay
	if dailyRequests == 0 {
		return math.Inf(1)
	}
	dailyCost := s.NodeHourlyUSD * s.BilledHoursPerDay
	return dailyCost / dailyRequests * 1000
}

// Savings is the fractional reduction in CostPer1000 of alt versus base.
func Savings(base, alt Scenario) float64 {
	b := CostPer1000(base)
	return (b - CostPer1000(alt)) / b
}
```

- [ ] **Step 4: Run test to verify it passes.**

Run: `cd advisor && go test ./internal/serving/ -v`
Expected: PASS (all four tests).

- [ ] **Step 5: Commit.**

```bash
git add advisor/internal/serving/
git commit -m "✨ feat(act6): add scale-to-zero serving cost model"
```

---

## Task 3: `serving-cost` subcommand (Go, TDD)

**Purpose:** Expose the cost model on the advisor CLI so the runbook computes real figures with one command.

**Files:**
- Modify: `advisor/internal/cli/cli.go`
- Test: `advisor/internal/cli/cli_test.go`
- Modify (wire subcommand): `advisor/cmd/capacity-advisor/main.go`

**Interfaces:**
- Consumes: `serving.Scenario`, `serving.CostPer1000`, `serving.Savings` (Task 2).
- Produces: `func RunServingCost(args []string, stdout io.Writer) error` — parses `--node-hourly`, `--active-hours`, `--rps` flags, prints per-1000 cost for always-on vs scale-to-zero and the savings percentage.

- [ ] **Step 1: Write the failing test.**

```go
func TestRunServingCost_printsSavings(t *testing.T) {
	var out bytes.Buffer
	err := RunServingCost([]string{"--node-hourly=0.70", "--active-hours=8", "--rps=10"}, &out)
	if err != nil {
		t.Fatal(err)
	}
	s := out.String()
	if !strings.Contains(s, "always-on") || !strings.Contains(s, "scale-to-zero") || !strings.Contains(s, "savings") {
		t.Fatalf("missing expected sections: %q", s)
	}
}
```

- [ ] **Step 2: Run test to verify it fails.**

Run: `cd advisor && go test ./internal/cli/ -run TestRunServingCost -v`
Expected: FAIL (RunServingCost undefined).

- [ ] **Step 3: Write minimal implementation** in `cli.go`.

```go
// RunServingCost prints the $/1000-request comparison for always-on vs
// scale-to-zero serving at the given node price, duty cycle, and throughput.
func RunServingCost(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("serving-cost", flag.ContinueOnError)
	nodeHourly := fs.Float64("node-hourly", 0, "effective node $/hr")
	activeHours := fs.Float64("active-hours", 8, "hours/day actually serving")
	rps := fs.Float64("rps", 1, "sustained requests/sec while active")
	if err := fs.Parse(args); err != nil {
		return err
	}
	always := serving.Scenario{NodeHourlyUSD: *nodeHourly, BilledHoursPerDay: 24, ActiveHoursPerDay: *activeHours, RequestsPerSecond: *rps}
	s2z := serving.Scenario{NodeHourlyUSD: *nodeHourly, BilledHoursPerDay: *activeHours, ActiveHoursPerDay: *activeHours, RequestsPerSecond: *rps}
	fmt.Fprintf(stdout, "always-on:      $%.4f / 1000 req\n", serving.CostPer1000(always))
	fmt.Fprintf(stdout, "scale-to-zero:  $%.4f / 1000 req\n", serving.CostPer1000(s2z))
	fmt.Fprintf(stdout, "savings:        %.1f%%\n", serving.Savings(always, s2z)*100)
	return nil
}
```

- [ ] **Step 4: Run test to verify it passes.**

Run: `cd advisor && go test ./internal/cli/ -run TestRunServingCost -v`
Expected: PASS.

- [ ] **Step 5: Wire into `main.go`** — add a `serving-cost` case dispatching to `cli.RunServingCost(os.Args[2:], os.Stdout)`, mirroring the existing subcommand switch.

- [ ] **Step 6: Run the full advisor suite.**

Run: `cd advisor && go test ./...`
Expected: PASS.

- [ ] **Step 7: Commit.**

```bash
git add advisor/internal/cli/ advisor/cmd/capacity-advisor/main.go
git commit -m "✨ feat(act6): add serving-cost subcommand"
```

---

## Task 4: Infra — version-pinned gVisor GPU spot node pool + snapshots

**Purpose:** Make the snapshot-capable environment reproducible from committed scripts (closing Act 5's "prose-only setup" debt, GPU-extended).

**Files:**
- Create: `infra/09-serving-nodepool.sh`
- Create: `infra/09-serving-iam.sh`
- Create: `infra/serving/snapshot-storage.md`

**Interfaces:**
- Consumes: an existing snapshot-enabled cluster (documented in `snapshot-storage.md`) or the flags to enable it.
- Produces: a node pool `l4-gvisor-spot` and a GCS snapshot bucket with the `gkenode` grant; consumed operationally by Task 7.

- [ ] **Step 1: Write `infra/09-serving-nodepool.sh`** creating the pinned pool.

```bash
#!/usr/bin/env bash
set -euo pipefail
source "$(dirname "$0")/lib.sh"   # reuse spotdemo::init/log/exists per existing infra scripts
spotdemo::init
# Snapshots require --enable-pod-snapshots on the cluster (see infra/serving/snapshot-storage.md).
gcloud container node-pools create l4-gvisor-spot \
  --cluster "${CLUSTER}" --region "${REGION}" --project "${PROJECT}" \
  --machine-type g2-standard-8 --accelerator type=nvidia-l4,count=1,gpu-driver-version=default \
  --spot --sandbox type=gvisor --num-nodes 0 --enable-autoscaling --min-nodes 0 --max-nodes 2
spotdemo::log "record GKE version + driver for restore-matching:"
gcloud container clusters describe "${CLUSTER}" --region "${REGION}" --project "${PROJECT}" \
  --format='value(currentMasterVersion)'
```

- [ ] **Step 2: Write `infra/09-serving-iam.sh`** granting the snapshot bucket to the `gkenode` service agent.

```bash
#!/usr/bin/env bash
set -euo pipefail
source "$(dirname "$0")/lib.sh"
spotdemo::init
: "${SNAP_BUCKET:?set SNAP_BUCKET to the snapshot GCS bucket}"
PROJECT_NUMBER="$(gcloud projects describe "${PROJECT}" --format='value(projectNumber)')"
GKENODE_SA="service-${PROJECT_NUMBER}@gcp-sa-gkenode.iam.gserviceaccount.com"
gcloud storage buckets add-iam-policy-binding "gs://${SNAP_BUCKET}" \
  --member="serviceAccount:${GKENODE_SA}" --role="roles/storage.admin"
spotdemo::log "granted storage.admin on ${SNAP_BUCKET} to ${GKENODE_SA}"
```

- [ ] **Step 3: Write `infra/serving/snapshot-storage.md`** — document `--enable-pod-snapshots`, the `PodSnapshotStorageConfig` with `tokenSource: federatedP4SA`, the bucket creation, and the version-matching requirement for restore. Include the exact `gcloud container clusters create/update ... --enable-pod-snapshots` command.

- [ ] **Step 4: Lint the scripts.**

Run: `bash -n infra/09-serving-nodepool.sh && bash -n infra/09-serving-iam.sh`
Expected: no syntax errors.

- [ ] **Step 5: Commit.**

```bash
git add infra/09-serving-nodepool.sh infra/09-serving-iam.sh infra/serving/
git commit -m "✨ feat(act6): reproducible gVisor GPU spot node pool + snapshot IAM"
```

---

## Task 5: Workload manifests — vLLM Deployment, Service, SA, namespace

**Purpose:** The committed serving workload, validated statically.

**Files:**
- Create: `workloads/06-serve/manifests/namespace.yaml`
- Create: `workloads/06-serve/manifests/serviceaccount.yaml`
- Create: `workloads/06-serve/manifests/deployment.yaml`
- Create: `workloads/06-serve/manifests/service.yaml`
- Modify: `Makefile` (add `workloads/06-serve/manifests` to the `kubeconform` sweep)

**Interfaces:**
- Consumes: node pool from Task 4.
- Produces: a `vllm` Deployment (1 replica, `runtimeClassName: gvisor`, compute-class + `nvidia.com/gpu: 1`, serving `Qwen/Qwen2.5-3B-Instruct`) and a `vllm` ClusterIP Service on port 8000; consumed by Tasks 6 and 7.

- [ ] **Step 1: Write the manifests.** `deployment.yaml` mirrors `workloads/02-embeddings/manifests/job.yaml`'s GPU/compute-class/toleration pattern but as a `Deployment` with `runtimeClassName: gvisor`, a vLLM container (`--model Qwen/Qwen2.5-3B-Instruct --max-model-len` sized for L4), a readiness probe on `/health`, and memory limits budgeting VRAM-serialization overhead. `service.yaml` exposes port 8000.

- [ ] **Step 2: Validate (expect fail first if a field is malformed, then pass).**

Run: `kubeconform -strict -ignore-missing-schemas -summary workloads/06-serve/manifests/`
Expected: `Invalid: 0, Errors: 0`.

- [ ] **Step 3: Add the directory to the Makefile `kubeconform` sweep** alongside the existing `workloads/05-agents/manifests` entry.

- [ ] **Step 4: Run the manifest check via make.**

Run: `make test 2>&1 | grep -A2 06-serve`
Expected: the 06-serve summary shows all valid.

- [ ] **Step 5: Commit.**

```bash
git add workloads/06-serve/manifests/namespace.yaml workloads/06-serve/manifests/serviceaccount.yaml workloads/06-serve/manifests/deployment.yaml workloads/06-serve/manifests/service.yaml Makefile
git commit -m "✨ feat(act6): vLLM serving workload manifests"
```

---

## Task 6: Snapshot CRDs + KEDA HTTP scale-to-zero

**Purpose:** The lifecycle machinery — periodic warm snapshot + request-driven 0↔1 with restore on wake.

**Files:**
- Create: `workloads/06-serve/manifests/podsnapshot.yaml` (StorageConfig, Policy, Trigger)
- Create: `workloads/06-serve/manifests/httpscaledobject.yaml` (KEDA HTTP add-on)
- Create: `workloads/06-serve/README.md` (apply order, KEDA HTTP add-on install pointer, restore-on-wake note)

**Interfaces:**
- Consumes: the `vllm` Deployment + Service (Task 5), the snapshot bucket/config (Task 4).
- Produces: an `HTTPScaledObject` targeting the `vllm` Deployment (min 0, max 1) and `PodSnapshot*` objects that snapshot the warm pod before scale-down; consumed operationally by Task 7.

- [ ] **Step 1: Write `podsnapshot.yaml`** — `PodSnapshotStorageConfig` (bucket + `tokenSource: federatedP4SA`), a `PodSnapshotPolicy` selecting the `vllm` pod, and a `PodSnapshotManualTrigger` (per the GKE Pod-snapshot CRD schema recorded in Act 5's runbook and `infra/serving/snapshot-storage.md`).

- [ ] **Step 2: Write `httpscaledobject.yaml`** — KEDA `HTTPScaledObject` routing the vLLM host to the `vllm` Service, `replicas: min 0 / max 1`, with the scaledown/`idle` window that triggers snapshot-then-zero.

- [ ] **Step 3: Validate manifests.**

Run: `kubeconform -strict -ignore-missing-schemas -summary workloads/06-serve/manifests/`
Expected: CRD kinds are `Skipped` (no schema) but `Errors: 0`.

- [ ] **Step 4: Write `workloads/06-serve/README.md`** — exact apply order, the KEDA HTTP add-on install command, and how the 0→1 path restores the latest PodSnapshot (so wake ≠ cold load).

- [ ] **Step 5: Commit.**

```bash
git add workloads/06-serve/manifests/podsnapshot.yaml workloads/06-serve/manifests/httpscaledobject.yaml workloads/06-serve/README.md
git commit -m "✨ feat(act6): snapshot CRDs + KEDA HTTP scale-to-zero"
```

---

## Task 7: Live runbook — economics, wake latency, preemption recovery

**Purpose:** Run the committed stack end-to-end on live infra and record the reader-facing proof, each number LIVE or PROJECTED.

**Files:**
- Create: `demo/act6/runbook.md`

**Interfaces:**
- Consumes: everything from Tasks 2–6, and the spike verdict from Task 1.
- Produces: dated beats with measured results.

- [ ] **Step 1: Beat 1 — deploy.** Run `infra/09-serving-nodepool.sh` + `infra/09-serving-iam.sh`, install the KEDA HTTP add-on, apply `workloads/06-serve/manifests/`. Record the pinned GKE version + driver. Confirm the service serves a request and then scales to zero (node released).

- [ ] **Step 2: Beat 2 — wake latency (LIVE).** From scaled-to-zero, send one request; measure first-response latency (restore path). Compare to the spike's `cold_load_seconds`. Confirm via vLLM logs no model reload on wake. Tag `LIVE`.

- [ ] **Step 3: Beat 3 — economics (LIVE).** Pull the effective L4 spot node price via the advisor, then run `capacity-advisor serving-cost --node-hourly=<spot> --active-hours=<observed> --rps=<measured>`. Record always-on vs scale-to-zero $/1000-req and savings %. Tag figures `LIVE` (price + throughput measured) and note any `PROJECTED` duty-cycle assumption.

- [ ] **Step 4: Beat 4 — preemption recovery (LIVE).** Trigger a spot reclaim (`gcloud compute instances simulate-maintenance-event` on the node, as Act 1 does). Confirm the service recovers via restore (warm), record downtime, and confirm no cold reload. Tag `LIVE`.

- [ ] **Step 5: Beat 5 — teardown.** Scale to zero / delete the node pool; confirm no lingering GPU nodes (cost hygiene). Note teardown is durably authorized.

- [ ] **Step 6: Commit.**

```bash
git add demo/act6/runbook.md
git commit -m "📝 docs(act6): live runbook — wake latency, economics, preemption recovery"
```

---

## Self-Review

**Spec coverage:**
- §1 goal (lifecycle lever on serving) → Tasks 1, 6, 7.
- §2 GPU-snapshot feasibility unproven-here → Task 1 hard gate.
- §3 in-scope: vLLM 1×L4 → Task 5; KEDA scale-to-zero → Task 6; snapshot restore → Tasks 1/6/7; preemption recovery → Task 7 Beat 4; cost → Tasks 2/3, Task 7 Beat 3; reproducible infra → Task 4; runbook → Task 7.
- §3 out-of-scope (HA, A100/Act 7, multi-GPU) → not present in any task. ✓
- §6 validation steps 1–4 → Task 1 (spike) + Task 7 Beats 2–4. ✓
- §7 dependencies (Act 5 repro debt) → Task 4. Version pinning → Task 4 Step 1 + Global Constraints. ✓
- §8 testing (kubeconform + Go TDD) → Tasks 2/3 (Go), 5/6 (kubeconform). ✓

**Placeholder scan:** No "TBD/TODO". Manifest bodies in Tasks 5–6 are described by pattern + exact validation command rather than full YAML, because they are copy-adapted from named existing manifests (`workloads/02-embeddings`, `05-agents`) and the CRD schemas are external; the acceptance gate (kubeconform + the live beats) is concrete. Go steps carry full code.

**Type consistency:** `Scenario` fields (`NodeHourlyUSD`, `BilledHoursPerDay`, `ActiveHoursPerDay`, `RequestsPerSecond`), `CostPer1000`, `Savings`, and `RunServingCost` are used identically across Tasks 2 and 3. ✓
