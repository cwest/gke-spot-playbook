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

package fake

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cwest/gke-spot-instance-node-pools/advisor/internal/evidence"
)

func TestLogFailsLoudlyWhenUnscripted(t *testing.T) {
	// A nil RefusalsFn is a test that forgot to script the log. Returning an
	// error rather than an empty slice keeps that from reading as "no evidence",
	// which is a legitimate result the reconciler acts on.
	var l Log
	since := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	got, err := l.Refusals(context.Background(), since)
	if err == nil {
		t.Fatalf("Refusals succeeded with no RefusalsFn set: %+v", got)
	}
	if got != nil {
		t.Errorf("Refusals returned %+v alongside an error, want nil", got)
	}
	if !strings.Contains(err.Error(), "RefusalsFn") {
		t.Errorf("error = %q, want it to name the unset field", err)
	}
	// Since is recorded even on the error path, so a test can still assert the
	// window the reconciler asked for.
	if !l.Since.Equal(since) {
		t.Errorf("Since = %v, want %v", l.Since, since)
	}
}

func TestLogRecordsTheWindowAndDelegates(t *testing.T) {
	want := []evidence.Observation{{MachineType: "g2-standard-4", Zone: "us-central1-a"}}
	var gotSince time.Time
	l := Log{RefusalsFn: func(_ context.Context, since time.Time) ([]evidence.Observation, error) {
		gotSince = since
		return want, nil
	}}

	first := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	got, err := l.Refusals(context.Background(), first)
	if err != nil {
		t.Fatalf("Refusals: %v", err)
	}
	if len(got) != 1 || got[0] != want[0] {
		t.Errorf("Refusals = %+v, want %+v", got, want)
	}
	if !gotSince.Equal(first) {
		t.Errorf("RefusalsFn saw since = %v, want %v", gotSince, first)
	}
	if !l.Since.Equal(first) {
		t.Errorf("Since = %v, want %v", l.Since, first)
	}

	// Since tracks the most recent query, not the first.
	second := first.Add(10 * time.Minute)
	if _, err := l.Refusals(context.Background(), second); err != nil {
		t.Fatalf("Refusals: %v", err)
	}
	if !l.Since.Equal(second) {
		t.Errorf("Since = %v after a second query, want %v", l.Since, second)
	}
}

func TestLogPropagatesTheScriptedError(t *testing.T) {
	boom := errors.New("boom")
	l := Log{RefusalsFn: func(context.Context, time.Time) ([]evidence.Observation, error) {
		return nil, boom
	}}
	if _, err := l.Refusals(context.Background(), time.Now()); !errors.Is(err, boom) {
		t.Errorf("error = %v, want %v", err, boom)
	}
}
