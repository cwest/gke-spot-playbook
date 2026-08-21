// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package cost turns node-inventory samples into a two-factor savings report:
// the spot discount (spot vs on-demand rates for the same node-hours) and the
// duty cycle (elastic peak-sized usage vs an always-on on-demand pool).
package cost

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/cwest/gke-spot-instance-node-pools/advisor/internal/pricing"
)

// Usage aggregates the samples for one (machine type, lifecycle) pair.
type Usage struct {
	MachineType string
	Lifecycle   string // "spot" | "on-demand"
	NodeSeconds float64
	Nodes       int // distinct nodes observed
	PeakNodes   int // max concurrent in any sample
}

// Report is the full cost readout for one observation window.
type Report struct {
	GeneratedAt        time.Time
	Region             string
	WindowSeconds      float64
	Usages             []Usage
	ActualUSD          float64  // Σ nodeSeconds × actual rate
	SpotAtOnDemandUSD  float64  // same node-seconds, on-demand rates
	AlwaysOnUSD        float64  // peak-sized on-demand pool × window
	AlwaysOnDailyUSD   float64  // extrapolated 24h
	SpotDiscountPct    float64  // 1 - Actual/SpotAtOnDemand
	DutyCyclePct       float64  // 1 - SpotAtOnDemand/AlwaysOn
	CombinedSavingsPct float64  // 1 - Actual/AlwaysOn
	Unpriced           []string // machine types we couldn't price
	Caveats            []string `json:",omitempty"` // non-fatal warnings surfaced in the report header

	// lines is the priced per-usage breakdown used only by RenderMarkdown; it is
	// unexported so it stays out of the JSON artifact (recomputed on each run).
	lines []lineItem
}

// lineItem is one priced row of the "what this run actually cost" table.
type lineItem struct {
	machineType, lifecycle string
	nodeHours, rate, cost  float64
}

// SpotSource resolves the latest spot rate for a machine type in a region.
type SpotSource interface {
	SpotHourlyUSD(ctx context.Context, region, machineType string) (float64, error)
}

// CSV column indices for the collector's schema:
// ts,node,machine_type,lifecycle,compute_class
const (
	colTS = iota
	colNode
	colMachineType
	colLifecycle
	colComputeClass
	numCols
)

// ParseSamples aggregates collector CSV rows into per-(machineType,lifecycle)
// usage. Each row represents one node alive for `interval`; NodeSeconds is
// rows×interval. WindowSeconds is (last−first timestamp)+interval, the span the
// samples actually cover. Unknown lifecycle values are rejected.
//
// The returned peakConcurrent map gives, per machine type, the peak CONCURRENT
// node count summed across BOTH lifecycles: for each timestamp the spot and
// on-demand counts for that machine type are added, then the max over
// timestamps is taken. This is what the always-on on-demand counterfactual must
// provision, and it exceeds max(per-lifecycle PeakNodes) whenever a machine
// type runs spot and on-demand at the same time.
//
// The returned warnings slice carries non-fatal advisories the caller should
// surface (e.g. a sample-interval mismatch); it is empty when nothing is amiss.
func ParseSamples(r io.Reader, interval time.Duration) ([]Usage, float64, map[string]int, []string, error) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = numCols
	rows, err := cr.ReadAll()
	if err != nil {
		return nil, 0, nil, nil, fmt.Errorf("read samples: %w", err)
	}
	if len(rows) == 0 {
		return nil, 0, nil, nil, errors.New("cost: empty samples file")
	}

	intervalSec := interval.Seconds()

	type key struct{ mt, life string }
	type agg struct {
		rows  int
		nodes map[string]struct{}
		perTS map[string]int // ts -> concurrent count for this key
	}
	aggs := map[key]*agg{}
	// machineTS[mt][ts] = concurrent nodes of that machine type across ALL
	// lifecycles at that timestamp.
	machineTS := map[string]map[string]int{}
	distinctTS := map[time.Time]struct{}{}
	var minTS, maxTS time.Time
	haveTS := false

	for i, row := range rows {
		if i == 0 {
			continue // header
		}
		life := row[colLifecycle]
		if life != "spot" && life != "on-demand" {
			return nil, 0, nil, nil, fmt.Errorf("cost: row %d: unknown lifecycle %q", i+1, life)
		}
		ts, err := time.Parse(time.RFC3339, row[colTS])
		if err != nil {
			return nil, 0, nil, nil, fmt.Errorf("cost: row %d: bad timestamp %q: %w", i+1, row[colTS], err)
		}
		if !haveTS || ts.Before(minTS) {
			minTS = ts
		}
		if !haveTS || ts.After(maxTS) {
			maxTS = ts
		}
		haveTS = true
		distinctTS[ts] = struct{}{}

		k := key{row[colMachineType], life}
		a := aggs[k]
		if a == nil {
			a = &agg{nodes: map[string]struct{}{}, perTS: map[string]int{}}
			aggs[k] = a
		}
		a.rows++
		a.nodes[row[colNode]] = struct{}{}
		a.perTS[row[colTS]]++

		mtTS := machineTS[row[colMachineType]]
		if mtTS == nil {
			mtTS = map[string]int{}
			machineTS[row[colMachineType]] = mtTS
		}
		mtTS[row[colTS]]++
	}

	usages := make([]Usage, 0, len(aggs))
	for k, a := range aggs {
		peak := 0
		for _, n := range a.perTS {
			if n > peak {
				peak = n
			}
		}
		usages = append(usages, Usage{
			MachineType: k.mt,
			Lifecycle:   k.life,
			NodeSeconds: float64(a.rows) * intervalSec,
			Nodes:       len(a.nodes),
			PeakNodes:   peak,
		})
	}
	// Deterministic order for stable rendering and golden tests.
	sort.Slice(usages, func(i, j int) bool {
		if usages[i].MachineType != usages[j].MachineType {
			return usages[i].MachineType < usages[j].MachineType
		}
		return usages[i].Lifecycle < usages[j].Lifecycle
	})

	peakConcurrent := make(map[string]int, len(machineTS))
	for mt, tsCounts := range machineTS {
		peak := 0
		for _, n := range tsCounts {
			if n > peak {
				peak = n
			}
		}
		peakConcurrent[mt] = peak
	}

	window := 0.0
	if haveTS {
		window = maxTS.Sub(minTS).Seconds() + intervalSec
	}

	var warnings []string
	if w := intervalMismatchWarning(distinctTS, interval); w != "" {
		warnings = append(warnings, w)
	}
	return usages, window, peakConcurrent, warnings, nil
}

