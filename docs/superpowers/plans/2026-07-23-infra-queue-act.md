# Cluster Infra + Act 1 Queue Processing Implementation Plan (Plan 2 of 4)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Stand up the advisor-informed GKE cluster (Standard, region chosen by the advisor) with the `batch-cpu` ComputeClass, and run Act 1: a Pub/Sub task queue drained by KEDA-scaled workers on spot nodes that provably lose zero tasks through a live preemption.

**Architecture:** Numbered idempotent bash scripts under `infra/` consume the advisor's `out/cluster-config.env` and `out/computeclass-cpu.yaml`. Act 1 is a small Go worker (streaming pull, one message at a time, SIGTERM drain inside the 15s spot grace window, completion receipts to a second topic) scaled 0→50 by KEDA's `gcp-pubsub` scaler via Workload Identity. A verify script proves published task IDs == distinct completed IDs after a `simulate-maintenance-event` node kill.

**Tech Stack:** bash (strict mode) + gcloud + kubectl, GKE Standard rapid channel (≥1.35.2-gke.1842000), KEDA (pinned 2.x), Go 1.24+ (`cloud.google.com/go/pubsub`, `pstest` for tests), Cloud Build + Artifact Registry, kubeconform + shellcheck in `make test`.

## Global Constraints

- Project `example-sandbox`; all GCP-touching commands run with `GOOGLE_APPLICATION_CREDENTIALS` **unset** (`env -u GOOGLE_APPLICATION_CREDENTIALS` or the common.sh guard below) — a stale export in `~/.zshenv` shadows ADC (memory: adc-env-override-gotcha).
- Cluster: GKE **Standard**, `--release-channel=rapid`, Workload Identity enabled, region/zones **only** from `out/cluster-config.env` (never hardcoded).
- GKE version floor 1.35.2-gke.1842000 (ComputeClass `priorityScore`); preflight enforces it.
- Spot workers: `terminationGracePeriodSeconds: 25`; tolerations for `cloud.google.com/gke-spot="true":NoSchedule` AND `cloud.google.com/compute-class=batch-cpu:NoSchedule`; `nodeSelector: {cloud.google.com/compute-class: batch-cpu}`.
- Naming: Pub/Sub topic `spot-demo-tasks`, subscription `spot-demo-tasks-sub` (ack deadline 60s), completions topic `spot-demo-completions`, sub `spot-demo-completions-verify`; namespace `act1-queue`; KSA `queue-worker`; GSA `spot-demo-worker@example-sandbox.iam.gserviceaccount.com`; KEDA GSA `spot-demo-keda@example-sandbox.iam.gserviceaccount.com`; AR repo `spot-demo` (region from cluster-config.env).
- All infra scripts: `#!/usr/bin/env bash`, `set -euo pipefail`, idempotent (safe to re-run), source `infra/lib/common.sh`.
- Commit style: Casey's conventional-commits-with-emoji; signed; **no AI attribution trailers**. Diagrams (if any) Mermaid only.
- Teardown must remove everything the scripts create (`infra/90-teardown.sh`), updated in the same task that creates a resource.

## File Structure

- `infra/lib/common.sh` — env guard, project/config loading, `require`, `log`, idempotency helpers
- `infra/00-preflight.sh` — auth/API/version/quota checks
- `infra/01-cluster.sh` — cluster create from cluster-config.env
- `infra/02-computeclasses.sh` — apply ComputeClass YAMLs (+ live CRD verification of the flexStart rung)
- `infra/03-pubsub.sh` — topics, subs, GSAs, IAM, WI bindings, AR repo
- `infra/04-build.sh` — Cloud Build of the worker image
- `infra/05-keda.sh` — KEDA install (pinned) + operator WI annotation + TriggerAuthentication prerequisite check
- `infra/90-teardown.sh` — full cleanup
- `workloads/01-queue/worker/` — Go module: `main.go`, `internal/work/work.go` (+ pstest tests), `Dockerfile`
- `workloads/01-queue/publisher/` — same module, `cmd`-style seeder run locally
- `workloads/01-queue/manifests/` — `namespace.yaml`, `serviceaccount.yaml`, `deployment.yaml`, `keda.yaml` (TriggerAuthentication + ScaledObject)
- `demo/act1/runbook.md`, `demo/act1/verify.sh`, `demo/watch.sh`, `demo/preempt.sh`
- `Makefile` — extend `test` with shellcheck + kubeconform + queue module tests

---

### Task 1: infra common library + preflight

**Files:**
- Create: `infra/lib/common.sh`, `infra/00-preflight.sh`
- Modify: `Makefile`
- Test: shellcheck (no bats framework — scripts are verified by shellcheck + a preflight live run at execution time)

**Interfaces:**
- Produces: `common.sh` functions used by every later script: `spotdemo::init` (unsets GOOGLE_APPLICATION_CREDENTIALS, sets `PROJECT`, sources `out/cluster-config.env` into `PROJECT/REGION/ZONES` when present), `spotdemo::log <msg>`, `spotdemo::require <cmd>`, `spotdemo::exists <cmd...>` (runs, returns 0/1, no output). Variables: `SPOTDEMO_ROOT` (repo root).

- [ ] **Step 1: Write `infra/lib/common.sh`**

