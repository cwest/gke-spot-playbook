# scaleUp Out-of-Resources Evidence Gap — Implementation Plan

> **For implementers:** This plan is written to be executed task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Collect genuine `scale.up.error.out.of.resources` stockouts on existing MIGs into the evidence ledger, and give Act 4 a reliable L4-spot recipe to provoke one on demand.

**Architecture:** A failed scale-up is split across two cluster-autoscaler-visibility log events joined by `eventId` — a `decision.scaleUp` carrying MIG name+zone, and an `eventResult` carrying the `out.of.resources` error naming the failing MIG (no zone). Broaden the reader's query to fetch both, add a page-level join that recovers `(shape, zone)`, and collapse the reconciler's `Log` interface to one `Refusals` method returning both noScaleUp and scaleUp observations. Provoke via an existing spot g2-standard (L4) pool bursted past its zone's spot supply.

**Tech Stack:** Go (advisor), `google.golang.org/api/logging/v2`, GKE cluster-autoscaler visibility logs, kubectl/gcloud on a live GKE cluster.

## Global Constraints

- `make test` needs `PIP_INDEX_URL=https://pypi.org/simple`.
- Go work runs from `advisor/`: `cd advisor && go test ./...`.
- Commits: emoji conventional style (`<emoji> <type>(<scope>): <subject>`), signed (`git commit -S`). Commit gate: `git status` + `git log -n 3` before each message.
- Diagrams Mermaid, never ASCII.
- Do NOT widen `migShapeRe`; L4 (`g2-standard-N`) parses as-is. `a2-ultragpu-1g` / `a3-highgpu-8g` stay out of scope.
- Never store an observation missing shape or zone; never store a non-stockout scale-up error.
- Live/billing cluster: Tasks 1 and 6 require an explicit go-ahead before running and tear the burst down immediately after. Project `example-sandbox`; interactive `gcloud`/`kubectl` want `env -u GOOGLE_APPLICATION_CREDENTIALS`.

---

## Task 1: Capture a real stockout and confirm the schema (LIVE GATE)

Not a TDD task — an operational capture that produces the fixture every later test depends on and confirms the one detail docs cannot: the format of `errorMsg.parameters` ("Failing MIG IDs" — short name vs. full URL). **Requires an explicit go-ahead before any command runs.**

**Files:**
- Create: `advisor/internal/evidence/gcplog/testdata/scaleup-stockout-decision.json` (real `decision.scaleUp` payload)
- Create: `advisor/internal/evidence/gcplog/testdata/scaleup-stockout-result.json` (real `resultInfo` payload)
- Modify: this plan — append a "Capture notes" block recording the confirmed MIG-ID format.

- [ ] **Step 1: Get go-ahead.** Confirm with Casey that the billing session may start.

- [ ] **Step 2: Stand up an existing spot L4 pool pinned to one zone.**

```bash
env -u GOOGLE_APPLICATION_CREDENTIALS gcloud container node-pools create scaleup-provoke-l4 \
  --cluster spot-demo --region us-central1 \
  --node-locations us-central1-a \
  --machine-type g2-standard-4 --accelerator type=nvidia-l4,count=1 \
  --spot --enable-autoscaling --num-nodes 0 --min-nodes 0 --max-nodes 8
```

- [ ] **Step 3: Burst demand past the zone's spot L4 supply.**

```bash
kubectl apply -f demo/act4/provoke-scaleup-stockout.yaml   # created in Task 5; hand-write a scratch copy now if needed
kubectl -n spot-demo get pods -w
```

- [ ] **Step 4: Watch for the stockout in the visibility log.**

```bash
env -u GOOGLE_APPLICATION_CREDENTIALS gcloud logging read \
  'logName="projects/example-sandbox/logs/container.googleapis.com%2Fcluster-autoscaler-visibility"
   AND resource.labels.cluster_name="spot-demo"
   AND (jsonPayload.decision.scaleUp:* OR jsonPayload.resultInfo:*)' \
  --freshness=30m --format=json --limit=50
```

Expected: at least one `resultInfo.results[]` with `errorMsg.messageId = scale.up.error.out.of.resources`, plus the `decision.scaleUp` sharing its `eventId`.

- [ ] **Step 5: Save the real pair to testdata** as the two files above (the `jsonPayload` object only, matching how `provoke-both-lists.json` is stored).

