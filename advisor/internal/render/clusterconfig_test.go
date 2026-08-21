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

package render

import (
	"strings"
	"testing"
)

func TestClusterConfig(t *testing.T) {
	got, err := ClusterConfig(fixtureAnalysis("cpu"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(got)
	for _, want := range []string{"PROJECT=p\n", "REGION=us-central1\n", "ZONES=us-central1-a,us-central1-b\n"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in:\n%s", want, s)
		}
	}
}

func TestClusterConfigAllDropped(t *testing.T) {
	a := fixtureAnalysis("cpu")
	for i := range a.Candidates {
		a.Candidates[i].Dropped = true
	}
	got, err := ClusterConfig(a)
	if err != nil {
		t.Fatalf("all-dropped should still render: %v", err)
	}
	s := string(got)
	if !strings.Contains(s, "# WARNING: region chosen from below-band candidates.") {
		t.Errorf("missing warning in:\n%s", s)
	}
	if !strings.Contains(s, "REGION=us-central1\n") {
		t.Errorf("expected region from dropped candidates in:\n%s", s)
	}
}

func TestClusterConfigEmpty(t *testing.T) {
	a := fixtureAnalysis("cpu")
	a.Candidates = nil
	if _, err := ClusterConfig(a); err == nil {
		t.Fatal("expected error when no candidates exist at all")
	}
}
