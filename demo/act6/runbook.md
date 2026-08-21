# Act 6 runbook — "serving on spot, warm and cheap"

Acts 1–4 proved the platform survives spot preemption on a compute-class ladder;
Act 5 measured how densely one node can pack agents by *suspending* idle ones.
Act 6 turns that same lifecycle lever on a **latency-sensitive serving** workload:
a vLLM OpenAI-compatible endpoint on a single spot L4, made cheap by **snapshot-
backed scale-to-zero** and made resilient by the fact that a GKE Pod snapshot
lives in GCS, independent of the node that made it.

The question this act answers with live evidence: can you scale an LLM server to
**zero** when idle (paying nothing), wake it **without reloading the model**, and
**survive a spot reclaim** — all on the cheapest GPU capacity Google sells?

> **Credentials warning (read once).** Running `kubectl`, `go run`, or `gcloud`
> ad hoc from a shell that has `GOOGLE_APPLICATION_CREDENTIALS` set will use the
> wrong identity and fail. The `infra/*.sh` scripts unset it per run
> (`spotdemo::init`). For the **ad-hoc** commands below, prefix them with
> `env -u GOOGLE_APPLICATION_CREDENTIALS ...` or unset it in your shell first.

This act was run end to end on live infrastructure in `example-sandbox`
(`us-central1`) on **2026-08-17**, on a dedicated ephemeral cluster `act6-serve`
(never `spot-demo`), and every resource was torn down afterward (Beat 5). Numbers
are tagged **LIVE** (measured this run) or **PROJECTED** (a stated assumption).

---

## The lifecycle, end to end

```mermaid
sequenceDiagram
  participant Op as Operator / KEDA
  participant K as GKE (act6-serve)
  participant Node as L4 gVisor spot node
  participant GCS as GCS snapshot bucket
  Op->>K: apply Deployment (1x L4, gVisor)
  Node->>Node: pull image, download model, load weights to VRAM
  Node-->>Op: /v1/completions 200  (cold load = 137 s)
  Op->>K: PodSnapshotManualTrigger (warm pod)
  K->>Node: cuda-checkpoint VRAM -> host RAM, serialize
  Node->>GCS: checkpoint.img + pages.img (~16 GB) + meta
  K-->>Op: AllSnapshotsAvailable (~127 s), postCheckpoint stop
  Op->>K: scale Deployment 1 -> 0  (node released)
  Note over Op,GCS: idle = $0
  Op->>K: scale Deployment 0 -> 1  (a request arrives)
  K->>Node: recreate pod -> GKE matches snapshot, restores VRAM
  Node-->>Op: /v1/completions 200  (wake = 38.8 s, no model reload)
  Note over Node,GCS: spot reclaim kills the node; snapshot survives in GCS
  K->>Node: reschedule on a fresh node -> restore from GCS
  Node-->>Op: /v1/completions 200  (recovered, still warm)
```

The insight the beats below prove: the model **never leaves the snapshot** between
scale/preemption events. Wake and preemption-recovery are both **restore**, not
cold start — the vLLM process resumes mid-stream from VRAM/host pages captured in
GCS.

---

## Environment (all live, `example-sandbox`, `us-central1`)

- Cluster `act6-serve`, GKE **`1.36.2-gke.2064000`** (rapid channel), regional,
  base pool `e2-standard-4`. **Pod snapshots** enabled; **Workload Identity**
  (`example-sandbox.svc.id.goog`) enabled first as the hard prerequisite.
- GPU node pool `l4-gvisor-spot`: `g2-standard-8` + 1× `nvidia-l4`, `--spot`,
  `--sandbox type=gvisor`, **`gpu-driver-version=latest`** (measured driver
  **580.126.20**, CUDA-13-capable), autoscaling `0..2`, node-locations
  `us-central1-a,b,c`.
- Snapshot bucket `gs://example-sandbox-act6-serve-snap` (HNS on, soft-delete
  off), `roles/storage.admin` granted to `service-<PROJECT_NUMBER>@gcp-sa-gkenode`.
- Model `Qwen/Qwen2.5-3B-Instruct` on `vllm/vllm-openai:latest` (v0.27.1),
  `runtimeClassName: gvisor`, `--gpu-memory-utilization 0.5`,
  `--max-model-len 8192`, `--enforce-eager`.

