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
	"context"
	"regexp"

	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/nvidia/nvsentinel/janitor/pkg/csp"
)

// Test utilities for controller testing
// Add test helper functions here as needed

// MockCSPClient is a mock implementation of the CSP client interface for testing
type MockCSPClient struct {
	terminateSignalSent bool
	terminateError      error
}

func (m *MockCSPClient) SendTerminateSignal(
	ctx context.Context,
	node corev1.Node,
) (csp.TerminateNodeRequestRef, error) {
	m.terminateSignalSent = true

	return "", m.terminateError
}

func (m *MockCSPClient) IsNodeReady(
	ctx context.Context,
	node corev1.Node,
	message string,
) (bool, error) {
	return true, nil
}

func (m *MockCSPClient) SendRebootSignal(
	ctx context.Context,
	node corev1.Node,
) (csp.ResetSignalRequestRef, error) {
	return "", nil
}

// nolint:gochecknoglobals,lll,unused // test pattern
var conditionReasonPattern = regexp.MustCompile("^[A-Za-z]([A-Za-z0-9_,:]*[A-Za-z0-9_])?$")

// Validates that the given condition is properly formed. This function validates
// that the reason field matches the regex included by kubebuilder in the given CRD:
// +kubebuilder:validation:Pattern=`^[A-Za-z]([A-Za-z0-9_,:]*[A-Za-z0-9_])?$`
//
//nolint:unused // used by ginkgo tests
func checkStatusConditions(conditions []metav1.Condition) {
	for _, condition := range conditions {
		gomega.Expect(conditionReasonPattern.MatchString(condition.Reason)).To(gomega.BeTrue())
	}
}
