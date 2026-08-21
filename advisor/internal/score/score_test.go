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

package score

import (
	"math"
	"testing"
)

func almost(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestUptimeFactor(t *testing.T) {
	cases := map[int]float64{3600: 1.0, 7200: 1.0, 600: 0.6, 60: 0.2, 0: 0.6}
	for sec, want := range cases {
		if got := UptimeFactor(sec); !almost(got, want) {
			t.Errorf("UptimeFactor(%d) = %v, want %v", sec, got, want)
		}
	}
}

func TestPreemptionFactor(t *testing.T) {
	flat := make([]float64, 30)
	for i := range flat {
		flat[i] = 0.2
	}
	if got := PreemptionFactor(flat); !almost(got, 0.8) {
		t.Errorf("flat: got %v, want 0.8", got)
	}
	// Worsening: 23 quiet days then 7 bad days -> base (1-0.5)=0.5, penalized ×0.8 = 0.4
	worse := make([]float64, 30)
	for i := 23; i < 30; i++ {
		worse[i] = 0.5
	}
	if got := PreemptionFactor(worse); !almost(got, 0.4) {
		t.Errorf("worsening: got %v, want 0.4", got)
	}
	if got := PreemptionFactor(nil); !almost(got, 1.0) {
		t.Errorf("empty history: got %v, want neutral 1.0", got)
	}
}

func TestPriceFactorAndComposite(t *testing.T) {
	if got := PriceFactor(0.02, 0.01); !almost(got, 0.5) {
		t.Errorf("PriceFactor = %v, want 0.5", got)
	}
	if got := PriceFactor(0, 0.01); !almost(got, 1.0) { // missing price -> neutral
		t.Errorf("missing price: got %v, want 1.0", got)
	}
	c, dropped := Composite(0.9, 1.0, 0.8, 0.5, 1.0, 1.0)
	if dropped || !almost(c, 0.9*0.9*1.0*0.8*0.5) {
		t.Errorf("Composite = %v dropped=%v", c, dropped)
	}
	if _, dropped := Composite(0.3, 1, 1, 1, 1.0, 1.0); !dropped {
		t.Error("obtainability 0.3 must be dropped")
	}
}

func TestCompositePriceExponent(t *testing.T) {
	base, _ := Composite(0.9, 1.0, 1.0, 0.5, 1.0, 1.0)
	squared, _ := Composite(0.9, 1.0, 1.0, 0.5, 2.0, 1.0)
	if !almost(squared, base*0.5) {
		t.Errorf("exponent 2: got %v, want %v", squared, base*0.5)
	}
	neutral, _ := Composite(0.9, 1.0, 1.0, 1.0, 3.0, 1.0)
	if !almost(neutral, 0.9*0.9) {
		t.Errorf("price 1.0 must be exponent-invariant: %v", neutral)
	}
}

func TestCompositeMultipliesByEvidence(t *testing.T) {
	full, dropped := Composite(0.9, 1.0, 1.0, 1.0, 1.0, 1.0)
	if dropped {
		t.Fatal("0.9 obtainability should not be dropped")
	}
	crushed, dropped := Composite(0.9, 1.0, 1.0, 1.0, 1.0, 0.05)
	if dropped {
		t.Fatal("evidence must lower the score, never drop the candidate")
	}
	if math.Abs(crushed-full*0.05) > 1e-9 {
		t.Fatalf("evidence-weighted composite = %v, want %v", crushed, full*0.05)
	}
}

func TestCompositeEvidenceDoesNotResurrectALowPrior(t *testing.T) {
	// The obtainability gate is independent of evidence: a prior below the
	// documented "Low" band is still dropped even with perfect evidence.
	if _, dropped := Composite(0.3, 1.0, 1.0, 1.0, 1.0, 1.0); !dropped {
		t.Fatal("obtainability below 0.4 must still be dropped")
	}
}
