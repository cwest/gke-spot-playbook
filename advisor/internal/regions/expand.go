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

// Package regions expands allowedRegions entries (keywords, explicit names,
// prefix globs) against the live region list.
package regions

import (
	"fmt"
	"sort"
	"strings"
)

var keywords = map[string][]string{
	"global":       {""},
	"us":           {"us-"},
	"eu":           {"europe-"},
	"asia":         {"asia-"},
	"northamerica": {"northamerica-"},
	"southamerica": {"southamerica-"},
	"australia":    {"australia-"},
	"me":           {"me-"},
	"africa":       {"africa-"},
}

func Expand(allowed, all []string) ([]string, error) {
	set := map[string]bool{}
	for _, entry := range allowed {
		prefixes, matched := prefixesFor(entry, all)
		entryMatched := matched
		for _, r := range all {
			for _, p := range prefixes {
				if strings.HasPrefix(r, p) {
					set[r] = true
					entryMatched = true
				}
			}
		}
		if matched {
			set[entry] = true
		}
		if !entryMatched {
			return nil, fmt.Errorf("allowedRegions entry %q matches no region", entry)
		}
	}
	if len(set) == 0 {
		return nil, fmt.Errorf("allowedRegions %v matched no regions", allowed)
	}
	out := make([]string, 0, len(set))
	for r := range set {
		out = append(out, r)
	}
	sort.Strings(out)
	return out, nil
}

// prefixesFor returns match prefixes for keyword/glob entries, and whether
// the entry is itself an exact region name in all.
func prefixesFor(entry string, all []string) ([]string, bool) {
	if p, ok := keywords[entry]; ok {
		return p, false
	}
	if strings.HasSuffix(entry, "*") {
		prefix := strings.TrimSuffix(entry, "*")
		for _, r := range all {
			if strings.HasPrefix(r, prefix) {
				return []string{prefix}, false
			}
		}
		return nil, false
	}
	for _, r := range all {
		if r == entry {
			return nil, true
		}
	}
	return nil, false
}
