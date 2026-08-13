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

package config

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"text/template"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"

	"github.com/nvidia/nvsentinel/lifecycle-manager/api/v1alpha1"
)

const (
	defaultMaxConcurrentGroups  = 3
	defaultMinimumNodesPerBatch = 1
	defaultNewNodeBatchPeriod   = 5 * time.Minute
	defaultNewNodeCondition     = "NewNodeValidated"
)

// Config holds the ValidationConfiguration and provider templates
type Config struct {
	Validation *v1alpha1.ValidationConfiguration
	Templates  map[string]*template.Template
}

var funcMap = template.FuncMap{
	"join": strings.Join,
}

func LoadConfig(path string) (*Config, error) {
	validation, err := loadRaw(path)
	if err != nil {
		return nil, err
	}

	applyDefaults(validation)

	if err := validate(validation); err != nil {
		return nil, fmt.Errorf("invalid ValidationConfiguration: %w", err)
	}

	templates, err := loadTemplates(validation)
	if err != nil {
		return nil, err
	}

	return &Config{
		Validation: validation,
		Templates:  templates,
	}, nil
}

func loadRaw(path string) (*v1alpha1.ValidationConfiguration, error) {
	if len(path) == 0 {
		return &v1alpha1.ValidationConfiguration{}, nil
	}

	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("failed to build scheme: %w", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file %q: %w", path, err)
	}

	obj, _, err := serializer.NewCodecFactory(scheme).UniversalDeserializer().Decode(data, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to decode config file %q: %w", path, err)
	}

	cfg, ok := obj.(*v1alpha1.ValidationConfiguration)
	if !ok {
		return nil, fmt.Errorf("config file %q contains unexpected type %T", path, obj)
	}

	return cfg, nil
}

// loadTemplates parses each unique templateFile referenced by providers.
// An error is returned if any file is missing or contains a template syntax error.
func loadTemplates(cfg *v1alpha1.ValidationConfiguration) (map[string]*template.Template, error) {
	templates := make(map[string]*template.Template, len(cfg.Spec.Providers))

	for name, provider := range cfg.Spec.Providers {
		if _, ok := templates[provider.TemplateFile]; !ok {
			path := filepath.Join(cfg.Spec.TemplateMountPath, provider.TemplateFile)

			tmpl, err := template.New(provider.TemplateFile).Funcs(funcMap).ParseFiles(path)
			if err != nil {
				return nil, fmt.Errorf("provider %q: failed to parse template %q: %w", name, path, err)
			}

			templates[provider.TemplateFile] = tmpl
		}
	}

	return templates, nil
}

func applyDefaults(cfg *v1alpha1.ValidationConfiguration) {
	if cfg.Spec.MaxConcurrentGroups == 0 {
		cfg.Spec.MaxConcurrentGroups = defaultMaxConcurrentGroups
	}

	for name, t := range cfg.Spec.Tests {
		if t.MinimumNodesPerBatch == 0 {
			t.MinimumNodesPerBatch = defaultMinimumNodesPerBatch
		}

		if len(t.BatchFailurePolicy) == 0 {
			t.BatchFailurePolicy = v1alpha1.BatchFailurePolicyFail
		}

		cfg.Spec.Tests[name] = t
	}

	if cfg.Spec.NewNodeValidation != nil {
		if cfg.Spec.NewNodeValidation.BatchPeriod.Duration == 0 {
			cfg.Spec.NewNodeValidation.BatchPeriod = metav1.Duration{Duration: defaultNewNodeBatchPeriod}
		}

		if len(cfg.Spec.NewNodeValidation.Condition) == 0 {
			cfg.Spec.NewNodeValidation.Condition = defaultNewNodeCondition
		}
	}
}