```bash
#!/usr/bin/env bash
# Shared helpers for spot-demo infra scripts. Source this; do not execute.
set -euo pipefail

SPOTDEMO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

spotdemo::log() { printf '>>> %s\n' "$*" >&2; }

spotdemo::require() {
  command -v "$1" >/dev/null 2>&1 || { spotdemo::log "missing required command: $1"; exit 1; }
}

# Run a probe command silently; usable in conditionals without set -e exits.
spotdemo::exists() { "$@" >/dev/null 2>&1; }

spotdemo::init() {
  # A stale GOOGLE_APPLICATION_CREDENTIALS shadows ADC logins (see repo docs).
  if [[ -n "${GOOGLE_APPLICATION_CREDENTIALS:-}" ]]; then
    spotdemo::log "unsetting GOOGLE_APPLICATION_CREDENTIALS=${GOOGLE_APPLICATION_CREDENTIALS} for this run"
    unset GOOGLE_APPLICATION_CREDENTIALS
  fi
  PROJECT="${PROJECT:-example-sandbox}"
  local env_file="${SPOTDEMO_ROOT}/out/cluster-config.env"
  if [[ -f "${env_file}" ]]; then
    # shellcheck source=/dev/null
    source "${env_file}"
    spotdemo::log "loaded ${env_file}: REGION=${REGION:-<unset>} ZONES=${ZONES:-<unset>}"
  fi
  export PROJECT
}
```

- [ ] **Step 2: Write `infra/00-preflight.sh`**

```bash
#!/usr/bin/env bash
# Verifies the environment can run the spot demo before anything is created.
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/lib/common.sh"
spotdemo::init

for cmd in gcloud kubectl go; do spotdemo::require "$cmd"; done

spotdemo::log "checking ADC identity can list regions on ${PROJECT}"
gcloud compute regions list --project "${PROJECT}" --limit 1 --format 'value(name)' >/dev/null

REQUIRED_APIS=(container.googleapis.com compute.googleapis.com pubsub.googleapis.com
  artifactregistry.googleapis.com cloudbuild.googleapis.com monitoring.googleapis.com)
enabled="$(gcloud services list --enabled --project "${PROJECT}" --format 'value(config.name)')"
for api in "${REQUIRED_APIS[@]}"; do
  if ! grep -q "^${api}$" <<<"${enabled}"; then
    spotdemo::log "enabling ${api}"
    gcloud services enable "${api}" --project "${PROJECT}"
  fi
done

if [[ -z "${REGION:-}" ]]; then
  spotdemo::log "no out/cluster-config.env yet — run the advisor first (see infra/01-cluster.sh header). Skipping version check."
  exit 0
fi

MIN_VERSION="1.35.2-gke.1842000"
DEFAULT_VERSION="$(gcloud container get-server-config --region "${REGION}" --project "${PROJECT}" \
  --format 'value(channels.filter("channel:RAPID").extract(defaultVersion).flatten())' 2>/dev/null)"
spotdemo::log "rapid channel default in ${REGION}: ${DEFAULT_VERSION}"
if [[ "$(printf '%s\n%s\n' "${MIN_VERSION}" "${DEFAULT_VERSION}" | sort -V | head -1)" != "${MIN_VERSION}" ]]; then
  spotdemo::log "FATAL: rapid default ${DEFAULT_VERSION} < required ${MIN_VERSION}"
  exit 1
fi
spotdemo::log "preflight OK"
```

- [ ] **Step 3: Extend Makefile**

```make
.PHONY: test test-go test-shell test-manifests
test: test-go test-shell test-manifests

test-go:
	cd advisor && go test ./...
	if [ -f workloads/01-queue/worker/go.mod ]; then cd workloads/01-queue/worker && go test ./...; fi

test-shell:
	shellcheck infra/*.sh infra/lib/*.sh demo/*.sh demo/act1/*.sh 2>/dev/null || shellcheck infra/*.sh infra/lib/*.sh

test-manifests:
	if [ -d workloads/01-queue/manifests ]; then \
	  kubeconform -strict -ignore-missing-schemas -summary workloads/01-queue/manifests/; fi
```

(Replace the existing `test:` target; keep the file tab-indented.)

- [ ] **Step 4: Verify**

Run: `shellcheck infra/lib/common.sh infra/00-preflight.sh` → no findings.
Run: `bash infra/00-preflight.sh` → logs API checks, ends `preflight OK` or the documented no-env-yet skip. (`shellcheck` and `kubeconform` install via `brew install shellcheck kubeconform` if missing.)

- [ ] **Step 5: Commit**

```bash
git add infra/ Makefile
git commit -m "✨ feat(infra): common library and preflight checks"
```

---

### Task 2: cluster create + teardown skeleton

**Files:**
- Create: `infra/01-cluster.sh`, `infra/90-teardown.sh`

**Interfaces:**
- Consumes: `spotdemo::init` (Task 1), advisor artifacts in `out/` (generated at execution time).
- Produces: cluster named `spot-demo` in `${REGION}`; kubeconfig context; `infra/90-teardown.sh` that later tasks append to.

- [ ] **Step 1: Write `infra/01-cluster.sh`**

```bash
#!/usr/bin/env bash
# Creates the advisor-informed GKE Standard cluster.
# Prereq: fresh advisor artifacts —
#   cd advisor && env -u GOOGLE_APPLICATION_CREDENTIALS \
#     go run ./cmd/capacity-advisor analyze --profile cpu-batch --config ../advisor.yaml --out ../out --render
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/lib/common.sh"
spotdemo::init

CLUSTER="${CLUSTER:-spot-demo}"
[[ -n "${REGION:-}" && -n "${ZONES:-}" ]] || { spotdemo::log "run the advisor first (no cluster-config.env)"; exit 1; }

if spotdemo::exists gcloud container clusters describe "${CLUSTER}" --region "${REGION}" --project "${PROJECT}"; then
  spotdemo::log "cluster ${CLUSTER} already exists in ${REGION}"
else
  spotdemo::log "creating ${CLUSTER} in ${REGION} (zones ${ZONES})"
  gcloud container clusters create "${CLUSTER}" \
    --project "${PROJECT}" --region "${REGION}" --node-locations "${ZONES}" \
    --release-channel rapid \
    --workload-pool "${PROJECT}.svc.id.goog" \
    --num-nodes 1 --machine-type e2-standard-4 \
    --enable-ip-alias --quiet
fi
gcloud container clusters get-credentials "${CLUSTER}" --region "${REGION}" --project "${PROJECT}"
kubectl get nodes -o wide
```

