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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	sigsyaml "sigs.k8s.io/yaml"

	"github.com/nvidia/nvsentinel/lifecycle-manager/api/v1alpha1"
	"github.com/nvidia/nvsentinel/lifecycle-manager/pkg/config"
)

func TestGroupNameAndAttemptObjectName(t *testing.T) {
	testCases := []struct {
		name string
		// If set, groupName is passed directly to attemptObjectName, and we bypass calling the groupName function
		groupName          string
		tests              []string
		index              int
		vrName             string
		attemptNumber      int
		expectedObjectName string
	}{
		{
			name:               "no truncation or hashing",
			tests:              []string{"dcgm-level4"},
			index:              1,
			vrName:             "vr-abc123",
			attemptNumber:      1,
			expectedObjectName: "vr-abc123-dcgm-level4-group-1-1",
		},
		{
			name:               "vrName-groupName exact fit",
			tests:              []string{"basic"},
			index:              1,
			vrName:             strings.Repeat("v", 47),
			attemptNumber:      1,
			expectedObjectName: strings.Repeat("v", 47) + "-basic-group-1-1",
		},
		{
			name:               "long vrName hash-truncated",
			tests:              []string{"basic"},
			index:              1,
			vrName:             strings.Repeat("v", 250),
			attemptNumber:      5,
			expectedObjectName: strings.Repeat("v", 52) + "-28a1549a-5",
		},
		{
			name:               "long test names hash-truncated",
			tests:              []string{strings.Repeat("x", 60), strings.Repeat("y", 60), strings.Repeat("z", 60)},
			index:              3,
			vrName:             strings.Repeat("v", 80),
			attemptNumber:      2,
			expectedObjectName: strings.Repeat("v", 52) + "-56a54bf0-2",
		},
		{
			name:               "long test names hash-truncated, short vrName preserved",
			tests:              []string{strings.Repeat("a", 60)},
			index:              1,
			vrName:             "vr",
			attemptNumber:      1,
			expectedObjectName: "vr-" + strings.Repeat("a", 49) + "-46f4f3e0-1",
		},
		{
			name:               "groupName has no length cap of its own, so a long one is still hashed by attemptObjectName",
			groupName:          strings.Repeat("a", 62),
			vrName:             "vr",
			attemptNumber:      1,
			expectedObjectName: "vr-" + strings.Repeat("a", 49) + "-8d56861e-1",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			grp := tc.groupName
			if len(grp) == 0 {
				grp = groupName(tc.tests, tc.index)
			}

			objectName := attemptObjectName(tc.vrName, grp, tc.attemptNumber)

			if len(objectName) > validation.DNS1123LabelMaxLength {
				t.Fatalf("object name %q exceeds %d characters", objectName, validation.DNS1123LabelMaxLength)
			}

			if errs := validation.IsDNS1123Label(objectName); len(errs) != 0 {
				t.Fatalf("object name %q is not a valid DNS-1123 label: %v", objectName, errs)
			}

			if len(tc.expectedObjectName) != 0 && objectName != tc.expectedObjectName {
				t.Fatalf("got object name %q, want %q", objectName, tc.expectedObjectName)
			}
		})
	}
}

var chartValuesPath = filepath.Join("..", "..", "..",
	"distros", "kubernetes", "nvsentinel", "charts", "lifecycle-manager", "values.yaml")

// loadChartConfig loads the lifecycle-manager chart's default config through config.LoadConfig, the same
// path the controller uses at startup. The chart's config keys map one-to-one onto ValidationConfigurationSpec
// (see templates/configmap.yaml), and its templates are written to a temporary templateMountPath.
func loadChartConfig() *config.Config {
	data, err := os.ReadFile(chartValuesPath)
	Expect(err).NotTo(HaveOccurred())

	var values struct {
		Config struct {
			v1alpha1.ValidationConfigurationSpec `json:",inline"`
			Templates                            map[string]string `json:"templates"`
		} `json:"config"`
	}
	Expect(sigsyaml.Unmarshal(data, &values)).To(Succeed())

	templateDir := GinkgoT().TempDir()
	for name, content := range values.Config.Templates {
		Expect(os.WriteFile(filepath.Join(templateDir, name), []byte(content), 0o600)).To(Succeed())
	}

	spec := values.Config.ValidationConfigurationSpec
	spec.TemplateMountPath = templateDir

	validationConfig, err := sigsyaml.Marshal(&v1alpha1.ValidationConfiguration{
		TypeMeta:   metav1.TypeMeta{APIVersion: v1alpha1.GroupVersion.String(), Kind: "ValidationConfiguration"},
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
		Spec:       spec,
	})
	Expect(err).NotTo(HaveOccurred())

	configPath := filepath.Join(templateDir, "config.yaml")
	Expect(os.WriteFile(configPath, validationConfig, 0o600)).To(Succeed())

	cfg, err := config.LoadConfig(configPath)
	Expect(err).NotTo(HaveOccurred())

	return cfg
}

func toleratesTaint(tolerations []corev1.Toleration, taint corev1.Taint) bool {
	for i := range tolerations {
		if tolerations[i].ToleratesTaint(logr.Discard(), &taint, false) {
			return true
		}
	}

	return false
}

var _ = Describe("chart k8s-job-template.yaml", func() {
	It("renders a Job that the API server accepts and that tolerates every NoExecute taint", func(ctx SpecContext) {
		cfg := loadChartConfig()
		r, err := NewValidationRequestReconciler(k8sClient, k8sClient, scheme.Scheme, cfg, testNamespace)
		Expect(err).NotTo(HaveOccurred())

		vr := &v1alpha1.ValidationRequest{ObjectMeta: metav1.ObjectMeta{Name: "vr-chart-template"}}
		group := &v1alpha1.TestGroupStatus{
			Name:     "dcgm-diag-test-1",
			Provider: "k8s-job-provider",
			Tests:    []string{"dcgm-diag-test"},
			Nodes:    []string{"node-chart-template"},
		}

		u, err := r.renderTestGroupObject(vr, group, "vr-chart-template-dcgm-diag-test-1")
		Expect(err).NotTo(HaveOccurred())

		job := &batchv1.Job{}
		Expect(runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, job)).To(Succeed())

		// A dry-run create runs API server validation, which rejects an invalid toleration such as an empty key
		// with an operator other than Exists.
		Expect(k8sClient.Create(ctx, job, client.DryRunAll)).To(Succeed())

		tolerations := job.Spec.Template.Spec.Tolerations

		By("tolerating NoExecute taints that are not configured anywhere")
		for _, taint := range []corev1.Taint{
			{Key: "example.com/bringup", Value: "in-progress", Effect: corev1.TaintEffectNoExecute},
			{Key: "example.com/maintenance", Effect: corev1.TaintEffectNoExecute},
		} {
			Expect(toleratesTaint(tolerations, taint)).To(BeTrue(), "taint %s=%s:%s", taint.Key, taint.Value, taint.Effect)
		}

		By("still tolerating the cordon taint")
		Expect(toleratesTaint(tolerations, corev1.Taint{
			Key: corev1.TaintNodeUnschedulable, Effect: corev1.TaintEffectNoSchedule,
		})).To(BeTrue())

		By("not widening the wildcard to other effects")
		Expect(toleratesTaint(tolerations, corev1.Taint{
			Key: "example.com/bringup", Value: "in-progress", Effect: corev1.TaintEffectNoSchedule,
		})).To(BeFalse())
	})
})
