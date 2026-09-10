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

package oci

import (
	"context"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/oracle/oci-go-sdk/v65/common"
	"github.com/oracle/oci-go-sdk/v65/core"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
)

type fakeCompute struct {
	actionErrors   []error
	actionRequests []core.InstanceActionRequest
}

func (f *fakeCompute) InstanceAction(
	_ context.Context,
	request core.InstanceActionRequest,
) (core.InstanceActionResponse, error) {
	index := len(f.actionRequests)
	f.actionRequests = append(f.actionRequests, request)
	if index < len(f.actionErrors) {
		return core.InstanceActionResponse{}, f.actionErrors[index]
	}

	return core.InstanceActionResponse{}, nil
}

type fakeServiceError struct {
	status  int
	code    string
	message string
}

func (e fakeServiceError) Error() string           { return e.message }
func (e fakeServiceError) GetHTTPStatusCode() int  { return e.status }
func (e fakeServiceError) GetCode() string         { return e.code }
func (e fakeServiceError) GetMessage() string      { return e.message }
func (e fakeServiceError) GetOpcRequestID() string { return "request-id" }

func instanceBusyError() error {
	return fakeServiceError{
		status:  http.StatusConflict,
		code:    "Conflict",
		message: "instance ocid1.instance.test is currently being modified, try again later",
	}
}

func testNode() corev1.Node {
	return corev1.Node{Spec: corev1.NodeSpec{ProviderID: "ocid1.instance.test"}}
}

func testClient(compute Compute) *Client {
	return &Client{compute: compute}
}

// TestSendRebootSignal_ComputeSucceeds_ReturnsTimestampAndRetryPolicy verifies
// that a successful reboot request returns a timestamp and configures retries.
func TestSendRebootSignal_ComputeSucceeds_ReturnsTimestampAndRetryPolicy(t *testing.T) {
	compute := &fakeCompute{}
	ref, err := testClient(compute).SendRebootSignal(context.Background(), testNode(), "")

	require.NoError(t, err)
	_, err = time.Parse(time.RFC3339, string(ref))
	require.NoError(t, err)
	require.Len(t, compute.actionRequests, 1)

	retryPolicy := compute.actionRequests[0].RequestMetadata.RetryPolicy
	require.NotNil(t, retryPolicy)
	assert.Equal(t, uint(rebootRetryAttempts), retryPolicy.MaximumNumberAttempts)
}

// TestSendRebootSignal_ComputeFails_ReturnsError verifies that a failed OCI
// request returns its error to the caller.
func TestSendRebootSignal_ComputeFails_ReturnsError(t *testing.T) {
	compute := &fakeCompute{actionErrors: []error{errors.New("permission denied")}}
	_, err := testClient(compute).SendRebootSignal(context.Background(), testNode(), "")

	require.ErrorContains(t, err, "permission denied")
	assert.Len(t, compute.actionRequests, 1)
}

// TestIsRetryableRebootError_VariousErrors_ReturnsExpectedClassification
// verifies the OCI defaults and the additional instance-modification conflict.
func TestIsRetryableRebootError_VariousErrors_ReturnsExpectedClassification(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		retryable bool
	}{
		{
			name:      "instance currently being modified",
			err:       instanceBusyError(),
			retryable: true,
		},
		{
			name: "OCI incorrect state",
			err: fakeServiceError{
				status: http.StatusConflict,
				code:   "IncorrectState",
			},
			retryable: true,
		},
		{
			name: "OCI lock conflict",
			err: fakeServiceError{
				status: http.StatusConflict,
				code:   "LockConflict",
			},
			retryable: true,
		},
		{
			name: "rate limited",
			err: fakeServiceError{
				status: http.StatusTooManyRequests,
				code:   "TooManyRequests",
			},
			retryable: true,
		},
		{
			name: "internal server error",
			err: fakeServiceError{
				status: http.StatusInternalServerError,
				code:   "InternalError",
			},
			retryable: true,
		},
		{
			name: "service unavailable",
			err: fakeServiceError{
				status: http.StatusServiceUnavailable,
				code:   "ServiceUnavailable",
			},
			retryable: true,
		},
		{
			name:      "connection closed",
			err:       io.EOF,
			retryable: true,
		},
		{
			name: "method not implemented",
			err: fakeServiceError{
				status: http.StatusNotImplemented,
				code:   "MethodNotImplemented",
			},
			retryable: false,
		},
		{
			name: "unrelated conflict",
			err: fakeServiceError{
				status:  http.StatusConflict,
				code:    "Conflict",
				message: "instance is already stopped",
			},
			retryable: false,
		},
		{
			name: "bad request",
			err: fakeServiceError{
				status: http.StatusBadRequest,
				code:   "InvalidParameter",
			},
			retryable: false,
		},
		{
			name:      "non-OCI error",
			err:       errors.New("permission denied"),
			retryable: false,
		},
		{
			name:      "nil error",
			err:       nil,
			retryable: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.retryable, isRetryableRebootError(test.err))
		})
	}
}

// TestRebootRetryPolicy_RetryableAndPermanentErrors_ReturnsExpectedDecision
// verifies that the request policy delegates to the reboot error classifier.
func TestRebootRetryPolicy_RetryableAndPermanentErrors_ReturnsExpectedDecision(t *testing.T) {
	policy := rebootRetryPolicy()

	assert.True(t, policy.ShouldRetryOperation(common.OCIOperationResponse{
		Error: instanceBusyError(),
	}))
	assert.False(t, policy.ShouldRetryOperation(common.OCIOperationResponse{
		Error: errors.New("permission denied"),
	}))
}
