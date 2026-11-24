// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

package main

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	cspv1alpha1 "github.com/nvidia/nvsentinel/api/gen/go/csp/v1alpha1"
	"github.com/nvidia/nvsentinel/janitor-provider/pkg/csp/generic"
)

// TestTranslateCSPError_VariousErrors_ReturnsExpectedStatus verifies that CSP
// errors retain known gRPC codes and plain errors become Internal errors.
func TestTranslateCSPError_VariousErrors_ReturnsExpectedStatus(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		wantCode codes.Code
		wantText string
	}{
		{
			name:     "plain provider error",
			err:      errors.New("permission denied"),
			wantCode: codes.Internal,
			wantText: "permission denied",
		},
		{
			name:     "retryable provider error",
			err:      status.Error(codes.Unavailable, "instance busy"),
			wantCode: codes.Unavailable,
			wantText: "instance busy",
		},
		{
			name:     "provider deadline",
			err:      status.Error(codes.DeadlineExceeded, "request timed out"),
			wantCode: codes.DeadlineExceeded,
			wantText: "request timed out",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := translateCSPError(test.err, "perform CSP operation")

			require.Error(t, err)
			assert.Equal(t, test.wantCode, status.Code(err))
			assert.ErrorContains(t, err, "perform CSP operation")
			assert.ErrorContains(t, err, test.wantText)
		})
	}
}

// TestTranslateCSPError_NilError_ReturnsNil verifies that the translator does
// not create an error when the provider returns nil.
func TestTranslateCSPError_NilError_ReturnsNil(t *testing.T) {
	require.NoError(t, translateCSPError(nil, "perform CSP operation"))
}

// TestSendRebootSignal_ForwardsDeadlineToGenericProvider verifies that an
// expired RPC request cannot create a Job through the real provider.
func TestSendRebootSignal_ForwardsDeadlineToGenericProvider(t *testing.T) {
	const namespace = "test-ns"
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "worker-1"},
		Status:     corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{BootID: "fixture-boot-id"}},
	}
	client := fake.NewSimpleClientset(node)
	provider := &janitorProviderServer{
		k8sClient: client,
		cspClient: generic.NewClientWithK8s(client, generic.Config{RebootJobNamespace: namespace}),
	}
	_, err := provider.SendRebootSignal(t.Context(), &cspv1alpha1.SendRebootSignalRequest{
		NodeName:          node.Name,
		ExecutionDeadline: timestamppb.New(time.Now().Add(-time.Minute)),
	})
	require.ErrorContains(t, err, "reboot deadline has expired")
	jobs, err := client.BatchV1().Jobs(namespace).List(t.Context(), metav1.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, jobs.Items)
}

func TestSendRebootSignal_RejectsMalformedDeadline(t *testing.T) {
	client := fake.NewSimpleClientset()
	provider := &janitorProviderServer{k8sClient: client}

	_, err := provider.SendRebootSignal(t.Context(), &cspv1alpha1.SendRebootSignalRequest{
		NodeName:          "worker-1",
		ExecutionDeadline: &timestamppb.Timestamp{Nanos: -1},
	})

	assert.Equal(t, codes.InvalidArgument, status.Code(err))
	assert.Empty(t, client.Actions())
}