func validate(cfg *v1alpha1.ValidationConfiguration) error {
	errs := validateTopLevel(cfg)
	errs = append(errs, validateProviders(cfg)...)
	errs = append(errs, validateTests(cfg)...)
	errs = append(errs, validateReadinessCriteria(cfg)...)
	errs = append(errs, validateSchedulingGate(cfg)...)

	if cfg.Spec.NewNodeValidation != nil {
		errs = append(errs, validateNewNodeValidation(cfg)...)
	}

	return utilerrors.NewAggregate(errs)
}

func validateTopLevel(cfg *v1alpha1.ValidationConfiguration) []error {
	var errs []error

	if len(cfg.Spec.Providers) == 0 {
		errs = append(errs, fmt.Errorf("spec.providers: at least one provider must be defined"))
	}

	if len(cfg.Spec.Tests) == 0 {
		errs = append(errs, fmt.Errorf("spec.tests: at least one test must be defined"))
	}

	if len(cfg.Spec.TemplateMountPath) == 0 {
		errs = append(errs, fmt.Errorf("spec.templateMountPath: must not be empty"))
	}

	if cfg.Spec.MaxConcurrentGroups < 1 {
		errs = append(errs, fmt.Errorf("spec.maxConcurrentGroups: must be >= 1"))
	}

	return errs
}

func validateProviders(cfg *v1alpha1.ValidationConfiguration) []error {
	var errs []error

	for name, p := range cfg.Spec.Providers {
		prefix := fmt.Sprintf("spec.providers[%s]", name)
		errs = append(errs, validateProvider(prefix, p)...)
	}

	return errs
}

func validateProvider(prefix string, p v1alpha1.ProviderConfig) []error {
	var errs []error

	if len(p.APIGroup) == 0 {
		errs = append(errs, fmt.Errorf("%s.apiGroup: must not be empty", prefix))
	}

	if len(p.Version) == 0 {
		errs = append(errs, fmt.Errorf("%s.version: must not be empty", prefix))
	}

	if len(p.Kind) == 0 {
		errs = append(errs, fmt.Errorf("%s.kind: must not be empty", prefix))
	}

	if len(p.Resource) == 0 {
		errs = append(errs, fmt.Errorf("%s.resource: must not be empty", prefix))
	}

	if p.Retries < 0 {
		errs = append(errs, fmt.Errorf("%s.retries: must be >= 0", prefix))
	}

	if p.Timeout.Duration <= 0 {
		errs = append(errs, fmt.Errorf("%s.timeout: must be greater than zero", prefix))
	}

	errs = append(errs, validateConditionSpec(prefix+".successfulCondition", p.SuccessfulCondition)...)
	errs = append(errs, validateConditionSpec(prefix+".failedCondition", p.FailedCondition)...)

	if len(p.TemplateFile) == 0 {
		errs = append(errs, fmt.Errorf("%s.templateFile: must not be empty", prefix))
	}

	return errs
}

func validateConditionSpec(prefix string, c v1alpha1.ConditionMatch) []error {
	var errs []error

	if len(c.Type) == 0 {
		errs = append(errs, fmt.Errorf("%s.type: must not be empty", prefix))
	}

	if len(c.Status) == 0 {
		errs = append(errs, fmt.Errorf("%s.status: must not be empty", prefix))
	}

	return errs
}

func validateTests(cfg *v1alpha1.ValidationConfiguration) []error {
	var errs []error

	seenDefaultTests := make(map[string]bool, len(cfg.Spec.DefaultTests))
	for _, name := range cfg.Spec.DefaultTests {
		if _, ok := cfg.Spec.Tests[name]; !ok {
			errs = append(errs, fmt.Errorf("spec.defaultTests: references unknown test %q", name))
		}

		if seenDefaultTests[name] {
			errs = append(errs, fmt.Errorf("spec.defaultTests: contains duplicate test %q", name))
		}

		seenDefaultTests[name] = true
	}

	for name, t := range cfg.Spec.Tests {
		prefix := fmt.Sprintf("spec.tests[%s]", name)

		if len(t.Provider) == 0 {
			errs = append(errs, fmt.Errorf("%s.provider: must not be empty", prefix))
		} else if _, ok := cfg.Spec.Providers[t.Provider]; !ok {
			errs = append(errs, fmt.Errorf("%s.provider: references unknown provider %q", prefix, t.Provider))
		}

		if t.MinimumNodesPerBatch < 1 {
			errs = append(errs, fmt.Errorf("%s.minimumNodesPerBatch: must be >= 1", prefix))
		}

		if t.BatchFailurePolicy != v1alpha1.BatchFailurePolicyFail &&
			t.BatchFailurePolicy != v1alpha1.BatchFailurePolicyIgnore {
			errs = append(errs, fmt.Errorf("%s.batchFailurePolicy: must be %q or %q, got %q",
				prefix, v1alpha1.BatchFailurePolicyFail, v1alpha1.BatchFailurePolicyIgnore, t.BatchFailurePolicy))
		}
	}

	return errs
}

