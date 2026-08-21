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
	"fmt"
	"strconv"
	"time"
)

type Phase int

const (
	Active Phase = iota
	Idle
)

func phaseAt(elapsed time.Duration, active, idle time.Duration) Phase {
	cycle := active + idle
	pos := elapsed % cycle
	if pos < active {
		return Active
	}
	return Idle
}

func parseSizeMiB(s string) (int, error) {
	v, err := strconv.Atoi(s)
	if err != nil {
		return 0, err
	}
	if v <= 0 {
		return 0, fmt.Errorf("size must be > 0, got %d", v)
	}
	return v, nil
}

type WorkingSet struct {
	buf []byte
}

func NewWorkingSet(mib int) *WorkingSet {
	buf := make([]byte, int64(mib)<<20)
	pageSize := 4096
	for i := 0; i < len(buf); i += pageSize {
		buf[i] = 1
	}
	return &WorkingSet{buf: buf}
}
