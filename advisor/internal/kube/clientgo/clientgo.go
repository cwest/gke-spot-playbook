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

// Package clientgo implements kube.Client against a real cluster using the
// in-cluster service account. ComputeClass is a CRD, so it goes through the
// dynamic client; everything else is core/v1.
package clientgo

import (
	"context"
	"encoding/json"
	"fmt"

	"gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	applycorev1 "k8s.io/client-go/applyconfigurations/core/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/cwest/gke-spot-instance-node-pools/advisor/internal/kube"
)

// fieldManager identifies our server-side-apply ownership. Applying under a
// stable manager is what lets the reconciler take over the fields a human
// kubectl-applied earlier without clobbering unrelated ones.
const fieldManager = "capacity-advisor"

var computeClassGVR = schema.GroupVersionResource{
	Group: "cloud.google.com", Version: "v1", Resource: "computeclasses",
}

type Client struct {
	cs  kubernetes.Interface
	dyn dynamic.Interface
}

func New(_ context.Context) (*Client, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("in-cluster config: %w", err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("core client: %w", err)
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("dynamic client: %w", err)
	}
	return &Client{cs: cs, dyn: dyn}, nil
}

func (c *Client) PendingClassPods(ctx context.Context, classNames []string) (int, error) {
	want := map[string]bool{}
	for _, n := range classNames {
		if n != "" {
			want[n] = true
		}
	}
	// Field-select server-side so a big cluster does not ship every pod.
	pods, err := c.cs.CoreV1().Pods(metav1.NamespaceAll).List(ctx, metav1.ListOptions{
		FieldSelector: "status.phase=Pending",
	})
	if err != nil {
		return 0, fmt.Errorf("list pending pods: %w", err)
	}
	n := 0
	for _, p := range pods.Items {
		if want[p.Spec.NodeSelector[kube.ClassLabel]] {
			n++
		}
	}
	return n, nil
}

func (c *Client) GetState(ctx context.Context, ns, name string) ([]byte, error) {
	cm, err := c.cs.CoreV1().ConfigMaps(ns).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil // cold start
	}
	if err != nil {
		return nil, fmt.Errorf("get state %s/%s: %w", ns, name, err)
	}
	state := cm.Data[kube.StateKey]
	if state == "" {
		return nil, nil // empty is cold start
	}
	return []byte(state), nil
}

func (c *Client) PutState(ctx context.Context, ns, name string, doc []byte) error {
	cm := &corev1.ConfigMap{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Data:       map[string]string{kube.StateKey: string(doc)},
	}
	_, err := c.cs.CoreV1().ConfigMaps(ns).Apply(ctx, configMapApply(cm), metav1.ApplyOptions{
		FieldManager: fieldManager, Force: true,
	})
	if err != nil {
		return fmt.Errorf("put state %s/%s: %w", ns, name, err)
	}
	return nil
}

func (c *Client) ApplyComputeClass(ctx context.Context, y []byte) error {
	obj, name, err := decodeComputeClass(y)
	if err != nil {
		return err
	}
	_, err = c.dyn.Resource(computeClassGVR).Patch(ctx, name, types.ApplyPatchType, obj,
		metav1.PatchOptions{FieldManager: fieldManager, Force: boolPtr(true)})
	if err != nil {
		return fmt.Errorf("apply computeclass %s: %w", name, err)
	}
	return nil
}

func (c *Client) EmitEvent(ctx context.Context, ns string, ev kube.Event) error {
	now := metav1.Now()
	e := &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "capacity-advisor-", Namespace: ns},
		InvolvedObject: corev1.ObjectReference{
			APIVersion: "cloud.google.com/v1", Kind: "ComputeClass", Name: ev.InvolvedName,
		},
		Reason:         ev.Reason,
		Message:        ev.Message,
		Type:           ev.Type,
		Source:         corev1.EventSource{Component: fieldManager},
		FirstTimestamp: now,
		LastTimestamp:  now,
		Count:          1,
	}
	if _, err := c.cs.CoreV1().Events(ns).Create(ctx, e, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("emit event %s: %w", ev.Reason, err)
	}
	return nil
}

// decodeComputeClass converts a rendered manifest into the JSON server-side
// apply wants, and pulls out metadata.name (the Patch target).
func decodeComputeClass(y []byte) ([]byte, string, error) {
	var m map[string]any
	if err := yaml.Unmarshal(y, &m); err != nil {
		return nil, "", fmt.Errorf("parse computeclass yaml: %w", err)
	}
	meta, ok := m["metadata"].(map[string]any)
	if !ok {
		return nil, "", fmt.Errorf("computeclass yaml metadata is not a map")
	}
	name, ok := meta["name"].(string)
	if !ok {
		return nil, "", fmt.Errorf("computeclass yaml metadata.name is not a string")
	}
	if name == "" {
		return nil, "", fmt.Errorf("computeclass yaml metadata.name is empty")
	}
	j, err := json.Marshal(m)
	if err != nil {
		return nil, "", fmt.Errorf("encode computeclass: %w", err)
	}
	return j, name, nil
}

// configMapApply builds the apply configuration for a ConfigMap without
// pulling in the generated applyconfiguration builders at every call site.
func configMapApply(cm *corev1.ConfigMap) *applycorev1.ConfigMapApplyConfiguration {
	return applycorev1.ConfigMap(cm.Name, cm.Namespace).WithData(cm.Data)
}

func boolPtr(b bool) *bool { return &b }

var _ kube.Client = (*Client)(nil)