func validateNewNodeValidation(cfg *v1alpha1.ValidationConfiguration) []error {
	var errs []error

	if cfg.Spec.NewNodeValidation.BatchPeriod.Duration < 0 {
		errs = append(errs, fmt.Errorf("spec.newNodeValidation.batchPeriod: must be greater than zero"))
	}

	if len(cfg.Spec.NewNodeValidation.NewNodeTests) == 0 && len(cfg.Spec.DefaultTests) == 0 {
		errs = append(errs, fmt.Errorf("spec.newNodeValidation: newNodeTests and defaultTests are both empty"))
	}

	seenNewNodeTests := make(map[string]bool, len(cfg.Spec.NewNodeValidation.NewNodeTests))
	for _, name := range cfg.Spec.NewNodeValidation.NewNodeTests {
		if _, ok := cfg.Spec.Tests[name]; !ok {
			errs = append(errs, fmt.Errorf("spec.newNodeValidation.newNodeTests: references unknown test %q", name))
		}

		if seenNewNodeTests[name] {
			errs = append(errs, fmt.Errorf("spec.newNodeValidation.newNodeTests: contains duplicate test %q", name))
		}

		seenNewNodeTests[name] = true
	}

	for i, c := range cfg.Spec.NewNodeValidation.Criteria {
		if len(c.Name) == 0 {
			errs = append(errs, fmt.Errorf("spec.newNodeValidation.criteria[%d].name: must not be empty", i))
		}

		if len(c.Expression) == 0 {
			errs = append(errs, fmt.Errorf("spec.newNodeValidation.criteria[%d].expression: must not be empty", i))
		}
	}

	return errs
}

func validateReadinessCriteria(cfg *v1alpha1.ValidationConfiguration) []error {
	var errs []error

	for i, c := range cfg.Spec.ReadinessCriteria {
		if len(c.Name) == 0 {
			errs = append(errs, fmt.Errorf("spec.readinessCriteria[%d].name: must not be empty", i))
		}

		if len(c.Expression) == 0 {
			errs = append(errs, fmt.Errorf("spec.readinessCriteria[%d].expression: must not be empty", i))
		}
	}

	return errs
}

func validateSchedulingGate(cfg *v1alpha1.ValidationConfiguration) []error {
	if cfg.Spec.SchedulingGate == nil {
		return nil
	}

	var errs []error

	for i, t := range cfg.Spec.SchedulingGate.Taints {
		if len(t.Key) == 0 {
			errs = append(errs, fmt.Errorf("spec.schedulingGate.taints[%d].key: must not be empty", i))
		}

		validEffects := []corev1.TaintEffect{
			corev1.TaintEffectNoSchedule,
			corev1.TaintEffectPreferNoSchedule,
			corev1.TaintEffectNoExecute,
			"",
		}
		if !slices.Contains(validEffects, corev1.TaintEffect(t.Effect)) {
			errs = append(errs, fmt.Errorf(
				"spec.schedulingGate.taints[%d].effect: must be NoSchedule, PreferNoSchedule, or NoExecute, got %q",
				i, t.Effect))
		}
	}

	return errs
}
