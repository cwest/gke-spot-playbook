# Operator runbook

## Migrating a single-region bucket to US multi-region

`infra/06-gpu-data.sh` creates `gs://${PROJECT}-spot-demo` in the US
multi-region. A bucket created by an older version of the script is pinned to one
region; the current script only warns about it and never deletes demo data.
Migrating is a deliberate, manual operation — run the steps below in order.

**Step 0 — set up the shell.** The infra scripts read `PROJECT` from
`spotdemo::init`, not from your shell, so export it yourself before pasting
anything else. A stale `GOOGLE_APPLICATION_CREDENTIALS` shadows ADC logins.
`spotdemo::init` unsets it inside the script's own process; for the commands
you paste by hand, scope the unset the same way with `env -u` rather than
clearing it for your whole terminal:

```bash
export PROJECT="${PROJECT:-example-sandbox}"
```

Prefix each `gcloud` command below with
`env -u GOOGLE_APPLICATION_CREDENTIALS` if you have that variable set.

**Step 1 — stage a copy in a US multi-region bucket.**

```bash
gcloud storage buckets create "gs://${PROJECT}-spot-demo-us" \
  --project "${PROJECT}" --location US --uniform-bucket-level-access
gcloud storage rsync -r \
  "gs://${PROJECT}-spot-demo" "gs://${PROJECT}-spot-demo-us"
```

**Step 2 — verify the copy.** The two object counts must match before you delete
anything.

```bash
gcloud storage ls -r "gs://${PROJECT}-spot-demo/**" | wc -l
gcloud storage ls -r "gs://${PROJECT}-spot-demo-us/**" | wc -l
```

**Step 3 — empty and delete the old bucket.** A bucket must be empty before it
can be deleted.

```bash
gcloud storage rm --recursive "gs://${PROJECT}-spot-demo/**"
gcloud storage buckets delete "gs://${PROJECT}-spot-demo" --project "${PROJECT}"
```

**Step 4 — re-run the infra script to recreate the bucket.** Do not hand-create
it: `06-gpu-data.sh` both creates `gs://${PROJECT}-spot-demo` in the US
multi-region *and* re-applies the `roles/storage.objectAdmin` bindings for
`spot-demo-embed` and `spot-demo-tune`. Without those bindings the Act 2 and
Act 3 workloads lose write access.

```bash
bash infra/06-gpu-data.sh
```

> Caution: recreating a bucket name in a different location can return `404` for
> up to ~10 minutes after the delete. If the create fails, wait and re-run —
> the script is idempotent.

**Step 5 — copy the data back.**

```bash
gcloud storage rsync -r \
  "gs://${PROJECT}-spot-demo-us" "gs://${PROJECT}-spot-demo"
```

**Step 6 — delete the staging bucket.**

```bash
gcloud storage rm --recursive "gs://${PROJECT}-spot-demo-us/**"
gcloud storage buckets delete "gs://${PROJECT}-spot-demo-us" --project "${PROJECT}"
```

Until you migrate, the demo still works — it just reads across regions after a
move.

## Installing the capacity reconciler

The capacity reconciler rescores the ComputeClass ladders from live capacity
advice and observed scale-up failures. It runs as a CronJob every 10 minutes,
server-side applying cluster-scoped ComputeClasses with no human in the loop.
Install it by running:

```bash
bash infra/08-reconciler.sh
```

The script is idempotent and produces a running CronJob that ticks every
10 minutes. The container image comes from `infra/04-build.sh`.

**Its events land in `default`, not `spot-demo`.** The reconciler's
`LadderRescored` and `RegionAdvisory` events are about ComputeClasses, which are
cluster-scoped, so their `involvedObject.namespace` is empty — and the API
server accepts an empty `involvedObject.namespace` only for events in
`default`. To watch the reconciler work:

```bash
kubectl -n default get events \
  --field-selector reason=LadderRescored --sort-by=.lastTimestamp
```

`kubectl -n spot-demo get events` shows the CronJob's pod lifecycle and nothing
about the ladder.

**Act 4 — "the ladder maintains itself"** is the live verification of that
CronJob: [`demo/act4/runbook.md`](../demo/act4/runbook.md) walks the four beats
(deploy, hold still, read a scale-up failure without over-reading it, forget it
on the decay curve) with the observed output. It also records the two silent
failure modes it caught: a tick that reports success but moves nothing, and a
tick that reports success while penalising zones that were never short.

## Enabling automated probing in the reconciler

By default, the reconciler is read-only in GCP (roles `compute.viewer` and
`logging.viewer`). To enable automated probing, set `PROBE_AUTOMATED=true` when
running the reconciler installer:

```bash
PROBE_AUTOMATED=true bash infra/08-reconciler.sh
```

This binds `roles/compute.instanceAdmin.v1` to the reconciler service account,
granting permissions to create, get, and delete spot instances for the
automated probe path. Without this flag, automated probing is disabled and the
reconciler cannot launch probe VMs.
