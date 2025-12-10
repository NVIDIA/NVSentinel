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

//go:build integration
// +build integration

package customdrain

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/node-drainer/pkg/config"
)

var (
	testEnv       *envtest.Environment
	cfg           *rest.Config
	k8sClient     kubernetes.Interface
	dynamicClient dynamic.Interface
	restMapper    *restmapper.DeferredDiscoveryRESTMapper
)

func setupTestEnvironment(t *testing.T) {
	t.Helper()

	testEnv = &envtest.Environment{
		CRDDirectoryPaths: []string{filepath.Join("testdata", "crds")},
	}

	var err error
	cfg, err = testEnv.Start()
	require.NoError(t, err)
	require.NotNil(t, cfg)

	k8sClient, err = kubernetes.NewForConfig(cfg)
	require.NoError(t, err)

	dynamicClient, err = dynamic.NewForConfig(cfg)
	require.NoError(t, err)

	discoveryClient, err := discovery.NewDiscoveryClientForConfig(cfg)
	require.NoError(t, err)

	cachedClient := memory.NewMemCacheClient(discoveryClient)
	restMapper = restmapper.NewDeferredDiscoveryRESTMapper(cachedClient)

	gvk := schema.GroupVersionKind{
		Group:   "drain.example.com",
		Version: "v1alpha1",
		Kind:    "DrainRequest",
	}
	require.Eventually(t, func() bool {
		_, err := restMapper.RESTMapping(gvk.GroupKind(), gvk.Version)
		return err == nil
	}, 10*time.Second, 500*time.Millisecond, "CRD should be registered within 10 seconds")
}

func teardownTestEnvironment(t *testing.T) {
	t.Helper()
	if testEnv != nil {
		err := testEnv.Stop()
		require.NoError(t, err)
	}
}

func TestCustomDrainClient_Integration(t *testing.T) {
	setupTestEnvironment(t)
	defer teardownTestEnvironment(t)

	t.Run("CRDNotFound", func(t *testing.T) {
		testCRDNotFound(t)
	})

	t.Run("StatusConditions", func(t *testing.T) {
		testStatusConditions(t)
	})
}

func newTestConfig() config.CustomDrainConfig {
	return config.CustomDrainConfig{
		Enabled:               true,
		TemplateMountPath:     "testdata",
		TemplateFileName:      "drain-template.yaml",
		Namespace:             "default",
		ApiGroup:              "drain.example.com",
		Version:               "v1alpha1",
		Kind:                  "DrainRequest",
		StatusConditionType:   "Complete",
		StatusConditionStatus: "True",
		Timeout:               config.Duration{Duration: 30 * time.Minute},
	}
}

func testCRDNotFound(t *testing.T) {
	cfg := newTestConfig()
	cfg.ApiGroup = "nonexistent.example.com"
	cfg.Kind = "FakeResource"

	client, err := NewClient(cfg, dynamicClient, restMapper)
	require.Error(t, err, "NewClient should error when CRD doesn't exist")
	require.Nil(t, client, "client should be nil when CRD validation fails")
	assert.Contains(t, err.Error(), "failed to find rest mapping for custom drain CRD",
		"Error should indicate CRD validation failure")
}

func testStatusConditions(t *testing.T) {
	ctx := context.Background()
	cfg := newTestConfig()

	client, err := NewClient(cfg, dynamicClient, restMapper)
	require.NoError(t, err)

	templateData := TemplateData{
		HealthEvent: &protos.HealthEvent{
			NodeName:  "test-node-3",
			CheckName: "condition-check",
		},
		EventID:     "event-status",
		PodsToDrain: map[string][]string{},
	}

	crName, err := client.CreateDrainCR(ctx, templateData)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = client.DeleteDrainCR(ctx, crName)
	})

	gvr := schema.GroupVersionResource{
		Group:    "drain.example.com",
		Version:  "v1alpha1",
		Resource: "drainrequests",
	}

	isComplete, err := client.GetCRStatus(ctx, crName)
	require.NoError(t, err)
	assert.False(t, isComplete)

	cr, err := dynamicClient.Resource(gvr).Namespace("default").Get(ctx, crName, metav1.GetOptions{})
	require.NoError(t, err)

	cr.Object["status"] = map[string]any{
		"conditions": []map[string]any{
			{
				"type":   "InProgress",
				"status": "True",
			},
		},
	}
	_, err = dynamicClient.Resource(gvr).Namespace("default").UpdateStatus(ctx, cr, metav1.UpdateOptions{})
	require.NoError(t, err)

	isComplete, err = client.GetCRStatus(ctx, crName)
	require.NoError(t, err)
	assert.False(t, isComplete)

	cr, err = dynamicClient.Resource(gvr).Namespace("default").Get(ctx, crName, metav1.GetOptions{})
	require.NoError(t, err)

	cr.Object["status"] = map[string]any{
		"conditions": []map[string]any{
			{
				"type":   "Complete",
				"status": "False",
			},
		},
	}
	_, err = dynamicClient.Resource(gvr).Namespace("default").UpdateStatus(ctx, cr, metav1.UpdateOptions{})
	require.NoError(t, err)

	isComplete, err = client.GetCRStatus(ctx, crName)
	require.NoError(t, err)
	assert.False(t, isComplete)

	cr, err = dynamicClient.Resource(gvr).Namespace("default").Get(ctx, crName, metav1.GetOptions{})
	require.NoError(t, err)

	cr.Object["status"] = map[string]any{
		"conditions": []map[string]any{
			{
				"type":   "Complete",
				"status": "true",
			},
		},
	}
	_, err = dynamicClient.Resource(gvr).Namespace("default").UpdateStatus(ctx, cr, metav1.UpdateOptions{})
	require.NoError(t, err)

	isComplete, err = client.GetCRStatus(ctx, crName)
	require.NoError(t, err)
	assert.True(t, isComplete, "Status check should be case-insensitive")

}
