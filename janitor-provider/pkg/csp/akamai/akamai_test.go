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

package akamai

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/nvidia/nvsentinel/commons/pkg/akamai"
)

const (
	testBootID    = "boot-before"
	testClusterID = 123456
)

// fakeAPI records the operations the client issues, so a test can assert that
// the destructive ones did or did not fire.
type fakeAPI struct {
	instance *akamai.Instance
	getErr   error
	lkeNode  *akamai.LKENode
	findErr  error
	opErr    error

	rebooted []int
	deleted  []int
	recycled []string
}

func (f *fakeAPI) GetInstance(_ context.Context, _ int) (*akamai.Instance, error) {
	return f.instance, f.getErr
}

func (f *fakeAPI) RebootInstance(_ context.Context, id int) error {
	if f.opErr != nil {
		return f.opErr
	}

	f.rebooted = append(f.rebooted, id)

	return nil
}

func (f *fakeAPI) DeleteInstance(_ context.Context, id int) error {
	if f.opErr != nil {
		return f.opErr
	}

	f.deleted = append(f.deleted, id)

	return nil
}

func (f *fakeAPI) FindLKENode(_ context.Context, _, _ int) (*akamai.LKENode, error) {
	return f.lkeNode, f.findErr
}

func (f *fakeAPI) RecycleLKENode(_ context.Context, _ int, nodeID string) error {
	if f.opErr != nil {
		return f.opErr
	}

	f.recycled = append(f.recycled, nodeID)

	return nil
}

func testNode(providerID, bootID string) corev1.Node {
	return corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "lke123456-gpu-abcde"},
		Spec:       corev1.NodeSpec{ProviderID: providerID},
		Status:     corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{BootID: bootID}},
	}
}

