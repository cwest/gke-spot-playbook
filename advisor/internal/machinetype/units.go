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

// Package machinetype maps machine types to normalization units.
package machinetype

import (
	"fmt"
	"strconv"
	"strings"
)

// g2 shapes -> attached NVIDIA L4 count.
var g2GPUs = map[string]float64{
	"g2-standard-4": 1, "g2-standard-8": 1, "g2-standard-12": 1,
	"g2-standard-16": 1, "g2-standard-32": 1, "g2-standard-24": 2,
	"g2-standard-48": 4, "g2-standard-96": 8,
}

// a2 (A100 40GB) shapes -> attached NVIDIA A100 count. Unlike n1+attached GPUs,
// the A100 is integral to the a2 machine type, so a probe needs no accelerator
// field to allocate one. Only a2-highgpu-1g is exercised by Act 7; the rest are
// here for unit correctness.
var a2GPUs = map[string]float64{
	"a2-highgpu-1g": 1, "a2-highgpu-2g": 2, "a2-highgpu-4g": 4,
	"a2-highgpu-8g": 8, "a2-megagpu-16g": 16,
}

// Units returns the per-instance denominator for price normalization:
// vCPUs for cpu profiles, GPUs for gpu profiles.
func Units(machineType, kind string) (float64, error) {
	if kind == "gpu" {
		if n, ok := g2GPUs[machineType]; ok {
			return n, nil
		}
		if n, ok := a2GPUs[machineType]; ok {
			return n, nil
		}
		return 0, fmt.Errorf("unknown GPU shape %q", machineType)
	}
	parts := strings.Split(machineType, "-")
	n, err := strconv.Atoi(parts[len(parts)-1])
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("cannot parse vCPUs from %q", machineType)
	}
	return float64(n), nil
}
