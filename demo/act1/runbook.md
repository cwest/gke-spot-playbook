# Act 1 runbook — "spot survives"

Three beats: **advise → provision/run → survive**, then a zero-loss ledger.

> **Credentials warning (read once).** Running `kubectl`, `go run`, or `gcloud`
> ad hoc from a shell that has `GOOGLE_APPLICATION_CREDENTIALS` set will use the
> wrong identity and fail (the coder SA lacks `container`/pubsub perms →
> `Forbidden`). The `infra/*.sh` and `demo/*.sh` scripts handle this for you
> (`spotdemo::init` unsets it per run). For the **ad-hoc** commands in this
> runbook (the `kubectl apply`, the publisher `go run`) prefix them with
> `env -u GOOGLE_APPLICATION_CREDENTIALS ...` or `unset` it in your shell first.

Prereqs: `gcloud` (authed ADC on `example-sandbox`), `kubectl`, `go`,
`shellcheck`; run from the repo root.

---

## Beat 0 — Advise (~30s)

Let the advisor pick the machine type, zone, and region from live spot signals.

```bash
cd advisor && env -u GOOGLE_APPLICATION_CREDENTIALS \
  go run ./cmd/capacity-advisor analyze \
    --profile cpu-batch --config ../advisor.yaml --out ../out --render
cd ..
open out/advice-report-cpu-batch.md   # or: less out/advice-report-cpu-batch.md
```

Talk track: open `out/advice-report-cpu-batch.md` and point at

- the **preemption sparkline** (30-day daily preemption pattern per candidate),
- the **composite score** ranking, and
- the **chosen region/zone** — for cpu-batch this lands on
  **t2d-standard-8 @ us-south1-b** (obtainability 0.90, spot **$0.0554/hr**).

The winning row is written to `out/cluster-config-cpu-batch.env`. Designate it for use by later scripts:

```bash
cp out/cluster-config-cpu-batch.env out/cluster-config.env   # designate the cluster region
```

> Running the full multi-act demo? Designate the gpu-batch env instead — GPU capacity picks the cluster region; see `demo/act2/runbook.md` Beat 0.

---

## Beat 1 — Provision / run (~10 min, cluster is the long pole)

Run the infra scripts **in order**. Each is idempotent and sources
`out/cluster-config.env`.

```bash
bash infra/00-preflight.sh        # APIs enabled, ADC works, rapid channel >= min GKE version
bash infra/01-cluster.sh          # GKE Standard cluster "spot-demo" in us-south1  (~8 min)
bash infra/02-computeclasses.sh   # apply batch-cpu ComputeClass (+ batch-gpu schema dry-run)
bash infra/03-pubsub.sh           # topics, subs, GSAs, IAM, Workload Identity, image repo
bash infra/04-build.sh            # Cloud Build -> queue-worker:v1 in Artifact Registry
bash infra/05-keda.sh             # install KEDA v2.20.1, bind operator SA via WI
```

Deploy the Act 1 workload. The deployment image lives in the `us` multi-region
registry, reachable at one hostname from every US region, so the manifest
applies as-is no matter which region the cluster landed in:

```bash
source out/cluster-config.env                 # REGION, ZONES, PROJECT
export -n GOOGLE_APPLICATION_CREDENTIALS 2>/dev/null; unset GOOGLE_APPLICATION_CREDENTIALS
kubectl apply -f workloads/01-queue/manifests/namespace.yaml
kubectl apply -f workloads/01-queue/manifests/serviceaccount.yaml
kubectl apply -f workloads/01-queue/manifests/deployment.yaml
kubectl apply -f workloads/01-queue/manifests/keda.yaml   # ScaledObject + TriggerAuthentication
```

Confirm the scaler is healthy at zero (READY=True, ACTIVE=False, 0 replicas):

```bash
kubectl -n act1-queue get scaledobject,deploy
```

Start the live view in a second pane:

```bash
bash demo/watch.sh
```

`demo/watch.sh` renders three sections every 5s: **NODES** (compute-class /
spot labels), **ACT 1 WORKERS** (KEDA-driven replica count), and **BACKLOG**
(the KEDA-observed `num_undelivered_messages / target` on the managed HPA).

Seed the queue (5000 tasks, ~1.5s simulated work each):

```bash
cd workloads/01-queue/worker
env -u GOOGLE_APPLICATION_CREDENTIALS \
  go run ./cmd/publisher --project example-sandbox --count 5000 --sleep-ms 1500
cd ../../..
```

Watch KEDA go **ACTIVE**: replicas ramp `0 → N` (max 50), the `batch-cpu`
ComputeClass provisions **t2d-standard-8 spot** nodes, and pods land on them.

---

## Beat 2 — Survive (the chaos beat)

With the queue draining and workers packed on spot nodes, kill one:

```bash
bash demo/preempt.sh
```

`demo/preempt.sh` picks one `gke-spot=true,compute-class=batch-cpu` node and
sends it a `simulate-maintenance-event` (a real preemption signal). In the
`watch.sh` pane: **WORKERS replicas dip**, then recover as KEDA/GKE reschedule
the evicted pods onto surviving or newly-provisioned spot capacity — and the
**BACKLOG keeps draining**. In-flight tasks are redelivered (Pub/Sub
at-least-once + 60s ack deadline), so nothing is dropped.

> If no batch-cpu spot nodes exist yet, `preempt.sh` logs
> `no batch-cpu spot nodes found` and exits non-zero — run it only after Beat 1
> has scaled up.

---

## Verify — the zero-loss ledger

```bash
COUNT=5000 bash demo/act1/verify.sh
```

`verify.sh` drains `spot-demo-completions-verify` (a dedicated subscription, so
it never competes with the workers) and counts **distinct** `task_id` receipts.
Duplicates are expected and fine (at-least-once); the assertion is
`distinct == COUNT`:

```
>>> ZERO TASKS LOST: 5000/5000 ✔
```

A shortfall prints `MISMATCH: <distinct>/<COUNT>` and exits non-zero — check the
backlog is truly empty before concluding loss (workers may still be finishing).
`COUNT=0` against an empty queue is a valid `0/0` pass (harness sanity check).

---

## Beat 3 — the bill (cost readout)

Turn the run into an actual dollar figure with the two-factor savings report:
the **spot discount** (spot vs on-demand rates for the same node-hours) and the
**duty cycle** (elastic peak-sized usage vs an always-on on-demand pool). The
report is built from a node-inventory time series the collector samples during
the drain, so **start the collector before you publish and stop it after the
queue drains.**

Start the collector in a spare pane (it samples `kubectl get nodes` to CSV every
`COLLECT_INTERVAL` seconds; `Ctrl-C` — or `kill` — to stop):

```bash
rm -f out/cost-samples.csv   # fresh CSV: appending across runs would inflate the window
COLLECT_INTERVAL=15 bash demo/cost/collector.sh out/cost-samples.csv &
COLLECTOR_PID=$!
```

Then run Beat 1's publisher (any `--count`) and let KEDA scale up and drain the
queue as usual. Once the backlog hits 0 and replicas are scaling back toward
zero, give the pool a couple of minutes of cool-down, then stop the collector:

```bash
kill "${COLLECTOR_PID}"
```

Build the report from the samples. The `cost` subcommand prices on-demand from
the **Cloud Billing Catalog** and spot from the advisor's **capacityHistory**
signal; `--interval` must match the collector's `COLLECT_INTERVAL` so node-hours
are computed correctly (rows × interval):

```bash
cd advisor && env -u GOOGLE_APPLICATION_CREDENTIALS go run ./cmd/capacity-advisor cost \
  --region us-south1 --project example-sandbox \
  --samples ../out/cost-samples.csv --interval 15s --out ../out \
  && cat ../out/cost-report.md
cd ..
```

The report writes `out/cost-report.md` (narrative) and `out/cost-report.json`
(machine-readable). It shows the actual run cost, each savings factor, and the
combined "cheaper than the standard way" figure.

---

## Cost note

Per **node-hour**, for the advisor-chosen `t2d-standard-8` (8 vCPU / 32 GB):

| Pricing            | $/hour   | Source                                                                 |
|--------------------|----------|------------------------------------------------------------------------|
| **Spot** (us-south1-b) | **$0.0554** | live advisor run — `out/analysis-cpu-batch.json` (`SpotHourlyUSD 0.055408`) |
| On-demand (list)   | **$0.3988** | Cloud Billing Catalog API, us-south1 t2d-standard-8 (verified 2026-07) |

Spot is **~86% cheaper** (~7.2×) than on-demand for identical hardware. Note:
us-south1 carries a regional premium; the on-demand figure comes from the Cloud
Billing Catalog API (it agrees with the verified readout further down).

**Per-5000-task burst (estimate).** Workers request 500m CPU, so ~15 pods pack
onto one 8-vCPU node; KEDA's 50-replica ceiling needs **~4 nodes**. The drain
takes **~3 min** of steady-state compute (5000 × 1.5s ÷ 50 ≈ 150s), call it
**~0.1 node-hour per node** including provision + cool-down → **~0.4 node-hours**:

- **Spot:**   4 × 0.1 × $0.0554 ≈ **$0.022**
- On-demand:  4 × 0.1 × $0.3988 ≈ **$0.160**

Order-of-magnitude figures — actual node count depends on bin-packing and
scale-up latency, but the spot-vs-on-demand ratio holds.

---

## Teardown

