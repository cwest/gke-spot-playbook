# Act 6 Task 1 — GPU snapshot/restore feasibility spike (HARD GATE)

**Question:** does a GKE GPU Pod snapshot/restore of a *warm* vLLM pod resume the
model from VRAM and come back **materially faster than a cold model load**, with
**no model reload**? If yes → GO (build Act 6 on snapshot-backed scale-to-zero).
If no → rescope to honest cold start.

This is throwaway/manual infra. Nothing here is the committed product path. All
infrastructure was torn down after the numbers below were captured.

## Verdict: **GO** ✅

| Metric | Value | Tag |
| --- | --- | --- |
| `cold_load_seconds` | **137 s** (pod/container start → first `/v1/completions` 200; includes ~5.8 GB model download + 8 s weight load + 28 s model load + 13 s engine init) | LIVE |
| `restore_seconds` | **30 s** (restore trigger → first `/v1/completions` 200 from the warm PodSnapshot) | LIVE |
| Model reload on restore? | **No** — proven (see Evidence) | LIVE |

Restore is **~4.5× faster** than cold load **and** performs **zero model reload**.
Both conditions for GO are met.

## Environment (all live, `example-sandbox`, `us-central1`)

- Cluster `act6-spike`, GKE `1.36.2-gke.2064000` (rapid), regional, single zone
  `us-central1-a`.
- **Pod snapshots** enabled (`--enable-pod-snapshots`) — see the hard-won
  prerequisites below.
- GPU node pool `l4-spot`: `g2-standard-8` + 1× `nvidia-l4`, `--spot`,
  `--sandbox type=gvisor`, **`gpu-driver-version=latest`** (580.126.20).
- Snapshot bucket `gs://example-sandbox-act6-snap` (HNS on, soft-delete off),
  with `roles/storage.admin` granted to the `gcp-sa-gkenode` service agent.
- Model: `Qwen/Qwen2.5-3B-Instruct` (ungated), served by `vllm/vllm-openai:latest`
  under `runtimeClassName: gvisor`, `--gpu-memory-utilization 0.5`,
  `--max-model-len 8192`, `--enforce-eager`.

## Flow

```mermaid
sequenceDiagram
  participant Op as Operator
  participant K as GKE (act6-spike)
  participant Node as L4 gVisor node
  participant GCS as GCS snapshot bucket
  Op->>K: apply vllm-pod.yaml (gvisor, 1x L4)
  Node->>Node: pull image, download model, load weights to VRAM
  Node-->>Op: /v1/completions 200  (cold_load = 137 s)
  Op->>K: apply snapshot.yaml (StorageConfig/Policy/ManualTrigger)
  K->>Node: cuda-checkpoint VRAM -> host RAM, serialize state
  Node->>GCS: write checkpoint.img, pages.img (~16 GB), pages_meta.img, metadata
  K-->>Op: PodSnapshot AllSnapshotsAvailable (~113 s), pod stopped
  Op->>K: delete pod; recreate with podsnapshot.gke.io/ps-name annotation
  K->>Node: re-inject VRAM/CUDA + host pages from snapshot (no model reload)
  Node-->>Op: /v1/completions 200  (restore = 30 s)
```

## Procedure (reproduce)

### 1. Snapshot-capable cluster (prerequisites matter — see Gotchas)

```bash
# Create a normal rapid-channel cluster FIRST, then enable Workload Identity,
# THEN enable pod snapshots. Enabling --enable-pod-snapshots at create time
# without Workload Identity fails with an opaque "Internal error".
gcloud container clusters create act6-spike \
  --project example-sandbox --region us-central1 \
  --node-locations us-central1-a --num-nodes 1 --machine-type e2-standard-4 \
  --release-channel rapid
gcloud container clusters update act6-spike --region us-central1 \
  --workload-pool=example-sandbox.svc.id.goog
gcloud container clusters update act6-spike --region us-central1 \
  --enable-pod-snapshots
```

### 2. Snapshot bucket + the Act 5 IAM lesson

```bash
gcloud storage buckets create gs://example-sandbox-act6-snap \
  --project example-sandbox --location us-central1 \
  --uniform-bucket-level-access --enable-hierarchical-namespace \
  --soft-delete-duration=0
# federatedP4SA writes the checkpoint as the gkenode service agent, NOT
# container-engine-robot. It needs storage.admin on the bucket (Act 5 lesson).
PROJECT_NUMBER=<your-project-number>  # gcloud projects describe "$PROJECT" --format='value(projectNumber)'
gcloud storage buckets add-iam-policy-binding gs://example-sandbox-act6-snap \
  --member=serviceAccount:service-${PROJECT_NUMBER}@gcp-sa-gkenode.iam.gserviceaccount.com \
  --role=roles/storage.admin --project example-sandbox
```

