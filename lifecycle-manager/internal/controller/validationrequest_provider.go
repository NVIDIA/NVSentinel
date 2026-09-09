// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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

package controller

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	sigsyaml "sigs.k8s.io/yaml"

	"github.com/nvidia/nvsentinel/lifecycle-manager/api/v1alpha1"
)

type TemplateContext struct {
	ValidationRequestName string
	TestGroupName         string
	Namespace             string
	TimeoutSeconds        int64
	Nodes                 []NodeTemplateContext
	Tests                 []string
	Image                 string
	Command               []string
	Env                   []EnvVarTemplateContext
	Tolerations           []TolerationTemplateContext
}

type NodeTemplateContext struct {
	NodeName string
}

type TolerationTemplateContext struct {
	Key      string
	Value    string
	Operator string
	Effect   string
}

type EnvVarTemplateContext struct {
	Name  string
	Value string
}

func (r *ValidationRequestReconciler) createTestGroupObject(ctx context.Context, vr *v1alpha1.ValidationRequest,
	group *v1alpha1.TestGroupStatus, objectName string) error {
	u, err := r.renderTestGroupObject(vr, group, objectName)
	if err != nil {
		return err
	}

	if err := ctrl.SetControllerReference(vr, u, r.Scheme); err != nil {
		return fmt.Errorf("set owner reference: %w", err)
	}

	if err := r.Create(ctx, u); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create provider resource %q: %w", objectName, err)
	}

	return nil
}

func (r *ValidationRequestReconciler) deleteTestGroupObject(ctx context.Context, group *v1alpha1.TestGroupStatus,
	objectName string) error {
	providerCfg := r.Config.Validation.Spec.Providers[group.Provider]

	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(providerGVK(providerCfg))
	u.SetName(objectName)
	u.SetNamespace(r.Namespace)

	// Explicitly request background cascading deletion. Some providers like kinds like Jobs default to
	// orphaning dependents on delete.
	deleteOpt := client.PropagationPolicy(metav1.DeletePropagationBackground)
	if err := r.Delete(ctx, u, deleteOpt); client.IgnoreNotFound(err) != nil {
		return fmt.Errorf("delete provider resource %q: %w", objectName, err)
	}

	return nil
}

func (r *ValidationRequestReconciler) checkTestGroupObjectStatus(ctx context.Context, group *v1alpha1.TestGroupStatus,
	objectName string) (succeeded bool, failed bool, err error) {
	providerCfg := r.Config.Validation.Spec.Providers[group.Provider]

	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(providerGVK(providerCfg))

	// This is the only place we do a consistent read to avoid a race condition between reconciling the ValidationRequest
	// after updating its status with the newly created test provider resource and checking the status of this resource.
	// Specifically, there could be a race condition where our local cache does not have the newly created test provider
	// resource which we would interpret as the resource being deleted. Alternative options would be to treat a not
	// found error as retryable within a timeout or by polling that the object exists immediately after creation in the
	// same reconcile loop.
	if err := r.APIReader.Get(ctx, types.NamespacedName{Name: objectName, Namespace: r.Namespace}, u); err != nil {
		if apierrors.IsNotFound(err) {
			return false, true, nil
		}

		return false, false, err
	}

	conditions, _, err := unstructured.NestedSlice(u.Object, "status", "conditions")
	if err != nil {
		return false, false, fmt.Errorf("read status.conditions of provider resource %q: %w", objectName, err)
	}

	for _, raw := range conditions {
		cond, ok := raw.(map[string]any)
		if !ok {
			continue
		}

		condType, _ := cond["type"].(string)
		condStatus, _ := cond["status"].(string)

		switch {
		case condType == providerCfg.SuccessfulCondition.Type && condStatus == providerCfg.SuccessfulCondition.Status:
			return true, false, nil
		case condType == providerCfg.FailedCondition.Type && condStatus == providerCfg.FailedCondition.Status:
			return false, true, nil
		}
	}

	return false, false, nil
}

