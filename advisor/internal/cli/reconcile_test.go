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

package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cwest/gke-spot-playbook/advisor/internal/advice"
	advfake "github.com/cwest/gke-spot-playbook/advisor/internal/advice/fake"
	"github.com/cwest/gke-spot-playbook/advisor/internal/evidence"
	kubefake "github.com/cwest/gke-spot-playbook/advisor/internal/kube/fake"
	probefake "github.com/cwest/gke-spot-playbook/advisor/internal/probe/fake"
	"github.com/cwest/gke-spot-playbook/advisor/internal/reconcile"
)

// stubLog satisfies reconcile.LogSource with a fixed observation set.
type stubLog struct{ obs []evidence.Observation }

func (s stubLog) Refusals(context.Context, time.Time) ([]evidence.Observation, error) {
	return s.obs, nil
}

func keysOf(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func writeReconcileConfig(t *testing.T) string {
	return writeConfigWith(t, "")
}

func writeConfigWith(t *testing.T, extra string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "advisor.yaml")
	body := `
project: example-sandbox
allowedRegions: [us-central1]
profiles:
  cpu-batch:
    kind: cpu
    machineTypes: [e2-standard-8, n2-standard-8]
    size: 20
`
	if extra != "" {
		body += extra
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// reconcileAPI returns an advice fake with two shapes in one region so the
// analysis produces a renderable ladder.
func reconcileAPI() advice.API {
	f := advfake.New()
	f.RegionsFn = func(project string) ([]string, error) {
		return []string{"us-central1"}, nil
	}
	f.CapacityFn = func(q advice.CapacityQuery) ([]advice.CapacityResult, error) {
		var out []advice.CapacityResult
		for i, mt := range q.MachineTypes {
			out = append(out, advice.CapacityResult{
				Obtainability:          0.9 - 0.1*float64(i),
				EstimatedUptimeSeconds: 7200,
				Shards: []advice.Shard{
					{MachineType: mt, Zone: "us-central1-a"},
					{MachineType: mt, Zone: "us-central1-b"},
				},
			})
		}
		return out, nil
	}
	f.HistoryFn = func(q advice.HistoryQuery) (*advice.HistoryResult, error) {
		return &advice.HistoryResult{
			DailyPreemptionRates: []float64{0.05, 0.05, 0.05},
			LatestSpotUSDPerHour: 0.20,
		}, nil
	}
	return f
}

func reconcileOpts(t *testing.T) ReconcileOpts {
	return ReconcileOpts{
		ConfigPath:    writeReconcileConfig(t),
		Profiles:      []string{"cpu-batch"},
		Namespace:     "spot-demo",
		StateName:     "capacity-advisor-state",
		ClusterRegion: "us-central1",
		Now:           time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC),
	}
}

func TestRunReconcileAppliesAndPersistsState(t *testing.T) {
	kc := kubefake.New()
	if err := RunReconcile(context.Background(), reconcileAPI(), kc, stubLog{}, reconcileOpts(t)); err != nil {
		t.Fatal(err)
	}
	if len(kc.Applied) == 0 {
		t.Fatal("no ComputeClass applied on the first tick")
	}
	// The fake keys state by "namespace/name" (see internal/kube/fake).
	if _, ok := kc.State["spot-demo/capacity-advisor-state"]; !ok {
		t.Fatalf("state was not persisted; keys = %v", keysOf(kc.State))
	}
}

func TestRunReconcileDryRunWritesNothing(t *testing.T) {
	kc := kubefake.New()
	o := reconcileOpts(t)
	o.DryRun = true
	if err := RunReconcile(context.Background(), reconcileAPI(), kc, stubLog{}, o); err != nil {
		t.Fatal(err)
	}
	if len(kc.Applied) != 0 {
		t.Errorf("dry run applied %d manifests", len(kc.Applied))
	}
	if len(kc.State) != 0 {
		t.Error("dry run persisted state")
	}
}

// TestRunReconcileWarnsOnUnknownProfileAndContinues pins the warn-and-continue
// ruling all the way out to the command. The production CronJob passes a fixed
// --profiles list; if one name stops resolving, the tick must still reconcile
// the profiles that do resolve rather than exiting non-zero and moving nothing.
// Only a wholly unresolvable list is fatal.
func TestRunReconcileWarnsOnUnknownProfileAndContinues(t *testing.T) {
	kc := kubefake.New()
	o := reconcileOpts(t)
	o.Profiles = []string{"cpu-batch", "nope-batch"}
	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	o.Out, o.ErrOut = out, errOut

	if err := RunReconcile(context.Background(), reconcileAPI(), kc, stubLog{}, o); err != nil {
		t.Fatalf("err = %v, want the tick to succeed and skip the unknown profile", err)
	}
	if !strings.Contains(errOut.String(), "nope-batch") {
		t.Errorf("output did not name the skipped profile; stdout = %q stderr = %q",
			out.String(), errOut.String())
	}
	if len(kc.Applied) != 1 {
		t.Errorf("cluster saw %d applies, want 1: the resolvable profile must still be reconciled",
			len(kc.Applied))
	}

	// A wholly unresolvable list is still fatal, so a typo'd deployment does not
	// silently reconcile nothing forever.
	kc2 := kubefake.New()
	o2 := reconcileOpts(t)
	o2.Profiles = []string{"nope-batch"}
	err := RunReconcile(context.Background(), reconcileAPI(), kc2, stubLog{}, o2)
	if err == nil || !strings.Contains(err.Error(), "nope-batch") {
		t.Fatalf("err = %v, want a failure naming nope-batch when no profile resolves", err)
	}
	if len(kc2.Applied) != 0 {
		t.Errorf("cluster saw %d applies for a wholly unresolvable list, want 0", len(kc2.Applied))
	}
}

func TestRunReconcileRequiresAClusterRegion(t *testing.T) {
	kc := kubefake.New()
	// Pin the contract: missing cluster region is rejected before reading cluster state.
	getStateCalled := false
	kc.GetStateFn = func(context.Context, string, string) ([]byte, error) {
		getStateCalled = true
		return nil, nil
	}
	o := reconcileOpts(t)
	o.ClusterRegion = ""
	err := RunReconcile(context.Background(), reconcileAPI(), kc, stubLog{}, o)
	if err == nil || !strings.Contains(err.Error(), "cluster-region") {
		t.Fatalf("err = %v, want the cluster-region error named", err)
	}
	if len(kc.Applied) != 0 {
		t.Errorf("cluster region rejection applied manifests; len(kc.Applied) = %d", len(kc.Applied))
	}
	if getStateCalled {
		t.Error("GetState was called despite missing cluster region; region validation must happen before Tick reads state")
	}
}

func TestRunReconcileReportWritesToOutputStreams(t *testing.T) {
	res := &reconcile.Result{
		Observations: 42,
		Applied:      []string{"applied-1", "applied-2"},
		Would:        []string{"would-1"},
		Waiting:      []string{"waiting-1"},
		NoOp:         []string{"noop-1"},
		Advisory:     "advisory-text",
		Warnings:     []string{"warning-1"},
	}

	out := &bytes.Buffer{}
	errOut := &bytes.Buffer{}

	report(out, errOut, res)

	outStr := out.String()
	errStr := errOut.String()

	// Check all stdout buckets
	if !strings.Contains(outStr, "observations: 42") {
		t.Errorf("stdout missing 'observations: 42'; got: %s", outStr)
	}
	if !strings.Contains(outStr, "applied-1") {
		t.Errorf("stdout missing 'applied-1'; got: %s", outStr)
	}
	if !strings.Contains(outStr, "applied-2") {
		t.Errorf("stdout missing 'applied-2'; got: %s", outStr)
	}
	if !strings.Contains(outStr, "would-1") {
		t.Errorf("stdout missing 'would-1'; got: %s", outStr)
	}
	if !strings.Contains(outStr, "waiting-1") {
		t.Errorf("stdout missing 'waiting-1'; got: %s", outStr)
	}
	if !strings.Contains(outStr, "noop-1") {
		t.Errorf("stdout missing 'noop-1'; got: %s", outStr)
	}
	if !strings.Contains(outStr, "advisory-text") {
		t.Errorf("stdout missing 'advisory-text'; got: %s", outStr)
	}

	// Check that warnings are in stderr, not stdout
	if !strings.Contains(errStr, "warning-1") {
		t.Errorf("stderr missing 'warning-1'; got: %s", errStr)
	}
	if strings.Contains(outStr, "warning-1") {
		t.Errorf("warning should not appear in stdout; got: %s", outStr)
	}

	// Also test that RunReconcile actually calls report with the custom streams.
	// Use a reconcile that returns a non-empty result to verify reporting.
	out2 := &bytes.Buffer{}
	errOut2 := &bytes.Buffer{}
	o := reconcileOpts(t)
	o.Out = out2
	o.ErrOut = errOut2
	if err := RunReconcile(context.Background(), reconcileAPI(), kubefake.New(), stubLog{}, o); err != nil {
		t.Fatalf("RunReconcile failed: %v", err)
	}
	outStr2 := out2.String()
	if !strings.Contains(outStr2, "observations:") {
		t.Errorf("RunReconcile did not call report (or report was not passed custom streams); got: %s", outStr2)
	}
}

func TestRunProbeRefusesWhenDisabled(t *testing.T) {
	err := RunProbe(context.Background(), &probefake.Compute{}, ProbeOpts{
		ConfigPath:  writeReconcileConfig(t),
		MachineType: "e2-standard-8",
		Zone:        "us-central1-a",
	})
	if err == nil || !strings.Contains(err.Error(), "probe.enabled") {
		t.Fatalf("err = %v, want a refusal naming the config gate", err)
	}
}

func TestRunProbePassesTheConfiguredBackstopAndProject(t *testing.T) {
	configPath := writeConfigWith(t, `
probe:
  enabled: true
  maxRunDurationSeconds: 120
`)
	c := &probefake.Compute{}
	if err := RunProbe(context.Background(), c, ProbeOpts{
		ConfigPath:  configPath,
		MachineType: "e2-standard-8",
		Zone:        "us-central1-a",
		Name:        "test-probe",
	}); err != nil {
		t.Fatalf("RunProbe failed: %v", err)
	}

	if len(c.Inserted) != 1 {
		t.Fatalf("expected 1 created VM, got %d", len(c.Inserted))
	}

	created := c.Inserted[0]

	if created.MaxRunSeconds != 120 {
		t.Errorf("MaxRunSeconds = %d, want 120", created.MaxRunSeconds)
	}

	if created.Project != "example-sandbox" {
		t.Errorf("Project = %q, want example-sandbox", created.Project)
	}

	if created.Zone != "us-central1-a" {
		t.Errorf("Zone = %q, want us-central1-a", created.Zone)
	}

	if created.MachineType != "e2-standard-8" {
		t.Errorf("MachineType = %q, want e2-standard-8", created.MachineType)
	}

	if len(c.Deleted) != 1 {
		t.Fatalf("expected 1 deleted VM, got %d", len(c.Deleted))
	}
}

func TestRunProbeGeneratesName(t *testing.T) {
	configPath := writeConfigWith(t, `
probe:
  enabled: true
  maxRunDurationSeconds: 120
`)
	c := &probefake.Compute{}
	if err := RunProbe(context.Background(), c, ProbeOpts{
		ConfigPath:  configPath,
		MachineType: "e2-standard-8",
		Zone:        "us-central1-a",
		Name:        "",
	}); err != nil {
		t.Fatalf("RunProbe failed: %v", err)
	}

	if len(c.Inserted) != 1 {
		t.Fatalf("expected 1 created VM, got %d", len(c.Inserted))
	}

	created := c.Inserted[0]

	if created.Name == "" {
		t.Error("generated name is empty")
	}

	if !strings.HasPrefix(created.Name, "capacity-probe-e2-standard-8-") {
		t.Errorf("generated name %q does not match capacity-probe-e2-standard-8-", created.Name)
	}
}