- [ ] **Step 2: Write `infra/90-teardown.sh`**

```bash
#!/usr/bin/env bash
# Destroys everything the spot demo created. Idempotent; safe when partial.
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/lib/common.sh"
spotdemo::init
CLUSTER="${CLUSTER:-spot-demo}"

if [[ -n "${REGION:-}" ]] && spotdemo::exists gcloud container clusters describe "${CLUSTER}" --region "${REGION}" --project "${PROJECT}"; then
  spotdemo::log "deleting cluster ${CLUSTER} (${REGION})"
  gcloud container clusters delete "${CLUSTER}" --region "${REGION}" --project "${PROJECT}" --quiet
fi
# Later tasks append resource deletions ABOVE this line.
spotdemo::log "teardown complete"
```

- [ ] **Step 3: Verify offline, then live**

`shellcheck infra/01-cluster.sh infra/90-teardown.sh` → clean.
Live (execution-time): run the advisor prereq command from the script header, `bash infra/00-preflight.sh`, then `bash infra/01-cluster.sh`. Expected: cluster ACTIVE, `kubectl get nodes` shows 1 node per zone from `ZONES`, server version ≥ 1.35.2-gke.1842000 (`kubectl version`). Record the version in the task report.

- [ ] **Step 4: Commit**

```bash
git add infra/
git commit -m "✨ feat(infra): advisor-informed cluster create and teardown"
```

---

### Task 3: ComputeClass apply + live flexStart CRD verification

**Files:**
- Create: `infra/02-computeclasses.sh`

