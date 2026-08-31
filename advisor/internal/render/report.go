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
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/cwest/gke-spot-playbook/advisor/internal/analyze"
)

var sparks = []rune("▁▂▃▄▅▆▇█")

// Sparkline renders daily preemption rates scaled 0..max.
func Sparkline(rates []float64) string {
	if len(rates) == 0 {
		return ""
	}
	peak := 0.0
	for _, r := range rates {
		if r > peak {
			peak = r
		}
	}
	out := make([]rune, len(rates))
	for i, r := range rates {
		idx := 0
		if peak > 0 {
			idx = int(r / peak * float64(len(sparks)-1))
		}
		out[i] = sparks[idx]
	}
	return string(out)
}

func ReportMarkdown(a *analyze.Analysis) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "# Capacity advice — %s\n\n", a.Profile)
	fmt.Fprintf(&b, "Generated %s | project `%s` | size %d | regions %v\n\n",
		a.GeneratedAt.UTC().Format(time.RFC3339), a.Project, a.Size, a.AllowedRegions)
	fmt.Fprintf(&b, "Scores are advisory (beta API), not capacity guarantees.\n\n")
	b.WriteString("| # | machine type | zone | obtainability | uptime | preemption (30d) | spot $/hr | composite | flags |\n")
	b.WriteString("|---|---|---|---|---|---|---|---|---|\n")
	for i, c := range a.Candidates {
		mark := ""
		if c.Dropped {
			mark = " ~~dropped~~"
		}
		fmt.Fprintf(&b, "| %d | %s | %s | %.2f | %ds | %s | %.4f | %.3f%s | %s |\n",
			i+1, c.MachineType, c.Zone, c.Obtainability, c.EstimatedUptimeSeconds,
			Sparkline(c.Daily), c.SpotHourlyUSD, c.Composite, mark, flagsOrDash(c.Flags))
	}
	if len(a.SkippedRegions) > 0 {
		fmt.Fprintf(&b, "\n## Skipped regions\n\n")
		fmt.Fprintf(&b, "These allowed regions were excluded (capacity API rejected the query — typically the machine family is not offered there):\n\n")
		for _, s := range a.SkippedRegions {
			fmt.Fprintf(&b, "- `%s` — %s\n", s.Region, s.Reason)
		}
	}
	return b.Bytes()
}

func flagsOrDash(f []string) string {
	if len(f) == 0 {
		return "—"
	}
	return strings.Join(f, ", ")
}

func ReportJSON(a *analyze.Analysis) ([]byte, error) {
	return json.MarshalIndent(a, "", "  ")
}
