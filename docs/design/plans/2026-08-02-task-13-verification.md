# Task 13 — live verification checklist

Companion to `2026-07-26-reconcile-mode.md` (Plan 4). Tasks 1–12 are complete
and review-clean; Task 13 is the live-cluster verification that gates the PR.

Two of Task 13's four gate items were resolved from the repo alone. What
remains needs the cluster.

## Resolved without the cluster

### `gke-provisioning=standard` node selector — passes

`infra/reconciler/cronjob.yaml` selects
`cloud.google.com/gke-provisioning: standard`. GKE applies that label
automatically to every non-spot node; it is the managed counterpart to
`gke-provisioning=spot`. Nothing has to set it, so the concern that the
selector leaves every Job unschedulable does not hold.

Confirm the pool the CronJob lands on is standard:

```bash
env -u GOOGLE_APPLICATION_CREDENTIALS \
  kubectl get nodes -l cloud.google.com/gke-provisioning=standard \
  -o custom-columns=NAME:.metadata.name --no-headers | head
```

Non-empty output closes it.

### Act 2 install ordering — was wrong, now fixed

`demo/act2/runbook.md` installed the reconciler above the `wait "${BUILD_PID}"`
barrier while `infra/04-build.sh` pushes the `capacity-advisor` image last.
The CronJob would begin its ten-minute schedule tens of minutes before its own
image existed. Fixed by moving `08-reconciler.sh` below the barrier, with the
reasoning recorded inline.

## Requires the cluster

### MIG name format vs `migShapeRe` — highest value

`advisor/internal/evidence/gcplog/gcplog.go`:

```go
var migShapeRe = regexp.MustCompile(`^nap-([a-z0-9]+-[a-z]+-[0-9]+)`)
```

If real NAP MIG names are `gke-<cluster>-nap-<shape>-<hash>` rather than
`nap-<shape>-<hash>`, the anchor never matches, no log entry yields a shape,
and the evidence store stays empty permanently.

This is the highest-value check on the list because **its failure is silent**.
Zero evidence is indistinguishable from "no preemptions have happened yet" —
the advisor keeps running, keeps scoring on priors alone, and never signals
that the evidence path is dead. Every other item on this list announces itself.

```bash
env -u GOOGLE_APPLICATION_CREDENTIALS \
  gcloud compute instance-groups managed list \
  --project example-sandbox --format='value(name)' | grep -i nap
```

- Names starting with `nap-` → the anchor is correct; close the item.
- Names starting with `gke-` → relax to an unanchored
  `nap-([a-z0-9]+-[a-z]+-[0-9]+)`, or re-anchor on the `gke-` prefix.

Do not relax `zoneRe` in the same file while you are there. Its anchoring is
deliberate and load-bearing: failure-reason parameters carry node-pool names,
and a loose match lets a pool called `pool-a100-x` pass for a zone and shadow
the real one. The comment above it says so.

Any change here needs a test, and the gcplog tests are the ones that have
shipped vacuous assertions before. Mutate the regex and confirm the test
actually fails before believing it.

### `spot-demo` PodSecurity label — informational

`infra/08-reconciler.sh` creates the namespace with a bare
`kubectl create namespace`; no PodSecurity labels appear anywhere in the repo.
On a Standard cluster PSA is unenforced by default, and the pod spec already
satisfies `restricted`.

```bash
env -u GOOGLE_APPLICATION_CREDENTIALS \
  kubectl get namespace spot-demo -o jsonpath='{.metadata.labels}'
```

Record the result. Act only if it shows an `enforce` level *and* a pod is
actually rejected.

## Carried items

### Hardcoded VPC in the insert probe

`advisor/internal/probe/gce/gce.go` pins
`Network: proto.String("global/networks/default")`. The insert probe fails
outright on any cluster in a custom VPC. Either confirm the project uses the
default network and note that the pin is deliberate, or thread the network
through from config.

### The plan's `README.md` edits have no target

Plan lines 4538, 4757 and 4765 direct edits to a root `README.md`. **No root
`README.md` exists.** `advisor/README.md` does exist — it is the advisor tool's
own documentation and is *not* the file the plan means.

Task 12 hit the same gap at plan lines 4187/4494 and redirected its edit to
`demo/act2/runbook.md`. Follow that precedent rather than creating a root
README at the end of the plan.

## Scope

Nothing on this list can invalidate the branch. The worst case is the MIG
regex plus a test.

Suggested order: the MIG anchor first, then let the two `kubectl` confirmations
fall out of the same session.