**Interfaces:**
- Consumes: cluster (Task 2), `out/computeclass-cpu.yaml` + `out/computeclass-gpu.yaml` (advisor artifacts; gpu one requires `analyze --profile gpu-batch` run).
- Produces: `batch-cpu` ComputeClass live in the cluster; **written verdict on the flexStart-in-priorities schema question** (deferred from Plan 1's review) in the task report.

- [ ] **Step 1: Write `infra/02-computeclasses.sh`**

```bash
#!/usr/bin/env bash
# Applies advisor-rendered ComputeClasses. batch-cpu is required; batch-gpu
# is validated server-side now (Plan 3 consumes it) and applied if present.
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/lib/common.sh"
spotdemo::init

CPU_CLASS="${SPOTDEMO_ROOT}/out/computeclass-cpu.yaml"
GPU_CLASS="${SPOTDEMO_ROOT}/out/computeclass-gpu.yaml"

[[ -f "${CPU_CLASS}" ]] || { spotdemo::log "missing ${CPU_CLASS} — run the advisor"; exit 1; }
kubectl apply --server-side -f "${CPU_CLASS}"
kubectl get computeclass batch-cpu -o yaml | head -40

if [[ -f "${GPU_CLASS}" ]]; then
  spotdemo::log "validating batch-gpu (server-side dry run: flexStart rung schema check)"
  kubectl apply --server-side --dry-run=server -f "${GPU_CLASS}"
  spotdemo::log "batch-gpu schema accepted"
fi
```

- [ ] **Step 2: Verify live (execution-time)**

```bash
cd advisor && env -u GOOGLE_APPLICATION_CREDENTIALS go run ./cmd/capacity-advisor analyze --profile gpu-batch --config ../advisor.yaml --out ../out --render && cd ..
bash infra/02-computeclasses.sh
```

Expected: `batch-cpu` created/configured; the gpu dry-run either passes (flexStart rung schema confirmed — record "CONFIRMED" in report) or the API server rejects the field (record the exact error; then fix `advisor/internal/render/computeclass.go`'s flexStart block to match the live CRD schema — `kubectl explain computeclass.spec.priorities --recursive | grep -A5 -i flex` shows the real fields — update goldens, keep advisor tests green, and include that fix in this task's commit).

**Note:** the gpu render targets whatever region gpu-batch scoring picks, which may differ from the cluster region. That's fine for a dry-run schema check; Plan 3 handles reconciling regions (single `advisor.yaml` region pin or per-profile cluster).

- [ ] **Step 3: Commit**

```bash
git add infra/ advisor/ out/.gitkeep 2>/dev/null || git add infra/ advisor/
git commit -m "✨ feat(infra): apply advisor-rendered compute classes"
```

---

### Task 4: Pub/Sub + IAM + Artifact Registry

**Files:**
- Create: `infra/03-pubsub.sh`
- Modify: `infra/90-teardown.sh` (append deletions above the marker line)

**Interfaces:**
- Consumes: `spotdemo::init`, `spotdemo::exists`.
- Produces (names later tasks depend on, from Global Constraints): topics `spot-demo-tasks` / `spot-demo-completions`; subs `spot-demo-tasks-sub` (ack 60s) / `spot-demo-completions-verify`; GSAs `spot-demo-worker@` (roles/pubsub.subscriber on tasks-sub, roles/pubsub.publisher on completions topic), `spot-demo-keda@` (roles/monitoring.viewer + roles/pubsub.viewer, project level); WI bindings worker↔`act1-queue/queue-worker`, keda↔`keda/keda-operator`; AR repo `spot-demo` in `${REGION}`.

- [ ] **Step 1: Write `infra/03-pubsub.sh`**

```bash
#!/usr/bin/env bash
# Creates Pub/Sub resources, service accounts, IAM and the image repo for Act 1.
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/lib/common.sh"
spotdemo::init
[[ -n "${REGION:-}" ]] || { spotdemo::log "REGION unset — run the advisor"; exit 1; }

ensure_topic() {
  spotdemo::exists gcloud pubsub topics describe "$1" --project "${PROJECT}" ||
    gcloud pubsub topics create "$1" --project "${PROJECT}"
}
ensure_topic spot-demo-tasks
ensure_topic spot-demo-completions

spotdemo::exists gcloud pubsub subscriptions describe spot-demo-tasks-sub --project "${PROJECT}" ||
  gcloud pubsub subscriptions create spot-demo-tasks-sub --topic spot-demo-tasks \
    --ack-deadline 60 --project "${PROJECT}"
spotdemo::exists gcloud pubsub subscriptions describe spot-demo-completions-verify --project "${PROJECT}" ||
  gcloud pubsub subscriptions create spot-demo-completions-verify --topic spot-demo-completions \
    --ack-deadline 30 --project "${PROJECT}"

ensure_gsa() {
  spotdemo::exists gcloud iam service-accounts describe "$1@${PROJECT}.iam.gserviceaccount.com" --project "${PROJECT}" ||
    gcloud iam service-accounts create "$1" --project "${PROJECT}" --display-name "$2"
}
ensure_gsa spot-demo-worker "Spot demo Act1 worker"
ensure_gsa spot-demo-keda "Spot demo KEDA scaler"

WORKER_GSA="spot-demo-worker@${PROJECT}.iam.gserviceaccount.com"
KEDA_GSA="spot-demo-keda@${PROJECT}.iam.gserviceaccount.com"

gcloud pubsub subscriptions add-iam-policy-binding spot-demo-tasks-sub \
  --member "serviceAccount:${WORKER_GSA}" --role roles/pubsub.subscriber --project "${PROJECT}" >/dev/null
gcloud pubsub topics add-iam-policy-binding spot-demo-completions \
  --member "serviceAccount:${WORKER_GSA}" --role roles/pubsub.publisher --project "${PROJECT}" >/dev/null
for role in roles/monitoring.viewer roles/pubsub.viewer; do
  gcloud projects add-iam-policy-binding "${PROJECT}" \
    --member "serviceAccount:${KEDA_GSA}" --role "${role}" --condition None >/dev/null
done

# Workload Identity: KSA -> GSA
gcloud iam service-accounts add-iam-policy-binding "${WORKER_GSA}" \
  --member "serviceAccount:${PROJECT}.svc.id.goog[act1-queue/queue-worker]" \
  --role roles/iam.workloadIdentityUser --project "${PROJECT}" >/dev/null
gcloud iam service-accounts add-iam-policy-binding "${KEDA_GSA}" \
  --member "serviceAccount:${PROJECT}.svc.id.goog[keda/keda-operator]" \
  --role roles/iam.workloadIdentityUser --project "${PROJECT}" >/dev/null

spotdemo::exists gcloud artifacts repositories describe spot-demo --location "${REGION}" --project "${PROJECT}" ||
  gcloud artifacts repositories create spot-demo --repository-format docker \
    --location "${REGION}" --project "${PROJECT}"
spotdemo::log "pubsub/iam/registry ready"
```

- [ ] **Step 2: Append to `infra/90-teardown.sh`** (above the marker comment)

```bash
for sub in spot-demo-tasks-sub spot-demo-completions-verify; do
  spotdemo::exists gcloud pubsub subscriptions describe "${sub}" --project "${PROJECT}" &&
    gcloud pubsub subscriptions delete "${sub}" --project "${PROJECT}" --quiet
done
for topic in spot-demo-tasks spot-demo-completions; do
  spotdemo::exists gcloud pubsub topics describe "${topic}" --project "${PROJECT}" &&
    gcloud pubsub topics delete "${topic}" --project "${PROJECT}" --quiet
done
for gsa in spot-demo-worker spot-demo-keda; do
  spotdemo::exists gcloud iam service-accounts describe "${gsa}@${PROJECT}.iam.gserviceaccount.com" --project "${PROJECT}" &&
    gcloud iam service-accounts delete "${gsa}@${PROJECT}.iam.gserviceaccount.com" --project "${PROJECT}" --quiet
done
if [[ -n "${REGION:-}" ]]; then
  spotdemo::exists gcloud artifacts repositories describe spot-demo --location "${REGION}" --project "${PROJECT}" &&
    gcloud artifacts repositories delete spot-demo --location "${REGION}" --project "${PROJECT}" --quiet
fi
```

- [ ] **Step 3: Verify**

`shellcheck infra/03-pubsub.sh infra/90-teardown.sh` clean; live: `bash infra/03-pubsub.sh` twice (second run must be a no-op), `gcloud pubsub topics list --project example-sandbox | grep spot-demo` shows both topics.

- [ ] **Step 4: Commit**

```bash
git add infra/
git commit -m "✨ feat(infra): pubsub, service accounts, workload identity, registry"
```

---

### Task 5: Act 1 worker (Go, TDD with pstest) + publisher + Dockerfile + build script

**Files:**
- Create: `workloads/01-queue/worker/go.mod` (module `github.com/cwest/gke-spot-instance-node-pools/workloads/01-queue/worker`), `workloads/01-queue/worker/main.go`, `workloads/01-queue/worker/internal/work/work.go`, `workloads/01-queue/worker/internal/work/work_test.go`, `workloads/01-queue/worker/Dockerfile`, `workloads/01-queue/publisher/main.go` (same module, `publisher` dir under worker module? NO — see below), `infra/04-build.sh`

Module layout: ONE module at `workloads/01-queue/worker` containing `cmd/worker/main.go`, `cmd/publisher/main.go`, and `internal/work/`. (Single go.mod keeps the image build and local publisher runs simple.)

**Interfaces:**
- Consumes: topic/sub names from Global Constraints (worker reads env `TASKS_SUB`, `COMPLETIONS_TOPIC`, `PROJECT`).
- Produces: image `${REGION}-docker.pkg.dev/${PROJECT}/spot-demo/queue-worker:v1` (Task 6 deployment references it); publisher command `go run ./cmd/publisher --project P --topic spot-demo-tasks --count 5000 --sleep-ms 1500`; each task message has attributes `task_id` (`task-%06d`) and `sleep_ms`; completions carry attribute `task_id`.

- [ ] **Step 1: Write the failing tests**

`workloads/01-queue/worker/internal/work/work_test.go`:

```go
package work

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/pubsub"
	"cloud.google.com/go/pubsub/pstest"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// harness spins up an in-memory Pub/Sub with tasks + completions plumbing.
type harness struct {
	client            *pubsub.Client
	tasksTopic        *pubsub.Topic
	tasksSub          *pubsub.Subscription
	completionsTopic  *pubsub.Topic
	completionsSub    *pubsub.Subscription
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	ctx := context.Background()
	srv := pstest.NewServer()
	t.Cleanup(func() { srv.Close() })
	conn, err := grpc.NewClient(srv.Addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	client, err := pubsub.NewClient(ctx, "p", option.WithGRPCConn(conn))
	if err != nil {
		t.Fatal(err)
	}
	tt, err := client.CreateTopic(ctx, "tasks")
	if err != nil {
		t.Fatal(err)
	}
	ts, err := client.CreateSubscription(ctx, "tasks-sub", pubsub.SubscriptionConfig{Topic: tt, AckDeadline: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	ct, err := client.CreateTopic(ctx, "completions")
	if err != nil {
		t.Fatal(err)
	}
	cs, err := client.CreateSubscription(ctx, "completions-sub", pubsub.SubscriptionConfig{Topic: ct})
	if err != nil {
		t.Fatal(err)
	}
	return &harness{client, tt, ts, ct, cs}
}

func (h *harness) publishTasks(t *testing.T, n int, sleepMS string) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < n; i++ {
		r := h.tasksTopic.Publish(ctx, &pubsub.Message{
			Data:       []byte("work"),
			Attributes: map[string]string{"task_id": fmt.Sprintf("task-%06d", i), "sleep_ms": sleepMS},
		})
		if _, err := r.Get(ctx); err != nil {
			t.Fatal(err)
		}
	}
}

func (h *harness) collectCompletions(t *testing.T, want int, timeout time.Duration) map[string]int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var mu sync.Mutex
	got := map[string]int{}
	_ = h.completionsSub.Receive(ctx, func(_ context.Context, m *pubsub.Message) {
		mu.Lock()
		got[m.Attributes["task_id"]]++
		if len(got) >= want {
			cancel()
		}
		mu.Unlock()
		m.Ack()
	})
	return got
}

func TestRunProcessesAllTasksAndPublishesCompletions(t *testing.T) {
	h := newHarness(t)
	h.publishTasks(t, 5, "10")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	w := &Worker{Sub: h.tasksSub, Completions: h.completionsTopic}
	go func() {
		time.Sleep(2 * time.Second) // let it drain, then stop like a SIGTERM would
		cancel()
	}()
	if err := w.Run(ctx); err != nil && ctx.Err() == nil {
		t.Fatal(err)
	}
	got := h.collectCompletions(t, 5, 5*time.Second)
	if len(got) != 5 {
		t.Fatalf("distinct completions = %d, want 5 (%v)", len(got), got)
	}
}

func TestRunStopsPromptlyOnCancelWithoutLosingTasks(t *testing.T) {
	h := newHarness(t)
	h.publishTasks(t, 3, "300")
	w := &Worker{Sub: h.tasksSub, Completions: h.completionsTopic}

	// First run: cancel almost immediately mid-work (simulates SIGTERM).
	ctx1, cancel1 := context.WithCancel(context.Background())
	go func() { time.Sleep(400 * time.Millisecond); cancel1() }()
	start := time.Now()
	_ = w.Run(ctx1)
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("Run did not stop promptly after cancel: %v", elapsed)
	}

	// Second run (fresh worker = replacement pod): everything still completes,
	// proving unacked tasks were redelivered, at-least-once end to end.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel2()
	go func() { time.Sleep(4 * time.Second); cancel2() }()
	_ = w.Run(ctx2)
	got := h.collectCompletions(t, 3, 5*time.Second)
	if len(got) != 3 {
		t.Fatalf("after restart, distinct completions = %d, want 3 (%v)", len(got), got)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

```bash
cd workloads/01-queue/worker && go mod init github.com/cwest/gke-spot-instance-node-pools/workloads/01-queue/worker \
  && go get cloud.google.com/go/pubsub google.golang.org/api google.golang.org/grpc
go test ./internal/work/
```
Expected: FAIL — `undefined: Worker`

- [ ] **Step 3: Implement `internal/work/work.go`**

```go
// Package work drains the tasks subscription, simulating per-task effort and
// emitting a completion receipt per task. Designed for spot nodes: Run returns
// promptly on ctx cancel (SIGTERM path) and never acks unfinished work, so
// preempted tasks are redelivered to a surviving replica.
package work

import (
	"context"
	"log"
	"strconv"
	"time"

	"cloud.google.com/go/pubsub"
)

type Worker struct {
	Sub         *pubsub.Subscription
	Completions *pubsub.Topic
}

// Run blocks until ctx is canceled. One message at a time per replica:
// horizontal scale comes from KEDA replicas, and small in-flight counts keep
// the SIGTERM drain inside the 15s spot grace window.
func (w *Worker) Run(ctx context.Context) error {
	w.Sub.ReceiveSettings.MaxOutstandingMessages = 1
	w.Sub.ReceiveSettings.NumGoroutines = 1
	return w.Sub.Receive(ctx, func(mctx context.Context, m *pubsub.Message) {
		id := m.Attributes["task_id"]
		sleepMS, err := strconv.Atoi(m.Attributes["sleep_ms"])
		if err != nil {
			sleepMS = 1000
		}
		select {
		case <-time.After(time.Duration(sleepMS) * time.Millisecond):
		case <-mctx.Done():
			m.Nack() // preemption mid-task: return it to the queue immediately
			return
		}
		res := w.Completions.Publish(mctx, &pubsub.Message{
			Data:       []byte(id),
			Attributes: map[string]string{"task_id": id},
		})
		if _, err := res.Get(mctx); err != nil {
			log.Printf("completion publish failed for %s (nacking): %v", id, err)
			m.Nack()
			return
		}
		m.Ack()
		log.Printf("done %s", id)
	})
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/work/` → PASS (both tests; second proves redelivery).

- [ ] **Step 5: Write `cmd/worker/main.go`**

```go
// queue-worker: Act 1 spot-node task consumer.
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"cloud.google.com/go/pubsub"

	"github.com/cwest/gke-spot-instance-node-pools/workloads/01-queue/worker/internal/work"
)

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

	project := env("PROJECT", "example-sandbox")
	client, err := pubsub.NewClient(ctx, project)
	if err != nil {
		log.Fatalf("pubsub client: %v", err)
	}
	defer client.Close()

	w := &work.Worker{
		Sub:         client.Subscription(env("TASKS_SUB", "spot-demo-tasks-sub")),
		Completions: client.Topic(env("COMPLETIONS_TOPIC", "spot-demo-completions")),
	}
	defer w.Completions.Stop()
	log.Printf("queue-worker starting (project=%s)", project)
	if err := w.Run(ctx); err != nil && ctx.Err() == nil {
		log.Fatalf("receive: %v", err)
	}
	log.Printf("queue-worker drained, exiting")
}
```

- [ ] **Step 6: Write `cmd/publisher/main.go`**

```go
// publisher seeds the Act 1 task queue. Run locally:
//
//	go run ./cmd/publisher --project example-sandbox --count 5000 --sleep-ms 1500
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"strconv"

	"cloud.google.com/go/pubsub"
)

