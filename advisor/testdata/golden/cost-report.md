# Cost report — us-central1

Generated 2026-07-24T06:05:00Z | observation window 2m30s

## What this run actually cost

| machine type | lifecycle | node-hours | rate $/hr | cost |
|---|---|---|---|---|
| e2-standard-4 | on-demand | 0.042 | 0.1500 | $0.0062 |
| t2d-standard-8 | spot | 0.100 | 0.0554 | $0.0055 |

**Actual: $0.0118**

## Factor 1 — the spot discount

Same node-hours at on-demand list prices: $0.0401 → spot saved 70.6%

## Factor 2 — the duty cycle

Always-on peak-sized on-demand pool for this window: $0.0626
(≈ $36.05/day if left running) → elasticity saved 36.0%

## Combined

70.6% spot discount compounded with 36.0% duty cycle → **81.2% cheaper than the standard way**
