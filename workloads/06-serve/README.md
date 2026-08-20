# vLLM Serving on Spot GPU with Snapshot-Backed Scale-to-Zero

This workload serves [Qwen/Qwen2.5-3B-Instruct](https://huggingface.co/Qwen/Qwen2.5-3B-Instruct) via an OpenAI-compatible API endpoint on Spot L4 GPUs under gVisor. It uses **GKE Pod snapshots** to checkpoint warm GPU VRAM/CUDA state to GCS, enabling fast restore-from-snapshot on scale-up (0→1) instead of cold model load, and **KEDA HTTP add-on** to scale to zero during idle periods.

## Performance

| Metric | Value | Source |
|--------|-------|--------|
| Cold load (0→1 without snapshot) | ~137s | Act 6 runbook (product) |
| Restore from snapshot (0→1 with snapshot) | ~38.8s | Act 6 runbook (product) |
| Speedup | **3.5×** faster | Act 6 runbook (product) |
| Model reload on restore? | **No** — VRAM state preserved | Act 6 runbook (product) |

The snapshot captures the entire warm state (VRAM, CUDA context, host memory) to GCS. On scale-up, GKE restores the pod from the latest snapshot instead of cold-loading the model, skipping the ~5.8GB model download and weight-loading steps.

## Prerequisites

Before applying these manifests, ensure the following infrastructure is in place:

1. **GKE cluster with Pod snapshots enabled** (see `infra/serving/snapshot-storage.md` for exact setup order — Workload Identity is a hard prerequisite).
2. **Snapshot storage bucket created** (see `infra/serving/snapshot-storage.md` for bucket creation with HNS and soft-delete settings) and **IAM configured** via `infra/09-serving-iam.sh` (grants `roles/storage.admin` to the gkenode service agent; does NOT create the bucket).
3. **GPU node pool** with gVisor and L4 GPUs (`infra/09-serving-nodepool.sh`).
4. **KEDA core and HTTP add-on installed**:

   ```bash
   helm repo add kedacore https://kedacore.github.io/charts
   helm repo update
   # Install KEDA core first (prerequisite for HTTP add-on)
   helm install keda kedacore/keda \
     --namespace keda --create-namespace
   # Then install the HTTP add-on
   helm install http-add-on kedacore/keda-add-ons-http \
     --namespace keda
   ```

   (See [KEDA HTTP Add-on docs](https://github.com/kedacore/http-add-on) for details.)

## Apply Order

Apply manifests in this exact order to satisfy dependencies:

```bash
# 1. Namespace (if not already created)
kubectl apply -f manifests/namespace.yaml

# 2. Service (required before HTTPScaledObject)
kubectl apply -f manifests/service.yaml

# 3. Deployment (creates the initial pod)
kubectl apply -f manifests/deployment.yaml

# 4. PodSnapshot configuration (StorageConfig, Policy, Trigger)
# IMPORTANT: Edit podsnapshot.yaml first — replace SNAP_BUCKET_PLACEHOLDER
# with your actual bucket name (e.g., example-sandbox-act6-snap).
kubectl apply -f manifests/podsnapshot.yaml

# 5. KEDA HTTP scaler (enables 0↔1 autoscaling)
kubectl apply -f manifests/httpscaledobject.yaml
```

### Bucket Placeholder

Before applying `podsnapshot.yaml`, replace `SNAP_BUCKET_PLACEHOLDER` with the actual GCS bucket name created by `infra/09-serving-iam.sh`. The bucket name typically follows the pattern `<PROJECT>-act6-snap` (e.g., `example-sandbox-act6-snap`).

```bash
# Example:
sed -i '' 's/SNAP_BUCKET_PLACEHOLDER/example-sandbox-act6-snap/' manifests/podsnapshot.yaml
kubectl apply -f manifests/podsnapshot.yaml
```

## Lifecycle: Snapshot-Backed Scale-to-Zero

### Scale-Down (1→0)

When no HTTP traffic arrives for 5 minutes (`scaledownPeriod: 300` in `httpscaledobject.yaml`):

1. KEDA HTTP scaler initiates scale-down.
2. Operator triggers the `PodSnapshotManualTrigger` to snapshot the warm pod.
3. GKE checkpoints the pod's VRAM/CUDA state and host memory to GCS (~113s, per Act 6 spike).
4. The `PodSnapshotPolicy` stops the pod (`postCheckpoint: stop`), freeing the GPU.

### Scale-Up (0→1)

When a new HTTP request arrives:

1. KEDA HTTP scaler detects pending traffic and scales the Deployment to 1 replica.
2. GKE **restores the pod from the latest PodSnapshot** instead of cold-starting.
3. The pod resumes with warm VRAM/CUDA state — **no model download, no weight load**.
4. The pod serves the request in ~30s (vs. ~137s cold load).

**Key insight:** The 0→1 path is **restore**, not cold start. The model never leaves VRAM between scale events; it's serialized to GCS and restored on wake.

## Operational Notes

- **Snapshot frequency:** Currently manual via `PodSnapshotManualTrigger`. For production, automate snapshot triggering before scale-down (e.g., via a pre-scale-down hook or operator).
- **Snapshot versioning:** Each snapshot is tied to a specific GKE version. Ensure the cluster version matches between snapshot creation and restore (see `infra/serving/snapshot-storage.md`).
- **Capacity failover:** The Act 6 spike observed that Spot L4 instances had capacity when on-demand was stocked-out. The node pool uses `--spot` with multi-zone locations to maximize obtainability. Snapshots persist in GCS independent of node lifetime, so snapshot→restore survives spot preemption.
- **Memory overhead:** The checkpoint copies VRAM into process memory before uploading to GCS. The deployment sets `--gpu-memory-utilization 0.5` (modest VRAM allocation) and `memory: 26Gi` (safe headroom for serialization) per the Act 6 spike's proven config.

## Troubleshooting

### Snapshot fails with IAM error

Verify the gkenode service agent has `roles/storage.admin` on the snapshot bucket:

```bash
PROJECT_NUMBER=$(gcloud projects describe <PROJECT> --format='value(projectNumber)')
GKENODE_SA="service-${PROJECT_NUMBER}@gcp-sa-gkenode.iam.gserviceaccount.com"

gcloud storage buckets add-iam-policy-binding gs://<SNAP_BUCKET> \
  --member="serviceAccount:${GKENODE_SA}" \
  --role="roles/storage.admin" \
  --project=<PROJECT>
```

### Pod fails to restore with CUDA error

Ensure the node pool was created with `gpu-driver-version=latest`. GKE's default L4 driver (535 / CUDA 12.2) is too old for current vLLM images. See `infra/09-serving-nodepool.sh`.

### KEDA scaler not scaling to zero

Check the `scaledownPeriod` in `httpscaledobject.yaml`. It must be long enough for the snapshot to complete (~113s) plus buffer. The default is 300s (5 minutes).

## References

- [Act 6 spike README](../../demo/act6/spike/README.md) — live-measured restore metrics and all discovered gotchas
- [Snapshot storage setup](../../infra/serving/snapshot-storage.md) — cluster prerequisites, IAM, bucket config
- [KEDA HTTP Add-on](https://github.com/kedacore/http-add-on) — HTTP-based autoscaling documentation