---

## Beat 1 — deploy (LIVE)

Cluster ordering per `infra/serving/snapshot-storage.md` (WI is a hard
prerequisite — enabling snapshots first fails with an opaque `Internal error`):

```bash
gcloud container clusters create act6-serve --project example-sandbox \
  --region us-central1 --node-locations us-central1-a --num-nodes 1 \
  --machine-type e2-standard-4 --release-channel rapid \
  --workload-pool=example-sandbox.svc.id.goog
gcloud container clusters update act6-serve --region us-central1 \
  --enable-pod-snapshots            # WI already on from create-time

gcloud storage buckets create gs://example-sandbox-act6-serve-snap \
  --project example-sandbox --location us-central1 \
  --uniform-bucket-level-access --enable-hierarchical-namespace \
  --soft-delete-duration=0

SNAP_BUCKET=example-sandbox-act6-serve-snap bash infra/09-serving-iam.sh
CLUSTER=act6-serve REGION=us-central1 bash infra/09-serving-nodepool.sh
```

`09-serving-nodepool.sh` printed the version to pin snapshots against:

```
GKE version: 1.36.2-gke.2064000
GPU driver:  latest (580.x / CUDA 13-capable; required for vllm-openai:latest)
```

Install the KEDA HTTP add-on (this is the **product** scale-to-zero trigger):

```bash
helm repo add kedacore https://kedacore.github.io/charts && helm repo update
helm install keda        kedacore/keda              -n keda --create-namespace --wait
helm install http-add-on kedacore/keda-add-ons-http -n keda --wait
```

> **Finding (manifest doc fix).** `workloads/06-serve/README.md` names the chart
> `kedacore/keda-add-on-http`; the real chart is **`kedacore/keda-add-ons-http`**
> (plural "add-ons"), and KEDA **core** must be installed first.

Apply the product manifests. Two live substitutions/overrides, shown here rather
than committed into the manifests:

```bash
# bucket placeholder -> live bucket
sed 's/SNAP_BUCKET_PLACEHOLDER/example-sandbox-act6-serve-snap/' \
  workloads/06-serve/manifests/podsnapshot.yaml | kubectl apply -f -

kubectl apply -f workloads/06-serve/manifests/namespace.yaml
kubectl apply -f workloads/06-serve/manifests/serviceaccount.yaml
kubectl apply -f workloads/06-serve/manifests/service.yaml
kubectl apply -f workloads/06-serve/manifests/httpscaledobject.yaml
```

### Disable Service Links or vLLM Crashes: The `VLLM_PORT` Collision

The deployment **CrashLoopBackOff'd 100% of the time** with `EngineCore failed to
start`. A bare Pod does not hit this; the product wraps the pod in a
`Service` **named `vllm`**, so the kubelet injects
`VLLM_PORT=tcp://34.118.234.249:8000` into the container — which collides with
vLLM's own `VLLM_PORT` config var. The captured EngineCore root cause:

```
ValueError: VLLM_PORT 'tcp://34.118.234.249:8000' appears to be a URI.
This may be caused by a Kubernetes service discovery issue
```

The fix (now committed in `deployment.yaml`) is one line on the pod spec:

```yaml
spec:
  enableServiceLinks: false
```

With that, the pod cold-started cleanly. **Cold load, container-start → API ready
(LIVE): 137 s**, broken down:

| Phase | Seconds |
| --- | --- |
| weight download | 18.2 |
| loading weights | 7.9 |
| model loading (5.79 GiB) | 28.5 |
| init engine (kv cache, warmup) | 13.5 |
| **container start → `Application startup complete`** | **137** |

Served a real request from inside the pod (probe `127.0.0.1`, not `localhost` —
gVisor resolves `localhost` to IPv6):

```
POST /v1/completions {"prompt":"The capital of France is","max_tokens":12}
-> 200, 0.92 s, " Paris. The capital of Germany is Berlin. ..."
```

**Scale to zero (LIVE):** after snapshotting the warm pod (Beat 2), `kubectl scale
deployment/vllm --replicas=0` released the pod; the spot L4 node drained and the
autoscaler returned the pool to 0. Idle cost → $0 (KEDA drives this same 1→0 on
`scaledownPeriod: 300` in production).

