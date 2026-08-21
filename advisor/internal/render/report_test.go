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
	"encoding/json"
	"strings"
	"testing"

	"github.com/cwest/gke-spot-instance-node-pools/advisor/internal/analyze"
)

func TestSparkline(t *testing.T) {
	if got := Sparkline(nil); got != "" {
		t.Errorf("empty: %q", got)
	}
	got := Sparkline([]float64{0, 0.25, 0.5, 1.0})
	if len([]rune(got)) != 4 || !strings.HasSuffix(got, "█") || []rune(got)[0] != '▁' {
		t.Errorf("Sparkline = %q", got)
	}
}

func TestReportMarkdownGolden(t *testing.T) {
	a := fixtureAnalysis("cpu")
	a.Candidates[0].Daily = []float64{0.1, 0.2, 0.4}
	a.Candidates[0].SpotHourlyUSD = 0.08
	a.Candidates[0].Flags = []string{"no-uptime"}
	checkGolden(t, "advice-report.md", ReportMarkdown(a))
}

func TestReportMarkdownListsSkippedRegions(t *testing.T) {
	a := &analyze.Analysis{
		Profile: "gpu-batch", Kind: "gpu",
		SkippedRegions: []analyze.SkippedRegion{{Region: "us-east5", Reason: "machine type not available"}},
	}
	md := string(ReportMarkdown(a))
	if !strings.Contains(md, "## Skipped regions") || !strings.Contains(md, "us-east5") {
		t.Fatalf("report missing skipped-regions section:\n%s", md)
	}
}

func TestReportJSONRoundTrips(t *testing.T) {
	b, err := ReportJSON(fixtureAnalysis("cpu"))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if m["Profile"] != "cpu-batch" {
		t.Errorf("Profile = %v", m["Profile"])
	}
}
