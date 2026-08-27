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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// ValidationConfiguration defines the configuration for the lifecycle-manager controller.
// The generated CRD YAML is deleted by 'make generate' and not deployed to the cluster.
// +kubebuilder:object:root=true
type ValidationConfiguration struct {
	metav1.TypeMeta `json:",inline"`

	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +optional
	Spec ValidationConfigurationSpec `json:"spec,omitempty"`
}

// ValidationConfigurationSpec holds all configuration for the validation-controller.
type ValidationConfigurationSpec struct {
	// NewNodeValidation groups the configuration for detecting and testing newly joined nodes.
	// +optional
	NewNodeValidation *NewNodeValidationConfig `json:"newNodeValidation,omitempty"`

	// SchedulingGate groups the scheduling gate controls managed during the lifecycle of a
	// validation request.
	// +optional
	SchedulingGate *SchedulingGateConfig `json:"schedulingGate,omitempty"`

	// ReadinessCriteria are CEL expressions that must all evaluate to true before a validation
	// test can be started on a given node.
	// +optional
	ReadinessCriteria []CriteriaSpec `json:"readinessCriteria,omitempty"`

	// DefaultTests is the default set of tests run against ValidationRequests that do not
	// specify tests explicitly.
	// +optional
	DefaultTests []string `json:"defaultTests,omitempty"`

	// TemplateMountPath is the directory from which templateFile paths are resolved.
	// +optional
	TemplateMountPath string `json:"templateMountPath,omitempty"`

	// Providers contains test provider settings, the key is the provider name referenced by
	// tests.
	// +optional
	Providers map[string]ProviderConfig `json:"providers,omitempty"`

	// MaxConcurrentGroups is the maximum number of test groups that may run concurrently.
	// Two groups that share a node will never run at the same time regardless of this setting.
	// +optional
	MaxConcurrentGroups int `json:"maxConcurrentGroups,omitempty"`

	// Tests is the set of supported tests that can be requested by clients in ValidationRequests,
	// the key is the test name.
	// +optional
	Tests map[string]TestConfig `json:"tests,omitempty"`
}

// NewNodeValidationConfig groups the configuration for detecting and testing newly joined nodes.
type NewNodeValidationConfig struct {
	// Condition is the node condition type the controller uses to track whether a node has
	// already been validated. The controller requires this condition to be absent or false before
	// targeting a node, and sets it to True once a ValidationRequest is created.
	// +optional
	Condition string `json:"condition,omitempty"`

	// Criteria are CEL expressions evaluated against each node to determine whether it requires
	// new node validation. All expressions must evaluate to true.
	// +optional
	Criteria []CriteriaSpec `json:"criteria,omitempty"`

	// NewNodeTests lists the tests to run for new nodes. These take precedence over defaultTests.
	// +optional
	NewNodeTests []string `json:"newNodeTests,omitempty"`

	// BatchPeriod is the window during which the controller collects eligible new nodes before
	// creating a ValidationRequest for them as a batch.
	// +optional
	BatchPeriod metav1.Duration `json:"batchPeriod,omitempty"`
}

// CriteriaSpec is a named CEL expression evaluated against a node and its pods.
type CriteriaSpec struct {
	// Name is a human-readable identifier for this criteria.
	// +required
	Name string `json:"name"`

	// Expression is the CEL expression.
	// +required
	Expression string `json:"expression"`
}

// SchedulingGateConfig groups the scheduling gate controls managed during validation.
type SchedulingGateConfig struct {
	// Cordon controls whether nodes are cordoned and uncordoned during validation.
	// +optional
	Cordon CordonConfig `json:"cordon,omitempty"`

	// Taints lists taints the controller removes from nodes when validation completes.

	// +optional
	Taints []TaintConfig `json:"taints,omitempty"`
}

// CordonConfig controls whether nodes are uncordoned after validation.
type CordonConfig struct {
	// Remove indicates whether nodes should be uncordoned after completing validation.
	// +optional
	Remove bool `json:"remove,omitempty"`
}