func main() {
	project := flag.String("project", "example-sandbox", "GCP project")
	topicID := flag.String("topic", "spot-demo-tasks", "tasks topic")
	count := flag.Int("count", 5000, "number of tasks")
	sleepMS := flag.Int("sleep-ms", 1500, "simulated per-task work duration")
	flag.Parse()

	ctx := context.Background()
	client, err := pubsub.NewClient(ctx, *project)
	if err != nil {
		log.Fatal(err)
	}
	defer client.Close()
	topic := client.Topic(*topicID)
	defer topic.Stop()

	var results []*pubsub.PublishResult
	for i := 0; i < *count; i++ {
		results = append(results, topic.Publish(ctx, &pubsub.Message{
			Data: []byte("work"),
			Attributes: map[string]string{
				"task_id":  fmt.Sprintf("task-%06d", i),
				"sleep_ms": strconv.Itoa(*sleepMS),
			},
		}))
	}
	for i, r := range results {
		if _, err := r.Get(ctx); err != nil {
			log.Fatalf("publish %d: %v", i, err)
		}
	}
	fmt.Printf("published %d tasks to %s\n", *count, *topicID)
}
```

- [ ] **Step 7: Dockerfile + build script**

`workloads/01-queue/worker/Dockerfile`:

```dockerfile
FROM golang:1.24 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /queue-worker ./cmd/worker

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /queue-worker /queue-worker
ENTRYPOINT ["/queue-worker"]
```

`infra/04-build.sh`:

```bash
#!/usr/bin/env bash
# Builds and pushes the Act 1 worker image via Cloud Build.
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/lib/common.sh"
spotdemo::init
[[ -n "${REGION:-}" ]] || { spotdemo::log "REGION unset — run the advisor"; exit 1; }
IMAGE="${REGION}-docker.pkg.dev/${PROJECT}/spot-demo/queue-worker:v1"
gcloud builds submit "${SPOTDEMO_ROOT}/workloads/01-queue/worker" \
  --project "${PROJECT}" --tag "${IMAGE}"
