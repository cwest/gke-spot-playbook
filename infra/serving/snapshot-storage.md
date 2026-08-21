# GKE Pod Snapshots — Setup for LLM Serving (Act 6)

This document covers enabling GKE Pod snapshots for GPU workloads, configuring
snapshot storage, and the version-matching requirements for successful restore.

## Prerequisites (Critical Ordering)

**IMPORTANT:** Workload Identity is a **hard prerequisite** for Pod snapshots.
Enabling `--enable-pod-snapshots` at cluster-create time without a workload pool
fails with an opaque `Internal error` during `CLUSTER_CONFIGURING`.

The **required order** is:

1. **Create the cluster** (rapid channel recommended for snapshot features)
2. **Enable Workload Identity** (`--workload-pool`)
3. **Enable Pod snapshots** (`--enable-pod-snapshots`)

Attempting to skip step 2 or reorder these steps will fail.

### 1. Create Cluster

```bash
gcloud container clusters create <CLUSTER_NAME> \
  --project <PROJECT> \
  --region <REGION> \
  --node-locations <ZONE1>,<ZONE2>,<ZONE3> \
  --num-nodes 1 \
  --machine-type e2-standard-4 \
  --release-channel rapid
```

### 2. Enable Workload Identity

```bash
gcloud container clusters update <CLUSTER_NAME> \
  --region <REGION> \
  --project <PROJECT> \
  --workload-pool=<PROJECT>.svc.id.goog
```

### 3. Enable Pod Snapshots

```bash
gcloud container clusters update <CLUSTER_NAME> \
  --region <REGION> \
  --project <PROJECT> \
  --enable-pod-snapshots
```

## Snapshot Storage Bucket

Create a GCS bucket with Hierarchical Namespace (HNS) enabled and soft-delete
disabled (snapshot images are large; soft-delete doubles storage cost).

The bucket name typically follows the pattern `<PROJECT>-act6-snap` (e.g.,
`example-sandbox-act6-snap`):

```bash
gcloud storage buckets create gs://<PROJECT>-act6-snap \
  --project <PROJECT> \
  --location <REGION> \
  --uniform-bucket-level-access \
  --enable-hierarchical-namespace \
  --soft-delete-duration=0
```

## IAM for federatedP4SA Token Source

With `tokenSource: federatedP4SA` in the `PodSnapshotStorageConfig` (recommended
for managed identity), the checkpoint image is written to GCS by the **gkenode
service agent** (`service-<PROJECT_NUMBER>@gcp-sa-gkenode.iam.gserviceaccount.com`),
**NOT** the container-engine-robot service account.

The gkenode SA must hold `roles/storage.admin` on the snapshot bucket. Grant it:

```bash
PROJECT_NUMBER=$(gcloud projects describe <PROJECT> --format='value(projectNumber)')
GKENODE_SA="service-${PROJECT_NUMBER}@gcp-sa-gkenode.iam.gserviceaccount.com"

gcloud storage buckets add-iam-policy-binding gs://<PROJECT>-act6-snap \
  --member="serviceAccount:${GKENODE_SA}" \
  --role="roles/storage.admin" \
  --project=<PROJECT>
```

(This is automated by `infra/09-serving-iam.sh`.)

## PodSnapshotStorageConfig

Deploy the storage config referencing your bucket and the `federatedP4SA` token
source (ensures IAM is managed by GKE Workload Identity without manual key
creation):

```yaml
apiVersion: podsnapshot.gke.io/v1
kind: PodSnapshotStorageConfig
metadata:
  name: <CONFIG_NAME>
  namespace: <NAMESPACE>
spec:
  snapshotStorageConfig:
    gcs:
      bucket: <PROJECT>-act6-snap
      tokenSource: federatedP4SA
```

Then define a `PodSnapshotPolicy` referencing this storage config and a
`PodSnapshotManualTrigger` to snapshot a specific pod (see the spike example in
`demo/act6/spike/snapshot.yaml`).

## Version Matching for Restore

A PodSnapshot captures the checkpoint at a **specific GKE version**. Restoring to
a cluster running a **different GKE version** may fail or behave unpredictably.

**Best practice:** pin the cluster to a specific GKE version before creating
snapshots, and verify the version before restoring:

```bash
# Record the version when snapshotting
gcloud container clusters describe <CLUSTER_NAME> \
  --region <REGION> --project <PROJECT> \
  --format='value(currentMasterVersion)'

# Verify it matches before restoring
```

The `infra/09-serving-nodepool.sh` script prints the GKE version after node pool
creation to simplify this matching.

## GPU Driver Version

For GPU workloads (e.g., vLLM serving), use `gpu-driver-version=latest` when
creating the node pool. GKE's default L4 driver (535 / CUDA 12.2) is too old for
current `vllm/vllm-openai:latest` images, which require CUDA 13-capable drivers.
The `latest` driver installs version 580.x (CUDA 13-capable).

Example (from `infra/09-serving-nodepool.sh`):

```bash
gcloud container node-pools create l4-gvisor-spot \
  --cluster <CLUSTER_NAME> --region <REGION> --project <PROJECT> \
  --node-locations us-central1-a,us-central1-b,us-central1-c \
  --machine-type g2-standard-8 \
  --accelerator type=nvidia-l4,count=1,gpu-driver-version=latest \
  --spot --sandbox type=gvisor \
  --num-nodes 0 --enable-autoscaling --min-nodes 0 --max-nodes 2
```

## Capacity and Region Failover

Act 6 spike observed that g2/L4 on-demand instances were region-stocked-out in
us-central1 during testing, while **spot instances had capacity**. Pod snapshots
persist in GCS independent of node lifetime, so a snapshot→restore flow survives
node preemption.

**Recommendations:**

- Use **multi-zone node locations** (e.g., `us-central1-a,us-central1-b,us-central1-c`)
  to improve spot obtainability.
- Plan for **region failover** if a given region exhausts spot capacity: the
  snapshot bucket can be replicated or the workload migrated to another region
  (though cross-region restore may have version-skew implications; test first).

## Restore Procedure

GKE restores pods from snapshots in two ways:

### 1. Automatic restore (product path: Deployment scale 0→1)

When a **Deployment** scales from `0→1`, GKE automatically restores the pod from
the **latest matching snapshot** if one exists, with **no annotation required**.
The restore matches by pod identity/spec (same pod template). This is the
production path used with KEDA scale-to-zero: the `HTTPScaledObject` scales the
Deployment to 1 replica, and GKE restores from the snapshot created before the
prior scale-down.

### 2. Explicit bare-pod restore (specific snapshot)

To restore a **bare Pod** from a **specific** snapshot:

1. Delete the original pod (or let the `postCheckpoint: stop` policy in the
   `PodSnapshotPolicy` stop it).
2. Recreate the pod with the **identical spec** plus the restore annotation:

   ```yaml
   apiVersion: v1
   kind: Pod
   metadata:
     name: <POD_NAME>
     namespace: <NAMESPACE>
     annotations:
       podsnapshot.gke.io/ps-name: <PODSNAPSHOT_NAME>
   spec:
     # ... identical to the original pod spec
   ```

The pod will restore from the snapshot instead of cold-starting. For GPU
workloads, this skips model download and VRAM load, reducing startup time
from ~137s (cold) to ~38.8s (restore) in the Act 6 product run.

## References

- Act 6 spike: `demo/act6/spike/README.md` (live-measured restore metrics, exact
  working configs, and all discovered gotchas)
- Spike snapshot manifest: `demo/act6/spike/snapshot.yaml` (working
  PodSnapshotStorageConfig / Policy / ManualTrigger)
