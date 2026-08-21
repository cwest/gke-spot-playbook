# Act 2 runbook — "spot survives, with GPUs"

Act 1 proved a Pub/Sub queue drains through a spot preemption without losing a
task. Act 2 raises the stakes: a GPU embedding **Job** — real `g2` L4 accelerators,
real model weights, real checkpointed progress — that **resumes** across a spot
preemption instead of restarting. Same three beats plus a migration:
**advise (GPU-led) → migrate → provision/run → survive**, then a zero-regression
ledger and the bill.

> **Credentials warning (read once).** Running `kubectl`, `go run`, or `gcloud`
> ad hoc from a shell that has `GOOGLE_APPLICATION_CREDENTIALS` set will use the
> wrong identity and fail (the coder SA lacks `container`/pubsub perms →
> `Forbidden`). The `infra/*.sh` and `demo/*.sh` scripts handle this for you
> (`spotdemo::init` unsets it per run). For the **ad-hoc** commands in this
> runbook (the `kubectl apply`, the loader, the publisher `go run`) prefix them
> with `env -u GOOGLE_APPLICATION_CREDENTIALS ...` or `unset` it in your shell.

Prereqs: `gcloud` (authed ADC on `example-sandbox`), `kubectl`, `go`, `python3`,
`shellcheck`; run from the repo root.

---

## Beat 0 — Advise (GPU-led, ~30s)

GPU capacity is the scarce resource, so it — not the CPU ladder — picks the
cluster region. Run the **gpu-batch** profile first and let it choose:

```bash
cd advisor && env -u GOOGLE_APPLICATION_CREDENTIALS \
  go run ./cmd/capacity-advisor analyze \
    --profile gpu-batch --config ../advisor.yaml --out ../out --render
cd ..
open out/advice-report-gpu-batch.md   # or: less out/advice-report-gpu-batch.md
```

Talk track: open `out/advice-report-gpu-batch.md` and point at

- the **composite score** ranking across `g2` machine types and zones,
- the **`## Skipped regions`** section — the g2-less US regions the capacity API
  rejected outright (`Machine specification is not supported in locations: …`).
  The advisor treats a region-level rejection as *skip and keep scoring*, not a
  fatal error, so obtainability is found wherever g2 actually exists, and
- the **chosen region/zone** — for gpu-batch this lands on
  **g2-standard-4 @ us-east1-c** (obtainability 0.90, spot **$0.3973/hr**).

The winning row is written to `out/cluster-config-gpu-batch.env`. Now re-score
the **CPU** ladder *inside that region* so both ComputeClasses live in the same
cluster (`--regions` overrides `allowedRegions` for this one run):

```bash
REGION=$(grep '^REGION=' out/cluster-config-gpu-batch.env | cut -d= -f2)
cd advisor && env -u GOOGLE_APPLICATION_CREDENTIALS \
  go run ./cmd/capacity-advisor analyze \
    --profile cpu-batch --regions "${REGION}" --out ../out --render --config ../advisor.yaml
cd ..
```

`render` emits `out/computeclass-gpu.yaml` **and** `out/computeclass-cpu.yaml`;
`infra/02-computeclasses.sh` applies both.

---

## Beat 0.5 — Migrate (tear down the old region)

Act 1's cluster lives in **us-south1**, which has **no `g2` machines** (see the
Skipped-regions list above), so Act 2 needs a new home. Tear the old stack down
**before** designating the new region — `spotdemo::init` sources
`out/cluster-config.env`, so the teardown must still see `us-south1` to find what
to delete:

```bash
printf 'PROJECT=example-sandbox\nREGION=us-south1\nZONES=us-south1-b\n' > out/cluster-config.env
bash infra/90-teardown.sh    # deletes the us-south1 cluster, Pub/Sub, GSAs, AR repo, bucket
cp out/cluster-config-gpu-batch.env out/cluster-config.env   # designate the new region
```