spotdemo::log "pushed ${IMAGE}"
```

- [ ] **Step 8: Full verify + commit**

```bash
cd workloads/01-queue/worker && go vet ./... && go test ./... && cd ../../..
shellcheck infra/04-build.sh
git add workloads/ infra/04-build.sh
git commit -m "✨ feat(act1): pstest-verified queue worker, publisher, and image build"
```

---

### Task 6: KEDA install + Act 1 manifests

**Files:**
- Create: `infra/05-keda.sh`, `workloads/01-queue/manifests/namespace.yaml`, `.../serviceaccount.yaml`, `.../deployment.yaml`, `.../keda.yaml`

**Interfaces:**
- Consumes: image tag from Task 5, names from Global Constraints, KEDA GSA WI binding from Task 4.
- Produces: running (0-replica) Act 1 stack; `kubectl apply -f workloads/01-queue/manifests/` is the deploy command demo/runbook uses.

- [ ] **Step 1: Write `infra/05-keda.sh`**

```bash
#!/usr/bin/env bash
# Installs KEDA (pinned) and binds its operator to the scaler GSA via WI.
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/lib/common.sh"
spotdemo::init

# Resolve-and-pin: at first execution, replace LATEST lookup with the tag it
# found, so re-runs are reproducible. (Implementer: run the gh command, then
# hardcode KEDA_VERSION to its output in this file before committing.)
KEDA_VERSION="${KEDA_VERSION:-$(gh api repos/kedacore/keda/releases/latest --jq .tag_name)}"
spotdemo::log "installing KEDA ${KEDA_VERSION}"
kubectl apply --server-side -f \
  "https://github.com/kedacore/keda/releases/download/${KEDA_VERSION}/keda-${KEDA_VERSION#v}.yaml"
kubectl -n keda annotate serviceaccount keda-operator \
  "iam.gke.io/gcp-service-account=spot-demo-keda@${PROJECT}.iam.gserviceaccount.com" --overwrite
