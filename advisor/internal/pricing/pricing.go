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

// Package pricing resolves machine-type list prices.
package pricing

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/cwest/gke-spot-instance-node-pools/advisor/internal/machinetype"
)

// ErrUnsupported marks machine families this demo does not price. The GPU
// family g2 is supported (core + RAM + attached-L4 SKUs); other GPU families
// (a2/a3/n1+GPU) and non-standard shapes remain out of scope.
var ErrUnsupported = errors.New("pricing: unsupported machine family")

// Shape is the vCPU/memory/GPU decomposition of a machine type, used to compose
// a list price from the per-core, per-GB, and per-GPU SKUs.
type Shape struct {
	Family   string
	VCPUs    float64
	MemoryGB float64
	GPUs     float64 // attached accelerator count (0 for pure-CPU families)
}

// gbPerVCPU for the standard shapes this demo prices. g2 attaches L4 GPUs whose
// count comes from machinetype.Units; the GPU SKU is a separate line item.
var gbPerVCPU = map[string]float64{"e2": 4, "n2": 4, "t2d": 4, "g2": 4}

// ParseShape decomposes a "<family>-standard-<n>" machine type into its family,
// vCPU count, and memory. Unknown families and non-standard shapes return
// ErrUnsupported.
func ParseShape(machineType string) (Shape, error) {
	parts := strings.Split(machineType, "-")
	if len(parts) != 3 || parts[1] != "standard" {
		return Shape{}, fmt.Errorf("%w: %s", ErrUnsupported, machineType)
	}
	ratio, ok := gbPerVCPU[parts[0]]
	if !ok {
		return Shape{}, fmt.Errorf("%w: %s", ErrUnsupported, machineType)
	}
	n, err := strconv.Atoi(parts[2])
	if err != nil || n <= 0 {
		return Shape{}, fmt.Errorf("%w: %s", ErrUnsupported, machineType)
	}
	shape := Shape{Family: parts[0], VCPUs: float64(n), MemoryGB: float64(n) * ratio}
	if parts[0] == "g2" {
		gpus, err := machinetype.Units(machineType, "gpu")
		if err != nil {
			return Shape{}, fmt.Errorf("%w: %s: %v", ErrUnsupported, machineType, err)
		}
		shape.GPUs = gpus
	}
	return shape, nil
}

// Source resolves list prices for machine types in a region.
type Source interface {
	// OnDemandHourlyUSD returns the list price for the machine type in region.
	OnDemandHourlyUSD(ctx context.Context, region, machineType string) (float64, error)
}
