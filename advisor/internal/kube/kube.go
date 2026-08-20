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

// Package kube is the reconciler's entire view of the cluster: five methods,
// no client-go types. Keeping the surface this narrow means the reconcile logic
// is testable without an API server, and the RBAC the CronJob needs is exactly
// what this interface implies — nothing wider.
package kube

import "context"

// ClassLabel is the node selector GKE uses to bind a pod to a custom compute
// class. Pending pods carrying it are the reconciler's "is anyone waiting?"
// signal.
const ClassLabel = "cloud.google.com/compute-class"

// StateKey is the ConfigMap data key holding the serialized reconciler state.
const StateKey = "state.json"

// Event is a cluster event the reconciler emits so its decisions are visible
// to `kubectl get events` without reading logs.
type Event struct {
	Reason       string
	Message      string
	Type         string // "Normal" or "Warning"
	InvolvedName string // the ComputeClass the event is about
}

type Client interface {
	// PendingClassPods counts Pending pods selecting any of classNames via
	// spec.nodeSelector. Pods bound through nodeAffinity are invisible. Zero
	// means nobody is waiting, which is the reconciler's cheap gate: it skips
	// the expensive log query entirely.
	PendingClassPods(ctx context.Context, classNames []string) (int, error)
	// GetState returns the state document, or (nil, nil) when it does not
	// exist yet. A missing or empty ConfigMap is a cold start, not an error.
	GetState(ctx context.Context, ns, name string) ([]byte, error)
	PutState(ctx context.Context, ns, name string, doc []byte) error
	// ApplyComputeClass server-side applies one ComputeClass manifest.
	ApplyComputeClass(ctx context.Context, yaml []byte) error
	EmitEvent(ctx context.Context, ns string, ev Event) error
}