func TestParseProviderID(t *testing.T) {
	tests := []struct {
		providerID string
		want       int
		wantErr    bool
	}{
		{"linode://11111111", 11111111, false},
		{"", 0, true},
		{"aws:///us-east-1a/i-0abc", 0, true},
		{"linode://", 0, true},
		{"linode://abc", 0, true},
		{"linode://0", 0, true},
		{"linode://-5", 0, true},
		{"linode://12/34", 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.providerID, func(t *testing.T) {
			got, err := parseProviderID(tt.providerID)
			if tt.wantErr {
				require.Error(t, err)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestSendRebootSignal(t *testing.T) {
	api := &fakeAPI{instance: &akamai.Instance{ID: 42, Status: akamai.InstanceStatusRunning}}

	ref, err := NewClient(api).SendRebootSignal(context.Background(), testNode("linode://42", testBootID), "cr")
	require.NoError(t, err)
	assert.Equal(t, []int{42}, api.rebooted)

	parsed, err := parseRequestRef(string(ref))
	require.NoError(t, err)
	assert.Equal(t, 42, parsed.instanceID)
	assert.Equal(t, testBootID, parsed.preRebootBootID)
}

// TestSendRebootSignalNoAction pins that nothing is rebooted when the node
// cannot be matched to a safe target.
func TestSendRebootSignalNoAction(t *testing.T) {
	tests := []struct {
		name string
		api  *fakeAPI
		node corev1.Node
	}{
		{"no provider ID", &fakeAPI{}, testNode("", testBootID)},
		{"foreign provider ID", &fakeAPI{}, testNode("aws:///z/i-1", testBootID)},
		{"no bootID", &fakeAPI{}, testNode("linode://42", "")},
		{"lookup fails", &fakeAPI{getErr: errors.New("boom")}, testNode("linode://42", testBootID)},
		{
			"instance deleting",
			&fakeAPI{instance: &akamai.Instance{ID: 42, Status: akamai.InstanceStatusDeleting}},
			testNode("linode://42", testBootID),
		},
		{
			"API refuses",
			&fakeAPI{instance: &akamai.Instance{ID: 42, Status: akamai.InstanceStatusRunning}, opErr: errors.New("busy")},
			testNode("linode://42", testBootID),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewClient(tt.api).SendRebootSignal(context.Background(), tt.node, "cr")
			require.Error(t, err)
			assert.Empty(t, tt.api.rebooted)
		})
	}
}

func TestIsNodeReady(t *testing.T) {
	ref := "42|" + testBootID + "|" + time.Now().UTC().Format(time.RFC3339)

	tests := []struct {
		name      string
		instance  *akamai.Instance
		getErr    error
		bootID    string
		wantReady bool
		wantErr   bool
	}{
		{"new boot and running", &akamai.Instance{Status: akamai.InstanceStatusRunning}, nil, "boot-after", true, false},
		{"same boot still running", &akamai.Instance{Status: akamai.InstanceStatusRunning}, nil, testBootID, false, false},
		{"kubelet not reported", &akamai.Instance{Status: akamai.InstanceStatusRunning}, nil, "", false, false},
		{"rebooting", &akamai.Instance{Status: "rebooting"}, nil, "boot-after", false, false},
		{"booting", &akamai.Instance{Status: "booting"}, nil, "boot-after", false, false},
		{"deleting", &akamai.Instance{Status: akamai.InstanceStatusDeleting}, nil, "boot-after", false, true},
		{"API error is not fatal", nil, errors.New("timeout"), "boot-after", false, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := &fakeAPI{instance: tt.instance, getErr: tt.getErr}

			ready, err := NewClient(api).IsNodeReady(context.Background(), testNode("linode://42", tt.bootID), ref)
			assert.Equal(t, tt.wantReady, ready)
			assert.Equal(t, tt.wantErr, err != nil)
		})
	}
}

func TestIsNodeReadyMalformedRef(t *testing.T) {
	refs := []string{
		"", "42", "42|boot", "x|boot|2026-01-01T00:00:00Z", "42||2026-01-01T00:00:00Z", "42|boot|nope",
	}

	for _, ref := range refs {
		_, err := NewClient(&fakeAPI{}).IsNodeReady(context.Background(), testNode("linode://42", "b"), ref)
		require.Error(t, err, ref)
	}
}

// TestSendTerminateSignalLKE pins that a node of an LKE cluster is recycled
// through LKE and never deleted as a bare instance.
func TestSendTerminateSignalLKE(t *testing.T) {
	cluster := testClusterID
	api := &fakeAPI{
		instance: &akamai.Instance{ID: 42, Status: akamai.InstanceStatusRunning, LKEClusterID: &cluster},
		lkeNode:  &akamai.LKENode{ID: "123456-bbb", InstanceID: 42},
	}

	ref, err := NewClient(api).SendTerminateSignal(context.Background(), testNode("linode://42", testBootID))
	require.NoError(t, err)
	assert.Equal(t, "42", string(ref))
	assert.Equal(t, []string{"123456-bbb"}, api.recycled)
	assert.Empty(t, api.deleted)
}

func TestSendTerminateSignalStandalone(t *testing.T) {
	api := &fakeAPI{instance: &akamai.Instance{ID: 42, Status: akamai.InstanceStatusRunning}}

	ref, err := NewClient(api).SendTerminateSignal(context.Background(), testNode("linode://42", testBootID))
	require.NoError(t, err)
	assert.Equal(t, "42", string(ref))
	assert.Equal(t, []int{42}, api.deleted)
	assert.Empty(t, api.recycled)
}

// TestSendTerminateSignalNoAction pins that nothing is removed when the node
// cannot be matched, or when its LKE node cannot be found.
func TestSendTerminateSignalNoAction(t *testing.T) {
	cluster := testClusterID

	tests := []struct {
		name string
		api  *fakeAPI
		node corev1.Node
	}{
		{"no provider ID", &fakeAPI{}, testNode("", testBootID)},
		{"lookup fails", &fakeAPI{getErr: errors.New("boom")}, testNode("linode://42", testBootID)},
		{
			"LKE node not found",
			&fakeAPI{
				instance: &akamai.Instance{ID: 42, LKEClusterID: &cluster},
				findErr:  errors.New("not a node of the cluster"),
			},
			testNode("linode://42", testBootID),
		},
		{
			"recycle refused",
			&fakeAPI{
				instance: &akamai.Instance{ID: 42, LKEClusterID: &cluster},
				lkeNode:  &akamai.LKENode{ID: "n"},
				opErr:    errors.New("denied"),
			},
			testNode("linode://42", testBootID),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewClient(tt.api).SendTerminateSignal(context.Background(), tt.node)
			require.Error(t, err)
			assert.Empty(t, tt.api.deleted)
			assert.Empty(t, tt.api.recycled)
		})
	}
}

func TestNewClientFromEnv(t *testing.T) {
	t.Setenv(APIEndpointEnvVar, "")
	t.Setenv(akamai.APITokenEnvVar, "")

	_, err := NewClientFromEnv(context.Background())
	require.ErrorContains(t, err, akamai.APITokenEnvVar)

	t.Setenv(akamai.APITokenEnvVar, "fake-token")
	t.Setenv(APIEndpointEnvVar, "http://api.linode.com/v4")

	_, err = NewClientFromEnv(context.Background())
	require.ErrorContains(t, err, APIEndpointEnvVar)

	t.Setenv(APIEndpointEnvVar, "")

	client, err := NewClientFromEnv(context.Background())
	require.NoError(t, err)
	assert.NotNil(t, client)
}
