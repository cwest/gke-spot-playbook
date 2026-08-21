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

package evidence

import (
	"encoding/json"
	"math"
	"testing"
	"time"
)

var testParams = Params{
	HalfLife: 30 * time.Minute,
	Floor:    0.05,
	DryBelow: 0.5,
	MaxAge:   6 * time.Hour,
}

func at(min int) time.Time {
	return time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC).Add(time.Duration(min) * time.Minute)
}

func TestFactorNoObservationIsNeutral(t *testing.T) {
	l := NewLedger()
	if got := l.Factor(Key{"g2-standard-4", "us-central1-a"}, at(0), testParams); got != 1.0 {
		t.Fatalf("Factor with no observation = %v, want 1.0", got)
	}
}

func TestFactorFreshObservationHitsFloor(t *testing.T) {
	l := NewLedger()
	l.Add(Observation{MachineType: "g2-standard-4", Zone: "us-central1-a", Reason: "zonal.resources.exceeded", At: at(0)})
	got := l.Factor(Key{"g2-standard-4", "us-central1-a"}, at(0), testParams)
	if math.Abs(got-0.05) > 1e-9 {
		t.Fatalf("fresh Factor = %v, want 0.05", got)
	}
}

func TestFactorRecoversWithHalfLife(t *testing.T) {
	l := NewLedger()
	l.Add(Observation{MachineType: "g2-standard-4", Zone: "us-central1-a", At: at(0)})
	k := Key{"g2-standard-4", "us-central1-a"}
	// One half-life: floor + (1-floor)*(1-0.5) = 0.05 + 0.475 = 0.525.
	if got := l.Factor(k, at(30), testParams); math.Abs(got-0.525) > 1e-9 {
		t.Fatalf("Factor at one half-life = %v, want 0.525", got)
	}
	// Two half-lives: floor + (1-floor)*(1-0.25) = 0.05 + 0.7125 = 0.7625. A
	// linear ramp also passes the one-half-life probe above (the curves cross
	// there by construction); only 2^(-age/halfLife) hits 0.7625 here.
	if got := l.Factor(k, at(60), testParams); math.Abs(got-0.7625) > 1e-9 {
		t.Fatalf("Factor at two half-lives = %v, want 0.7625", got)
	}
	// Monotonically increasing.
	if l.Factor(k, at(60), testParams) <= l.Factor(k, at(30), testParams) {
		t.Fatal("Factor did not recover between 30m and 60m")
	}
}

func TestFactorClampsClockSkew(t *testing.T) {
	l := NewLedger()
	l.Add(Observation{MachineType: "g2-standard-4", Zone: "us-central1-a", At: at(0)})
	// Cloud Logging timestamps are compared against a local clock, so `now` can
	// land before the observation. A negative age must clamp to zero rather than
	// producing a factor above 1.0 that would boost a shape that just failed.
	got := l.Factor(Key{"g2-standard-4", "us-central1-a"}, at(-10), testParams)
	if math.Abs(got-0.05) > 1e-9 {
		t.Fatalf("Factor with now before the observation = %v, want 0.05", got)
	}
}

func TestFactorBeyondMaxAgeIsNeutral(t *testing.T) {
	l := NewLedger()
	l.Add(Observation{MachineType: "g2-standard-4", Zone: "us-central1-a", At: at(0)})
	k := Key{"g2-standard-4", "us-central1-a"}
	if got := l.Factor(k, at(7*60), testParams); got != 1.0 {
		t.Fatalf("Factor past MaxAge = %v, want 1.0", got)
	}
	// The cutoff is inclusive: exactly MaxAge is already neutral, matching
	// Prune, which drops an entry at exactly MaxAge.
	if got := l.Factor(k, at(6*60), testParams); got != 1.0 {
		t.Fatalf("Factor at exactly MaxAge = %v, want 1.0", got)
	}
	// One nanosecond inside the cutoff still decays.
	if got := l.Factor(k, at(6*60).Add(-time.Nanosecond), testParams); math.Abs(got-0.99976806640625) > 1e-9 {
		t.Fatalf("Factor a nanosecond inside MaxAge = %v, want 0.99976806640625", got)
	}
}

