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

package labelprovider

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/nvidia/nvsentinel/janitor-provider/pkg/model"
)

func newTestClient(objects ...runtime.Object) *Client {
	return NewClientWithK8s(context.Background(), fake.NewSimpleClientset(objects...), Config{})
}

func newNode(name string, labels map[string]string) corev1.Node {
	return corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: labels,
		},
	}
}

func TestSendRebootSignal_SetsDefaultNKELabel(t *testing.T) {
	node := newNode("worker-1", map[string]string{"existing-label": "keep-me"})
	client := newTestClient(&node)
	ctx := context.Background()

	ref, err := client.SendRebootSignal(ctx, node, "")
	require.NoError(t, err)
	assert.Equal(t, model.ResetSignalRequestRef(defaultRebootKey), ref)

	updated, err := client.k8sClient.CoreV1().Nodes().Get(ctx, node.Name, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, defaultLabelValue, updated.Labels[defaultRebootKey])
	assert.Equal(t, "keep-me", updated.Labels["existing-label"])
}

func TestSendRebootSignal_InitializesLabels(t *testing.T) {
	node := newNode("worker-1", nil)
	client := newTestClient(&node)
	ctx := context.Background()

	_, err := client.SendRebootSignal(ctx, node, "")
	require.NoError(t, err)

	updated, err := client.k8sClient.CoreV1().Nodes().Get(ctx, node.Name, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, defaultLabelValue, updated.Labels[defaultRebootKey])
}

func TestSendRebootSignal_UsesConfiguredKey(t *testing.T) {
	node := newNode("worker-1", nil)
	client := NewClientWithK8s(context.Background(), fake.NewSimpleClientset(&node), Config{
		RebootKey:    "example.com/reboot",
		TerminateKey: "example.com/terminate",
		Value:        "requested-by-tests",
	})
	ctx := context.Background()

	ref, err := client.SendRebootSignal(ctx, node, "")
	require.NoError(t, err)
	assert.Equal(t, model.ResetSignalRequestRef("example.com/reboot"), ref)

	updated, err := client.k8sClient.CoreV1().Nodes().Get(ctx, node.Name, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "requested-by-tests", updated.Labels["example.com/reboot"])
	assert.Empty(t, updated.Labels[defaultRebootKey])
}

func TestSendRebootSignal_NodeMissing(t *testing.T) {
	client := newTestClient()
	ctx := context.Background()

	_, err := client.SendRebootSignal(ctx, newNode("missing", nil), "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to set reboot label")
}

func TestSendRebootSignal_UpdateConflictRetries(t *testing.T) {
	node := newNode("worker-1", nil)
	fakeClient := fake.NewSimpleClientset(&node)
	client := NewClientWithK8s(context.Background(), fakeClient, Config{})
	ctx := context.Background()

	var updates atomic.Int32

	fakeClient.PrependReactor("update", "nodes", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if updates.Add(1) == 1 {
			return true, nil, apierrors.NewConflict(
				corev1.Resource("nodes"), "worker-1", errors.New("conflict"))
		}

		return false, nil, nil
	})

	_, err := client.SendRebootSignal(ctx, node, "")
	require.NoError(t, err)
	assert.GreaterOrEqual(t, updates.Load(), int32(2))

	updated, err := client.k8sClient.CoreV1().Nodes().Get(ctx, node.Name, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, defaultLabelValue, updated.Labels[defaultRebootKey])
}

func TestSendRebootSignal_UpdateError(t *testing.T) {
	node := newNode("worker-1", nil)
	fakeClient := fake.NewSimpleClientset(&node)
	client := NewClientWithK8s(context.Background(), fakeClient, Config{})
	ctx := context.Background()

	fakeClient.PrependReactor("update", "nodes", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("update failed")
	})

	_, err := client.SendRebootSignal(ctx, node, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to set reboot label")
}

func TestIsNodeReady_LabelPresent(t *testing.T) {
	node := newNode("worker-1", map[string]string{defaultRebootKey: defaultLabelValue})
	client := newTestClient(&node)

	ready, err := client.IsNodeReady(context.Background(), node, "")
	require.NoError(t, err)
	assert.False(t, ready)
}

func TestIsNodeReady_LabelRemoved(t *testing.T) {
	node := newNode("worker-1", map[string]string{"other": "value"})
	client := newTestClient(&node)

	ready, err := client.IsNodeReady(context.Background(), node, "")
	require.NoError(t, err)
	assert.True(t, ready)
}

func TestIsNodeReady_NilLabels(t *testing.T) {
	node := newNode("worker-1", nil)
	client := newTestClient(&node)

	ready, err := client.IsNodeReady(context.Background(), node, "")
	require.NoError(t, err)
	assert.True(t, ready)
}