kubectl -n keda rollout status deploy/keda-operator --timeout 180s
```

- [ ] **Step 2: Manifests**

`namespace.yaml`:

```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: act1-queue
  labels:
    team: spot-demo
    act: queue
```

`serviceaccount.yaml`:

```yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: queue-worker
  namespace: act1-queue
  annotations:
    iam.gke.io/gcp-service-account: spot-demo-worker@example-sandbox.iam.gserviceaccount.com
```

`deployment.yaml`:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: queue-worker
  namespace: act1-queue
spec:
  replicas: 0 # KEDA owns the replica count
  selector:
    matchLabels: {app: queue-worker}
  template:
    metadata:
      labels: {app: queue-worker}
    spec:
      serviceAccountName: queue-worker
      terminationGracePeriodSeconds: 25
      nodeSelector:
        cloud.google.com/compute-class: batch-cpu
      tolerations:
      - {key: cloud.google.com/gke-spot, operator: Equal, value: "true", effect: NoSchedule}
      - {key: cloud.google.com/compute-class, operator: Equal, value: batch-cpu, effect: NoSchedule}
      containers:
      - name: worker
        image: REGION-docker.pkg.dev/example-sandbox/spot-demo/queue-worker:v1 # patched by runbook: sed or kustomize at deploy time
        env:
        - {name: PROJECT, value: example-sandbox}
        - {name: TASKS_SUB, value: spot-demo-tasks-sub}
        - {name: COMPLETIONS_TOPIC, value: spot-demo-completions}
        resources:
          requests: {cpu: 500m, memory: 256Mi}
          limits: {memory: 256Mi}
```

`keda.yaml`:

```yaml
apiVersion: keda.sh/v1alpha1
kind: TriggerAuthentication
metadata:
  name: keda-gcp-wi
  namespace: act1-queue
spec:
  podIdentity:
    provider: gcp
---
apiVersion: keda.sh/v1alpha1
kind: ScaledObject
metadata:
  name: queue-worker
  namespace: act1-queue
spec:
  scaleTargetRef:
    name: queue-worker
  minReplicaCount: 0
  maxReplicaCount: 50
  cooldownPeriod: 60
  triggers:
  - type: gcp-pubsub
    authenticationRef:
      name: keda-gcp-wi
    metadata:
      subscriptionName: spot-demo-tasks-sub
      mode: SubscriptionSize
      value: "10"
```

