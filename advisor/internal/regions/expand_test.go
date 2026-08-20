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

package regions

import (
	"testing"

	"github.com/google/go-cmp/cmp"
)

var all = []string{
	"africa-south1", "asia-east1", "asia-south1", "australia-southeast1",
	"europe-west1", "europe-west4", "me-central1", "northamerica-northeast1",
	"southamerica-east1", "us-central1", "us-east4", "us-west1",
}

func TestExpand(t *testing.T) {
	cases := []struct {
		name    string
		allowed []string
		want    []string
	}{
		{"keyword us", []string{"us"}, []string{"us-central1", "us-east4", "us-west1"}},
		{"keyword eu", []string{"eu"}, []string{"europe-west1", "europe-west4"}},
		{"global", []string{"global"}, all},
		{"explicit", []string{"us-east4"}, []string{"us-east4"}},
		{"prefix glob", []string{"europe-west*"}, []string{"europe-west1", "europe-west4"}},
		{"union dedup", []string{"us", "us-central1", "asia-east1"},
			[]string{"asia-east1", "us-central1", "us-east4", "us-west1"}},
	}
	for _, tc := range cases {
		got, err := Expand(tc.allowed, all)
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if diff := cmp.Diff(tc.want, got); diff != "" {
			t.Errorf("%s: (-want +got)\n%s", tc.name, diff)
		}
	}
}

func TestExpandErrors(t *testing.T) {
	for name, allowed := range map[string][]string{
		"typo region": {"us-centrall"},
		"dead glob":   {"mars-*"},
		"bad keyword": {"moon"},
	} {
		if _, err := Expand(allowed, all); err == nil {
			t.Errorf("%s: expected error, got nil", name)
		}
	}

	// Test keyword with no matching regions using a fixture without me-* regions
	allWithoutMe := []string{
		"africa-south1", "asia-east1", "asia-south1", "australia-southeast1",
		"europe-west1", "europe-west4", "northamerica-northeast1",
		"southamerica-east1", "us-central1", "us-east4", "us-west1",
	}
	if _, err := Expand([]string{"me", "us-central1"}, allWithoutMe); err == nil {
		t.Errorf("keyword with no matching regions: expected error, got nil")
	}
}