// intervalMismatchWarning returns a non-fatal advisory when the modal delta
// between consecutive DISTINCT sample timestamps disagrees with the caller's
// interval. NodeSeconds is rows×interval, so a wrong interval silently skews
// every dollar figure; the modal (most common) delta tolerates the odd gap from
// a missed sample. With fewer than two distinct timestamps there is nothing to
// compare and it returns "".
func intervalMismatchWarning(distinct map[time.Time]struct{}, interval time.Duration) string {
	if len(distinct) < 2 {
		return ""
	}
	ts := make([]time.Time, 0, len(distinct))
	for t := range distinct {
		ts = append(ts, t)
	}
	sort.Slice(ts, func(i, j int) bool { return ts[i].Before(ts[j]) })

	counts := map[time.Duration]int{}
	for i := 1; i < len(ts); i++ {
		counts[ts[i].Sub(ts[i-1])]++
	}
	var modal time.Duration
	best := 0
	for d, c := range counts {
		if c > best || (c == best && d < modal) {
			best, modal = c, d
		}
	}
	if modal == interval {
		return ""
	}
	return fmt.Sprintf("sample-interval mismatch: rows are spaced ~%s but --interval is %s; node-hours (rows × interval) may be inaccurate", modal, interval)
}

// Build resolves rates and computes the two-factor savings. Machine types the
// on-demand source cannot price (pricing.ErrUnsupported) are recorded in
// Unpriced and excluded from every total. Percentages are guarded against zero
// denominators.
func Build(ctx context.Context, usages []Usage, window float64, peakConcurrent map[string]int, region string, onDemand pricing.Source, spot SpotSource, now time.Time) (*Report, error) {
	rep := &Report{
		GeneratedAt:   now.UTC(),
		Region:        region,
		WindowSeconds: window,
		Usages:        usages,
	}

	// Resolve on-demand rates once per machine type; classify unsupported types.
	odRate := map[string]float64{}
	unpriced := map[string]struct{}{}
	for _, u := range usages {
		if _, done := odRate[u.MachineType]; done {
			continue
		}
		if _, skip := unpriced[u.MachineType]; skip {
			continue
		}
		rate, err := onDemand.OnDemandHourlyUSD(ctx, region, u.MachineType)
		if err != nil {
			if errors.Is(err, pricing.ErrUnsupported) {
				unpriced[u.MachineType] = struct{}{}
				continue
			}
			return nil, fmt.Errorf("on-demand price %s: %w", u.MachineType, err)
		}
		odRate[u.MachineType] = rate
	}

	spotRate := map[string]float64{}
	for _, u := range usages {
		if _, skip := unpriced[u.MachineType]; skip {
			continue
		}
		od := odRate[u.MachineType]
		hours := u.NodeSeconds / 3600

		actualRate := od
		if u.Lifecycle == "spot" {
			sr, ok := spotRate[u.MachineType]
			if !ok {
				r, err := spot.SpotHourlyUSD(ctx, region, u.MachineType)
				if err != nil {
					return nil, fmt.Errorf("spot price %s: %w", u.MachineType, err)
				}
				sr = r
				spotRate[u.MachineType] = sr
			}
			actualRate = sr
		}
		rep.ActualUSD += hours * actualRate
		rep.SpotAtOnDemandUSD += hours * od
		rep.lines = append(rep.lines, lineItem{
			machineType: u.MachineType, lifecycle: u.Lifecycle,
			nodeHours: hours, rate: actualRate, cost: hours * actualRate,
		})
	}

	// Always-on counterfactual: size the on-demand pool to the peak CONCURRENT
	// nodes across BOTH lifecycles per machine type (peakConcurrent), so a
	// machine type running spot and on-demand simultaneously is provisioned for
	// the summed peak rather than the larger single-lifecycle peak. Unpriced
	// machine types are excluded here just as they are from every other total.
	for mt, peak := range peakConcurrent {
		if _, skip := unpriced[mt]; skip {
			continue
		}
		rep.AlwaysOnUSD += float64(peak) * odRate[mt] * window / 3600
	}
	if window > 0 {
		rep.AlwaysOnDailyUSD = rep.AlwaysOnUSD * 86400 / window
	}

	if rep.SpotAtOnDemandUSD > 0 {
		rep.SpotDiscountPct = 1 - rep.ActualUSD/rep.SpotAtOnDemandUSD
	}
	if rep.AlwaysOnUSD > 0 {
		rep.DutyCyclePct = 1 - rep.SpotAtOnDemandUSD/rep.AlwaysOnUSD
		rep.CombinedSavingsPct = 1 - rep.ActualUSD/rep.AlwaysOnUSD
	}

	for mt := range unpriced {
		rep.Unpriced = append(rep.Unpriced, mt)
	}
	sort.Strings(rep.Unpriced)

	return rep, nil
}

