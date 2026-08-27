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

// Package cli holds testable command implementations; main.go only wires flags.
package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cwest/gke-spot-playbook/advisor/internal/advice"
	"github.com/cwest/gke-spot-playbook/advisor/internal/analyze"
	"github.com/cwest/gke-spot-playbook/advisor/internal/config"
	"github.com/cwest/gke-spot-playbook/advisor/internal/cost"
	"github.com/cwest/gke-spot-playbook/advisor/internal/density"
	"github.com/cwest/gke-spot-playbook/advisor/internal/kube"
	"github.com/cwest/gke-spot-playbook/advisor/internal/pricing"
	"github.com/cwest/gke-spot-playbook/advisor/internal/probe"
	"github.com/cwest/gke-spot-playbook/advisor/internal/reconcile"
	"github.com/cwest/gke-spot-playbook/advisor/internal/render"
	"github.com/cwest/gke-spot-playbook/advisor/internal/serving"
)

type AnalyzeOpts struct {
	ConfigPath string
	Profile    string
	OutDir     string
	Render     bool
	Now        time.Time
	// Regions, when non-empty, replaces the config's allowedRegions for this
	// run (values go through keyword/glob expansion like the config).
	Regions []string
}

func RunAnalyze(ctx context.Context, api advice.API, opts AnalyzeOpts) error {
	cfg, err := config.Load(opts.ConfigPath)
	if err != nil {
		return err
	}
	if len(opts.Regions) > 0 {
		cfg.AllowedRegions = opts.Regions
	}
	a, err := analyze.Run(ctx, api, cfg, opts.Profile, opts.Now, nil)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(opts.OutDir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(opts.OutDir, fmt.Sprintf("analysis-%s.json", opts.Profile))
	if err := os.WriteFile(path, b, 0o644); err != nil {
		return err
	}
	if !opts.Render {
		return nil
	}
	return renderAll(a, opts.OutDir, cfg.Caps.MaxSpotRungs)
}

func RunRender(analysisPath, outDir string, maxRungs int) error {
	b, err := os.ReadFile(analysisPath)
	if err != nil {
		return err
	}
	a := &analyze.Analysis{}
	if err := json.Unmarshal(b, a); err != nil {
		return fmt.Errorf("parse %s: %w", analysisPath, err)
	}
	// The analysis JSON is user-tweakable; renderers assume kept-first-by-composite
	// ordering, so re-sort before rendering.
	analyze.SortCandidates(a.Candidates)
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	return renderAll(a, outDir, maxRungs)
}

type CostOpts struct {
	SamplesPath string
	Region      string
	OutDir      string
	Project     string
	Interval    time.Duration
	OnDemand    pricing.Source
	Spot        cost.SpotSource
	Now         time.Time
}

// RunCost parses collected node samples, builds the two-factor cost report, and
// writes the markdown and JSON artifacts to OutDir.
func RunCost(ctx context.Context, opts CostOpts) error {
	if opts.Interval <= 0 {
		return fmt.Errorf("cost: --interval must be > 0, got %s", opts.Interval)
	}
	f, err := os.Open(opts.SamplesPath)
	if err != nil {
		return err
	}
	defer f.Close()
	usages, window, peakConcurrent, warnings, err := cost.ParseSamples(f, opts.Interval)
	if err != nil {
		return err
	}
	for _, w := range warnings {
		fmt.Fprintln(os.Stderr, "warning: "+w)
	}
	rep, err := cost.Build(ctx, usages, window, peakConcurrent, opts.Region, opts.OnDemand, opts.Spot, opts.Now)
	if err != nil {
		return err
	}
	rep.Caveats = warnings
	if err := os.MkdirAll(opts.OutDir, 0o755); err != nil {
		return err
	}
	rj, err := cost.RenderJSON(rep)
	if err != nil {
		return err
	}
	files := map[string][]byte{
		"cost-report.md":   cost.RenderMarkdown(rep),
		"cost-report.json": rj,
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(opts.OutDir, name), content, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// NewSpotSource adapts the advice API's regional capacity history into a
// cost.SpotSource, picking the latest observed spot list price. It keeps main.go
// wiring-only by holding the project the history query needs.
func NewSpotSource(api advice.API, project string) cost.SpotSource {
	return spotViaAdvice{api: api, project: project}
}

type spotViaAdvice struct {
	api     advice.API
	project string
}

func (s spotViaAdvice) SpotHourlyUSD(ctx context.Context, region, machineType string) (float64, error) {
	h, err := s.api.CapacityHistory(ctx, advice.HistoryQuery{
		Project: s.project, Region: region, MachineType: machineType,
	})
	if err != nil {
		return 0, err
	}
	if h.LatestSpotUSDPerHour <= 0 {
		return 0, fmt.Errorf("no spot price for %s in %s", machineType, region)
	}
	return h.LatestSpotUSDPerHour, nil
}

func renderAll(a *analyze.Analysis, outDir string, maxRungs int) error {
	cc, err := render.ComputeClass(a, "batch-"+a.Kind, maxRungs)
	if err != nil {
		return err
	}
	env, err := render.ClusterConfig(a)
	if err != nil {
		return err
	}
	rj, err := render.ReportJSON(a)
	if err != nil {
		return err
	}
	files := map[string][]byte{
		fmt.Sprintf("computeclass-%s.yaml", a.Kind):     cc,
		fmt.Sprintf("advice-report-%s.md", a.Profile):   render.ReportMarkdown(a),
		fmt.Sprintf("advice-report-%s.json", a.Profile): rj,
		fmt.Sprintf("cluster-config-%s.env", a.Profile): env,
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(outDir, name), content, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// ReconcileOpts separates policy from deployment identity. ConfigPath and
// Profiles are policy — reviewed, versioned, the same on every cluster.
// Namespace, StateName and ClusterRegion are identity — they belong to the
// manifest that deployed this pod, so they arrive as flags, not config.
type ReconcileOpts struct {
	ConfigPath string
	Profiles   []string

	Namespace     string
	StateName     string
	ClusterRegion string

	DryRun bool
	Now    time.Time

	// Out and ErrOut default to os.Stdout and os.Stderr if nil. Tests pass
	// custom writers to verify the tick actually reported its outcome.
	Out    io.Writer
	ErrOut io.Writer
}

func RunReconcile(ctx context.Context, api advice.API, kc kube.Client,
	lg reconcile.LogSource, opts ReconcileOpts) error {

	cfg, err := config.Load(opts.ConfigPath)
	if err != nil {
		return err
	}
	// Validate cluster region before Tick reads cluster state. A missing or
	// empty cluster-region fails instantly without touching the cluster; if we
	// let Tick run, the reconciler reads state before discovering the region
	// is invalid. This check keeps the fast path cheap.
	if opts.ClusterRegion == "" {
		return fmt.Errorf("cluster-region is required; set --cluster-region or CLUSTER_REGION env var")
	}
	// Profile names are deliberately NOT pre-validated here. An unknown profile
	// warns and continues: Tick skips it and reconciles the rest, because the
	// CronJob passes a fixed --profiles list and one stale name must not stop
	// every other class from being re-ranked. Tick still fails the run when no
	// requested profile resolves at all.
	res, err := reconcile.Tick(ctx, reconcile.Deps{
		API: api, Kube: kc, Log: lg, Cfg: cfg,
		Namespace: opts.Namespace, StateName: opts.StateName,
		ClusterRegion: opts.ClusterRegion,
		Now:           opts.Now, DryRun: opts.DryRun,
	}, opts.Profiles)
	if err != nil {
		return err
	}
	out := opts.Out
	if out == nil {
		out = os.Stdout
	}
	errOut := opts.ErrOut
	if errOut == nil {
		errOut = os.Stderr
	}
	report(out, errOut, res)
	return nil
}

// report prints the tick outcome. A CronJob's log is the only place an operator
// sees why the ladder did or did not move, so every bucket is printed even when
// empty-ish, and warnings go to stderr where log-based alerts can find them.
func report(out, errOut io.Writer, r *reconcile.Result) {
	fmt.Fprintf(out, "observations: %d\n", r.Observations)
	for _, s := range r.Applied {
		fmt.Fprintf(out, "applied:  %s\n", s)
	}
	for _, s := range r.Would {
		fmt.Fprintf(out, "would:    %s\n", s)
	}
	for _, s := range r.Waiting {
		fmt.Fprintf(out, "waiting:  %s\n", s)
	}
	for _, s := range r.NoOp {
		fmt.Fprintf(out, "no-op:    %s\n", s)
	}
	if r.Advisory != "" {
		fmt.Fprintf(out, "advisory: %s\n", r.Advisory)
	}
	for _, s := range r.Warnings {
		fmt.Fprintf(errOut, "warning:  %s\n", s)
	}
}

type DensityOpts struct {
	NodeMachineType string
	Region          string
	OutDir          string
	OnDemand        pricing.Source
	P1Agents        int
	P2Agents        int
	P3Agents        int
}

// RunDensity computes and renders the agent-density report.
func RunDensity(ctx context.Context, opts DensityOpts) error {
	nodeHourlyUSD, err := opts.OnDemand.OnDemandHourlyUSD(ctx, opts.Region, opts.NodeMachineType)
	if err != nil {
		return fmt.Errorf("fetch pricing: %w", err)
	}

	points := []density.Point{
		{Name: "p1", Agents: opts.P1Agents},
		{Name: "p2", Agents: opts.P2Agents},
		{Name: "p3", Agents: opts.P3Agents},
	}

	rep, err := density.Build(nodeHourlyUSD, points)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(opts.OutDir, 0o755); err != nil {
		return err
	}

	markdown := renderDensityMarkdown(rep)
	if err := os.WriteFile(filepath.Join(opts.OutDir, "agent-density-report.md"), markdown, 0o644); err != nil {
		return err
	}

	return nil
}

func renderDensityMarkdown(rep *density.Report) []byte {
	var sb strings.Builder
	sb.WriteString("# Agent Density Report\n\n")
	sb.WriteString(fmt.Sprintf("**Node Hourly Cost:** $%.2f\n\n", rep.NodeHourlyUSD))

	sb.WriteString("## Cost per Agent\n\n")
	sb.WriteString("| Point | Agents | $/Agent |\n")
	sb.WriteString("|-------|--------|----------|\n")
	for _, pc := range rep.Points {
		sb.WriteString(fmt.Sprintf("| %s | %d | $%.4f |\n", pc.Name, pc.Agents, pc.USDPerAgent))
	}
	sb.WriteString("\n")

	sb.WriteString("## Ratios\n\n")
	sb.WriteString(fmt.Sprintf("- **Isolation Ratio** (P2/P1): %.2f\n", rep.IsolationRatio))
	sb.WriteString(fmt.Sprintf("- **Lifecycle Ratio** (P3/P2): %.2f\n", rep.LifecycleRatio))
	sb.WriteString("\n")

	return []byte(sb.String())
}

type ProbeOpts struct {
	ConfigPath  string
	MachineType string
	Zone        string
	Name        string
}

func RunProbe(ctx context.Context, c probe.Compute, opts ProbeOpts) error {
	cfg, err := config.Load(opts.ConfigPath)
	if err != nil {
		return err
	}
	if !cfg.Probe.Enabled {
		return fmt.Errorf("probing is off: set probe.enabled in %s (it creates real VMs and costs money)", opts.ConfigPath)
	}
	name := opts.Name
	if name == "" {
		// Deterministic from the target and the clock so a rerun cannot collide
		// with an in-flight probe of the same shape.
		name = fmt.Sprintf("capacity-probe-%s-%d",
			strings.ReplaceAll(opts.MachineType, ".", "-"), time.Now().Unix())
	}
	res := probe.Run(ctx, c, probe.Request{
		Project: cfg.Project, Zone: opts.Zone, MachineType: opts.MachineType,
		Name: name, MaxRunSeconds: int64(cfg.Probe.MaxRunDurationSeconds),
	})
	b, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(b))
	if !res.Obtained {
		// Non-zero exit so a shell caller can branch on the verdict, but the
		// message is the payload above, not an error string.
		return fmt.Errorf("probe: %s in %s not obtained", opts.MachineType, opts.Zone)
	}
	return nil
}

// RunServingCost prints the $/1000-request comparison for always-on vs
// scale-to-zero serving at the given node price, duty cycle, and throughput.
func RunServingCost(nodeHourly, activeHours, rps float64, stdout io.Writer) error {
	always := serving.Scenario{NodeHourlyUSD: nodeHourly, BilledHoursPerDay: 24, ActiveHoursPerDay: activeHours, RequestsPerSecond: rps}
	s2z := serving.Scenario{NodeHourlyUSD: nodeHourly, BilledHoursPerDay: activeHours, ActiveHoursPerDay: activeHours, RequestsPerSecond: rps}
	fmt.Fprintf(stdout, "always-on:      $%.4f / 1000 req\n", serving.CostPer1000(always))
	fmt.Fprintf(stdout, "scale-to-zero:  $%.4f / 1000 req\n", serving.CostPer1000(s2z))
	fmt.Fprintf(stdout, "savings:        %.1f%%\n", serving.Savings(always, s2z)*100)
	return nil
}