/*
Each TemplateContext field is populated from the following sources:
- ValidationRequestName = vr.Name
- TestGroupName         = group.Name
- Namespace             = r.Namespace
- TimeoutSeconds        = r.Config.Validation.Spec.Providers[group.Provider].Timeout
- Nodes                 = group.Nodes
- Tests                 = group.Tests
- Image, Command, Env   = r.Config.Validation.Spec.Tests[group.Tests[0]]
- Tolerations           = a fixed toleration for the unschedulable taint and one per taint in
r.Config.Validation.Spec.SchedulingGate.Taints

If a given test provider supports batching and we batch multiple tests, we will consume settings from the
first test configuration. This relates to the image, command, and env test settings. In the future, we could choose to
reject test configurations which set different values for these fields if their test provider supports test batching.
In general, these 3 fields should not be set if a test provider supports test batching. Additionally, if a test does
support batching, it will likely need to template the tests field above because the tests to run cannot be derived
directly from the test provider template. In other words, the test provider which supports test batching will need
knowledge of which subset of tests it supports to run.
*/
func (r *ValidationRequestReconciler) renderTestGroupObject(vr *v1alpha1.ValidationRequest,
	group *v1alpha1.TestGroupStatus, objectName string) (*unstructured.Unstructured, error) {
	providerCfg := r.Config.Validation.Spec.Providers[group.Provider]
	tmpl := r.Config.Templates[providerCfg.TemplateFile]

	nodeContext := make([]NodeTemplateContext, len(group.Nodes))
	for i, n := range group.Nodes {
		nodeContext[i] = NodeTemplateContext{NodeName: n}
	}

	var (
		image   string
		command []string
		env     []EnvVarTemplateContext
	)

	if len(group.Tests) > 0 {
		testCfg := r.Config.Validation.Spec.Tests[group.Tests[0]]
		image = testCfg.Image
		command = testCfg.Command

		for _, e := range testCfg.Env {
			env = append(env, EnvVarTemplateContext{Name: e.Name, Value: e.Value})
		}
	}

	tolerations := []TolerationTemplateContext{
		{
			Key:      corev1.TaintNodeUnschedulable,
			Operator: string(corev1.TolerationOpExists),
			Effect:   string(corev1.TaintEffectNoSchedule),
		},
	}

	if schedulingGate := r.Config.Validation.Spec.SchedulingGate; schedulingGate != nil {
		for _, t := range schedulingGate.Taints {
			operator := string(corev1.TolerationOpExists)
			if t.Value != "" {
				operator = string(corev1.TolerationOpEqual)
			}

			tolerations = append(tolerations, TolerationTemplateContext{
				Key:      t.Key,
				Value:    t.Value,
				Operator: operator,
				Effect:   t.Effect,
			})
		}
	}

	template := TemplateContext{
		ValidationRequestName: vr.Name,
		TestGroupName:         group.Name,
		Namespace:             r.Namespace,
		TimeoutSeconds:        providerCfg.TimeoutSeconds,
		Nodes:                 nodeContext,
		Tests:                 group.Tests,
		Image:                 image,
		Command:               command,
		Env:                   env,
		Tolerations:           tolerations,
	}

	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, filepath.Base(providerCfg.TemplateFile), template); err != nil {
		return nil, fmt.Errorf("render template %q: %w", providerCfg.TemplateFile, err)
	}

	var obj map[string]any
	if err := sigsyaml.Unmarshal(buf.Bytes(), &obj); err != nil {
		return nil, fmt.Errorf("parse rendered template: %w", err)
	}

	u := &unstructured.Unstructured{Object: obj}
	u.SetName(objectName)
	u.SetNamespace(r.Namespace)

	return u, nil
}

/*
The groupName is pre-determined by the groupName function which is derived from the set of tests and the TestGroup
index. The attemptObjectName is determined from the ValidationRequest, groupName for the current TestGroup, and the
attempt number.

1. No hash needed
Input: tests: ["dcgm-level4", "nccl-loopback"], vrName: "vr-8f3a2b", attempt 1
groupName  = dcgm-level4-nccl-loopback-group-1
attemptName = vr-8f3a2b-dcgm-level4-nccl-loopback-group-1-1

2. Hash required for long test names
Input: tests: ["multi-node-nccl-all-reduce-benchmark", "nemotron4-15b-goodput-check"], vrName: "vr-8f3a2b", attempt 1
groupName  = multi-node-nccl-all-reduce-benchmark-nemotr-4c26a2c1-group-2
attemptName = multi-node-nccl-all-reduce-benchmark-nemotr-4c26a2c1-group-2-1
*/
func attemptObjectName(vrName, grpName string, attemptNumber int) string {
	suffix := fmt.Sprintf("%s-%d", grpName, attemptNumber)

	if len(suffix) >= validation.DNS1123LabelMaxLength {
		return strings.TrimRight(suffix[:validation.DNS1123LabelMaxLength], "-")
	}

	remaining := validation.DNS1123LabelMaxLength - len(suffix) - 1
	if remaining <= 0 {
		return suffix
	}

	if len(vrName) > remaining {
		vrName = vrName[:remaining]
	}

	return fmt.Sprintf("%s-%s", vrName, suffix)
}

func providerGVK(p v1alpha1.ProviderConfig) schema.GroupVersionKind {
	return schema.GroupVersionKind{
		Group:   p.APIGroup,
		Version: p.Version,
		Kind:    p.Kind,
	}
}