// TaintConfig describes a taint the controller removes from a node when validation completes.
type TaintConfig struct {
	// Key is the taint key.
	// +required
	Key string `json:"key"`
	// Value is the taint value.
	// +optional
	Value string `json:"value,omitempty"`
	// Effect is the taint effect: NoSchedule, PreferNoSchedule, or NoExecute.
	// +optional
	Effect string `json:"effect,omitempty"`
	// Remove indicates whether this taint should be lifted after validation completes.
	// +optional
	Remove bool `json:"remove,omitempty"`
}

// EnvVarConfig defines a single environment variable for a k8s-job-provider test's container.
type EnvVarConfig struct {
	// Name is the environment variable name.
	// +required
	Name string `json:"name"`
	// Value is the environment variable value.
	// +optional
	Value string `json:"value,omitempty"`
}

// ProviderConfig defines a test provider configuration.
type ProviderConfig struct {
	// APIGroup is the Kubernetes API group of the resources this provider creates.
	// +required
	APIGroup string `json:"apiGroup"`

	// Version is the Kubernetes API version of the resources this provider creates (e.g. "v1").
	// +required
	Version string `json:"version"`

	// Kind is the Kubernetes resource kind this provider creates.
	// +required
	Kind string `json:"kind"`

	// Resource is the plural lowercase resource name of the resources this provider creates.
	// +required
	Resource string `json:"resource"`

	// TemplateFile is the filename relative to templateMountPath of the Go text/template
	// rendered by the controller to construct the provider resource for each test group.
	// +optional
	TemplateFile string `json:"templateFile,omitempty"`

	// SupportsTestBatching indicates whether this provider can execute multiple tests in a
	// single provider resource.
	// +optional
	SupportsTestBatching bool `json:"supportsTestBatching,omitempty"`

	// Retries is the number of times a failed test group is retried before marking it failed.
	// +optional
	Retries int `json:"retries,omitempty"`

	// TimeoutSeconds is the maximum number of seconds allowed for a single test group attempt.
	// +optional
	TimeoutSeconds int64 `json:"timeoutSeconds,omitempty"`

	// SuccessfulCondition describes the condition on the provider resource that indicates a
	// test run succeeded.
	// +optional
	SuccessfulCondition ConditionMatch `json:"successfulCondition,omitempty"`

	// FailedCondition describes the condition on the provider resource that indicates a
	// test run failed.
	// +optional
	FailedCondition ConditionMatch `json:"failedCondition,omitempty"`
}

// ConditionMatch specifies a condition type and status to match on a provider resource.
type ConditionMatch struct {
	// Type is the condition type to match.
	// +required
	Type string `json:"type"`
	// Status is the condition status to match.
	// +required
	Status string `json:"status"`
}

// BatchFailurePolicy controls how a test group is handled when batch minimums are not met.
// +kubebuilder:validation:Enum=fail;ignore
type BatchFailurePolicy string

const (
	// BatchFailurePolicyFail marks the ValidationRequest as failed if the test is not meeting batch requirements.
	BatchFailurePolicyFail BatchFailurePolicy = "fail"
	// BatchFailurePolicyIgnore skips the test if it is not meeting batch requirements.
	BatchFailurePolicyIgnore BatchFailurePolicy = "ignore"
)

// TestConfig defines a named test and how it is executed.
type TestConfig struct {
	// Provider references the name of a ProviderConfig that executes this test.
	// +required
	Provider string `json:"provider"`

	// Image is the container image referenced in test provider resource templates.
	// +optional
	Image string `json:"image,omitempty"`

	// Command overrides the container entrypoint referenced in test provider resource templates.
	// +optional
	Command []string `json:"command,omitempty"`

	// Env sets environment variables on test provider resource templates.
	// +optional
	Env []EnvVarConfig `json:"env,omitempty"`

	// SupportsBatchingNodes indicates whether multiple nodes can be tested together in a
	// single provider resource.
	// +optional
	SupportsBatchingNodes bool `json:"supportsBatchingNodes,omitempty"`

	// MinimumNodesPerBatch is the minimum number of nodes required to form a batch.
	// If this minimum cannot be met, the test refers to its BatchFailurePolicy.
	// +optional
	MinimumNodesPerBatch int `json:"minimumNodesPerBatch,omitempty"`

	// BatchFailurePolicy defines what to do when batch minimums are not met.
	// +optional
	BatchFailurePolicy BatchFailurePolicy `json:"batchFailurePolicy,omitempty"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &ValidationConfiguration{})
		return nil
	})
}
