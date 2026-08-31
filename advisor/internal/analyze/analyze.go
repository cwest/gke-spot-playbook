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

// Package analyze orchestrates advice queries into a scored Analysis.
package analyze

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/cwest/gke-spot-playbook/advisor/internal/advice"
	"github.com/cwest/gke-spot-playbook/advisor/internal/config"
	"github.com/cwest/gke-spot-playbook/advisor/internal/evidence"
	"github.com/cwest/gke-spot-playbook/advisor/internal/machinetype"
	"github.com/cwest/gke-spot-playbook/advisor/internal/regions"
	"github.com/cwest/gke-spot-playbook/advisor/internal/score"
)

// EvidenceSource supplies observed-failure penalties per shape and zone. It is
// an interface rather than a *evidence.Ledger so the one-shot `analyze` command
// can pass nil and stay evidence-free.
type EvidenceSource interface {
	Factor(machineType, zone string) float64
	Dry(machineType, zone string) bool
}

type ledgerSource struct {
	l   *evidence.Ledger
	now time.Time
	p   evidence.Params
}

func (s ledgerSource) Factor(mt, zone string) float64 {
	return s.l.Factor(evidence.Key{MachineType: mt, Zone: zone}, s.now, s.p)
}

func (s ledgerSource) Dry(mt, zone string) bool {
	return evidence.Dry(s.Factor(mt, zone), s.p)
}

// LedgerSource adapts a ledger to EvidenceSource at a fixed instant, so every
// candidate in one tick is scored against the same clock.
func LedgerSource(l *evidence.Ledger, now time.Time, p evidence.Params) EvidenceSource {
	return ledgerSource{l: l, now: now, p: p}
}

type Candidate struct {
	MachineType, Zone, Region string
	Obtainability             float64
	EstimatedUptimeSeconds    int
	Daily                     []float64
	SpotHourlyUSD             float64
	Units                     float64
	UptimeFactor              float64
	PreemptionFactor          float64
	PriceFactor               float64
	EvidenceFactor            float64
	EvidenceDry               bool
	Composite                 float64
	Dropped                   bool
	Flags                     []string
}

// SkippedRegion records a region dropped from the analysis because the
// capacity API rejected it (e.g. the machine family does not exist there).
type SkippedRegion struct {
	Region string `json:"region"`
	Reason string `json:"reason"`
}

type Analysis struct {
	GeneratedAt    time.Time
	Project        string
	Profile        string
	Kind           string
	Size           int32
	AllowedRegions []string
	Candidates     []Candidate
	SkippedRegions []SkippedRegion `json:"skippedRegions,omitempty"`
}

const capacityBatchSize = 5 // documented API cap on machine types per request

func Run(ctx context.Context, api advice.API, cfg *config.Config, profileName string, now time.Time, ev EvidenceSource) (*Analysis, error) {
	profile, ok := cfg.Profiles[profileName]
	if !ok {
		return nil, fmt.Errorf("unknown profile %q", profileName)
	}
	all, err := api.Regions(ctx, cfg.Project)
	if err != nil {
		return nil, fmt.Errorf("list regions: %w", err)
	}
	allowed, err := regions.Expand(cfg.AllowedRegions, all)
	if err != nil {
		return nil, err
	}

	var cands []Candidate
	var skipped []SkippedRegion
	for _, region := range allowed {
		var regionCands []Candidate
		regionOK := true
		for _, batch := range chunk(profile.MachineTypes, capacityBatchSize) {
			results, err := api.Capacity(ctx, advice.CapacityQuery{
				Project: cfg.Project, Region: region,
				MachineTypes: batch, Size: profile.Size, Kind: profile.Kind,
			})
			if err != nil {
				// A single bad region (e.g. no g2 machine types there) must not
				// abort the whole analysis; record it and move on.
				skipped = append(skipped, SkippedRegion{Region: region, Reason: err.Error()})
				regionOK = false
				break
			}
			for _, res := range results {
				for _, shard := range res.Shards {
					c, err := buildCandidate(ctx, api, cfg.Project, region, profile, res, shard)
					if err != nil {
						return nil, err // config-level error (unknown shape): still fatal
					}
					regionCands = append(regionCands, c)
				}
			}
		}
		if regionOK {
			cands = append(cands, regionCands...)
		}
	}
	if len(cands) == 0 && len(skipped) > 0 {
		return nil, fmt.Errorf("analyze: all %d region(s) failed; first: %s: %s",
			len(skipped), skipped[0].Region, skipped[0].Reason)
	}
	finalize(cands, cfg.Scoring, ev)
	return &Analysis{
		GeneratedAt: now, Project: cfg.Project, Profile: profileName,
		Kind: profile.Kind, Size: profile.Size,
		AllowedRegions: allowed, Candidates: cands,
		SkippedRegions: skipped,
	}, nil
}