(Implementer: verify the `gcp-pubsub` trigger metadata keys and TriggerAuthentication `podIdentity.provider: gcp` against the installed KEDA version's docs — `https://keda.sh/docs/<version>/scalers/gcp-pub-sub/` — before the live deploy; adjust keys if that version renamed them and note it in your report.)

- [ ] **Step 3: Verify offline**

```bash
shellcheck infra/05-keda.sh
kubeconform -strict -ignore-missing-schemas -summary workloads/01-queue/manifests/
```
Expected: 0 invalid (ScaledObject/TriggerAuthentication skip via ignore-missing-schemas; core resources validate strictly).

- [ ] **Step 4: Verify live (execution-time)**

```bash
bash infra/05-keda.sh
sed "s|REGION-docker|${REGION}-docker|" workloads/01-queue/manifests/deployment.yaml | kubectl apply -f - \
  && kubectl apply -f workloads/01-queue/manifests/namespace.yaml -f workloads/01-queue/manifests/serviceaccount.yaml -f workloads/01-queue/manifests/keda.yaml
kubectl -n act1-queue get scaledobject queue-worker
```
Expected: ScaledObject READY=True (may take ~1 min); deployment at 0 replicas.

- [ ] **Step 5: Commit**

```bash
git add infra/05-keda.sh workloads/01-queue/manifests/
git commit -m "✨ feat(act1): keda install and workload manifests"
```

---

### Task 7: demo tooling — runbook, watch, preempt, verify

**Files:**
- Create: `demo/act1/runbook.md`, `demo/act1/verify.sh`, `demo/watch.sh`, `demo/preempt.sh`

**Interfaces:**
- Consumes: everything above.
- Produces: the three-beat Act 1 demo (advise → provision/run → survive) plus verification.

- [ ] **Step 1: `demo/watch.sh`**

```bash
#!/usr/bin/env bash
# Live split view: nodes, worker replicas, queue backlog.
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/../infra/lib/common.sh"
spotdemo::init
watch -n 5 "
echo '=== NODES (compute-class / spot) ==='
kubectl get nodes -L cloud.google.com/compute-class,cloud.google.com/gke-spot --no-headers 2>/dev/null
echo
echo '=== ACT 1 WORKERS ==='
kubectl -n act1-queue get deploy queue-worker --no-headers 2>/dev/null
echo
echo '=== BACKLOG (num_undelivered_messages, ~1min lag) ==='
gcloud monitoring time-series list --project ${PROJECT} \
  --filter 'metric.type=\"pubsub.googleapis.com/subscription/num_undelivered_messages\" AND resource.labels.subscription_id=\"spot-demo-tasks-sub\"' \
  --interval-start-time \$(date -u -v-3M +%Y-%m-%dT%H:%M:%SZ) 2>/dev/null | grep -m1 'int64Value' || echo 'no datapoints yet'
"
```

(Implementer: `gcloud monitoring time-series list` flags changed across SDK releases — run `gcloud monitoring time-series list --help` first and adapt the backlog block to the installed SDK, or fall back to `gcloud pubsub subscriptions describe` + `kubectl get scaledobject` external metrics. Keep the three-section layout.)

- [ ] **Step 2: `demo/preempt.sh`**

```bash
#!/usr/bin/env bash
# The chaos beat: preempt one random spot node serving batch-cpu.
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/../infra/lib/common.sh"
spotdemo::init

node="$(kubectl get nodes -l cloud.google.com/gke-spot=true,cloud.google.com/compute-class=batch-cpu \
  -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)"
[[ -n "${node}" ]] || { spotdemo::log "no batch-cpu spot nodes found"; exit 1; }
zone="$(kubectl get node "${node}" -o jsonpath='{.metadata.labels.topology\.kubernetes\.io/zone}')"
spotdemo::log "simulating preemption of ${node} (${zone})"
gcloud compute instances simulate-maintenance-event "${node}" --zone "${zone}" --project "${PROJECT}"
spotdemo::log "sent — watch the queue keep draining"
```

- [ ] **Step 3: `demo/act1/verify.sh`**

```bash
#!/usr/bin/env bash
# The zero-loss ledger: distinct completion receipts must equal COUNT.
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/../../infra/lib/common.sh"
spotdemo::init
COUNT="${COUNT:-5000}"

spotdemo::log "pulling completion receipts (this drains spot-demo-completions-verify)"
tmp="$(mktemp)"
trap 'rm -f "${tmp}"' EXIT
while :; do
  n="$(gcloud pubsub subscriptions pull spot-demo-completions-verify --project "${PROJECT}" \
    --limit 1000 --auto-ack --format 'value(message.attributes.task_id)' | tee -a "${tmp}" | wc -l)"
  [[ "${n}" -gt 0 ]] || break
done
distinct="$(sort -u "${tmp}" | grep -c . || true)"
total="$(grep -c . "${tmp}" || true)"
spotdemo::log "receipts: ${total} total, ${distinct} distinct (duplicates are fine: at-least-once)"
if [[ "${distinct}" -eq "${COUNT}" ]]; then
  spotdemo::log "ZERO TASKS LOST: ${distinct}/${COUNT} ✔"
else
  spotdemo::log "MISMATCH: ${distinct}/${COUNT} — check backlog before concluding loss"
  exit 1
fi
```

- [ ] **Step 4: `demo/act1/runbook.md`**

Write these sections with the exact commands from earlier tasks (no new commands): **Beat 0 — Advise** (advisor analyze --render for cpu-batch; open `out/advice-report.md`, point at the sparklines and the chosen region); **Beat 1 — Provision/run** (00→05 scripts in order; publisher seeds 5000 tasks; `demo/watch.sh` shows KEDA scaling 0→N on spot nodes); **Beat 2 — Survive** (`demo/preempt.sh`; watch replicas dip and backlog keep draining); **Verify** (`demo/act1/verify.sh` → ZERO TASKS LOST); **Cost note** (t2d spot $/hr from the live report vs on-demand; per-5000-task cost estimate); **Teardown** (`infra/90-teardown.sh`). Include expected timings (cluster ~8 min, drain ~5000×1.5s/50 workers ≈ 3 min).

- [ ] **Step 5: Verify + commit**

```bash
shellcheck demo/watch.sh demo/preempt.sh demo/act1/verify.sh
git add demo/
git commit -m "✨ feat(demo): act 1 runbook, watch, preempt, and zero-loss verify"
```

---

### Task 8: Act 1 live end-to-end (execution-time evidence)

**Files:**
- Modify: `demo/act1/runbook.md` (append "Verified run" section with real outputs)

**Interfaces:** consumes everything; produces the recorded proof.

- [ ] **Step 1: Full sequence, fresh advisor data**

```bash
cd advisor && env -u GOOGLE_APPLICATION_CREDENTIALS go run ./cmd/capacity-advisor analyze --profile cpu-batch --config ../advisor.yaml --out ../out --render && cd ..
bash infra/00-preflight.sh && bash infra/01-cluster.sh && bash infra/02-computeclasses.sh \
  && bash infra/03-pubsub.sh && bash infra/04-build.sh && bash infra/05-keda.sh
# deploy manifests (Task 6 Step 4 commands), then:
cd workloads/01-queue/worker && env -u GOOGLE_APPLICATION_CREDENTIALS go run ./cmd/publisher --project example-sandbox --count 5000 --sleep-ms 1500 && cd ../../..
```

- [ ] **Step 2: Observe scale-up** — `demo/watch.sh` until workers > 20 and spot nodes appear with `compute-class=batch-cpu`. Record `kubectl get nodes -L ...` output.

- [ ] **Step 3: Chaos** — `bash demo/preempt.sh` mid-drain. Record replica dip + recovery.

- [ ] **Step 4: Verify** — wait for backlog 0 and workers back to 0 (scale-to-zero), then `COUNT=5000 bash demo/act1/verify.sh` → `ZERO TASKS LOST: 5000/5000 ✔`. Record output verbatim.

- [ ] **Step 5: Append "Verified run" to runbook** (date, cluster version, node machine types/zones actually provisioned, drain duration, verify output), commit:

```bash
git add demo/act1/runbook.md
git commit -m "🧪 test(act1): record verified live run evidence"
```

Leave the cluster UP (Plan 3 reuses it) unless the user asks for teardown.

---

## Self-Review Notes

- **Spec coverage (Plan 2 scope):** infra scripts + preflight + teardown (§3/§7 — Tasks 1,2,4), ComputeClass apply + deferred flexStart CRD verification (§4/§Plan-1-ledger — Task 3), Act 1 worker/KEDA/manifests (§5 Act 1 — Tasks 5,6), three-beat demo + watch/preempt/cost (§6 — Task 7), live e2e evidence (§8 — Task 8). Kueue, Firestore, GPU pools: Plans 3; reconciler: Plan 4.
- **Known-drift acknowledgments baked into tasks:** KEDA version resolve-and-pin (Task 6), gcloud monitoring CLI flag drift (Task 7 watch.sh), KEDA scaler metadata key names (Task 6) — each instructs the implementer to verify against live docs and record findings rather than trust this plan.
- **Type consistency:** worker module path, image tag, topic/sub/GSA/KSA names all sourced from Global Constraints; deployment env names match `cmd/worker/main.go` (`PROJECT`, `TASKS_SUB`, `COMPLETIONS_TOPIC`).
