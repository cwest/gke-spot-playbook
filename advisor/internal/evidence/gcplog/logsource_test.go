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

package gcplog_test

import (
	"context"
	"time"

	"github.com/cwest/gke-spot-playbook/advisor/internal/evidence"
	"github.com/cwest/gke-spot-playbook/advisor/internal/evidence/gcplog"
	"github.com/cwest/gke-spot-playbook/advisor/internal/evidence/gcplog/fake"
)

// logSource mirrors the interface the reconciler will consume. Asserting it
// here means a rename on either side breaks this package instead of surfacing
// later, when the reconciler first tries to substitute the fake for the reader.
type logSource interface {
	Refusals(ctx context.Context, since time.Time) ([]evidence.Observation, error)
}

var (
	_ logSource = (*gcplog.Reader)(nil)
	_ logSource = (*fake.Log)(nil)
)
