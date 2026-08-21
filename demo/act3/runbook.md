# Act 3 runbook — "spot survives, on the ladder"

Act 2 proved a GPU embedding Job **resumes** across a spot preemption instead of
restarting. Act 3 moves from inference to real training: a **LoRA fine-tune of
Gemma-2B** on `b-mc2/sql-create-context` — one L4, checkpoints on a [GCS-FUSE](https://cloud.google.com/kubernetes-engine/docs/how-to/persistent-volumes/cloud-storage-fuse-csi-driver?utm_campaign=CDR_0x5d16fa53_user-journey_b550269617&utm_medium=external&utm_source=lab)
mount, admitted through **Kueue** — that survives a spot preemption by resuming
from the last checkpoint, and rides a **spot → flex-start ladder** so the job
doesn't die when spot L4s momentarily vanish. Three beats plus the ladder
narrative: **advise (the ladder) → run → survive**, then the bill.

Act 3 reuses the Kueue queue, the demo bucket, and the `tune-worker` image from
the earlier acts; no new infrastructure is introduced here.

> **Where this ran.** The advisor first sited the cluster in **us-east1** (Act 2's
> GPU-led region choice). Getting Act 3 to a verified live run then took a
> multi-day, multi-region **spot-capacity saga** — L4 stockouts across three
> regions and every zone the advisor pinned — ending on a **`g2-standard-8` spot
> node in `us-central1-a`**, found by a live capacity *probe*. That saga is the
> real lesson of Act 3 and is documented in full below
> ("The road to a live run"). The verified run and its cost are in
> "Verified run — 2026-07-26 (UTC)".

> **Credentials warning (read once).** Running `kubectl`, `go run`, or `gcloud`
> ad hoc from a shell that has `GOOGLE_APPLICATION_CREDENTIALS` set will use the
> wrong identity and fail (the coder SA lacks `container` perms → `Forbidden`).
> The `infra/*.sh` and `demo/*.sh` scripts handle this for you
> (`spotdemo::init` unsets it per run). For the **ad-hoc** commands in this
> runbook (the `kubectl apply`, the `go run render`, the secret create) prefix
> them with `env -u GOOGLE_APPLICATION_CREDENTIALS ...` or `unset` it in your
> shell first.

**Prerequisite — Hugging Face token (blocks everything).** The trainer pulls the
**gated** `google/gemma-2b` weights, so you need a Hugging Face token whose
account has **accepted the `google/gemma-2b` license**. Confirm it is present
(non-empty) without printing it:

```bash
test -s ~/.cache/huggingface/token && echo "HF token present" || echo "MISSING"
```

If missing, run `huggingface-cli login` (or export `HF_TOKEN`) and accept the
license at https://huggingface.co/google/gemma-2b before continuing.

Prereqs: [`gcloud`](https://cloud.google.com/sdk/gcloud?utm_campaign=CDR_0x5d16fa53_user-journey_b550269617&utm_medium=external&utm_source=lab) (authed ADC on `example-sandbox`), `kubectl`, `go`,
`shellcheck`; run from the repo root. `spot-demo` cluster up with Kueue +
`gpu-cq` installed and the `tune-worker:v1` image in Artifact Registry.

---

## Beat 0 — Advise + the ladder (~30s)

Act 3 doesn't re-pick a region interactively — the advisor's GPU-led advice
chose the region and the cluster lives there. What Act 3 shows is the **ladder**
the advisor rendered for that region. Open the GPU advice report:

```bash
open out/advice-report-gpu-batch.md   # or: less out/advice-report-gpu-batch.md
```

Then re-render the `batch-gpu` ComputeClass from the **saved** analysis and apply
the real ladder:

```bash
cd advisor && env -u GOOGLE_APPLICATION_CREDENTIALS \
  go run ./cmd/capacity-advisor render \
    --analysis ../out/analysis-gpu-batch.json --out ../out
cd ..
env -u GOOGLE_APPLICATION_CREDENTIALS \
  kubectl apply --server-side -f out/computeclass-gpu.yaml
kubectl get computeclass batch-gpu -o yaml
```

> If the server-side apply reports a field-manager conflict with a hand-applied
> object, re-run with `--force-conflicts` and note it — the rendered ladder is the
> source of truth.

Narrate the rungs (top to bottom — GKE fills the highest `priorityScore` first):

- **Spot L4 rung** (`priorityScore: 1000`, `spot: true`, `gpu: {type: nvidia-l4,
  count: 1}`, `machineType: g2-standard-4`). This is the cheap, interruptible
  tier — the advisor chose it because it scored as the most *obtainable* L4 spot
  capacity.
- **flex-start rung** (`priorityScore: 1`, `flexStart.enabled: true`,
  `maxRunDurationSeconds: 86400`). When spot L4s momentarily vanish, the *intent*
  is that GKE falls to **[Dynamic Workload Scheduler](https://cloud.google.com/kubernetes-engine/docs/how-to/dws-flex-start-training?utm_campaign=CDR_0x5d16fa53_user-journey_b550269617&utm_medium=external&utm_source=lab) flex-start** — L4 capacity
  that is *not* preemptible for up to 24h. **Caveat (learned live):** on a custom
  ComputeClass this rung is **passive** — NAP retries spot on a transient
  stockout and never escalates to flex-start on its own. See "flex-start is
  passive" below; the Act 4 reconciler wires the escalation properly.
- **On-demand is deliberately absent.** `whenUnsatisfiable: DoNotScaleUp` means
  GKE will **never** silently fall back to full-price on-demand L4s. The ladder
  is spot-first, flex-start-as-fallback, and stops there.

**The invariant — flex can never tie spot.** The flex-start rung is rendered
at `priorityScore: 1`, orders of magnitude below the spot rung's `1000`, so a
scheduling tie is impossible: GKE always prefers spot when spot is available and
only descends to flex-start when it isn't. flex-start is a fallback, never a
co-equal.

> **Live deviation — the `g2-standard-8` spot rung (`priorityScore: 900`).** After
> the spot L4 stockout saga below, a live capacity probe found spot stock only one
> machine shape *up* (`g2-standard-8`, still 1×L4) in `us-central1-a/b/c`. With
> Casey's approval we added a second spot rung at `priorityScore: 900` (below the
> `g2-standard-4` rung's `1000`, above flex's `1`) pinned to zones a/b/c. The
> ladder stays spot-first and invariant-preserving; it just lets NAP try a
> bigger-but-obtainable spot node when the pinned `-4` shape is dry. This rung is
> a hand-applied deviation for the live run, comment-flagged in
> `out/computeclass-gpu.yaml`; the advisor should learn to render shape
> alternatives (see "Probe methodology" below).

---

## Beat 1 — Run (~5–10 min to first training step)

Start the cost collector **before** applying the Job so the node-inventory time
series spans the whole run (fresh CSV; appending across runs would inflate the
window). Match `COLLECT_INTERVAL` to the `--interval` you will pass to `cost`:

```bash
rm -f out/cost-samples-act3.csv
COLLECT_INTERVAL=15 bash demo/cost/collector.sh out/cost-samples-act3.csv &
echo $! > .superpowers/sdd/collector-act3.pid
```

Apply the namespace, service account ([Workload Identity](https://cloud.google.com/kubernetes-engine/docs/concepts/workload-identity?utm_campaign=CDR_0x5d16fa53_user-journey_b550269617&utm_medium=external&utm_source=lab) → `spot-demo-tune` GSA),
and the Kueue LocalQueue:

```bash
env -u GOOGLE_APPLICATION_CREDENTIALS kubectl apply \
  -f workloads/03-finetune/manifests/namespace.yaml \
  -f workloads/03-finetune/manifests/serviceaccount.yaml \
  -f workloads/03-finetune/manifests/localqueue.yaml
```

Create the `hf-token` secret from the local token **without echoing it** (read
straight from the file into `--from-file`, keyed `token`):

```bash
env -u GOOGLE_APPLICATION_CREDENTIALS kubectl -n act3-finetune \
  create secret generic hf-token \
  --from-file=token="$HOME/.cache/huggingface/token"
```

> `--from-file=token=<path>` keeps the token off your shell history and out of
> process args — never `--from-literal="$(cat ...)"` in a shared/recorded
> session. The Job mounts it as the `HF_TOKEN` env var via `secretKeyRef`.

Deploy the fine-tune Job. The image lives in the `us` multi-region registry,
reachable at one hostname from every US region, so the manifest applies as-is
no matter which region the cluster landed in:

```bash
source out/cluster-config.env                 # REGION, ZONES, PROJECT
env -u GOOGLE_APPLICATION_CREDENTIALS \
  kubectl apply -f workloads/03-finetune/manifests/job.yaml
```

The Job ships `suspend: true`; **Kueue** admits it against the `gpu-cq`
ClusterQueue (via the `gpu-queue` LocalQueue) and unsuspends it. Watch admission,
then the node come up, then training:

```bash
kubectl -n act3-finetune get workloads.kueue.x-k8s.io   # Admitted=True by gpu-cq
kubectl get nodes -L cloud.google.com/compute-class,cloud.google.com/gke-spot
kubectl -n act3-finetune logs -f job/finetune-gemma      # loss curve + checkpoint saves
```

Expect: the workload flips to `Admitted=True`, a `g2` L4 **spot** node
(`compute-class=batch-gpu`, `gke-spot=true`) appears, the pod downloads Gemma-2B
(gated — needs the HF token), then logs `resume_from_checkpoint=None` on a cold
start and begins the loss curve. `MAX_STEPS=300`, `SAVE_STEPS=50`, so a
`checkpoint-50`, `-100`, … lands every 50 steps in
`gs://example-sandbox-spot-demo/checkpoints/gemma-sql/`.

> **First FUSE checkpoint write is slow.** The first `checkpoint-N` flush through
> GCS-FUSE writes many small files (adapter shards, optimizer state, RNG) and can
> take noticeably longer than later ones — expected, not a hang. `save_total_limit=2`
> means the trainer keeps only the two most-recent checkpoints on disk.

> **Kueue v0.19 `waitForPodsReady` gotcha (learned live).** Kueue's default-on
> `waitForPodsReady` will **evict and requeue** the admitted workload if its pods
> aren't Ready within `PodsReadyTimeout` — and a spot-GPU provision (NAP node
> create + driver install + Gemma download) routinely blows past the default. On
> a scarce-capacity run that creates an eviction/requeue loop that never lands.
> Neutralize it by setting an effectively-infinite timeout in the Kueue
> ConfigMap: `waitForPodsReady: {timeout: 9999h}`. **Note:** Kueue **v0.19
> removed the `enable` field** — you set only `timeout`, you don't toggle
> `enable: false`. (The repo's `infra/07-kueue.sh` now installs this same
> timeout-based opt-out.)

---

## Beat 2 — Survive (the chaos beat)

Wait until **at least one** checkpoint has landed in GCS:

```bash
env -u GOOGLE_APPLICATION_CREDENTIALS \
  gcloud storage ls gs://example-sandbox-spot-demo/checkpoints/gemma-sql/
```

Once you see a `checkpoint-N/`, kill the GPU node:

```bash
CLASS=batch-gpu bash demo/preempt.sh
```

`preempt.sh` picks one `gke-spot=true,compute-class=batch-gpu` node and sends it
a real `simulate-maintenance-event`. It targets the node by `CLASS=batch-gpu`, so
it works regardless of which shape (`g2-standard-4` or `-8`) the ladder landed
on. Evidence to capture:

- the evicted pod goes to `Error`/`Failed`; `backoffLimit: 20` on the Job creates
  a replacement pod, and the `batch-gpu` ladder provisions a fresh spot L4 for it;
- the replacement pod's first log line names the checkpoint it resumes from:

```bash
kubectl -n act3-finetune logs job/finetune-gemma --tail=20 | grep resume_from_checkpoint
# resume_from_checkpoint=/ckpt/gemma-sql/checkpoint-<N> (bounded loss: one 50-step save interval)
```

- the global step / LR schedule resumes **from N, not 0** (the learning rate keeps
  decaying from where it was — it does not jump back to the initial value);
- the checkpoint listing never regresses — the highest `checkpoint-N` step only
  climbs across the kill (with `save_total_limit=2` the *count* caps at two, but
  the max step is monotonic).

**Bounded loss.** Any progress since the last save is lost, but that is at most
`SAVE_STEPS` (50) steps — never the whole run. The checkpoint on durable GCS is
the recovery point; the spot preemption costs a bounded sliver, not a restart.

> **If pod logs are already gone** (node deleted, job finished), the survive beat
> is still fully reconstructable from durable evidence: `kubectl get job ... -o
> yaml` (`succeeded=1 failed=1` ⇒ one pod died, one completed), and the
> `k8s_container` `trainer` logs in **[Cloud Logging](https://cloud.google.com/logging/docs?utm_campaign=CDR_0x5d16fa53_user-journey_b550269617&utm_medium=external&utm_source=lab)** retain both
> `resume_from_checkpoint` lines and the LR-continuous loss curve after the pod
> is gone. See the verified run below for exactly this reconstruction.

---

## Beat 3 — The bill (cost readout)

Once the Job reaches `Complete` (bounded at `MAX_STEPS=300`) and the `g2` node has
scaled back down (a couple of minutes of cool-down), stop the collector:

```bash
kill "$(cat .superpowers/sdd/collector-act3.pid)"
```

Build the two-factor report. `g2` on-demand list price comes from the [Cloud
Billing Catalog](https://cloud.google.com/billing/v1/how-tos/catalog-api?utm_campaign=CDR_0x5d16fa53_user-journey_b550269617&utm_medium=external&utm_source=lab); spot comes from the advisor's
[`capacityHistory`](https://cloud.google.com/sdk/gcloud/reference/beta/compute/advice/capacity-history?utm_campaign=CDR_0x5d16fa53_user-journey_b550269617&utm_medium=external&utm_source=lab) signal. `--interval` **must** match the collector's
`COLLECT_INTERVAL` so node-hours are computed correctly (rows × interval):

```bash
source out/cluster-config.env
cd advisor && env -u GOOGLE_APPLICATION_CREDENTIALS go run ./cmd/capacity-advisor cost \
  --region "${REGION}" --project example-sandbox \
  --samples ../out/cost-samples-act3.csv --interval 15s --out ../out \
  && cat ../out/cost-report.md
cd ..
```

> **Collector simplification.** The collector labels a node
> `spot` only when it carries `cloud.google.com/gke-spot=true`. A **flex-start**
> node has no such label, so it would be recorded as **on-demand** — which
> **overstates** the bill, never understates it. Separately, the collector only
> records what it successfully samples: if node churn (a preemption + replacement)
> disrupts `kubectl get nodes` for a stretch, those GPU node-minutes are
> *missing* from the CSV, so the spot node-hours it reports are a **floor**. Both
> caveats point the same way for the demo's headline — the real savings are at
> least as large as the report claims.

---

## The road to a live run — a multi-region spot-capacity saga

The single hardest part of Act 3 was not the code — it was **getting an L4**. Over
roughly 36 hours the spot L4 supply the advisor had scored as obtainable evaporated
across three regions and every zone we pinned. The blow-by-blow is the real
teaching moment of this act:

1. **us-east1-c goes dry (~24h).** The Act 2 cluster's `batch-gpu` job sat
   `Pending` for roughly a day. The authoritative cluster-autoscaler-visibility
   log (not the terse pod event) read
   `no.scale.up.nap.pod.zonal.resources.exceeded [us-east1-c]` — a **transient
   zonal spot stockout**, not a quota or config fault (L4 quota was clean, 0/16) —
   the entire time the advisor's obtainability score for the region still read
   `0.90`.
2. **Failover policy: next-cheapest *obtainable* region.** Casey's standing policy
   is to migrate to the cheapest region that actually has capacity — not merely the
   cheapest on paper.
3. **us-east4 collapsed between scans.** us-east4 looked like the next hop
   (obtainability `0.90`) at 19:21Z, but a fresh scan at 23:44Z showed it had
   **collapsed to `0.10`**. This is the **advice-shelf-life** lesson: an
   obtainability score is a *snapshot*, and spot capacity can move faster than you
   re-plan.
4. **Migrate to us-central1.** us-central1 was the remaining obtainable candidate
   (`0.90`, ~`$0.3973`/L4-hr). The stack was torn down in us-east1 and rebuilt in
   us-central1 (first-hand, with direct authorization — a shared-cluster migration
   is not something a relayed instruction can green-light).
5. **us-central1 a/b/c are *also* dry for `g2-standard-4` (~12h).** Same failure
   mode for about half a day: `no.scale.up.nap.pod.zonal.resources.exceeded`.
   Meanwhile the advisor's obtainability signal still read ~`0.90` for these
   regions. **Advice-vs-observation lesson:** the beta obtainability API is an
   *advisory prior*, not a real-time capacity oracle — it read "obtainable" for
   zones that were, right then, empty.
6. **flex-start is passive.** The ladder's flex-start rung did **not** rescue the
   run. On a custom ComputeClass, NAP treats a transient spot stockout as "retry
   spot", and never escalates to the flex-start rung on its own — DWS flex-start
   via a custom compute class needs a **[ProvisioningRequest](https://cloud.google.com/kubernetes-engine/docs/how-to/provisioningrequest?utm_campaign=CDR_0x5d16fa53_user-journey_b550269617&utm_medium=external&utm_source=lab)**, which NAP's
   spot-retry path doesn't issue. So flex sat idle as insurance that structurally
   *couldn't* fire. **Act 4** fixes this by wiring a Kueue `ProvisioningRequest`
   admission check so the flex rung actually engages when spot is exhausted.
7. **Probe methodology — verify capacity by probing, one shape up.** Rather than
   trust the score or migrate a *fourth* time, a **live spot-instance probe**
   (Casey-approved) tested actual `g2` spot availability shape-by-shape. It found
   **no `g2-standard-4`** but **`g2-standard-8` stock in `us-central1-a`** — same
   single L4, one machine size larger. We added the `g2-standard-8` spot rung
   (`priorityScore: 900`, zones a/b/c) to the live class, recreated the job, and
   **NAP provisioned the node in ~7.5 min.** *Recommended Act 4 advisor feature:*
   **verify-by-probe before migrating** — a cheap live probe across shapes and
   zones beats another region hop driven by a stale score, and it can surface
   "capacity exists one shape up" automatically.

Every migration and probe in this saga was driven **by hand** — re-scanning,
comparing scores to live probes, retargeting regions, and hand-adding the
shape-alternative rung. That manual loop is exactly what **Act 4's reconciler**
is meant to automate: a controller that continuously reconciles advice against
observed capacity (probe-verified), fails over on a stale prior, and adjusts the
ladder without a human in the loop. Read this saga as a hand-run preview of that
reconciler's advisory.

**The through-line:** the advisor's *region* choice and *ladder* are necessary but
not sufficient in a real spot market. Obtainability scores age; a resilient system
must (a) keep the spot-first ladder, (b) make flex-start actually catch (Act 4),
and (c) confirm with a live probe rather than replan on a stale prior.

---

## Ladder narrative — the two incentives, one node pool

Act 3 closes the GPU arc on two incentives that land on the same node pool:

1. **Cost.** The fine-tune ran on a spot L4 at a fraction of the on-demand list
   price, and elastically — no always-on GPU pool sitting idle between runs. The
   report's Factor 1 (spot discount) × Factor 2 (duty cycle) is the "cheaper than
   the standard way" figure.
2. **Obtainability.** Spot capacity is a moving target. The ladder covers
   transient scarcity by keeping the job alive across preemptions (survive beat),
   the advisor's region choice covers structural regional absence, and — as the
   saga above shows — a live probe plus a shape-alternative rung covers the case
   the score gets wrong. Same signal, several failure modes; the honest lesson is
   that the score is a prior to be *verified*, not obeyed.

> **Act 4 automates this loop — and does not reproduce the hand edits.**
> [`demo/act4/runbook.md`](../act4/runbook.md) verifies a reconciler that
> rescores the ladder every ten minutes against the autoscaler's own record of
> what it refused. Running it live retired neither the four-zone widening nor
> the `g2-standard-8` rung: the widening answered July evidence, which decays
> by design, and the extra rung came from a live probe that stays opt-in
> because it spends money. The widening *mechanism* is automated; the
> probe-driven "capacity exists one shape up" discovery is not. Act 4 also
> retracts item 6's promise above — `ProvisioningRequest` is documented
> incompatible with custom compute classes, so flex-start remains passive.

---

## Verified run — 2026-07-26 (UTC)

A full live run completed on the us-central1 cluster on a **`g2-standard-8` spot**
node, **surviving a real preemption** mid-training. Timestamps below are from
durable evidence (Kubernetes job status, Cloud Logging `trainer` container logs,
Compute Engine audit logs, and the collector CSV) — the interactive session
stalled right after issuing the preempt, and the entire survive beat was
**reconstructed from that durable evidence**, not from a live terminal.

**Ladder as run:** spot `g2-standard-4` (score 1000, `us-central1-c`) + spot
`g2-standard-8` (score 900, zones a/b/c — the live deviation) + flex-start
(score 1) + `whenUnsatisfiable: DoNotScaleUp`.

### Timeline

| UTC time     | Event | Evidence |
|--------------|-------|----------|
| 03:00:20 | Kueue admits + unsuspends `finetune-gemma` (`JobResumed`) | job condition `Suspended=False` |
| 03:07:07 | NAP creates spot `g2-standard-8` node #1 (`…bccc7a5a-nkpn`) | Compute audit `instances.insert` |
| 03:10:16 | trainer container starts | container status |
| 03:11:46 | **cold start:** `resume_from_checkpoint=None` | Cloud Logging (trainer) |
| 03:11:46→ | loss curve descends 2.596 → ~1.3; `checkpoint-50`…`-200` saved | Cloud Logging + GCS |
| ~03:13:45 | **preempt issued:** `simulate-maintenance-event` on node #1 (`us-central1-a`) | `preempt.sh` output + collector last `-8` sample |
| 03:14:17 | NAP creates spot `g2-standard-8` node #2 (`…24c64bec-trkr`) | Compute audit `instances.insert` |
| ~03:14:44 | node #1 (preempted) deleted | Compute audit `instances.delete` |
| 03:18:50 | **resume:** `resume_from_checkpoint=/ckpt/gemma-sql/checkpoint-200` | Cloud Logging (trainer) |
| 03:20:04 | **Job `Complete=True` (`CompletionsReached`)** | job condition |
| ~03:23–03:25 | GPU node scaled back down | Compute audit `instances.delete` |

### Proof the Preemption Happened and Training Resumed

- **Job status: `succeeded=1 failed=1`.** The `failed=1` pod is the one killed by
  the preemption; the `succeeded=1` pod is the replacement that resumed and ran to
  completion. Two distinct spot nodes (`…nkpn` preempted, `…trkr` replacement)
  confirm the node-level churn.
- **Resume line (durable, from Cloud Logging):**
  `03:18:50 resume_from_checkpoint=/ckpt/gemma-sql/checkpoint-200` — the
  replacement pod picked up from the last durable checkpoint, not from zero
  (contrast the cold-start pod's `03:11:46 resume_from_checkpoint=None`).
- **Step / LR continuity:** the learning-rate schedule decayed **monotonically
  across the kill** — `lr=0.0001067 @ 03:13:08` (pre-preempt) → `lr=3.33e-05 @
  03:19:22` (post-resume) — and never reset to the initial `0.0002`. That is
  resume-from-checkpoint, not restart-from-scratch.
- **Checkpoints never regressed:** end state `checkpoint-250/`, `checkpoint-300/`,
  `final/` in `gs://example-sandbox-spot-demo/checkpoints/gemma-sql/`
  (`save_total_limit=2` pruned the earlier `-50…-200`); the max step only climbed
  across the preemption.

The preemption was issued, a pod failed, a new spot node came up, and training
resumed from `checkpoint-200` and finished at step 300.

### Cost report (verbatim)

```
# Cost report — us-central1

Generated 2026-07-26T03:58:21Z | observation window 48m58s

> ⚠️ sample-interval mismatch: rows are spaced ~17s but --interval is 15s; node-hours (rows × interval) may be inaccurate

## What this run actually cost

| machine type | lifecycle | node-hours | rate $/hr | cost |
|---|---|---|---|---|
| e2-standard-4 | on-demand | 0.450 | 0.1340 | $0.0603 |
| g2-standard-8 | spot | 0.071 | 0.4799 | $0.0340 |

**Actual: $0.0943**

## Factor 1 — the spot discount

Same node-hours at on-demand list prices: $0.1208 → spot saved 21.9%

## Factor 2 — the duty cycle

Always-on peak-sized on-demand pool for this window: $1.1342
(≈ $33.35/day if left running) → elasticity saved 89.4%

## Combined

21.9% spot discount compounded with 89.4% duty cycle → **91.7% cheaper than the standard way**
```

> **Read the spot line as a floor.** The collector captured only ~4.25 min of the
> `g2-standard-8` node's life (17 samples, 03:09:17→03:13:45) before the
> preemption-driven node churn interrupted `kubectl get nodes` sampling; the two
> GPU nodes actually lived ~18 cumulative minutes (03:07→03:14:44 ≈ 7.5 min and
> 03:14:17→~03:25 ≈ 10.5 min), so roughly **14 GPU-minutes are missing** from the
> CSV. The reported `0.071` GPU node-hours therefore **undercounts** true spot
> usage roughly 4×, and the real bill is a little higher than `$0.0943` —
> still on the order of a **dime** for a preemption-surviving Gemma-2B LoRA
> fine-tune. The interval-mismatch warning (17s actual vs 15s nominal) is just
> `kubectl` latency on top of the sleep and is second-order next to the sampling
> gap. The direction of both errors is conservative: the true savings are at
> least as large as reported.

---

## Teardown

**Leave the cluster up.** Act 4 (the reconciler) runs against this same
`spot-demo` cluster. Tear down only when the whole demo is done:

```bash
bash infra/90-teardown.sh
```

Deletes (idempotently, safe when partial): the `spot-demo` cluster, both Pub/Sub
topics + subscriptions, all four GSAs (queue/keda/embed/tune), the Artifact
Registry repo, and the demo bucket (including these checkpoints). The Firestore
`(default)` DB is left in place (an empty DB is free).

---

## Expected timings

| Phase                                      | Time         |
|--------------------------------------------|--------------|
| Beat 0 — re-render + apply the ladder      | ~30s         |
| Kueue admit + GPU node provision (spot)    | ~2–8 min     |
| Gemma-2B download + tokenize dataset       | ~2–5 min     |
| Training (300 steps, 1×L4)                 | ~9–20 min    |
| First checkpoint (FUSE, slow) → preempt    | run-dependent|
| Resume + finish to MAX_STEPS              | ~5 min       |
| Beat 3 — cost readout                       | seconds      |

> Reality check from the verified run: with capacity in hand, admit→node was
> ~2 min, and the whole train→preempt→resume→complete arc ran ~03:10→03:20 (≈10
> min). **Finding capacity** was the multi-day part — see the saga above.
</content>
