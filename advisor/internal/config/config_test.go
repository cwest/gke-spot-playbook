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

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cwest/gke-spot-instance-node-pools/advisor/internal/evidence"
)

func write(t *testing.T, s string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "advisor.yaml")
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

const valid = `
project: example-sandbox
allowedRegions: ["us"]
profiles:
  cpu-batch:
    kind: cpu
    machineTypes: [e2-standard-8]
    size: 20
`

func TestLoadValidAppliesDefaults(t *testing.T) {
	c, err := Load(write(t, valid))
	if err != nil {
		t.Fatal(err)
	}
	if c.Project != "example-sandbox" {
		t.Errorf("project = %q", c.Project)
	}
	if c.Hysteresis.MinScoreDelta != 0.15 || c.Hysteresis.ConsecutiveTicks != 3 {
		t.Errorf("hysteresis defaults not applied: %+v", c.Hysteresis)
	}
	if c.Caps.MaxSpotRungs != 3 {
		t.Errorf("caps default not applied: %+v", c.Caps)
	}
}

func TestLoadRejectsBadConfigs(t *testing.T) {
	cases := map[string]string{
		"missing project":      strings.Replace(valid, "project: example-sandbox", "", 1),
		"empty allowedRegions": strings.Replace(valid, `allowedRegions: ["us"]`, "allowedRegions: []", 1),
		"empty machineTypes":   strings.Replace(valid, "machineTypes: [e2-standard-8]", "machineTypes: []", 1),
		"bad kind":             strings.Replace(valid, "kind: cpu", "kind: quantum", 1),
		"zero size":            strings.Replace(valid, "size: 20", "size: 0", 1),
	}
	for name, y := range cases {
		if _, err := Load(write(t, y)); err == nil {
			t.Errorf("%s: expected error, got nil", name)
		}
	}
}

func TestScoringDefaultsAndValidation(t *testing.T) {
	c, err := Load(write(t, valid))
	if err != nil {
		t.Fatal(err)
	}
	if c.Scoring.PriceExponent != 1.0 {
		t.Errorf("PriceExponent default = %v, want 1.0", c.Scoring.PriceExponent)
	}
	if c.Scoring.MaxHourlyUSDPerUnit != 0 {
		t.Errorf("MaxHourlyUSDPerUnit default = %v, want 0 (disabled)", c.Scoring.MaxHourlyUSDPerUnit)
	}
	withScoring := valid + `
scoring:
  priceExponent: 2.5
  maxHourlyUSDPerUnit: 0.02
`
	c, err = Load(write(t, withScoring))
	if err != nil {
		t.Fatal(err)
	}
	if c.Scoring.PriceExponent != 2.5 || c.Scoring.MaxHourlyUSDPerUnit != 0.02 {
		t.Errorf("scoring not loaded: %+v", c.Scoring)
	}
	if _, err := Load(write(t, valid+"\nscoring:\n  priceExponent: -1\n")); err == nil {
		t.Error("negative priceExponent must be rejected")
	}
	if _, err := Load(write(t, valid+"\nscoring:\n  maxHourlyUSDPerUnit: -0.01\n")); err == nil {
		t.Error("negative maxHourlyUSDPerUnit must be rejected")
	}
}

func TestLoadAppliesEvidenceDefaults(t *testing.T) {
	path := write(t, `
project: p
allowedRegions: [us]
profiles:
  cpu-batch:
    kind: cpu
    machineTypes: [e2-standard-8]
    size: 20
`)
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Evidence.HalfLifeMinutes != 30 {
		t.Errorf("HalfLifeMinutes = %d, want 30", c.Evidence.HalfLifeMinutes)
	}
	if c.Evidence.Floor != 0.05 {
		t.Errorf("Floor = %v, want 0.05", c.Evidence.Floor)
	}
	if c.Evidence.DryBelow != 0.5 {
		t.Errorf("DryBelow = %v, want 0.5", c.Evidence.DryBelow)
	}
	if c.Evidence.MaxAgeHours != 6 {
		t.Errorf("MaxAgeHours = %d, want 6", c.Evidence.MaxAgeHours)
	}
	if c.Evidence.FastPathTicks != 1 {
		t.Errorf("FastPathTicks = %d, want 1", c.Evidence.FastPathTicks)
	}
	// evidence.Params is comparable, so pin every field at once: dropping Floor
	// would send a fresh failure's multiplier to a hard 0, and dropping
	// DryBelow would make Dry false for every finite factor.
	want := evidence.Params{HalfLife: 30 * time.Minute, Floor: 0.05, DryBelow: 0.5, MaxAge: 6 * time.Hour}
	if p := c.Evidence.Params(); p != want {
		t.Errorf("Params() = %+v, want %+v", p, want)
	}
}

