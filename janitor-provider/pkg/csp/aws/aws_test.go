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

package aws

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type fakeEC2 struct {
	terminateInput *ec2.TerminateInstancesInput
	terminateErr   error
	terminateCalls int
}

func (f *fakeEC2) RebootInstances(
	_ context.Context,
	_ *ec2.RebootInstancesInput,
	_ ...func(*ec2.Options),
) (*ec2.RebootInstancesOutput, error) {
	return &ec2.RebootInstancesOutput{}, nil
}

func (f *fakeEC2) TerminateInstances(
	_ context.Context,
	input *ec2.TerminateInstancesInput,
	_ ...func(*ec2.Options),
) (*ec2.TerminateInstancesOutput, error) {
	f.terminateInput = input
	f.terminateCalls++

	return &ec2.TerminateInstancesOutput{}, f.terminateErr
}

func node(providerID string) corev1.Node {
	return corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "ip-10-0-0-1.us-east-2.compute.internal"},
		Spec:       corev1.NodeSpec{ProviderID: providerID},
	}
}

func TestSendTerminateSignal(t *testing.T) {
	fake := &fakeEC2{}
	c := &Client{ec2: fake}

	ref, err := c.SendTerminateSignal(context.Background(), node("aws:///us-east-2b/i-0123456789abcdef0"))
	require.NoError(t, err)

	assert.Equal(t, "i-0123456789abcdef0", string(ref))
	require.Equal(t, 1, fake.terminateCalls)
	assert.Equal(t, []string{"i-0123456789abcdef0"}, fake.terminateInput.InstanceIds)
}

func TestSendTerminateSignalErrors(t *testing.T) {
	apiErr := errors.New("UnauthorizedOperation")

	tests := []struct {
		name       string
		providerID string
		apiErr     error
		wantCalls  int
		wantErrIs  error
	}{
		{name: "missing provider ID", providerID: ""},
		{name: "malformed provider ID", providerID: "aws:///i-0123456789abcdef0"},
		{name: "empty instance ID segment", providerID: "aws:///us-east-2b/"},
		{name: "EC2 API error", providerID: "aws:///us-east-2b/i-0123456789abcdef0", apiErr: apiErr, wantCalls: 1, wantErrIs: apiErr},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &fakeEC2{terminateErr: tt.apiErr}
			c := &Client{ec2: fake}

			ref, err := c.SendTerminateSignal(context.Background(), node(tt.providerID))
			require.Error(t, err)

			assert.Empty(t, ref)
			assert.Equal(t, tt.wantCalls, fake.terminateCalls)

			if tt.wantErrIs != nil {
				assert.ErrorIs(t, err, tt.wantErrIs)
			}
		})
	}
}