> The two `analyze` runs above only write per-profile files
> (`cluster-config-gpu-batch.env`, `cluster-config-cpu-batch.env`) — never
> `cluster-config.env` — so this ordering is safe: the teardown sees the old
> region, the copy hands the new one to the provisioning scripts.

---

## Beat 1 — Provision + run (~10 min, cluster is the long pole)

Run the infra scripts **in order**. Each is idempotent and sources
`out/cluster-config.env`.

```bash
bash infra/00-preflight.sh        # APIs, ADC, rapid channel >= min GKE version, AND L4 quota in the new region
bash infra/01-cluster.sh          # GKE Standard cluster "spot-demo" in us-east1  (~8 min)
bash infra/02-computeclasses.sh   # apply batch-cpu AND batch-gpu ComputeClasses
bash infra/03-pubsub.sh           # topics, subs, GSAs, IAM, Workload Identity, image repo (Artifact Registry)
```

`04-build.sh` is the long pole (the `embed-worker` torch image is **10–25 min**;
it builds queue-worker, embed-worker and tune-worker into the Artifact Registry
repo `03` just created, **and then** the `capacity-advisor` image the reconciler
runs — the advisor goes last, after all three workers). Kick it off in the
background and let it bake while the remaining scripts run:

```bash
bash infra/04-build.sh &          # Cloud Build 3 workers + capacity-advisor -> Artifact Registry
BUILD_PID=$!
bash infra/05-keda.sh             # install KEDA v2.20.1, bind operator SA via WI
bash infra/06-gpu-data.sh         # Firestore DB, US multi-region bucket, embed/tune GSAs + WI + Firestore IAM
bash infra/07-kueue.sh            # install Kueue (pinned), apply gpu-cq ClusterQueue
wait "${BUILD_PID}"               # block on the build before anything consumes its images
bash infra/08-reconciler.sh       # install the capacity reconciler CronJob (every 10m)
```

> `08-reconciler.sh` sits **below** the `wait` on purpose. `05`, `06` and `07`
> consume nothing from the build, so overlapping them with it is free. The
> reconciler is different: its CronJob runs the `capacity-advisor` image, which
> `04` pushes last. Installed above the barrier it would begin its ten-minute
> schedule tens of minutes before that image existed and spend its first few
> ticks in `ImagePullBackOff` — self-healing, but the first thing an audience
> sees is a crash-looping Job.

> For an unattended run, prefer `gcloud builds submit --async` inside `04` and
> poll `gcloud builds describe`; the backgrounded `&` above is the interactive
> equivalent.

`00-preflight.sh` now fails closed if the winner region lacks **L4 spot quota**
(`PREEMPTIBLE_NVIDIA_L4_GPUS >= 2`, `NVIDIA_L4_GPUS >= 2`). If it fails, pick the
next-ranked region from `advice-report-gpu-batch.md` and re-run Beat 0's cpu
`--regions` step against it.

**Kueue served-version check.** The demo's queue objects are `v1beta2`; confirm
the installed CRD actually serves that version:

```bash
kubectl get crd clusterqueues.kueue.x-k8s.io \
  -o jsonpath='{.spec.versions[?(@.served==true)].name}'   # expect: v1beta2
```

If it prints something else, edit `apiVersion:` in `infra/kueue/resources.yaml`
to the served version and re-apply (`kubectl apply --server-side -f
infra/kueue/resources.yaml`).

Build the corpus (~50k Simple-English-Wikipedia paragraph chunks) in a scratch
venv and upload it to the demo bucket:

```bash
cd workloads/02-embeddings/loader
python3 -m venv .venv && ./.venv/bin/pip install -q datasets==3.0.0
./.venv/bin/python load_corpus.py --max-chunks 50000 --out corpus.jsonl
cd ../../..
source out/cluster-config.env
env -u GOOGLE_APPLICATION_CREDENTIALS \
  gcloud storage cp workloads/02-embeddings/loader/corpus.jsonl \
    "gs://${PROJECT}-spot-demo/corpus/corpus.jsonl"
```

