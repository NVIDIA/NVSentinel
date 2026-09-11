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
	"net/http"
	"testing"

	"github.com/oracle/oci-go-sdk/v65/core"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
)

type fakeCompute struct {
	actionError   error
	actionRequest core.InstanceActionRequest
	calls         int
}

func (f *fakeCompute) InstanceAction(
	_ context.Context,
	request core.InstanceActionRequest,
) (core.InstanceActionResponse, error) {
	f.actionRequest = request
	f.calls++

	return core.InstanceActionResponse{}, f.actionError
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

// TestSendRebootSignal_ComputeSucceeds_UsesStableTokenWithoutSDKRetry verifies
// that controller retries stay idempotent without enabling OCI SDK retries.
func TestSendRebootSignal_ComputeSucceeds_UsesStableTokenWithoutSDKRetry(t *testing.T) {
	compute := &fakeCompute{}
	client := &Client{compute: compute}

	_, err := client.SendRebootSignal(context.Background(), testNode(), "rebootnode-test")
	require.NoError(t, err)
	require.NotNil(t, compute.actionRequest.OpcRetryToken)
	token := *compute.actionRequest.OpcRetryToken

	_, err = client.SendRebootSignal(context.Background(), testNode(), "rebootnode-test")
	require.NoError(t, err)
	assert.Equal(t, 2, compute.calls)
	assert.Equal(t, token, *compute.actionRequest.OpcRetryToken)
	assert.Nil(t, compute.actionRequest.RequestMetadata.RetryPolicy)
}

// TestSendRebootSignal_ComputeFails_ReturnsExpectedStatus verifies the custom
// conflict, OCI default retry, and permanent error paths.
func TestSendRebootSignal_ComputeFails_ReturnsExpectedStatus(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		wantCode codes.Code
	}{
		{
			name:     "instance currently being modified",
			err:      instanceBusyError(),
			wantCode: codes.Unavailable,
		},
		{
			name: "rate limited",
			err: fakeServiceError{
				status: http.StatusTooManyRequests,
				code:   "TooManyRequests",
			},
			wantCode: codes.Unavailable,
		},
		{
			name: "unrelated conflict",
			err: fakeServiceError{
				status:  http.StatusConflict,
				code:    "Conflict",
				message: "instance is already stopped",
			},
			wantCode: codes.Unknown,
		},
		{
			name:     "permission denied",
			err:      errors.New("permission denied"),
			wantCode: codes.Unknown,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			compute := &fakeCompute{actionError: test.err}
			client := &Client{compute: compute}

			_, err := client.SendRebootSignal(context.Background(), testNode(), "rebootnode-test")

			require.Error(t, err)
			assert.Equal(t, test.wantCode, status.Code(err))
			assert.Equal(t, 1, compute.calls)
		})
	}
}
