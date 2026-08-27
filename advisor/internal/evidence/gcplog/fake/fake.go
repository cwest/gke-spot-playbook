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

// Package fake provides a scriptable log source for tests.
package fake

import (
	"context"
	"errors"
	"time"

	"github.com/cwest/gke-spot-playbook/advisor/internal/evidence"
)

type Log struct {
	RefusalsFn func(ctx context.Context, since time.Time) ([]evidence.Observation, error)
	// Since records the timestamp of the most recent query, so tests can
	// assert the reconciler bounds its window.
	Since time.Time
}

func (l *Log) Refusals(ctx context.Context, since time.Time) ([]evidence.Observation, error) {
	l.Since = since
	if l.RefusalsFn == nil {
		return nil, errors.New("fake: RefusalsFn not set")
	}
	return l.RefusalsFn(ctx, since)
}