func TestValidateRejectsOutOfRangeEvidenceFloor(t *testing.T) {
	path := write(t, `
project: p
allowedRegions: [us]
profiles:
  cpu-batch: {kind: cpu, machineTypes: [e2-standard-8], size: 20}
evidence:
  floor: 1.5
`)
	if _, err := Load(path); err == nil {
		t.Fatal("expected an error for floor > 1")
	}
}

// The zero value of every evidence field is replaced by a default before
// validate runs, so each case has to supply an explicitly out-of-range value.
func TestLoadRejectsBadEvidenceConfigs(t *testing.T) {
	cases := map[string]string{
		"dryBelow above 1":         valid + "\nevidence:\n  dryBelow: 1.5\n",
		"negative halfLifeMinutes": valid + "\nevidence:\n  halfLifeMinutes: -1\n",
		"negative maxAgeHours":     valid + "\nevidence:\n  maxAgeHours: -1\n",
		"negative fastPathTicks":   valid + "\nevidence:\n  fastPathTicks: -1\n",
	}
	for name, y := range cases {
		if _, err := Load(write(t, y)); err == nil {
			t.Errorf("%s: expected error, got nil", name)
		}
	}
}

func TestLoadAppliesProbeDefaults(t *testing.T) {
	path := write(t, `
project: p
allowedRegions: [us]
profiles:
  cpu-batch: {kind: cpu, machineTypes: [e2-standard-8], size: 20}
`)
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Probe.Enabled {
		t.Error("probing must be off by default: it costs money")
	}
	if c.Probe.MaxRunDurationSeconds != 300 {
		t.Errorf("MaxRunDurationSeconds = %d, want 300", c.Probe.MaxRunDurationSeconds)
	}
}

func TestValidateRejectsAnEnabledProbeWithoutABackstop(t *testing.T) {
	path := write(t, `
project: p
allowedRegions: [us]
profiles:
  cpu-batch: {kind: cpu, machineTypes: [e2-standard-8], size: 20}
probe:
  enabled: true
  maxRunDurationSeconds: -1
`)
	if _, err := Load(path); err == nil {
		t.Fatal("expected an error for a negative max run duration")
	}
}

func TestProbeAutomatedDefaultsOffAndParses(t *testing.T) {
	c, err := Parse([]byte("project: p\nallowedRegions: [us]\nprofiles:\n  cpu-batch: {kind: cpu, machineTypes: [e2-standard-8], size: 20}\nprobe:\n  automated: true\n  network: net-x\n  subnet: sub-y\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !c.Probe.Automated || c.Probe.Network != "net-x" || c.Probe.Subnet != "sub-y" {
		t.Fatalf("got %+v", c.Probe)
	}
	def, _ := Parse([]byte("project: p\nallowedRegions: [us]\nprofiles:\n  cpu-batch: {kind: cpu, machineTypes: [e2-standard-8], size: 20}\n"))
	if def.Probe.Automated {
		t.Fatal("automated must default false")
	}
}

func TestDemoA100ConfigLoads(t *testing.T) {
	c, err := Load("../../../demo/act7/advisor-a100.yaml")
	if err != nil {
		t.Fatalf("demo A100 config must load: %v", err)
	}
	p, ok := c.Profiles["gpu-a100"]
	if !ok {
		t.Fatal("demo config must define the gpu-a100 profile")
	}
	if p.Kind != "gpu" || len(p.MachineTypes) == 0 || p.MachineTypes[0] != "a2-highgpu-1g" {
		t.Fatalf("gpu-a100 profile = %+v, want kind gpu with a2-highgpu-1g", p)
	}
	want := map[string]bool{"us-central1": false, "us-east1": false, "europe-west4": false}
	for _, r := range c.AllowedRegions {
		if _, ok := want[r]; ok {
			want[r] = true
		}
	}
	for r, seen := range want {
		if !seen {
			t.Errorf("A100 ladder missing region %q (allowedRegions=%v)", r, c.AllowedRegions)
		}
	}
	if !c.Probe.Automated {
		t.Error("demo A100 config must enable automated probing (the act's whole point)")
	}
}