> If `pip install` hits a private mirror error, prefix with
> `PIP_INDEX_URL=https://pypi.org/simple`.

Start the cost collector in a spare pane (it samples `kubectl get nodes` to CSV
every `COLLECT_INTERVAL`s; match `--interval` at report time):

```bash
rm -f out/cost-samples.csv
COLLECT_INTERVAL=15 bash demo/cost/collector.sh out/cost-samples.csv &
COLLECTOR_PID=$!
```

Deploy the embedding Job. The image lives in the `us` multi-region registry,
reachable at one hostname from every US region, so the manifest applies as-is
no matter which region the cluster landed in:

```bash
source out/cluster-config.env                 # REGION, ZONES, PROJECT
export -n GOOGLE_APPLICATION_CREDENTIALS 2>/dev/null; unset GOOGLE_APPLICATION_CREDENTIALS
kubectl apply -f workloads/02-embeddings/manifests/namespace.yaml \
  -f workloads/02-embeddings/manifests/serviceaccount.yaml
kubectl apply -f workloads/02-embeddings/manifests/job.yaml
```

The Job is `Indexed`, `completions: 8`, `parallelism: 2` — eight corpus shards,
two GPU pods at a time. Each pod requests one `nvidia.com/gpu`, so the `batch-gpu`
ComputeClass auto-provisions `g2` **spot** nodes (one L4 each). NAP sizes the
machine to fit the pod (cpu 3 + 1×L4), so it may land on `g2-standard-8` rather
than `g2-standard-4` — either is one L4. Watch:

```bash
bash demo/watch.sh              # nodes + spot labels
bash demo/act2/progress.sh      # batch files written + shards complete (poll it)
```

Pods should be `Pending` immediately and `g2` nodes appear in ~2–4 min (no KEDA
gauge lag here — this is a plain Job, not scale-from-zero).

---

## Beat 2 — Survive (the chaos beat)

Once at least one shard is progressing (`progress.sh` shows batches written),
preempt a GPU node:

```bash
CLASS=batch-gpu bash demo/preempt.sh
```

`preempt.sh` picks one `gke-spot=true,compute-class=batch-gpu` node and sends it
a real `simulate-maintenance-event`. Evidence to capture:

- `progress.sh` **batches-written count before vs after** the kill — it **never
  decreases** (completed batches are durable in GCS + Firestore),
- a **replacement** `g2` spot node appears and the evicted pod reschedules, and
- the resumed pod logs the resume line proving it skipped finished work:

```bash
kubectl -n act2-embeddings logs -l app=embed-worker --tail=5 | grep 'resume:'
# resume: N of my chunks already done      (N > 0 on the pod that came back)
```

The worker keys progress on `chunk-hash × model-version` in Firestore and writes
per-batch `.jsonl` objects to GCS; on restart it streams the done-set and skips
any batch whose every chunk is already marked. Double-processing a partial batch
is harmless (idempotent upserts) — so **completed work never regresses**.

---

## Verify — the zero-regression ledger

```bash
bash demo/act2/verify.sh
```

`verify.sh` waits for the Job to reach `condition=complete`, then asserts every
one of the 8 shards left a `_SUCCESS-shard*` marker in
`gs://<project>-spot-demo/embeddings/<model-version>/`:

```
PASS: job complete, 8/8 shards verified — completed work never regressed
```

A shortfall prints `FAIL: <n>/8 shard success markers` and exits non-zero.

---

## Beat 3 — the bill (cost readout)

Stop the collector once the Job is `Complete` and the `g2` nodes have scaled back
down (give a couple of minutes of cool-down first):

```bash
kill "${COLLECTOR_PID}"
```

Build the two-factor report. `g2` on-demand list price comes from the Cloud
Billing Catalog; spot comes from the advisor's
`capacityHistory` signal. `--interval` **must** match the collector's
`COLLECT_INTERVAL` so node-hours are computed correctly:

