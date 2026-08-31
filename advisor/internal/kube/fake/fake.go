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

// Package fake is an in-memory kube.Client for tests.
package fake

import (
	"context"

	"github.com/cwest/gke-spot-playbook/advisor/internal/kube"
)

type Cluster struct {
	PendingClassPodsFn  func(ctx context.Context, classNames []string) (int, error)
	GetStateFn          func(ctx context.Context, ns, name string) ([]byte, error)
	PutStateFn          func(ctx context.Context, ns, name string, doc []byte) error
	ApplyComputeClassFn func(ctx context.Context, yaml []byte) error
	EmitEventFn         func(ctx context.Context, ns string, ev kube.Event) error

	// Recorded traffic. Unlike the advice fakes, the defaults here succeed
	// rather than erroring: a reconcile test cares about one method at a time,
	// and the other four should not need stubbing to get out of the way.
	State   map[string][]byte
	Applied [][]byte
	Events  []kube.Event

	// EventNS records the namespace of every EmitEvent call, including ones the
	// stub then fails. Dropping this argument is what let a real bug reach a
	// live cluster: ComputeClasses are cluster-scoped, so their events carry an
	// empty involvedObject.namespace and the API server rejects them outside
	// "default" — invisible to a fake that only kept the Event itself.
	//
	// It does not index-align with Events, which holds only the calls that
	// reached the default recording path. Assert over the whole slice.
	EventNS []string
}

func New() *Cluster { return &Cluster{State: map[string][]byte{}} }

func (c *Cluster) PendingClassPods(ctx context.Context, classNames []string) (int, error) {
	if c.PendingClassPodsFn != nil {
		return c.PendingClassPodsFn(ctx, classNames)
	}
	return 0, nil
}

func (c *Cluster) GetState(ctx context.Context, ns, name string) ([]byte, error) {
	if c.GetStateFn != nil {
		return c.GetStateFn(ctx, ns, name)
	}
	v := c.State[ns+"/"+name]
	if len(v) == 0 {
		return nil, nil
	}
	return v, nil
}

func (c *Cluster) PutState(ctx context.Context, ns, name string, doc []byte) error {
	if c.PutStateFn != nil {
		return c.PutStateFn(ctx, ns, name, doc)
	}
	if c.State == nil {
		c.State = map[string][]byte{}
	}
	c.State[ns+"/"+name] = doc
	return nil
}

func (c *Cluster) ApplyComputeClass(ctx context.Context, yaml []byte) error {
	if c.ApplyComputeClassFn != nil {
		return c.ApplyComputeClassFn(ctx, yaml)
	}
	c.Applied = append(c.Applied, yaml)
	return nil
}

func (c *Cluster) EmitEvent(ctx context.Context, ns string, ev kube.Event) error {
	c.EventNS = append(c.EventNS, ns)
	if c.EmitEventFn != nil {
		return c.EmitEventFn(ctx, ns, ev)
	}
	c.Events = append(c.Events, ev)
	return nil
}