### 3. GPU gVisor node pool — driver version is critical

```bash
gcloud container node-pools create l4-spot --cluster act6-spike \
  --region us-central1 --project example-sandbox --node-locations us-central1-a \
  --machine-type g2-standard-8 \
  --accelerator type=nvidia-l4,count=1,gpu-driver-version=latest \
  --spot --sandbox type=gvisor --num-nodes 1
```

### 4. Cold load

```bash
kubectl create namespace act6-spike
kubectl apply -f vllm-pod.yaml           # measure container-start -> first 200
```

### 5. Snapshot the warm pod

```bash
kubectl apply -f snapshot.yaml           # StorageConfig + Policy + ManualTrigger
kubectl get podsnapshot -n act6-spike    # wait for AllSnapshotsAvailable
```

### 6. Restore and measure

```bash
kubectl delete pod vllm-spike
# recreate the identical pod spec + the restore annotation:
#   metadata.annotations["podsnapshot.gke.io/ps-name"] = <PodSnapshot name>
kubectl apply -f <pod-with-ps-name-annotation>.yaml   # measure trigger -> first 200
```

## Evidence

**Checkpoint + restore succeeded (GKE events):**

```
Successfully checkpointed the pod to PodSnapshot act6-spike/62bc2818-...
Successfully restored the pod from PodSnapshot  act6-spike/62bc2818-...
```

**Checkpoint written to GCS (IAM correct):** `checkpoint.img`, `pages.img`
(~16 GB — VRAM + host pages), `pages_meta.img`, `metadata`.

**No model reload on restore:** the restored pod served `/v1/completions` 200
with **none** of the cold-start log lines present — no `Loading safetensors
checkpoint shards`, no `Loading weights took …`, no `Model loading took …`, no
`init engine … took`, no `Starting vLLM API server`. Cold start showed all of
these; restore showed none.

**Lossless mid-stream resume (uptime proof):** the restored container started at
`17:26:40Z`; ~70 s later the in-container `vllm serve` process reported **8m25s**
of elapsed uptime (`ps -o etime`) — i.e. its clock continued from the *original*
cold start at `~17:19:25Z`. A freshly cold-started process cannot show 8 minutes
of uptime in 70 seconds. The process resumed from VRAM/host memory, it did not
restart. (Same differentiator proven for CPU pods in Act 5, now for a GPU pod.)

## Gotchas Discovered

1. **Workload Identity is a hard prerequisite for pod snapshots.**
   `--enable-pod-snapshots` at cluster-create time (without a workload pool)
   fails at `CLUSTER_CONFIGURING` with only `Internal error` in the logs.
   Create → enable Workload Identity → enable pod snapshots works.
2. **GPU driver must be `gpu-driver-version=latest`.** GKE's default L4 driver
   (535 / CUDA 12.2) is too old for `vllm/vllm-openai:latest`
   (`RuntimeError: NVIDIA driver ... too old (found version 12020)`). `latest`
   installs 580 (CUDA 13-capable).
3. **Do NOT set `VLLM_ENABLE_CUDA_COMPATIBILITY=1` on top of the latest driver.**
   The forward-compat libs then conflict with the already-new-enough driver →
   CUDA error 803 (`cudaErrorSystemDriverMismatch`) at `init_device`.
4. **Capacity: g2/L4 on-demand was region-stocked-out** across us-central1 zones
   during the spike (`ZONE_RESOURCE_POOL_EXHAUSTED`), while **spot had capacity**.
   Spot preempts (~9 min observed), but a PodSnapshot persists in GCS
   independent of node lifetime, so snapshot→restore survives preemption between
   the two. Multi-zone and region failover for the GPU pool is worth planning for
   the same reason.
5. **gVisor stdout capture is flaky** via `kubectl logs` — set
   `PYTHONUNBUFFERED=1` and use `--timestamps`; `localhost` resolves to IPv6, so
   probe `127.0.0.1` inside the pod.
6. **Budget host RAM for the VRAM serialization.** The checkpoint copies VRAM
   into process memory before uploading; keep `--gpu-memory-utilization` modest
   and the pod memory limit well above the model's VRAM (here 0.5 util / 26Gi).

## Files

- `vllm-pod.yaml` — the warm vLLM pod (gVisor, 1× L4).
- `snapshot.yaml` — PodSnapshotStorageConfig / Policy / ManualTrigger.