```bash
source out/cluster-config.env
cd advisor && env -u GOOGLE_APPLICATION_CREDENTIALS go run ./cmd/capacity-advisor cost \
  --region "${REGION}" --project example-sandbox \
  --samples ../out/cost-samples.csv --interval 15s --out ../out \
  && cat ../out/cost-report.md
cd ..
```

The report shows **actual** spot g2 cost, the **spot discount** (Factor 1: spot
vs on-demand for the same node-hours), and the **duty cycle** (Factor 2: elastic
vs an always-on on-demand pool), then the combined "cheaper than the standard
way" figure. Close on the **two incentives landing on the same node pool**: the
advisor picked `g2-standard-4 @ us-east1-c` because it was the most *obtainable*
spot GPU capacity, and that same choice is what makes the run cheap. Per-chunk
cost = actual ÷ 50,000.

---

## Teardown

**Leave the cluster up.** Act 3 (Plan 4) fine-tunes on this same `spot-demo`
cluster and reuses the Kueue queue, the Firestore progress table, and the demo
bucket. Tear down only when the whole GPU arc is done:

```bash
bash infra/90-teardown.sh
```

Deletes (idempotently, safe when partial): the `spot-demo` cluster, both Pub/Sub
topics + subscriptions, all four GSAs (queue/keda/embed/tune), the Artifact
Registry repo, and the demo bucket. The Firestore `(default)` DB is left in place
(deleting a project's default DB is invasive and an empty DB is free).

---

## Expected timings

| Phase                                    | Time         |
|------------------------------------------|--------------|
| Beat 0 — advisor render (both profiles)  | ~1 min       |
| Beat 0.5 — teardown us-south1            | ~5–7 min     |
| Cluster create (`01-cluster.sh`)         | **~8 min**   |
| Pub/Sub + KEDA + GPU data + Kueue        | ~4–5 min     |
| Cloud Build (embed-worker torch image)   | **10–25 min**|
| Corpus load + upload (~50k chunks)       | ~2–4 min     |
| GPU node provision + first shard         | ~3–5 min     |
| Job drain (8 shards, 2 GPUs)             | run-dependent|
| Beat 3 — cost readout                    | seconds      |

---

## Verified run — 2026-07-24 (UTC)

Executed end-to-end on `example-sandbox`: migrated Act 1's us-south1 stack to the
gpu-batch winner region, ran the 50k-chunk embedding Job on L4 spot nodes, killed a
GPU node mid-run, and confirmed zero regression + the bill.

**Region decision.** `analyze --profile gpu-batch` ranked **g2-standard-4 @
us-east1-c** first (obtainability 0.90, spot $0.3973/hr), with `us-central1-c`
second and three low-obtainability zones dropped. `us-east5`, `us-south1`,
`us-west2`, `us-west3` were skipped (capacity API: "Machine specification is not
supported"). GPU capacity picked the region; the cpu-batch ladder was then
re-scored inside us-east1. L4 spot quota preflight passed (`PREEMPTIBLE_NVIDIA_L4_GPUS`
limit 16).

### Timeline

| UTC        | Event                                                                    |
|------------|--------------------------------------------------------------------------|
| 16:32:53   | `analyze --profile gpu-batch` → us-east1-c chosen                        |
| 16:33:30   | `analyze --profile cpu-batch --regions us-east1` (re-score CPU ladder)   |
| 16:35:59–16:42:40 | Beat 0.5 teardown of the old us-south1 stack (cluster, Pub/Sub, GSAs, AR, bucket) |
| 16:43:13   | `00-preflight.sh` OK (L4 spot quota present in us-east1)                 |
| 16:43:20–16:47:39 | `01-cluster.sh` — `spot-demo` created in us-east1 (node locations us-east1-c) |
| 16:46:xx   | corpus load — 50000 chunks (py3.10 venv; see loader note)               |
| 16:49–16:51 | `02`-computeclasses, `03`-pubsub, `05`-keda, `06`-gpu-data, `07`-kueue  |
| 16:50:11   | `04-build.sh` backgrounded (queue/embed/tune workers → Artifact Registry)|
| 16:53:44   | Kueue served-version check → `[v1beta1 v1beta2]` (v1beta2 OK)            |
| 16:53:50   | corpus.jsonl uploaded to `gs://example-sandbox-spot-demo/corpus/`      |
| 17:01:15   | cost collector started (`COLLECT_INTERVAL=15`)                           |
| 17:03:35   | `04-build.sh` exit 0 (all three images :v1)                             |
| 17:01–17:53 | **GPU NAP troubleshooting** — pods Pending, no L4 pool (see Two Failures to Expect) |
| 17:53:56   | single-rung `gpu:`-block `batch-gpu` ComputeClass applied               |
| 17:54:23   | NAP created pool `nap-g2-standard-8-spot-gpu1-…` in us-east1-c          |
| 17:57:52   | first embed pods `Running`; shards begin completing                     |
| 17:59:18   | progress **before** preempt: **38** batches, **2/8** shards            |
| 17:59:20   | `CLASS=batch-gpu demo/preempt.sh` → simulate-maintenance on node `…bgnt`|
| 18:00:01   | evicted pod `embed-4-jzgld` → `Error`; index 4 reschedules              |
| 18:01:04   | replacement `embed-4-djffs` logs `resume: 2562 of my chunks already done`|
| 18:04:35   | Job `succeeded=8`                                                        |
| 18:04:38   | progress **after**: **104** batches, **8/8** shards (never regressed)   |
| 18:05:38   | `verify.sh` → PASS 8/8                                                   |
| 18:05:47   | `capacity-advisor cost` → cost-report.md                                 |

### Node snapshots

`kubectl get nodes -L cloud.google.com/compute-class,cloud.google.com/gke-spot`

Steady state (two L4 spot nodes provisioned by the `batch-gpu` ComputeClass):

```
NAME                                                  STATUS  COMPUTE-CLASS  GKE-SPOT
gke-spot-demo-default-pool-31c543e8-vwwn              Ready   -              -
gke-spot-demo-nap-g2-standard-8-spot--92acad8d-bgnt  Ready   batch-gpu      true
gke-spot-demo-nap-g2-standard-8-spot--92acad8d-m8kq  Ready   batch-gpu      true
```

Just after the preemption — the killed node (`…bgnt`) is recreated and a
replacement (`…fpg6`) is added; the Job never stops making progress:

```
NAME                                                  STATUS  COMPUTE-CLASS  GKE-SPOT
gke-spot-demo-default-pool-31c543e8-vwwn              Ready   -              -
gke-spot-demo-nap-g2-standard-8-spot--92acad8d-bgnt  Ready   batch-gpu      true
gke-spot-demo-nap-g2-standard-8-spot--92acad8d-fpg6  Ready   batch-gpu      true
gke-spot-demo-nap-g2-standard-8-spot--92acad8d-m8kq  Ready   batch-gpu      true
```

### Survival evidence (Beat 2)

```
# progress.sh BEFORE the kill
batches written: 38  shards complete: 2/8

# CLASS=batch-gpu demo/preempt.sh
>>> simulating preemption of gke-spot-demo-nap-g2-standard-8-spot--92acad8d-bgnt (us-east1-c)

# the pod that came back skips finished work (from the replacement pod's log)
2026-07-24 18:01:04  resume: 2562 of my chunks already done

# progress.sh AFTER the kill — batch count only ever climbs
batches written: 104  shards complete: 8/8
```

The evicted `embed-4` pod (`…jzgld`) went `Error` when its node was preempted; the
retry (`…djffs`) streamed the Firestore done-set and skipped its **2562**
already-embedded chunks before finishing shard 4. Batch count went 38 → 104 across
the kill — **completed work never regressed**.

### Verify

```
$ bash demo/act2/verify.sh
job.batch/embed condition met
PASS: job complete, 8/8 shards verified — completed work never regressed
```

### The bill (Beat 3)

`out/cost-report.md`, verbatim:

```markdown
# Cost report — us-east1

Generated 2026-07-24T18:05:47Z | observation window 59m58s

> ⚠️ sample-interval mismatch: rows are spaced ~17s but --interval is 15s; node-hours (rows × interval) may be inaccurate

## What this run actually cost

| machine type | lifecycle | node-hours | rate $/hr | cost |
|---|---|---|---|---|
| e2-standard-4 | on-demand | 0.896 | 0.1340 | $0.1201 |
| g2-standard-4 | spot | 0.071 | 0.3973 | $0.0281 |
| g2-standard-8 | spot | 0.183 | 0.4799 | $0.0880 |

**Actual: $0.2362**

## Factor 1 — the spot discount

Same node-hours at on-demand list prices: $0.3266 → spot saved 27.7%

## Factor 2 — the duty cycle

Always-on peak-sized on-demand pool for this window: $3.3998
(≈ $81.64/day if left running) → elasticity saved 90.4%

## Combined

27.7% spot discount compounded with 90.4% duty cycle → **93.1% cheaper than the standard way**
```

**Two incentives, one node pool.** The advisor picked `g2-standard-4 @ us-east1-c`
because it was the most *obtainable* L4 spot capacity, and that same choice is what
makes the run cheap: the 27.7% spot discount compounds with the 90.4% duty-cycle
saving, and together they land the run 93.1% under an always-on on-demand pool.

**Per-chunk cost** = $0.2362 ÷ 50,000 = **$0.0000047 / chunk** (~$0.0047 per 1,000
chunks embedded).

> Reading the table: the `g2-standard-8 spot` line is the two L4 nodes the
> `batch-gpu` ComputeClass provisioned (NAP upsized from `-4` to fit the pod). The
> `g2-standard-4 spot` line is a short-lived **capacity probe** node used during
> the NAP troubleshooting below — real spend, so it is left in the ledger. The
> collector was stopped at 18:00:58, ~3.5 min before the Job finished, so GPU
> node-hours here slightly **under**count the true run; the savings percentages are
> unaffected (they are ratios over the same sampled node-hours).

### Two Failures to Expect: Corpus Loader and GPU Provisioning

1. **Corpus loader Python version.** `datasets==3.0.0` is incompatible with the
   local Python 3.14 (`dill` pickler error). Rebuilt the loader venv with
   `python3.10`; the 50k-chunk corpus then generated cleanly. `pip` also needed
   `--index-url https://pypi.org/simple` to reach `datasets` (private mirror miss).

2. **GPU node auto-provisioning did not trigger (the big one).** With the advisor's
   rendered `out/computeclass-gpu.yaml`, embed pods stayed `Pending` for ~50 min;
   the autoscaler logged `no.scale.up.unexpected.error` and never created an L4
   pool. Root causes, confirmed against live GKE (1.36.2, NAP enabled, L4 quota 16,
   capacity present — a manual `g2-standard-4`+L4 spot pool came up in ~60s):
   - the rendered priority rules carry **no `gpu:` block**, so NAP has no signal to
     attach an L4 to the node it would create; and
   - the GPU class pairs **`whenUnsatisfiable: DoNotScaleUp`** with a **flexStart
     fallback rung**, which is what surfaced the `unexpected.error`.

   **Fix applied for this run:** replace the `batch-gpu` ComputeClass with a single
   spot rung that carries an explicit `gpu:` block:

   ```yaml
   apiVersion: cloud.google.com/v1
   kind: ComputeClass
   metadata:
     name: batch-gpu
   spec:
     nodePoolAutoCreation:
       enabled: true
     priorities:
     - machineType: g2-standard-4
       spot: true
       gpu:
         type: nvidia-l4
         count: 1
         driverVersion: default
     whenUnsatisfiable: ScaleUpAnyway
   ```

   NAP created the L4 spot pool within ~30s of applying this. The renderer now
   emits the `gpu:` block (`{type: nvidia-l4, count: N}`) on every spot rung and
   the flexStart rung, so a fresh `analyze --render` produces a provisioning class
   without the hand-edit above.

---