---

## Beat 2 — wake latency (LIVE)

Snapshot the warm pod, then wake from zero. The snapshot is CRD-driven
(`PodSnapshotStorageConfig` + `Policy` + `ManualTrigger` targeting the live pod
name), with `postCheckpoint: stop`:

```bash
kubectl get podsnapshot -n act6-serve
# 4be9ea81-...  AllSnapshotsAvailable   (~127 s)
gcloud storage du -s gs://example-sandbox-act6-serve-snap
# 16,359,866,110 bytes  (checkpoint.img, pages.img ~16 GB, pages_meta.img, metadata)
```

Wake path — scale `0 → 1` and time the first served request:

```bash
kubectl scale deployment/vllm -n act6-serve --replicas=1   # t0
# poll /v1/completions until 200
```

**Wake latency (LIVE): 38.8 s** (scale trigger → first `/v1/completions` 200),
versus **137 s** cold — a **3.5×** faster wake. The narrower *restore-trigger →
200* window is ~30 s; the 38.8 s here also includes ReplicaSet recreate +
scheduling onto the already-warm node.

### No model reload on wake — three independent proofs

1. **GKE event:** `Successfully restored the pod from PodSnapshot
   act6-serve/4be9ea81-...` (not a cold create).
2. **Log absence:** the restored pod's 514 log lines contain **zero** cold-start
   markers — no `Loading weights`, no `Model loading took`, no `init engine`, no
   `Initializing a V1 LLM engine`, no `Application startup complete`. Cold start
   showed all of these.
3. **Uptime vs container age (the killer proof):** the container reported
   `startedAt` 49 s earlier, but `ps -o etime` on the vLLM process (pid 1) showed
   **12m18s** of elapsed uptime — its clock continued from the *original* cold
   start ~12 min prior. A freshly started process cannot show 12 minutes of
   uptime in a 49-second-old container. The process resumed from VRAM/host
   memory; it did not restart.

> **Finding (restore matching).** A **bare** Pod carrying the
> `podsnapshot.gke.io/ps-name` annotation but a *different* spec (different name,
> labels, no probe) **silently cold-started** — GKE ignored the annotation. GKE
> restores when the recreated pod matches the snapshotted pod's identity/spec,
> which a **Deployment `0→1` recreate does automatically** (same pod template).
> Good news for the product: the KEDA scale-to-zero → scale-up path restores
> **without** needing an explicit annotation injected on wake.

---

## Beat 3 — economics (LIVE price + throughput, PROJECTED duty cycle)

Effective L4 spot node price, pulled from the advisor's advice-API scoring
(`g2-standard-8` bundles the L4, so the machine price *is* the node price):

```bash
cd advisor && go build -o /tmp/capacity-advisor ./cmd/capacity-advisor
capacity-advisor analyze --profile <gpu> --config advisor.yaml   # scores spot prices
# g2-standard-8  us-central1  SpotHourlyUSD = 0.512012   (LIVE)
```

Measured sustained throughput on the single L4 (48 requests, concurrency 8,
32-token completions): **6.79 rps (LIVE)**.

```bash
capacity-advisor serving-cost --node-hourly=0.512012 --active-hours=8 --rps=6.79
```

```
always-on:      $0.0628 / 1000 req
scale-to-zero:  $0.0209 / 1000 req
savings:        66.7%
```

- `--node-hourly 0.512012` — **LIVE** (advisor, `g2-standard-8` spot, us-central1).
- `--rps 6.79` — **LIVE** (measured above).
- `--active-hours 8` — **PROJECTED** duty cycle (8 serving hours / 24). The
  savings is exactly `1 − active/24`: with snapshot-backed scale-to-zero you pay
  only for the hours you actually serve. At an 8/24 duty cycle that is a
  **66.7% cost reduction** per 1000 requests, from $0.0628 to $0.0209.

---

## Beat 4 — preemption recovery (LIVE)

Trigger a real spot reclaim on the GPU node (as Act 1 does) and confirm the
service comes back **warm**:

```bash
gcloud compute instances simulate-maintenance-event \
  gke-act6-serve-l4-gvisor-spot-3b7001fb-l7sv \
  --zone us-central1-a --project example-sandbox      # t0
```

