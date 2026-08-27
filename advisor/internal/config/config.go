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

// Package config loads and validates advisor.yaml.
package config

import (
	"fmt"
	"os"
	"time"

	"github.com/cwest/gke-spot-playbook/advisor/internal/evidence"
	"gopkg.in/yaml.v3"
)

type Profile struct {
	Kind         string   `yaml:"kind"` // "cpu" or "gpu"
	MachineTypes []string `yaml:"machineTypes"`
	Size         int32    `yaml:"size"`
}

type Hysteresis struct {
	MinScoreDelta    float64 `yaml:"minScoreDelta"`
	ConsecutiveTicks int     `yaml:"consecutiveTicks"`
}

type Caps struct {
	MaxSpotRungs int `yaml:"maxSpotRungs"`
}

type Scoring struct {
	PriceExponent       float64 `yaml:"priceExponent"`
	MaxHourlyUSDPerUnit float64 `yaml:"maxHourlyUSDPerUnit"`
}

// Evidence tunes how observed provisioning failures decay. Defaults come from
// the Plan 4 design: a failure crushes a rung for roughly half an hour, which
// is long enough to route around a stockout and short enough that a recovered
// zone is retried the same afternoon.
type Evidence struct {
	HalfLifeMinutes int     `yaml:"halfLifeMinutes"`
	Floor           float64 `yaml:"floor"`
	DryBelow        float64 `yaml:"dryBelow"`
	MaxAgeHours     int     `yaml:"maxAgeHours"`
	FastPathTicks   int     `yaml:"fastPathTicks"`
}

func (e Evidence) Params() evidence.Params {
	return evidence.Params{
		HalfLife: time.Duration(e.HalfLifeMinutes) * time.Minute,
		Floor:    e.Floor,
		DryBelow: e.DryBelow,
		MaxAge:   time.Duration(e.MaxAgeHours) * time.Hour,
	}
}

// Probe controls the opt-in live capacity probe. Off by default: it creates
// real VMs and costs real money.
type Probe struct {
	Enabled               bool   `yaml:"enabled"`
	Automated             bool   `yaml:"automated"`
	MaxRunDurationSeconds int    `yaml:"maxRunDurationSeconds"`
	Network               string `yaml:"network"`
	Subnet                string `yaml:"subnet"`
}

type Config struct {
	Project        string             `yaml:"project"`
	AllowedRegions []string           `yaml:"allowedRegions"`
	Profiles       map[string]Profile `yaml:"profiles"`
	Hysteresis     Hysteresis         `yaml:"hysteresis"`
	Caps           Caps               `yaml:"caps"`
	Scoring        Scoring            `yaml:"scoring"`
	Evidence       Evidence           `yaml:"evidence"`
	Probe          Probe              `yaml:"probe"`
}

func Parse(b []byte) (*Config, error) {
	c := &Config{}
	if err := yaml.Unmarshal(b, c); err != nil {
		return nil, err
	}
	return applyDefaults(c)
}

func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	c, err := Parse(b)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

func applyDefaults(c *Config) (*Config, error) {
	if c.Hysteresis.MinScoreDelta == 0 {
		c.Hysteresis.MinScoreDelta = 0.15
	}
	if c.Hysteresis.ConsecutiveTicks == 0 {
		c.Hysteresis.ConsecutiveTicks = 3
	}
	if c.Caps.MaxSpotRungs == 0 {
		c.Caps.MaxSpotRungs = 3
	}
	if c.Scoring.PriceExponent == 0 {
		c.Scoring.PriceExponent = 1.0
	}
	if c.Evidence.HalfLifeMinutes == 0 {
		c.Evidence.HalfLifeMinutes = 30
	}
	if c.Evidence.Floor == 0 {
		c.Evidence.Floor = 0.05
	}
	if c.Evidence.DryBelow == 0 {
		c.Evidence.DryBelow = 0.5
	}
	if c.Evidence.MaxAgeHours == 0 {
		c.Evidence.MaxAgeHours = 6
	}
	if c.Evidence.FastPathTicks == 0 {
		c.Evidence.FastPathTicks = 1
	}
	if c.Probe.MaxRunDurationSeconds == 0 {
		c.Probe.MaxRunDurationSeconds = 300
	}
	return c, nil
}

func (c *Config) validate() error {
	if c.Project == "" {
		return fmt.Errorf("project is required")
	}
	if len(c.AllowedRegions) == 0 {
		return fmt.Errorf("allowedRegions must not be empty")
	}
	for name, p := range c.Profiles {
		if p.Kind != "cpu" && p.Kind != "gpu" {
			return fmt.Errorf("profile %s: kind must be cpu or gpu, got %q", name, p.Kind)
		}
		if len(p.MachineTypes) == 0 {
			return fmt.Errorf("profile %s: machineTypes must not be empty", name)
		}
		if p.Size <= 0 {
			return fmt.Errorf("profile %s: size must be > 0", name)
		}
	}
	if c.Scoring.PriceExponent <= 0 {
		return fmt.Errorf("scoring: priceExponent must be > 0")
	}
	if c.Scoring.MaxHourlyUSDPerUnit < 0 {
		return fmt.Errorf("scoring: maxHourlyUSDPerUnit must be >= 0")
	}
	if c.Evidence.Floor <= 0 || c.Evidence.Floor > 1 {
		return fmt.Errorf("evidence: floor must be in (0, 1]")
	}
	if c.Evidence.DryBelow <= 0 || c.Evidence.DryBelow > 1 {
		return fmt.Errorf("evidence: dryBelow must be in (0, 1]")
	}
	if c.Evidence.HalfLifeMinutes <= 0 {
		return fmt.Errorf("evidence: halfLifeMinutes must be > 0")
	}
	if c.Evidence.MaxAgeHours <= 0 {
		return fmt.Errorf("evidence: maxAgeHours must be > 0")
	}
	if c.Evidence.FastPathTicks <= 0 {
		return fmt.Errorf("evidence: fastPathTicks must be > 0")
	}
	if c.Probe.MaxRunDurationSeconds <= 0 {
		return fmt.Errorf("probe: maxRunDurationSeconds must be > 0")
	}
	return nil
}