func TestFactorIsPerZone(t *testing.T) {
	l := NewLedger()
	l.Add(Observation{MachineType: "g2-standard-4", Zone: "us-central1-a", At: at(0)})
	if got := l.Factor(Key{"g2-standard-4", "us-central1-b"}, at(0), testParams); got != 1.0 {
		t.Fatalf("sibling zone Factor = %v, want 1.0", got)
	}
	if got := l.Factor(Key{"g2-standard-8", "us-central1-a"}, at(0), testParams); got != 1.0 {
		t.Fatalf("sibling shape Factor = %v, want 1.0", got)
	}
	// The key encoding must be injective: {"a","bc"} and {"ab","c"} concatenate
	// to the same string without a separator, and would then share evidence.
	l.Add(Observation{MachineType: "a", Zone: "bc", At: at(0)})
	if got := l.Factor(Key{"a", "bc"}, at(0), testParams); math.Abs(got-0.05) > 1e-9 {
		t.Fatalf(`Factor for {"a","bc"} = %v, want 0.05`, got)
	}
	if got := l.Factor(Key{"ab", "c"}, at(0), testParams); got != 1.0 {
		t.Fatalf(`Factor for {"ab","c"} = %v, want 1.0 (must not collide with {"a","bc"})`, got)
	}
}

func TestAddKeepsNewestPerKey(t *testing.T) {
	l := NewLedger()
	k := Key{"g2-standard-4", "us-central1-a"}
	l.Add(Observation{MachineType: k.MachineType, Zone: k.Zone, At: at(60)})
	l.Add(Observation{MachineType: k.MachineType, Zone: k.Zone, At: at(0)}) // older, must not win
	if got := l.Factor(k, at(60), testParams); math.Abs(got-0.05) > 1e-9 {
		t.Fatalf("Factor = %v, want the newer observation to win (0.05)", got)
	}
	if len(l.Latest) != 1 {
		t.Fatalf("ledger holds %d entries, want 1", len(l.Latest))
	}
}

func TestPruneDropsStaleEntries(t *testing.T) {
	l := NewLedger()
	l.Add(Observation{MachineType: "a", Zone: "z1", At: at(0)})
	l.Add(Observation{MachineType: "b", Zone: "z2", At: at(5*60 + 59)})
	// Exactly maxAge old: the cutoff is inclusive, so this one goes too.
	l.Add(Observation{MachineType: "c", Zone: "z3", At: at(60)})
	l.Prune(at(7*60), 6*time.Hour)
	if len(l.Latest) != 1 {
		t.Fatalf("after Prune ledger holds %d entries, want 1", len(l.Latest))
	}
	if _, ok := l.Latest[l.id(Key{"b", "z2"})]; !ok {
		t.Fatalf("after Prune the surviving entry is %v, want b/z2", l.Latest)
	}
}

func TestDry(t *testing.T) {
	if !Dry(0.05, testParams) {
		t.Fatal("0.05 should be dry")
	}
	if Dry(1.0, testParams) {
		t.Fatal("1.0 should not be dry")
	}
	if Dry(0.5, testParams) {
		t.Fatal("DryBelow is exclusive: 0.5 should not be dry")
	}
}

func TestLedgerRoundTripsThroughJSON(t *testing.T) {
	l := NewLedger()
	l.Add(Observation{MachineType: "g2-standard-4", Zone: "us-central1-a", Reason: "r", At: at(0)})
	b, err := json.Marshal(l)
	if err != nil {
		t.Fatal(err)
	}
	// The on-disk form is a cross-task contract: task 9 stores this in the
	// state ConfigMap. Marshal and unmarshal are the same code, so a tag rename
	// cancels out in a round-trip; pin the actual bytes. This is a raw string
	// literal: the map key's NUL separator appears below exactly as
	// encoding/json escapes it.
	const want = `{"latest":{"g2-standard-4\u0000us-central1-a":` +
		`{"machineType":"g2-standard-4","zone":"us-central1-a","reason":"r","at":"2026-07-26T12:00:00Z"}}}`
	if string(b) != want {
		t.Fatalf("wire form =\n%s\nwant\n%s", b, want)
	}
	var got Ledger
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if f := got.Factor(Key{"g2-standard-4", "us-central1-a"}, at(0), testParams); math.Abs(f-0.05) > 1e-9 {
		t.Fatalf("post-roundtrip Factor = %v, want 0.05", f)
	}
}
