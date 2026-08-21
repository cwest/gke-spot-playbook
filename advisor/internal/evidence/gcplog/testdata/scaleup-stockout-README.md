# scaleup-stockout-*.json — provenance

These two fixtures model a genuine `scale.up.error.out.of.resources` on an
existing NAP MIG, split across the two log events the reader must join by
`eventId`:

- `scaleup-stockout-decision.json` — a `decision.scaleUp` carrying the MIG
  name and zone.
- `scaleup-stockout-result.json` — the `eventResult` naming the failing MIG.

**These are hand-authored, not a raw capture.** A live L4 provocation on
2026-08-03 did not stock out (`us-central1-a` had L4 spot capacity; the
scale-up succeeded), so no real failure pair was captured. What IS grounded in
that live run:

- The **structure** is confirmed from real `spot-demo` visibility logs:
  `decision.scaleUp.increasedMigs[].mig.{name,zone}`, `decision.eventId`,
  `resultInfo.results[].eventId`, and that `errorMsg` is present only on
  failure (a successful result carries just `eventId`).
- The `errorMsg` shape (`messageId` + `parameters` = failing MIG IDs) is from
  GKE's cluster-autoscaler-visibility documentation.
- The MIG name uses the **NAP** form (`gke-<cluster>-nap-<shape>-...`) because
  that is the only form `migShapeRe` parses — an explicit-pool MIG name has no
  `nap-` segment and yields no shape.

**Still unverified against reality:** whether `errorMsg.parameters` carries a
bare MIG name or a full resource URL. `migName()` normalizes both, so the
parser is robust either way. Replace these files with a real capture if/when an
A100 (`a2-highgpu`) spot provocation genuinely stocks out.
