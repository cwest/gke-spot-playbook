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

// capacity-advisor scores spot candidates via the GCE capacity advice APIs
// and renders GKE ComputeClass ladders.
package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/cwest/gke-spot-playbook/advisor/internal/advice/gcp"
	"github.com/cwest/gke-spot-playbook/advisor/internal/cli"
	"github.com/cwest/gke-spot-playbook/advisor/internal/config"
	"github.com/cwest/gke-spot-playbook/advisor/internal/evidence/gcplog"
	"github.com/cwest/gke-spot-playbook/advisor/internal/kube/clientgo"
	"github.com/cwest/gke-spot-playbook/advisor/internal/pricing"
	probegce "github.com/cwest/gke-spot-playbook/advisor/internal/probe/gce"
)

func main() {
	root := &cobra.Command{Use: "capacity-advisor", SilenceUsage: true}

	var aOpts cli.AnalyzeOpts
	analyzeCmd := &cobra.Command{
		Use:   "analyze",
		Short: "Query the advice APIs and score spot candidates",
		RunE: func(cmd *cobra.Command, _ []string) error {
			api, err := gcp.New(cmd.Context())
			if err != nil {
				return err
			}
			aOpts.Now = time.Now().UTC()
			return cli.RunAnalyze(cmd.Context(), api, aOpts)
		},
	}
	analyzeCmd.Flags().StringVar(&aOpts.ConfigPath, "config", "advisor.yaml", "path to advisor.yaml")
	analyzeCmd.Flags().StringVar(&aOpts.Profile, "profile", "", "profile name (required)")
	analyzeCmd.Flags().StringVar(&aOpts.OutDir, "out", "out", "output directory")
	analyzeCmd.Flags().BoolVar(&aOpts.Render, "render", false, "also render artifacts")
	regionsFlag := analyzeCmd.Flags().String("regions", "", "comma-separated region override (values go through keyword/glob expansion)")
	analyzeCmd.MarkFlagRequired("profile")

	// Wrap the RunE to parse regions flag
	origRunE := analyzeCmd.RunE
	analyzeCmd.RunE = func(cmd *cobra.Command, args []string) error {
		if *regionsFlag != "" {
			for _, r := range strings.Split(*regionsFlag, ",") {
				if r = strings.TrimSpace(r); r != "" {
					aOpts.Regions = append(aOpts.Regions, r)
				}
			}
		}
		return origRunE(cmd, args)
	}

	var analysisPath, outDir string
	var maxRungs int
	renderCmd := &cobra.Command{
		Use:   "render",
		Short: "Render artifacts from a saved analysis",
		RunE: func(*cobra.Command, []string) error {
			return cli.RunRender(analysisPath, outDir, maxRungs)
		},
	}
	renderCmd.Flags().StringVar(&analysisPath, "analysis", "", "analysis JSON path (required)")
	renderCmd.Flags().StringVar(&outDir, "out", "out", "output directory")
	renderCmd.Flags().IntVar(&maxRungs, "max-rungs", 3, "max spot rungs")
	renderCmd.MarkFlagRequired("analysis")

	var cOpts cli.CostOpts
	costCmd := &cobra.Command{
		Use:   "cost",
		Short: "Turn collected node samples into a two-factor savings report",
		RunE: func(cmd *cobra.Command, _ []string) error {
			onDemand, err := pricing.NewBillingSource(cmd.Context())
			if err != nil {
				return err
			}
			api, err := gcp.New(cmd.Context())
			if err != nil {
				return err
			}
			cOpts.OnDemand = onDemand
			cOpts.Spot = cli.NewSpotSource(api, cOpts.Project)
			cOpts.Now = time.Now().UTC()
			return cli.RunCost(cmd.Context(), cOpts)
		},
	}
	costCmd.Flags().StringVar(&cOpts.SamplesPath, "samples", "out/cost-samples.csv", "path to collected samples CSV")
	costCmd.Flags().StringVar(&cOpts.Region, "region", "", "cluster region (required)")
	costCmd.Flags().StringVar(&cOpts.OutDir, "out", "out", "output directory")
	costCmd.Flags().StringVar(&cOpts.Project, "project", "", "GCP project for spot price lookup (required)")
	costCmd.Flags().DurationVar(&cOpts.Interval, "interval", 30*time.Second, "sampling interval")
	costCmd.MarkFlagRequired("region")
	costCmd.MarkFlagRequired("project")

	var rOpts cli.ReconcileOpts
	var profilesFlag, clusterName string
	reconcileCmd := &cobra.Command{
		Use:   "reconcile",
		Short: "Score, render and apply ComputeClass ladders in-cluster",
		RunE: func(cmd *cobra.Command, _ []string) error {
			api, err := gcp.New(cmd.Context())
			if err != nil {
				return err
			}
			kc, err := clientgo.New(cmd.Context())
			if err != nil {
				return err
			}
			// gcplog.New takes (project, cluster, location) — the visibility log
			// is filtered by cluster resource labels, so both are required.
			lg, err := gcplog.New(cmd.Context(),
				projectFromEnvOr(rOpts.ConfigPath), clusterName, rOpts.ClusterRegion)
			if err != nil {
				return err
			}
			for _, p := range strings.Split(profilesFlag, ",") {
				if p = strings.TrimSpace(p); p != "" {
					rOpts.Profiles = append(rOpts.Profiles, p)
				}
			}
			rOpts.Now = time.Now().UTC()
			return cli.RunReconcile(cmd.Context(), api, kc, lg, rOpts)
		},
	}
	reconcileCmd.Flags().StringVar(&rOpts.ConfigPath, "config", "/etc/advisor/advisor.yaml", "path to advisor.yaml")
	reconcileCmd.Flags().StringVar(&profilesFlag, "profiles", "cpu-batch,gpu-batch", "comma-separated profile names")
	reconcileCmd.Flags().StringVar(&rOpts.Namespace, "namespace", envOr("POD_NAMESPACE", "spot-demo"), "namespace holding the state ConfigMap (events always go to default: their ComputeClass is cluster-scoped)")
	reconcileCmd.Flags().StringVar(&rOpts.StateName, "state-name", envOr("STATE_NAME", "capacity-advisor-state"), "name of the state ConfigMap")
	reconcileCmd.Flags().StringVar(&rOpts.ClusterRegion, "cluster-region", envOr("CLUSTER_REGION", ""), "region this cluster runs in (required)")
	reconcileCmd.Flags().StringVar(&clusterName, "cluster-name", envOr("CLUSTER_NAME", "spot-demo"), "cluster name, used to filter the autoscaler visibility log")
	reconcileCmd.Flags().BoolVar(&rOpts.DryRun, "dry-run", false, "report what would change without applying")

	var dOpts cli.DensityOpts
	densityCmd := &cobra.Command{
		Use:   "agent-density",
		Short: "Compute agent density metrics ($/agent + lever ratios)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			billingSource, err := pricing.NewBillingSource(cmd.Context())
			if err != nil {
				return err
			}
			dOpts.OnDemand = billingSource
			return cli.RunDensity(cmd.Context(), dOpts)
		},
	}
	densityCmd.Flags().StringVar(&dOpts.NodeMachineType, "node-machine-type", "", "node machine type (required)")
	densityCmd.Flags().StringVar(&dOpts.Region, "region", "", "GCP region (required)")
	densityCmd.Flags().StringVar(&dOpts.OutDir, "out", "out", "output directory")
	densityCmd.Flags().IntVar(&dOpts.P1Agents, "p1", 0, "agents at P1 baseline (required)")
	densityCmd.Flags().IntVar(&dOpts.P2Agents, "p2", 0, "agents at P2 sandbox (required)")
	densityCmd.Flags().IntVar(&dOpts.P3Agents, "p3", 0, "agents at P3 lifecycle (required)")
	densityCmd.MarkFlagRequired("node-machine-type")
	densityCmd.MarkFlagRequired("region")
	densityCmd.MarkFlagRequired("p1")
	densityCmd.MarkFlagRequired("p2")
	densityCmd.MarkFlagRequired("p3")

	var pOpts cli.ProbeOpts
	probeCmd := &cobra.Command{
		Use:   "probe",
		Short: "Create one spot VM to verify a zone can supply a shape, then delete it",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := probegce.New(cmd.Context())
			if err != nil {
				return err
			}
			defer c.Close()
			return cli.RunProbe(cmd.Context(), c, pOpts)
		},
	}
	probeCmd.Flags().StringVar(&pOpts.ConfigPath, "config", "advisor.yaml", "path to advisor.yaml")
	probeCmd.Flags().StringVar(&pOpts.MachineType, "machine-type", "", "machine type to probe (required)")
	probeCmd.Flags().StringVar(&pOpts.Zone, "zone", "", "zone to probe (required)")
	probeCmd.Flags().StringVar(&pOpts.Name, "name", "", "instance name (default: generated)")
	probeCmd.MarkFlagRequired("machine-type")
	probeCmd.MarkFlagRequired("zone")

	var nodeHourly, activeHours, rps float64
	servingCostCmd := &cobra.Command{
		Use:   "serving-cost",
		Short: "Compare cost per 1000 requests for always-on vs scale-to-zero serving",
		RunE: func(*cobra.Command, []string) error {
			return cli.RunServingCost(nodeHourly, activeHours, rps, os.Stdout)
		},
	}
	servingCostCmd.Flags().Float64Var(&nodeHourly, "node-hourly", 0, "effective node $/hr (required)")
	servingCostCmd.Flags().Float64Var(&activeHours, "active-hours", 8, "hours/day actually serving")
	servingCostCmd.Flags().Float64Var(&rps, "rps", 1, "sustained requests/sec while active")
	servingCostCmd.MarkFlagRequired("node-hourly")

	root.AddCommand(analyzeCmd, renderCmd, costCmd, reconcileCmd, densityCmd, probeCmd, servingCostCmd)
	if err := root.ExecuteContext(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// projectFromEnvOr reads the project for the Logging client. In-cluster this is
// injected by the manifest; falling back to the config file makes PROJECT_ID
// optional and keeps the cluster configuration as the single source of truth.
func projectFromEnvOr(configPath string) string {
	if v := os.Getenv("PROJECT_ID"); v != "" {
		return v
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		return ""
	}
	return cfg.Project
}
