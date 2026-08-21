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

// Package evidence records observed provisioning failures and turns them into a
// decaying score multiplier. The capacity advice API reports a prior — what
// Google expects a zone can supply. An observed NAP scale-up failure is
// evidence, and evidence outranks the prior. Observations decay exponentially
// so a stockout steers the ladder for the next hour, not the rest of the day.
package evidence

import (
	"math"
	"time"
)

// Key identifies a shape in a zone. Evidence is never generalized across
// either axis: g2-standard-4 being out in us-central1-a says nothing about
// g2-standard-8 there, nor about g2-standard-4 next door.
type Key struct {
	MachineType string
	Zone        string
}

// Observation is one recorded provisioning failure.
type Observation struct {
	MachineType string    `json:"machineType"`
	Zone        string    `json:"zone"`
	Reason      string    `json:"reason"`
	At          time.Time `json:"at"`
}

func (o Observation) key() Key { return Key{o.MachineType, o.Zone} }

// Params configures decay. HalfLife controls recovery speed, Floor is the
// multiplier applied the instant a failure is observed, DryBelow is the
// threshold under which a rung is treated as unavailable, and MaxAge is the
// point past which an observation is ignored entirely.
type Params struct {
	HalfLife time.Duration
	Floor    float64
	DryBelow float64
	MaxAge   time.Duration
}

// Ledger holds the newest observation per key. Only the newest matters: two
// failures for the same shape and zone are the same fact, observed twice.
type Ledger struct {
	Latest map[string]Observation `json:"latest"`
}

func NewLedger() *Ledger { return &Ledger{Latest: map[string]Observation{}} }

func (l *Ledger) id(k Key) string { return k.MachineType + "\x00" + k.Zone }

func (l *Ledger) Add(o Observation) {
	if l.Latest == nil {
		l.Latest = map[string]Observation{}
	}
	id := l.id(o.key())
	if prev, ok := l.Latest[id]; ok && !o.At.After(prev.At) {
		return
	}
	l.Latest[id] = o
}

// Prune drops observations older than maxAge so the state ConfigMap cannot
// grow without bound.
func (l *Ledger) Prune(now time.Time, maxAge time.Duration) {
	for id, o := range l.Latest {
		if now.Sub(o.At) >= maxAge {
			delete(l.Latest, id)
		}
	}
}

// Factor returns the multiplier for k: 1.0 when there is no usable evidence,
// p.Floor at the instant of a failure, recovering exponentially toward 1.0.
func (l *Ledger) Factor(k Key, now time.Time, p Params) float64 {
	o, ok := l.Latest[l.id(k)]
	if !ok {
		return 1.0
	}
	age := now.Sub(o.At)
	if age < 0 {
		age = 0
	}
	if p.MaxAge > 0 && age >= p.MaxAge {
		return 1.0
	}
	if p.HalfLife <= 0 {
		return p.Floor
	}
	decayed := math.Pow(0.5, age.Seconds()/p.HalfLife.Seconds())
	return p.Floor + (1-p.Floor)*(1-decayed)
}

// Dry reports whether a factor is low enough to treat the rung as unavailable.
func Dry(factor float64, p Params) bool { return factor < p.DryBelow }