```bash
bash infra/90-teardown.sh
```

Deletes (idempotently, safe when partial): the `spot-demo` cluster, the two
subscriptions and topics, both GSAs, and the Artifact Registry repo.

---

## Expected timings

| Phase                              | Time        |
|------------------------------------|-------------|
| Beat 0 — advisor render            | ~30s        |
| Cluster create (`01-cluster.sh`)   | **~8 min**  |
| Pub/Sub + IAM + build + KEDA       | ~3–4 min    |
| Scale-up 0→N on fresh spot nodes   | ~1–2 min    |
| Drain 5000 tasks (~1.5s / 50 wkrs) | **~3 min**  |
| Beat 3 — cost readout (collector spans the run; +~2 min cooldown; report renders in seconds) | run + ~2 min |
| Teardown                           | ~5–7 min    |

---

## Verified run — 2026-07-24 (UTC)

Live end-to-end executed against the provisioned `spot-demo` cluster. Infra
scripts 00–05 were already applied and idle-verified (idempotent; not re-run to
avoid the advisor re-picking a different zone). Sequence started at the deploy
freshness check, then publisher → scale-up → preemption → drain → verify.

**Environment**

- Cluster: `spot-demo`, region `us-south1`, control plane `v1.36.2-gke.1498000`.
- Advisor-chosen target: `t2d-standard-8` **spot**, zone `us-south1-b`,
  ComputeClass `batch-cpu`. Nodes were auto-provisioned by GKE NAP into node
  pool `gke-spot-demo-nap-t2d-standard-8-spot-*` (all `compute-class=batch-cpu`,
  `gke-spot=true`, machine type `t2d-standard-8`, zone `us-south1-b`).
- KEDA `ScaledObject` READY at 0 replicas before the run; deploy `0/0`.

**Timeline (UTC)**

| Time      | Event                                                                             |
|-----------|-----------------------------------------------------------------------------------|
| 06:08:23  | Baseline: 1 default-pool node, ScaledObject READY, deploy `0/0`.                   |
| 06:08:43  | Publisher ran; `published 5000 tasks to spot-demo-tasks` (~3s).                    |
| 06:08:53  | KEDA HPA present; external metric `<unknown>` (Pub/Sub gauge not yet ingested).    |
| 06:14:34  | KEDA activated **0→1**; first spot node `...jbzz` Ready (batch-cpu/spot, 48s old). |
| 06:16:17  | Metric resolved `1250/10`; 8 replicas.                                             |
| 06:17:09  | 30/50 replicas; 3 spot nodes Ready; HPA desired at ceiling 50.                     |
| 06:17:55  | **Peak: 50/50 replicas, all Running, 4 `t2d-standard-8` spot nodes Ready.**        |
| 06:18:16  | `preempt.sh` fired at node `...2l4v` (us-south1-b).                                |
| 06:18:31  | Dip: **45/50 available**, target node `NotReady`, 5 pods `Pending`.               |
| 06:18:57  | GKE provisioned a **5th** spot node; evicted pods rescheduled → 50 Running.        |
| 06:19:23  | Back to **50/50 available**; backlog still draining (HPA `92960m`).                |
| 06:19:49  | Preempted node `...2l4v` rejoined **Ready** after the simulated maintenance.       |
| 06:23:11  | Backlog metric `0`; ScaledObject ACTIVE `False`; HPA `0/10`.                       |
| 06:25:14  | **Scaled to zero**: deploy `0/0`, 0 pods.                                          |
| 06:27:00  | Verify: `5000 total, 5000 distinct` → ZERO TASKS LOST.                             |

**Node evidence at peak** (`kubectl get nodes -L cloud.google.com/compute-class,cloud.google.com/gke-spot`, 06:17:55):

```
NAME                                                  STATUS   ROLES    AGE    VERSION               COMPUTE-CLASS   GKE-SPOT
gke-spot-demo-default-pool-f159fda5-ckgm              Ready    <none>   67m    v1.36.2-gke.1498000                   
gke-spot-demo-nap-t2d-standard-8-spot-a8c737e4-2l4v   Ready    <none>   42s    v1.36.2-gke.1498000   batch-cpu       true
gke-spot-demo-nap-t2d-standard-8-spot-a8c737e4-gslf   Ready    <none>   71s    v1.36.2-gke.1498000   batch-cpu       true
gke-spot-demo-nap-t2d-standard-8-spot-a8c737e4-jbzz   Ready    <none>   4m8s   v1.36.2-gke.1498000   batch-cpu       true
gke-spot-demo-nap-t2d-standard-8-spot-a8c737e4-r8dh   Ready    <none>   57s    v1.36.2-gke.1498000   batch-cpu       true
```

**Preemption evidence** (`preempt.sh` output + observed recovery):