func buildCandidate(ctx context.Context, api advice.API, project, region string,
	profile config.Profile, res advice.CapacityResult, shard advice.Shard) (Candidate, error) {

	c := Candidate{
		MachineType: shard.MachineType, Zone: shard.Zone, Region: region,
		Obtainability:          res.Obtainability,
		EstimatedUptimeSeconds: res.EstimatedUptimeSeconds,
	}
	units, err := machinetype.Units(shard.MachineType, profile.Kind)
	if err != nil {
		return c, err
	}
	c.Units = units

	hist, err := api.CapacityHistory(ctx, advice.HistoryQuery{
		Project: project, Region: region, Zone: shard.Zone, MachineType: shard.MachineType,
	})
	if err != nil {
		return c, err
	}
	c.Daily = hist.DailyPreemptionRates
	c.SpotHourlyUSD = hist.LatestSpotUSDPerHour
	if len(c.Daily) == 0 {
		c.Flags = append(c.Flags, "no-history")
	}
	if c.SpotHourlyUSD <= 0 {
		c.Flags = append(c.Flags, "no-price")
	}
	if res.EstimatedUptimeSeconds == 0 {
		c.Flags = append(c.Flags, "no-uptime")
	}
	return c, nil
}

// finalize computes cross-candidate price normalization and composites,
// then sorts: kept candidates by composite desc, dropped candidates last.
func finalize(cands []Candidate, sc config.Scoring, ev EvidenceSource) {
	minPerUnit := 0.0
	for _, c := range cands {
		if c.SpotHourlyUSD > 0 {
			p := c.SpotHourlyUSD / c.Units
			if minPerUnit == 0 || p < minPerUnit {
				minPerUnit = p
			}
		}
	}
	for i := range cands {
		c := &cands[i]
		c.UptimeFactor = score.UptimeFactor(c.EstimatedUptimeSeconds)
		c.PreemptionFactor = score.PreemptionFactor(c.Daily)
		perUnit := 0.0
		if c.SpotHourlyUSD > 0 {
			perUnit = c.SpotHourlyUSD / c.Units
		}
		// Evidence is recorded before any drop gate: a candidate the budget
		// cap removes still reports what was observed about its zone, because
		// widening reads the evidence of dropped candidates too. Leaving these
		// at the Go zero value would publish 0.0 — "crushed" — for a shape
		// nobody ever looked at.
		c.EvidenceFactor, c.EvidenceDry = 1.0, false
		if ev != nil {
			c.EvidenceFactor = ev.Factor(c.MachineType, c.Zone)
			c.EvidenceDry = ev.Dry(c.MachineType, c.Zone)
			if c.EvidenceDry {
				c.Flags = append(c.Flags, "evidence-dry")
			}
		}
		if sc.MaxHourlyUSDPerUnit > 0 && perUnit > 0 && perUnit > sc.MaxHourlyUSDPerUnit {
			c.Dropped = true
			c.Composite = 0
			c.Flags = append(c.Flags, "over-budget")
			continue
		}
		c.PriceFactor = score.PriceFactor(perUnit, minPerUnit)
		c.Composite, c.Dropped = score.Composite(c.Obtainability, c.UptimeFactor, c.PreemptionFactor, c.PriceFactor, sc.PriceExponent, c.EvidenceFactor)
		if c.Dropped {
			c.Flags = append(c.Flags, "low-obtainability")
		}
	}
	SortCandidates(cands)
}

// SortCandidates orders candidates kept-first-by-composite: kept candidates by
// composite descending, dropped candidates last. Renderers depend on this
// ordering, so RunRender re-applies it to user-tweaked analysis JSON.
func SortCandidates(cands []Candidate) {
	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].Dropped != cands[j].Dropped {
			return !cands[i].Dropped
		}
		return cands[i].Composite > cands[j].Composite
	})
}

func chunk(xs []string, n int) [][]string {
	var out [][]string
	for len(xs) > n {
		out = append(out, xs[:n])
		xs = xs[n:]
	}
	return append(out, xs)
}