// RenderMarkdown renders the report as the two-factor savings narrative: what the
// run actually cost, the spot discount, the duty cycle, and their combination.
func RenderMarkdown(rep *Report) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "# Cost report — %s\n\n", rep.Region)
	fmt.Fprintf(&b, "Generated %s | observation window %s\n\n",
		rep.GeneratedAt.UTC().Format(time.RFC3339), humanWindow(rep.WindowSeconds))

	for _, c := range rep.Caveats {
		fmt.Fprintf(&b, "> ⚠️ %s\n\n", c)
	}

	b.WriteString("## What this run actually cost\n\n")
	b.WriteString("| machine type | lifecycle | node-hours | rate $/hr | cost |\n")
	b.WriteString("|---|---|---|---|---|\n")
	for _, l := range rep.lines {
		fmt.Fprintf(&b, "| %s | %s | %.3f | %.4f | $%.4f |\n",
			l.machineType, l.lifecycle, l.nodeHours, l.rate, l.cost)
	}
	fmt.Fprintf(&b, "\n**Actual: $%.4f**\n\n", rep.ActualUSD)

	b.WriteString("## Factor 1 — the spot discount\n\n")
	fmt.Fprintf(&b, "Same node-hours at on-demand list prices: $%.4f → spot saved %.1f%%\n\n",
		rep.SpotAtOnDemandUSD, rep.SpotDiscountPct*100)

	b.WriteString("## Factor 2 — the duty cycle\n\n")
	fmt.Fprintf(&b, "Always-on peak-sized on-demand pool for this window: $%.4f\n", rep.AlwaysOnUSD)
	fmt.Fprintf(&b, "(≈ $%.2f/day if left running) → elasticity saved %.1f%%\n\n",
		rep.AlwaysOnDailyUSD, rep.DutyCyclePct*100)

	b.WriteString("## Combined\n\n")
	fmt.Fprintf(&b, "%.1f%% spot discount compounded with %.1f%% duty cycle → **%.1f%% cheaper than the standard way**\n",
		rep.SpotDiscountPct*100, rep.DutyCyclePct*100, rep.CombinedSavingsPct*100)

	if len(rep.Unpriced) > 0 {
		fmt.Fprintf(&b, "\n> Note: excluded unpriced machine types: %s\n", strings.Join(rep.Unpriced, ", "))
	}
	return b.Bytes()
}

// RenderJSON serializes the report for machine consumption.
func RenderJSON(rep *Report) ([]byte, error) {
	return json.MarshalIndent(rep, "", "  ")
}

// humanWindow formats seconds as "{m}m{s}s" (e.g. 150 → "2m30s").
func humanWindow(sec float64) string {
	total := int(sec + 0.5)
	return fmt.Sprintf("%dm%ds", total/60, total%60)
}