- [ ] **Step 6: Record the confirmed format.** Append to this plan under "Capture notes": is `errorMsg.parameters[]` a bare MIG name (`gke-spot-demo-...-grp`) or a full URL? Does it match `increasedMigs[].mig.name` exactly, or by suffix? This decides `matchMig` in Task 2.

- [ ] **Step 7: Tear down the burst** (keep or delete the pool per cost appetite; note which).

```bash
kubectl delete -f demo/act4/provoke-scaleup-stockout.yaml
# optional: gcloud container node-pools delete scaleup-provoke-l4 --cluster spot-demo --region us-central1
```

- [ ] **Step 8: Commit the fixtures.**

```bash
cd <repo-root>   # the gke-spot-playbook working tree
git add advisor/internal/evidence/gcplog/testdata/scaleup-stockout-*.json docs/design/plans/2026-08-03-scaleup-evidence-gap.md
git commit -S -m "🧪 test(evidence): capture a real scaleUp stockout fixture"
```

### Capture notes

**Attempt 1 (L4, 2026-08-03): no stockout.** A spot L4 pool bursted with 16 GPU
pods scaled to 8 nodes — `us-central1-a` had L4 spot capacity, so the scale-up
succeeded (real result carried no `errorMsg`). Torn down; cluster back to
`default-pool`. Findings:

