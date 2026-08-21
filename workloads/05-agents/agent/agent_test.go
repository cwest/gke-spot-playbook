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

package main

import (
	"testing"
	"time"
)

func TestPhaseAtCyclesActiveThenIdle(t *testing.T) {
	active, idle := 10*time.Second, 30*time.Second
	cases := []struct {
		el   time.Duration
		want Phase
	}{
		{0, Active}, {9 * time.Second, Active}, {10 * time.Second, Idle},
		{39 * time.Second, Idle}, {40 * time.Second, Active}, // wraps
	}
	for _, c := range cases {
		if got := phaseAt(c.el, active, idle); got != c.want {
			t.Errorf("phaseAt(%v)=%v want %v", c.el, got, c.want)
		}
	}
}

func TestParseSizeMiB(t *testing.T) {
	for in, want := range map[string]int{"256": 256, "1024": 1024} {
		if got, err := parseSizeMiB(in); err != nil || got != want {
			t.Errorf("parseSizeMiB(%q)=%d,%v want %d", in, got, err, want)
		}
	}
	if _, err := parseSizeMiB("0"); err == nil {
		t.Error("zero size must error (a snapshot of nothing proves nothing)")
	}
}
