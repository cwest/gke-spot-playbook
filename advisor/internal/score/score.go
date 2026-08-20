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

// Package score computes candidate factors and the composite score.
// Composite = obtainability² × uptime × preemption × price^priceExponent ×
// evidence; obtainability is squared because capacity you cannot get has no
// price, and evidence multiplies last because an observed failure outranks any
// prior about the same zone.
package score

import "math"

const dropBelow = 0.4 // documented "Low" obtainability band

func UptimeFactor(sec int) float64 {
	switch {
	case sec >= 3600:
		return 1.0
	case sec >= 600:
		return 0.6
	case sec > 0:
		return 0.2
	default:
		return 0.6 // missing signal: neutral, caller flags it
	}
}

// PreemptionFactor is 1 - mean(last ≤7 daily rates), penalized ×0.8 when the
// recent week is >10% worse than the preceding days. Empty history is neutral.
func PreemptionFactor(daily []float64) float64 {
	if len(daily) == 0 {
		return 1.0
	}
	split := len(daily) - 7
	if split < 0 {
		split = 0
	}
	recent := mean(daily[split:])
	f := 1 - recent
	if split >= 7 { // enough history to judge a trend
		if prior := mean(daily[:split]); recent > prior*1.1 {
			f *= 0.8
		}
	}
	if f < 0 {
		return 0
	}
	return f
}

// PriceFactor normalizes to the cheapest candidate: cheapest = 1.0.
// A missing price (0) is neutral; the caller flags it in the report.
func PriceFactor(perUnit, minPerUnit float64) float64 {
	if perUnit <= 0 || minPerUnit <= 0 {
		return 1.0
	}
	return minPerUnit / perUnit
}

// Composite folds every factor into one number. evidenceFactor is a
// multiplicand rather than a gate: a candidate crushed by evidence must stay in
// the analysis so the renderer can widen around it, whereas a dropped candidate
// disappears entirely. Pass 1.0 when there is no evidence.
func Composite(obtainability, uptime, preemption, price, priceExponent, evidenceFactor float64) (float64, bool) {
	if obtainability < dropBelow {
		return 0, true
	}
	return obtainability * obtainability * uptime * preemption *
		math.Pow(price, priceExponent) * evidenceFactor, false
}

func mean(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := 0.0
	for _, x := range xs {
		s += x
	}
	return s / float64(len(xs))
}