- **Correction to this task's method.** An explicit node pool produces MIG
  names like `gke-<cluster>-scaleup-provoke-l4-...` with **no `nap-` segment**,
  which `migShapeRe` never parses. The evidence path only recognizes NAP MIGs,
  so a genuine, collectable scaleUp stockout must come from the **`batch-gpu`
  NAP ComputeClass** (Task 5's committed manifest, which was right), not an
  explicit pool. Step 2's `gcloud node-pools create` is therefore wrong; a redo
  drives batch-gpu NAP.
- **Structure confirmed from the real run:** `decision.scaleUp.increasedMigs[]
  .mig.{name,zone}`, `decision.eventId`, `resultInfo.results[].eventId`, and
  `errorMsg` present only on failure. `errorMsg.parameters` format (name vs URL)
  still unverified — `migName()` handles both.
- **Fixture is hand-authored** from that confirmed structure + the documented
  `errorMsg` (commit `6bd29b6`), so Tasks 2 and 4 proceed now. See
  `testdata/scaleup-stockout-README.md`.
- **A100 next (decision 2026-08-03):** land the code, then attempt a genuine
  capture with `a2-highgpu` spot (quota `PREEMPTIBLE_NVIDIA_A100_GPUS`=64
  available) via batch-gpu NAP. That reopens **migShapeRe widening** for the
  trailing `g` (`a2-highgpu-1g`) — a new subtask when the A100 attempt runs.

---

## Task 2: Parse scaleUp stockouts via the eventId join

Add the payload structs and a pure, page-level join function. No interface change yet — this function is unit-tested in isolation against the Task 1 fixture.

**Files:**
- Modify: `advisor/internal/evidence/gcplog/gcplog.go` (add structs, constant, `scaleUpFailures`, `matchMig`)
- Test: `advisor/internal/evidence/gcplog/gcplog_test.go`
- Uses: `advisor/internal/evidence/gcplog/testdata/scaleup-stockout-*.json`

**Interfaces:**
- Produces: `func scaleUpFailures(entries []logEntry) []evidence.Observation` where `type logEntry struct { Payload []byte; TS time.Time }`; and `const scaleUpOutOfResources = "scale.up.error.out.of.resources"`.
- Consumes: existing `migShapeRe`, `evidence.Observation`.

- [ ] **Step 1: Write the failing happy-path test.**

Reconcile the inline JSON below with `testdata/scaleup-stockout-*.json` from Task 1 first (field paths, MIG-ID format). Representative shape:

```go
func TestScaleUpFailures_HappyPath(t *testing.T) {
	decision := []byte(`{"decision":{"eventId":"e1","scaleUp":{"increasedMigs":[
	  {"mig":{"name":"gke-spot-demo-nap-g2-standard-4-spot-abc-grp","zone":"us-central1-a"}}]}}}`)
	result := []byte(`{"resultInfo":{"results":[
	  {"eventId":"e1","errorMsg":{"messageId":"scale.up.error.out.of.resources",
	   "parameters":["gke-spot-demo-nap-g2-standard-4-spot-abc-grp"]}}]}}`)
	ts := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	got := scaleUpFailures([]logEntry{{decision, ts}, {result, ts}})
	want := []evidence.Observation{{
		MachineType: "g2-standard-4", Zone: "us-central1-a",
		Reason: "scale.up.error.out.of.resources", At: ts,
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v want %+v", got, want)
	}
}
```

- [ ] **Step 2: Run it — expect a compile failure** (`scaleUpFailures`/`logEntry` undefined).

Run: `cd advisor && go test ./internal/evidence/gcplog/ -run TestScaleUpFailures_HappyPath -v`
Expected: FAIL (undefined).

- [ ] **Step 3: Implement the structs, constant, and join.**

```go
const scaleUpOutOfResources = "scale.up.error.out.of.resources"

type logEntry struct {
	Payload []byte
	TS      time.Time
}

type decisionPayload struct {
	Decision struct {
		EventID string `json:"eventId"`
		ScaleUp struct {
			IncreasedMigs []struct {
				Mig struct {
					Name string `json:"name"`
					Zone string `json:"zone"`
				} `json:"mig"`
			} `json:"increasedMigs"`
		} `json:"scaleUp"`
	} `json:"decision"`
}

type resultPayload struct {
	ResultInfo struct {
		Results []struct {
			EventID  string `json:"eventId"`
			ErrorMsg struct {
				MessageID  string   `json:"messageId"`
				Parameters []string `json:"parameters"`
			} `json:"errorMsg"`
		} `json:"results"`
	} `json:"resultInfo"`
}

// migName reduces a MIG ID (bare name or full resource URL) to its trailing
// name segment, so a decision's increasedMigs name and a result's failing-MIG
// parameter compare regardless of which form the API used. Task 1 confirms the
// form; suffix reduction is correct for both.
func migName(id string) string {
	if i := strings.LastIndex(id, "/"); i >= 0 {
		return id[i+1:]
	}
	return id
}

// scaleUpFailures joins scale-up decisions to their eventResults across a page
// of log entries and emits one (shape, zone) observation per failing MIG whose
// scale-up was refused for out-of-resources. Anything it cannot attribute to
// both a shape and a zone is dropped.
func scaleUpFailures(entries []logEntry) []evidence.Observation {
	// eventId -> failing-MIG-name -> zone
	zoneByEventMig := map[string]map[string]string{}
	for _, e := range entries {
		var d decisionPayload
		if json.Unmarshal(e.Payload, &d) != nil || d.Decision.EventID == "" {
			continue
		}
		m := zoneByEventMig[d.Decision.EventID]
		if m == nil {
			m = map[string]string{}
			zoneByEventMig[d.Decision.EventID] = m
		}
		for _, im := range d.Decision.ScaleUp.IncreasedMigs {
			m[migName(im.Mig.Name)] = im.Mig.Zone
		}
	}
	var out []evidence.Observation
	for _, e := range entries {
		var rp resultPayload
		if json.Unmarshal(e.Payload, &rp) != nil {
			continue
		}
		for _, res := range rp.ResultInfo.Results {
			if res.ErrorMsg.MessageID != scaleUpOutOfResources {
				continue
			}
			migs := zoneByEventMig[res.EventID] // nil if orphan → no zones
			for _, param := range res.ErrorMsg.Parameters {
				name := migName(param)
				zone, ok := migs[name]
				if !ok || zone == "" {
					continue
				}
				s := migShapeRe.FindStringSubmatch(name)
				if len(s) != 2 {
					continue
				}
				out = append(out, evidence.Observation{
					MachineType: s[1], Zone: zone,
					Reason: scaleUpOutOfResources, At: e.TS,
				})
			}
		}
	}
	return out
}
```

- [ ] **Step 4: Run the happy-path test — expect PASS.**

Run: `cd advisor && go test ./internal/evidence/gcplog/ -run TestScaleUpFailures_HappyPath -v`
Expected: PASS.

- [ ] **Step 5: Add the edge-case tests.**

```go
func TestScaleUpFailures_OrphanResultDropped(t *testing.T) {
	// result references eventId with no decision in the page
	result := []byte(`{"resultInfo":{"results":[{"eventId":"missing",
	  "errorMsg":{"messageId":"scale.up.error.out.of.resources",
	  "parameters":["gke-spot-demo-nap-g2-standard-4-spot-abc-grp"]}}]}}`)
	if got := scaleUpFailures([]logEntry{{result, time.Now()}}); got != nil {
		t.Fatalf("orphan result must yield nothing, got %+v", got)
	}
}

func TestScaleUpFailures_PartialFailureOneZone(t *testing.T) {
	decision := []byte(`{"decision":{"eventId":"e1","scaleUp":{"increasedMigs":[
	  {"mig":{"name":"mig-a","zone":"us-central1-a"}},
	  {"mig":{"name":"mig-b","zone":"us-central1-b"}}]}}}`)
	// only mig-a (a g2-standard-4 NAP MIG) failed; use real NAP names
	decision = []byte(`{"decision":{"eventId":"e1","scaleUp":{"increasedMigs":[
	  {"mig":{"name":"gke-spot-demo-nap-g2-standard-4-spot-aaa-grp","zone":"us-central1-a"}},
	  {"mig":{"name":"gke-spot-demo-nap-g2-standard-4-spot-bbb-grp","zone":"us-central1-b"}}]}}}`)
	result := []byte(`{"resultInfo":{"results":[{"eventId":"e1",
	  "errorMsg":{"messageId":"scale.up.error.out.of.resources",
	  "parameters":["gke-spot-demo-nap-g2-standard-4-spot-aaa-grp"]}}]}}`)
	got := scaleUpFailures([]logEntry{{decision, time.Unix(0, 0)}, {result, time.Unix(0, 0)}})
	if len(got) != 1 || got[0].Zone != "us-central1-a" {
		t.Fatalf("want exactly one obs in us-central1-a, got %+v", got)
	}
}

func TestScaleUpFailures_NonStockoutIgnored(t *testing.T) {
	decision := []byte(`{"decision":{"eventId":"e1","scaleUp":{"increasedMigs":[
	  {"mig":{"name":"gke-spot-demo-nap-g2-standard-4-spot-abc-grp","zone":"us-central1-a"}}]}}}`)
	result := []byte(`{"resultInfo":{"results":[{"eventId":"e1",
	  "errorMsg":{"messageId":"scale.up.error.quota.exceeded",
	  "parameters":["gke-spot-demo-nap-g2-standard-4-spot-abc-grp"]}}]}}`)
	if got := scaleUpFailures([]logEntry{{decision, time.Unix(0, 0)}, {result, time.Unix(0, 0)}}); got != nil {
		t.Fatalf("quota error must be ignored, got %+v", got)
	}
}

func TestScaleUpFailures_UnparseableMigDropped(t *testing.T) {
	decision := []byte(`{"decision":{"eventId":"e1","scaleUp":{"increasedMigs":[
	  {"mig":{"name":"gke-spot-demo-default-pool-xyz-grp","zone":"us-central1-a"}}]}}}`)
	result := []byte(`{"resultInfo":{"results":[{"eventId":"e1",
	  "errorMsg":{"messageId":"scale.up.error.out.of.resources",
	  "parameters":["gke-spot-demo-default-pool-xyz-grp"]}}]}}`)
	if got := scaleUpFailures([]logEntry{{decision, time.Unix(0, 0)}, {result, time.Unix(0, 0)}}); got != nil {
		t.Fatalf("non-NAP MIG has no parseable shape; must drop, got %+v", got)
	}
}
```

- [ ] **Step 6: Run the full package — expect PASS.**

Run: `cd advisor && go test ./internal/evidence/gcplog/ -v`
Expected: PASS (all new + existing).

- [ ] **Step 7: Mutation check.** Temporarily change `res.ErrorMsg.MessageID != scaleUpOutOfResources` to `== `; confirm `TestScaleUpFailures_NonStockoutIgnored` and the happy path flip. Revert. Confirm `git diff` empty after revert.

- [ ] **Step 8: Commit.**

```bash
git add advisor/internal/evidence/gcplog/gcplog.go advisor/internal/evidence/gcplog/gcplog_test.go
git commit -S -m "✨ feat(evidence): join scaleUp stockouts into (shape, zone) evidence"
```

---

## Task 3: Broaden the Logging filter

**Files:**
- Modify: `advisor/internal/evidence/gcplog/gcplog.go` (`Filter`)
- Test: `advisor/internal/evidence/gcplog/gcplog_test.go` (or `logsource_test.go`, wherever `Filter` is currently asserted)

**Interfaces:**
- Produces: same `Filter(project, cluster, location string, since time.Time) string` signature; output now also matches `decision.scaleUp` and `resultInfo`.

- [ ] **Step 1: Write the failing test.**

```go
func TestFilterIncludesScaleUpAndResults(t *testing.T) {
	f := Filter("p", "spot-demo", "us-central1", time.Unix(0, 0).UTC())
	for _, want := range []string{
		"jsonPayload.noDecisionStatus.noScaleUp:*",
		"jsonPayload.decision.scaleUp:*",
		"jsonPayload.resultInfo:*",
	} {
		if !strings.Contains(f, want) {
			t.Errorf("filter missing %q\n%s", want, f)
		}
	}
}
```

- [ ] **Step 2: Run it — expect FAIL** (only noScaleUp present).

Run: `cd advisor && go test ./internal/evidence/gcplog/ -run TestFilterIncludesScaleUpAndResults -v`
Expected: FAIL.

- [ ] **Step 3: Implement.** Replace the single `noScaleUp:*` line with an OR group:

```go
func Filter(project, cluster, location string, since time.Time) string {
	return strings.Join([]string{
		fmt.Sprintf(`logName="projects/%s/logs/%s"`, project, logName),
		fmt.Sprintf(`resource.labels.cluster_name=%q`, cluster),
		fmt.Sprintf(`resource.labels.location=%q`, location),
		`(jsonPayload.noDecisionStatus.noScaleUp:* OR jsonPayload.decision.scaleUp:* OR jsonPayload.resultInfo:*)`,
		fmt.Sprintf(`timestamp>=%q`, since.UTC().Format(time.RFC3339)),
	}, "\n")
}
```

- [ ] **Step 4: Run it — expect PASS.**

Run: `cd advisor && go test ./internal/evidence/gcplog/ -run TestFilterIncludesScaleUpAndResults -v`
Expected: PASS.

- [ ] **Step 5: Commit.**

```bash
git add advisor/internal/evidence/gcplog/gcplog.go advisor/internal/evidence/gcplog/gcplog_test.go
git commit -S -m "✨ feat(evidence): widen the visibility-log filter to scale-up results"
```

---

## Task 4: Wire the join into the reader and rename `NoScaleUp` → `Refusals`

Collapse the reconciler `Log` interface to one method and have the reader return both noScaleUp and scaleUp observations from one query.

**Files:**
- Modify: `advisor/internal/evidence/gcplog/gcplog.go` (rename method `NoScaleUp`→`Refusals`; build `[]logEntry`; call `ParseEntry` per entry AND `scaleUpFailures` over the slice)
- Modify: `advisor/internal/evidence/gcplog/fake/fake.go` (`NoScaleUpFn`→`RefusalsFn`, method rename)
- Modify: `advisor/internal/reconcile/reconcile.go` (interface method + `ingest` call site `:254`)
- Modify: `advisor/internal/reconcile/reconcile_test.go` (~9 `NoScaleUpFn:` sites)
- Modify: `advisor/internal/cli/reconcile_test.go` (any `NoScaleUpFn:` sites)

**Interfaces:**
- Produces: `Refusals(ctx context.Context, since time.Time) ([]evidence.Observation, error)` on both the interface and `*gcplog.Reader`; `gcplogfake.Log{RefusalsFn: ...}`.

- [ ] **Step 1: Write the failing reader test** asserting the method returns both kinds from one page (build the reader loop to accept injected entries, or test via the exported method with a fake `*logging.Service` if one exists; otherwise assert at the `scaleUpFailures`+`ParseEntry` merge boundary). Minimal seam test:

```go
// Matches the existing inline fixture pattern in gcplog_test.go (os.ReadFile);
// there is no readFixture helper — do not invent one.
func TestReaderMergesNoScaleUpAndScaleUp(t *testing.T) {
	read := func(name string) []byte {
		b, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	ns := read("provoke-both-lists.json")            // yields ParseEntry obs (may be 0 — pod facts)
	dec := read("scaleup-stockout-decision.json")
	res := read("scaleup-stockout-result.json")
	ts := time.Unix(0, 0)
	var got []evidence.Observation
	got = append(got, ParseEntry(ns, ts)...)
	got = append(got, scaleUpFailures([]logEntry{{dec, ts}, {res, ts}})...)
	if len(got) == 0 {
		t.Fatal("expected at least the scaleUp stockout observation")
	}
}
```

- [ ] **Step 2: Run it — expect FAIL/compile error** until helpers exist.

Run: `cd advisor && go test ./internal/evidence/gcplog/ -run TestReaderMergesNoScaleUpAndScaleUp -v`
Expected: FAIL.

- [ ] **Step 3: Rename and wire the reader.** In `gcplog.go`, rename `NoScaleUp` to `Refusals`; inside, collect `entries := []logEntry{{Payload: b, TS: ts}}` in the existing loop, keep `out = append(out, ParseEntry(b, ts)...)`, and after the loop `out = append(out, scaleUpFailures(entries)...)`.

- [ ] **Step 4: Rename the fake by hand.** In `fake/fake.go`, rename the field `NoScaleUpFn`→`RefusalsFn`, the method `func (l *Log) NoScaleUp`→`Refusals`, and the `"fake: NoScaleUpFn not set"` error string. Rename any `.NoScaleUp(` / `NoScaleUpFn` in `fake/fake_test.go` too.

- [ ] **Step 5: Rename the interface + call site.** In `reconcile.go`, change the interface method and update `ingest` (`obs, err := d.Log.Refusals(ctx, since)` at `:254`).

- [ ] **Step 6: Rename the reconciler/cli test call sites.** These set the fake via the `NoScaleUpFn:` field only, so a field-name replace suffices:

```bash
cd advisor && grep -rl 'NoScaleUpFn' internal/reconcile internal/cli | xargs sed -i '' 's/NoScaleUpFn/RefusalsFn/g'
```

Guard: the exported reader method (`gcplog.go`) is renamed by hand in Step 3. Do NOT blanket-sed `gcplog.go` — its `payload` struct field `NoScaleUp` (json tag `noScaleUp`) and the lowercase `noScaleUp` JSON paths in `Filter`/comments must stay. Verify:

```bash
grep -rn 'func.*NoScaleUp\|NoScaleUpFn\|\.NoScaleUp(' internal || echo "no NoScaleUp identifiers remain"
grep -c 'noScaleUp' internal/evidence/gcplog/gcplog.go   # lowercase JSON path must still be present (>0)
```

- [ ] **Step 7: Run the affected packages — expect PASS.**

Run: `cd advisor && go test ./internal/evidence/gcplog/... ./internal/reconcile/... ./internal/cli/... -v`
Expected: PASS.

- [ ] **Step 8: Full suite.**

Run: `PIP_INDEX_URL=https://pypi.org/simple make test`
Expected: EXIT 0, no FAIL/panic.

- [ ] **Step 9: Commit.**

```bash
git add -A
git commit -S -m "♻️ refactor(reconcile): collapse Log to one Refusals collecting both signals"
```

---

## Task 5: Provocation burst manifest

**Files:**
- Create: `demo/act4/provoke-scaleup-stockout.yaml`

**Interfaces:** none (demo artifact). Consistent with `provoke-noscaleup.yaml`, this manifest is NOT added to the `test-manifests` target.

- [ ] **Step 1: Write the manifest** — a Job with high parallelism requesting L4 GPUs against `batch-gpu`, pinned via the existing spot pool from Task 1, sized to outrun one zone's spot L4 supply. Header comment states what it produces (a `scaleUp` stockout on the existing MIG, not a NAP noScaleUp) and points at the runbook.

```yaml
# Provokes a scale.up.error.out.of.resources on an EXISTING spot L4 MIG.
# Unlike provoke-noscaleup.yaml (which makes NAP refuse a too-big pod), this
# drives real demand into the scaleup-provoke-l4 pool until its zone runs out of
# spot L4, which the autoscaler reports as a scaleUp eventResult error — the
# signal the evidence reader now collects. See demo/act4/runbook.md.
#
# Prereq: the scaleup-provoke-l4 spot pool exists (see runbook). Tear this down
# immediately after the tick observes the stockout — it burns real L4 spot.
apiVersion: batch/v1
kind: Job
metadata:
  name: provoke-scaleup-stockout
  namespace: spot-demo
spec:
  parallelism: 16          # well past max-nodes 8 × 1 GPU, to force scale-up beyond supply
  completions: 16
  backoffLimit: 0
  template:
    spec:
      restartPolicy: Never
      nodeSelector:
        cloud.google.com/compute-class: batch-gpu
      tolerations:
      - key: nvidia.com/gpu
        operator: Exists
        effect: NoSchedule
      - key: cloud.google.com/gke-spot
        operator: Equal
        value: "true"
        effect: NoSchedule
      containers:
      - name: pause
        image: registry.k8s.io/pause:3.10
        resources:
          requests:
            nvidia.com/gpu: "1"
          limits:
            nvidia.com/gpu: "1"
```

- [ ] **Step 2: Validate it parses.**

Run: `kubeconform -strict -ignore-missing-schemas -summary demo/act4/provoke-scaleup-stockout.yaml`
Expected: Valid (or Skipped for the batch/v1 Job if no schema; no Invalid/Errors).

- [ ] **Step 3: Commit.**

```bash
git add demo/act4/provoke-scaleup-stockout.yaml
git commit -S -m "✨ feat(act4): add the L4 scaleUp-stockout provocation manifest"
```

---

## Task 6: Runbook Beat + live end-to-end verification (LIVE GATE)

Rewrite the Act 4 runbook to document the provocation and record a real end-to-end run: provoke → reconcile tick → `observations > 0` → ledger entry → evidence-fast-path ladder change. **Requires an explicit go-ahead.**

**Files:**
- Modify: `demo/act4/runbook.md`

- [ ] **Step 1: Get go-ahead** for the billing session.

- [ ] **Step 2: Ensure the pool exists** (recreate from Task 1 Step 2 if torn down).

- [ ] **Step 3: Provoke and reconcile.**

```bash
kubectl apply -f demo/act4/provoke-scaleup-stockout.yaml
# wait for the pool to attempt scale-up and hit out-of-resources, then run a tick:
kubectl -n spot-demo create job --from=cronjob/capacity-advisor manual-tick-$(date +%s)
kubectl -n spot-demo logs job/manual-tick-... | grep -i 'observations\|evidence\|ladder'
```

- [ ] **Step 4: Confirm the ledger entry** carries `(g2-standard-4, us-central1-a)` with reason `scale.up.error.out.of.resources`, and the class ladder changed via the evidence fast path (`FastPathTicks`, not the full `ConsecutiveTicks`).

- [ ] **Step 5: Tear down the burst** (Task 1 Step 7).

- [ ] **Step 6: Write the Beat** into `runbook.md` — the prereq pool, the apply, the expected `observations: 1`, the ledger entry, and the ladder change, quoting the real tick output (as `ef056b6` recorded the reconciler verification). State the cost/teardown note.

- [ ] **Step 7: Commit.**

```bash
git add demo/act4/runbook.md
git commit -S -m "📝 docs(act4): document and verify the scaleUp-stockout provocation"
```

---

## Self-Review

**Spec coverage:**
- §5.1 interface rename → Task 4. §5.2 filter → Task 3. §5.3 join/parse → Task 2, wired in Task 4. §5.4 data flow → realized across Tasks 2–4. §6 provocation → Tasks 5–6. §7 live-first → Tasks 1 (capture) and 6 (e2e) gate the code tasks. §8 testing → Task 2 Steps 1/5 (six cases incl. merge in Task 4 Step 1), mutation in Task 2 Step 7. §9 risks: MIG-ID format → Task 1 Step 6 + `migName`; L4 availability → Task 1 tuning; orphans → `TestScaleUpFailures_OrphanResultDropped`.
- Invariants (§4): drop-unattributable → orphan/unparseable/no-zone drops; enumerate-don't-allowlist inverted for scaleUp → only the explicit stockout messageId, tested by `NonStockoutIgnored`.

**Placeholder scan:** No TBD/TODO in code steps; "Capture notes" is an intentional live-capture output slot, and Tasks 2–4 carry real representative JSON so they are runnable before capture, with an explicit reconcile instruction.

**Type consistency:** `Refusals(ctx, since) ([]evidence.Observation, error)` used identically in gcplog, fake, and reconcile. `logEntry{Payload, TS}`, `scaleUpFailures([]logEntry) []evidence.Observation`, `migName(string) string`, `scaleUpOutOfResources` referenced consistently across Tasks 2 and 4. `Observation` fields (`MachineType`, `Zone`, `Reason`, `At`) match `evidence.go`.