```
>>> simulating preemption of gke-spot-demo-nap-t2d-standard-8-spot-a8c737e4-2l4v (us-south1-b)
Simulating maintenance on instance(s) [...zones/us-south1-b/instances/gke-spot-demo-nap-t2d-standard-8-spot-a8c737e4-2l4v]...
......done.
>>> sent — watch the queue keep draining
```

Replicas dipped `50 → 45` available as the node went `NotReady`; 5 evicted pods
went `Pending`, a 5th spot node was auto-provisioned, and the deployment returned
to `50/50` within ~1 min. The preempted node itself rejoined `Ready` ~90s later.
The backlog never stopped draining (HPA external metric `98960m → 92960m` across
the dip). In-flight tasks were redelivered (Pub/Sub at-least-once, 60s ack
deadline) — no loss.

**Verify output (verbatim):**

```
>>> pulling completion receipts (this drains spot-demo-completions-verify)
>>> receipts: 5000 total, 5000 distinct (duplicates are fine: at-least-once)
>>> ZERO TASKS LOST: 5000/5000 ✔
```

**Headline numbers**

- Max replicas: **50/50** (KEDA ceiling reached).
- Spot nodes: **4** at steady-state peak, **5** during preemption recovery
  (all `t2d-standard-8` spot, `batch-cpu`, us-south1-b).
- Scale-up: first spot node was already Ready at KEDA activation (node pool auto-creation began on the first Pending pods); 0 → 50 in ~3.5 min.
- Drain: active compute drain (first worker 06:14:34 → backlog 0 at 06:23:11)
  ≈ **8.5 min**; wall-clock from publish to backlog-0 ≈ 14.5 min including the
  Pub/Sub `num_undelivered_messages` ingestion lag (~5.5 min) before KEDA could
  activate from zero.
- Verify: **5000/5000 distinct, zero loss.**

> **Note on the num_undelivered_messages ingestion lag.** KEDA activation waited
> ~5.5 min after publish because the Pub/Sub `num_undelivered_messages` gauge
> served its stale pre-publish value (`0`) until Cloud Monitoring ingested a
> fresh sample; KEDA scale-from-zero keys off that metric, so the HPA showed
> `<unknown>` and replicas stayed at 0 until it landed. Once resolved, scale-up
> was immediate. This is a metric-ingestion property of Pub/Sub, not a demo bug;
> for a tighter live demo, publish a small warm-up batch a few minutes ahead so
> the gauge is already non-stale.

---

#### Cost readout (2000-task re-run)

A second live run (**2000 tasks**, same mechanics) was executed with
`demo/cost/collector.sh` sampling node inventory every 15s from just before the
publish through a 2-min post-drain cool-down, then priced with
`capacity-advisor cost` (on-demand from the Cloud Billing Catalog, spot from the
advisor's capacityHistory). The Billing Catalog SKU lookup resolved live with no
code change.

**Run timeline (UTC):** collector started 14:02:11 → 2000 tasks published
14:02:22 → KEDA activated and the first `t2d-standard-8` spot node appeared at
14:08:04 (~5.5 min `num_undelivered_messages` lag again) → **peak 4 concurrent
spot nodes, 50/50 replicas** at 14:10:26 → backlog 0 / scaled to zero by 14:15:47
→ collector stopped 14:17:57. **197 samples** over a 16m1s window (60 samples of
the always-on `e2-standard-4` default pool + 137 `t2d-standard-8` spot rows
across 4 distinct spot nodes).

The verbatim report (`out/cost-report.md`):

```markdown
# Cost report — us-south1

Generated 2026-07-24T14:18:17Z | observation window 16m1s

## What this run actually cost

| machine type | lifecycle | node-hours | rate $/hr | cost |
|---|---|---|---|---|
| e2-standard-4 | on-demand | 0.250 | 0.1581 | $0.0395 |
| t2d-standard-8 | spot | 0.571 | 0.0554 | $0.0316 |

**Actual: $0.0712**

## Factor 1 — the spot discount

Same node-hours at on-demand list prices: $0.2672 → spot saved 73.4%

## Factor 2 — the duty cycle

Always-on peak-sized on-demand pool for this window: $0.4680
(≈ $42.08/day if left running) → elasticity saved 42.9%

## Combined

73.4% spot discount compounded with 42.9% duty cycle → **84.8% cheaper than the standard way**
```

That ~86% per-node spot discount lands the two incentives on the same node pool:
the advisor picked `t2d-standard-8 @ us-south1-b` because it was the most
*obtainable* spot capacity, and that same choice is what makes the run cheap. The
73.4% aggregate spot discount sits below the per-node figure because the
always-on `e2-standard-4` default/system node runs on-demand for the whole
window—it is counted honestly rather than excluded from the bill, which drags the
blended rate up toward on-demand.