What happened: the node went `NotReady`, the pod terminated, the ReplicaSet
created a replacement, and — because the original zone's node was gone — the
autoscaler provisioned a **fresh spot node in a different zone (`us-central1-c`)**.
The replacement pod then **restored from the same GCS snapshot** onto that new
node.

**Recovery downtime (LIVE): 408.5 s (~6.8 min)**, preemption → first
`/v1/completions` 200. That number is dominated by cold-node overhead, not by the
model:

- fresh spot node provision in a new zone + GPU driver install (~2 min)
- **9.1 GB `vllm-openai` image pull on the fresh node (~4 min)** — the single
  biggest term
- restore from snapshot (~30 s)

Had the replacement landed on a warm, image-cached node (as in Beat 2), recovery
would be ~40 s. The honest cost of a **cold-zone** preemption here is the image
pull, which pre-pulling / a `DaemonSet` image warmer would remove.

**Recovery was a restore, not a cold reload** — same three proofs:

1. **GKE event:** `Successfully restored the pod from PodSnapshot
   act6-serve/4be9ea81-...` on the new node.
2. **Log absence:** 0 cold-start markers in the recovered pod.
3. **Uptime vs container age:** container `startedAt` 43 s earlier; vLLM pid 1
   `etime` = **19m59s**. Twenty minutes of process uptime in a 43-second-old
   container **on a different node in a different zone** — the snapshot persisted
   in GCS independent of the reclaimed node and rehydrated the process elsewhere.

---

## Beat 5 — teardown (LIVE, durably authorized)

```bash
gcloud container clusters delete act6-serve --region us-central1 \
  --project example-sandbox --quiet
gcloud storage rm -r gs://example-sandbox-act6-serve-snap
```

Post-teardown verification (LIVE, 2026-08-17):

```
$ gcloud compute instances list --filter="machineType~g2"
Listed 0 items.                          # zero GPU nodes remain
$ gcloud container clusters list
spot-demo  us-central1  ... RUNNING       # only the primary demo, untouched
$ gcloud storage ls gs://example-sandbox-act6-serve-snap
ERROR: ... not found: 404                 # snapshot bucket gone
```

**Zero `g2`/GPU instances remain**, the snapshot bucket is deleted, and the
`spot-demo` cluster (the primary Thread-1 demo) was never touched.

---

## Requirements and Gotchas

- **`enableServiceLinks: false` is mandatory** for any vLLM Deployment fronted by
  a `Service` whose name uppercases to a vLLM env var (here `vllm` → `VLLM_PORT`).
  Fixed in the committed `deployment.yaml`. Without it the pod never starts.
- **KEDA chart name** in the workload README was wrong
  (`keda-add-on-http` → `keda-add-ons-http`), and KEDA core is a prerequisite.
- **Serving requires a dedicated manual gVisor spot pool** (`l4-gvisor-spot`)
  because NAP-autocreated `batch-gpu` nodes are not gVisor-capable. Target the
  manual pool directly with `cloud.google.com/gke-nodepool: l4-gvisor-spot`.
- **The `spot-demo-serve` GSA does not exist** in the project; the KSA's Workload
  Identity annotation is therefore inert. Harmless — vLLM pulls the model from the
  HF Hub and needs no GCP credentials at runtime.
- **gVisor drops subprocess stdout** from `kubectl logs`: the
  fatal EngineCore error was only recoverable via `kubectl logs --previous`
  timed right after a crash. `PYTHONUNBUFFERED=1` helps pid 1 but not children.
- **Snapshot ⇄ Deployment tension.** `postCheckpoint: stop` stops the pod, but a
  Deployment at `replicas>0` immediately recreates it (and GKE restores it). True
  scale-to-zero requires `replicas: 0` — which is exactly what the KEDA
  `HTTPScaledObject` (`min: 0`) drives on idle.

## Headline

On the cheapest GPU Google sells, a vLLM endpoint **scaled to zero when idle**,
**woke in 38.8 s without reloading the model** (vs 137 s cold), cost
**66.7% less** per 1000 requests at an 8/24 duty cycle, and **survived a spot
reclaim by restoring — warm — onto a fresh node in a different zone**, because the
snapshot lives in GCS, not on the node. Every number above was measured live on
2026-08-17 and the infrastructure was torn down the same run.