func TestIsNodeReady_UsesConfiguredKey(t *testing.T) {
	node := newNode("worker-1", map[string]string{"example.com/reboot": "requested-by-tests"})
	client := NewClientWithK8s(context.Background(), fake.NewSimpleClientset(&node), Config{
		RebootKey: "example.com/reboot",
	})
	ctx := context.Background()

	ready, err := client.IsNodeReady(ctx, node, "")
	require.NoError(t, err)
	assert.False(t, ready)

	updated, err := client.k8sClient.CoreV1().Nodes().Get(ctx, node.Name, metav1.GetOptions{})
	require.NoError(t, err)
	delete(updated.Labels, "example.com/reboot")
	_, err = client.k8sClient.CoreV1().Nodes().Update(ctx, updated, metav1.UpdateOptions{})
	require.NoError(t, err)

	ready, err = client.IsNodeReady(ctx, node, "")
	require.NoError(t, err)
	assert.True(t, ready)
}

func TestIsNodeReady_NodeMissing(t *testing.T) {
	client := newTestClient()

	_, err := client.IsNodeReady(context.Background(), newNode("missing", nil), "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to get node")
}

func TestRebootReadyWhenLabelRemoved(t *testing.T) {
	node := newNode("worker-1", map[string]string{"existing-label": "keep-me"})
	client := newTestClient(&node)
	ctx := context.Background()

	_, err := client.SendRebootSignal(ctx, node, "")
	require.NoError(t, err)

	ready, err := client.IsNodeReady(ctx, node, "")
	require.NoError(t, err)
	assert.False(t, ready)

	updated, err := client.k8sClient.CoreV1().Nodes().Get(ctx, node.Name, metav1.GetOptions{})
	require.NoError(t, err)
	delete(updated.Labels, defaultRebootKey)
	_, err = client.k8sClient.CoreV1().Nodes().Update(ctx, updated, metav1.UpdateOptions{})
	require.NoError(t, err)

	ready, err = client.IsNodeReady(ctx, node, "")
	require.NoError(t, err)
	assert.True(t, ready)
	assert.Equal(t, "keep-me", updated.Labels["existing-label"])
}

func TestSendTerminateSignal_SetsDefaultNKELabel(t *testing.T) {
	node := newNode("worker-1", map[string]string{"existing-label": "keep-me"})
	client := newTestClient(&node)
	ctx := context.Background()

	ref, err := client.SendTerminateSignal(ctx, node)
	require.NoError(t, err)
	assert.Equal(t, model.TerminateNodeRequestRef(defaultTerminateKey), ref)

	updated, err := client.k8sClient.CoreV1().Nodes().Get(ctx, node.Name, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, defaultLabelValue, updated.Labels[defaultTerminateKey])
	assert.Equal(t, "keep-me", updated.Labels["existing-label"])
	assert.Empty(t, updated.Labels[defaultRebootKey])
}

func TestSendTerminateSignal_UsesConfiguredKey(t *testing.T) {
	node := newNode("worker-1", nil)
	client := NewClientWithK8s(context.Background(), fake.NewSimpleClientset(&node), Config{
		TerminateKey: "example.com/terminate",
		Value:        "requested-by-tests",
	})
	ctx := context.Background()

	ref, err := client.SendTerminateSignal(ctx, node)
	require.NoError(t, err)
	assert.Equal(t, model.TerminateNodeRequestRef("example.com/terminate"), ref)

	updated, err := client.k8sClient.CoreV1().Nodes().Get(ctx, node.Name, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "requested-by-tests", updated.Labels["example.com/terminate"])
}

func TestSendTerminateSignal_NodeMissing(t *testing.T) {
	client := newTestClient()

	_, err := client.SendTerminateSignal(context.Background(), newNode("missing", nil))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to set terminate label")
}

func TestLoadConfigFromEnv_Defaults(t *testing.T) {
	t.Setenv(rebootKeyEnv, "")
	t.Setenv(terminateKeyEnv, "")
	t.Setenv(valueEnv, "")

	config := loadConfigFromEnv()
	assert.Equal(t, defaultRebootKey, config.RebootKey)
	assert.Equal(t, defaultTerminateKey, config.TerminateKey)
	assert.Equal(t, defaultLabelValue, config.Value)
}

func TestLoadConfigFromEnv_Overrides(t *testing.T) {
	t.Setenv(rebootKeyEnv, "example.com/reboot")
	t.Setenv(terminateKeyEnv, "example.com/terminate")
	t.Setenv(valueEnv, "requested-by-tests")

	config := loadConfigFromEnv()
	assert.Equal(t, "example.com/reboot", config.RebootKey)
	assert.Equal(t, "example.com/terminate", config.TerminateKey)
	assert.Equal(t, "requested-by-tests", config.Value)
}

func TestWithDefaults_FillsEmptyFields(t *testing.T) {
	config := withDefaults(Config{})
	assert.Equal(t, defaultRebootKey, config.RebootKey)
	assert.Equal(t, defaultTerminateKey, config.TerminateKey)
	assert.Equal(t, defaultLabelValue, config.Value)
}
