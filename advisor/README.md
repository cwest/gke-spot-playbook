# capacity-advisor

`capacity-advisor` queries the Compute Engine [capacity-advice beta APIs](https://cloud.google.com/compute/docs/reference/rest/beta/advice/capacity?utm_campaign=CDR_0x5d16fa53_user-journey_b550269617&utm_medium=external&utm_source=lab) to score
[Spot VM](https://cloud.google.com/compute/docs/instances/spot?utm_campaign=CDR_0x5d16fa53_user-journey_b550269617&utm_medium=external&utm_source=lab) candidates across the regions and machine types you care about, then
renders GKE artifacts from the result: a ComputeClass ladder of fallback rungs, a
human-readable ranking report, and a `cluster-config.env` you can source when
creating a cluster. It ranks candidates by a composite of obtainability,
estimated uptime, historical preemption, and Spot price so you can pick the most
durable, cheapest Spot capacity instead of guessing.

## Prerequisites

- A GCP project with the **Compute Engine API** enabled.
- Credentials with at least [**`roles/compute.viewer`**](https://cloud.google.com/iam/docs/roles-permissions/compute?utm_campaign=CDR_0x5d16fa53_user-journey_b550269617&utm_medium=external&utm_source=lab) on that project.
- [Application Default Credentials](https://cloud.google.com/docs/authentication/application-default-credentials?utm_campaign=CDR_0x5d16fa53_user-journey_b550269617&utm_medium=external&utm_source=lab) configured locally:

  ```bash
  gcloud auth application-default login
  ```

The capacity-advice endpoints are **beta/preview**; their scores are advisory and
may drift between calls.

## Configuration

Behaviour is driven by an `advisor.yaml` (see the repo root for a full example):

```yaml
project: example-sandbox
allowedRegions: ["us"]          # keywords, globs (us-*), or explicit regions
profiles:
  cpu-batch:
    kind: cpu
    machineTypes: [e2-standard-8, n2-standard-8, t2d-standard-8]
    size: 20
caps:
  maxSpotRungs: 3
```

Per-run `--regions` flag override is supported; it replaces the configured
`allowedRegions` for that run. Region values still go through keyword and glob
expansion (e.g., `us`, `us-*`) as defined in the config. Analyze also writes a
`cluster-config-<profile>.env` artifact to `--out` (designate it via `cp` when
provisioning your cluster).

## Commands

### `analyze`

Query the advice APIs, score candidates, and write `analysis-<profile>.json`.
With `--render`, also emit the rendered artifacts in one pass.

```bash
go run ./cmd/capacity-advisor analyze \
  --profile cpu-batch \
  --config ../advisor.yaml \
  --out ../out \
  --render
```

| Flag        | Default        | Description                        |
|-------------|----------------|------------------------------------|
| `--config`  | `advisor.yaml` | path to the config file            |
| `--profile` | *(required)*   | profile name to analyze            |
| `--out`     | `out`          | output directory                   |
| `--render`  | `false`        | also render artifacts after scoring|

### `render`

Re-render artifacts from a previously saved analysis, without re-hitting the API:

```bash
go run ./cmd/capacity-advisor render \
  --analysis ../out/analysis-cpu-batch.json \
  --out ../out \
  --max-rungs 3
```

| Flag          | Default | Description                     |
|---------------|---------|---------------------------------|
| `--analysis`  | *(required)* | saved analysis JSON path   |
| `--out`       | `out`   | output directory                |
| `--max-rungs` | `3`     | maximum ComputeClass Spot rungs |

## Output

`analyze --render` (or a subsequent `render`) writes into `--out`:

- `analysis-<profile>.json` — the full scored analysis (input to `render`).
- `computeclass-<cpu|gpu>.yaml` — a GKE ComputeClass with ranked Spot rungs.
- `advice-report-<profile>.md` — ranked candidate table with preemption sparklines.
- `advice-report-<profile>.json` — the same ranking as machine-readable JSON.
- `cluster-config-<profile>.env` — `PROJECT`, `REGION`, and `ZONES` env vars for the
  advisor-chosen region (no machine types).

When you want to use a single `cluster-config.env` path (e.g. in a shell sourcing workflow),
the designation step in demo runbooks creates it via `cp out/cluster-config-<profile>.env out/cluster-config.env`.

## Note

Scores come from a **beta** advice API and are advisory only — they are a ranking
signal, not a capacity or price guarantee. Live data changes between runs, so
re-run `analyze` before acting on the results.
