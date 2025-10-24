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

package reconciler

import (
	"context"
	"testing"
	"text/template"

	storeconnector "github.com/nvidia/nvsentinel/platform-connectors/pkg/connectors/store"
	platformconnector "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/stretchr/testify/assert"
	"go.mongodb.org/mongo-driver/bson/primitive"
	metameta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
)

// MockDynamicClient implements necessary methods from dynamic.Interface
type MockDynamicClient struct {
	dynamic.Interface
	createFunc func(gvr schema.GroupVersionResource, obj *unstructured.Unstructured, opts metav1.CreateOptions) (*unstructured.Unstructured, error)
}

func (m *MockDynamicClient) Resource(gvr schema.GroupVersionResource) dynamic.NamespaceableResourceInterface {
	return &MockNamespaceableResource{
		createFunc: m.createFunc,
	}
}

type MockNamespaceableResource struct {
	dynamic.NamespaceableResourceInterface
	createFunc func(gvr schema.GroupVersionResource, obj *unstructured.Unstructured, opts metav1.CreateOptions) (*unstructured.Unstructured, error)
}

func (m *MockNamespaceableResource) Namespace(namespace string) dynamic.ResourceInterface {
	return &MockResourceInterface{
		createFunc: m.createFunc,
	}
}

func (m *MockNamespaceableResource) Create(ctx context.Context, obj *unstructured.Unstructured, opts metav1.CreateOptions, subresources ...string) (*unstructured.Unstructured, error) {
	gvr := schema.GroupVersionResource{
		Group:    "janitor.dgxc.nvidia.com",
		Version:  "v1alpha1",
		Resource: "rebootnodes",
	}
	return m.createFunc(gvr, obj, opts)
}

type MockResourceInterface struct {
	dynamic.ResourceInterface
	createFunc func(gvr schema.GroupVersionResource, obj *unstructured.Unstructured, opts metav1.CreateOptions) (*unstructured.Unstructured, error)
}

func (m *MockResourceInterface) Create(ctx context.Context, obj *unstructured.Unstructured, opts metav1.CreateOptions, subresources ...string) (*unstructured.Unstructured, error) {
	gvr := schema.GroupVersionResource{
		Group:    "janitor.dgxc.nvidia.com",
		Version:  "v1alpha1",
		Resource: "rebootnodes",
	}
	return m.createFunc(gvr, obj, opts)
}

// MockDiscoveryClient implements discovery.DiscoveryInterface
type MockDiscoveryClient struct {
	discovery.DiscoveryInterface
}

func (m *MockDiscoveryClient) ServerResourcesForGroupVersion(groupVersion string) (*metav1.APIResourceList, error) {
	return &metav1.APIResourceList{
		GroupVersion: "janitor.dgxc.nvidia.com/v1alpha1",
		APIResources: []metav1.APIResource{
			{
				Name:         "rebootnodes",
				SingularName: "rebootnode",
				Namespaced:   true,
				Kind:         "RebootNode",
				Verbs:        []string{"create", "delete", "get", "list", "patch", "update", "watch"},
			},
		},
	}, nil
}

func (m *MockDiscoveryClient) ServerGroupsAndResources() ([]*metav1.APIGroup, []*metav1.APIResourceList, error) {
	return []*metav1.APIGroup{
			{
				Name: "janitor.dgxc.nvidia.com",
				Versions: []metav1.GroupVersionForDiscovery{
					{
						GroupVersion: "janitor.dgxc.nvidia.com/v1alpha1",
						Version:      "v1alpha1",
					},
				},
				PreferredVersion: metav1.GroupVersionForDiscovery{
					GroupVersion: "janitor.dgxc.nvidia.com/v1alpha1",
					Version:      "v1alpha1",
				},
			},
		}, []*metav1.APIResourceList{
			{
				GroupVersion: "janitor.dgxc.nvidia.com/v1alpha1",
				APIResources: []metav1.APIResource{
					{
						Name:         "rebootnodes",
						SingularName: "rebootnode",
						Namespaced:   true,
						Kind:         "RebootNode",
						Verbs:        []string{"create", "delete", "get", "list", "patch", "update", "watch"},
					},
				},
			},
		}, nil
}

func (m *MockDiscoveryClient) ServerGroups() (*metav1.APIGroupList, error) {
	return &metav1.APIGroupList{
		Groups: []metav1.APIGroup{
			{
				Name: "janitor.dgxc.nvidia.com",
				Versions: []metav1.GroupVersionForDiscovery{
					{
						GroupVersion: "janitor.dgxc.nvidia.com/v1alpha1",
						Version:      "v1alpha1",
					},
				},
				PreferredVersion: metav1.GroupVersionForDiscovery{
					GroupVersion: "janitor.dgxc.nvidia.com/v1alpha1",
					Version:      "v1alpha1",
				},
			},
		},
	}, nil
}

func (m *MockDiscoveryClient) ServerResources() ([]*metav1.APIResourceList, error) {
	return nil, nil
}

func (m *MockDiscoveryClient) ServerPreferredResources() ([]*metav1.APIResourceList, error) {
	return []*metav1.APIResourceList{
		{
			GroupVersion: "janitor.dgxc.nvidia.com/v1alpha1",
			APIResources: []metav1.APIResource{
				{
					Name:         "rebootnodes",
					SingularName: "rebootnode",
					Namespaced:   true,
					Kind:         "RebootNode",
					Verbs:        []string{"create", "delete", "get", "list", "patch", "update", "watch"},
				},
			},
		},
	}, nil
}

func (m *MockDiscoveryClient) ServerPreferredNamespacedResources() ([]*metav1.APIResourceList, error) {
	return m.ServerPreferredResources()
}

// MockRESTMapper is a simple mock that returns a fixed GVR
type MockRESTMapper struct{}

func (m *MockRESTMapper) RESTMapping(gk schema.GroupKind, versions ...string) (*metameta.RESTMapping, error) {
	return &metameta.RESTMapping{
		Resource: schema.GroupVersionResource{
			Group:    "janitor.dgxc.nvidia.com",
			Version:  "v1alpha1",
			Resource: "rebootnodes",
		},
	}, nil
}

func TestNewK8sClient(t *testing.T) {
	// Skip test if in-cluster config works
	if _, err := rest.InClusterConfig(); err == nil {
		t.Skip("Skipping test as in-cluster config is available")
	}

	tests := []struct {
		name       string
		kubeconfig string
		dryRun     bool
		wantErr    bool
	}{
		{
			name:       "Empty kubeconfig without in-cluster config",
			kubeconfig: "",
			dryRun:     false,
			wantErr:    true,
		},
		{
			name:       "Invalid kubeconfig path",
			kubeconfig: "invalid/path/to/config",
			dryRun:     false,
			wantErr:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client, clientSet, err := NewK8sClient(tt.kubeconfig, tt.dryRun, TemplateData{
				Namespace:         "dgxc-janitor",
				Version:           "v1alpha1",
				ApiGroup:          "janitor.dgxc.nvidia.com",
				TemplateMountPath: "templates",
				TemplateFileName:  "rebootnode-template.yaml",
			})
			if tt.wantErr {
				assert.Error(t, err)
				assert.Nil(t, client)
				assert.Nil(t, clientSet)
			} else {
				assert.NoError(t, err)
				assert.NotNil(t, client)
				assert.NotNil(t, clientSet)
				if tt.dryRun {
					assert.Equal(t, []string{metav1.DryRunAll}, client.dryRunMode)
				} else {
					assert.Empty(t, client.dryRunMode)
				}
			}
		})
	}
}

func TestCreateRebootNodeResource(t *testing.T) {
	tests := []struct {
		name              string
		nodeName          string
		dryRun            bool
		recommendedAction platformconnector.RecommenedAction
		shouldSucceed     bool
		expectedError     bool
		shouldCreate      bool
	}{
		{
			name:              "Successful rebootnode creation",
			nodeName:          "test-node-1",
			dryRun:            false,
			recommendedAction: platformconnector.RecommenedAction_RESTART_BM,
			shouldSucceed:     true,
			expectedError:     false,
			shouldCreate:      true,
		},
		{
			name:              "Skip rebootnode creation with dry run",
			nodeName:          "test-node-2",
			dryRun:            true,
			recommendedAction: platformconnector.RecommenedAction_RESTART_BM,
			shouldSucceed:     true,
			expectedError:     false,
			shouldCreate:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			createCalled := false

			// Create a fake dynamic client
			mockClient := &MockDynamicClient{
				createFunc: func(gvr schema.GroupVersionResource, obj *unstructured.Unstructured, opts metav1.CreateOptions) (*unstructured.Unstructured, error) {
					createCalled = true
					// Verify the rebootnode resource structure
					assert.Equal(t, "janitor.dgxc.nvidia.com", gvr.Group)
					assert.Equal(t, "v1alpha1", gvr.Version)
					assert.Equal(t, "rebootnodes", gvr.Resource)

					// Verify the object structure
					metadata, found, err := unstructured.NestedMap(obj.Object, "metadata")
					assert.NoError(t, err)
					assert.True(t, found)
					assert.Contains(t, metadata["name"], tt.nodeName)
					return obj, nil
				},
			}

			// Create template
			tmpl := template.New("rebootnode")
			tmpl, err := tmpl.Parse(`apiVersion: {{.ApiGroup}}/{{.Version}}
kind: RebootNode
metadata:
  name: maintenance-{{.NodeName}}-{{.HealthEventID}}
spec:
  nodeName: {{.NodeName}}`)
			assert.NoError(t, err)

			// Create K8sClient with mock
			mockDiscovery := &MockDiscoveryClient{}
			cachedClient := memory.NewMemCacheClient(mockDiscovery)
			mockMapper := restmapper.NewDeferredDiscoveryRESTMapper(cachedClient)
			client := &FaultRemediationClient{
				clientset:  mockClient,
				kubeClient: nil, // Not needed for this test since it only tests maintenance resource creation
				restMapper: mockMapper,
				dryRunMode: []string{},
				template:   tmpl,
				templateData: TemplateData{
					Version:  "v1alpha1",
					ApiGroup: "janitor.dgxc.nvidia.com",
				},
			}
			if tt.dryRun {
				client.dryRunMode = []string{metav1.DryRunAll}
			}

			// Create a HealthEventDoc object
			healthEventDoc := &HealthEventDoc{
				ID: primitive.NewObjectID(),
				HealthEventWithStatus: storeconnector.HealthEventWithStatus{
					HealthEvent: &platformconnector.HealthEvent{
						NodeName:          tt.nodeName,
						RecommendedAction: tt.recommendedAction,
					},
				},
			}

			// Test CreateMaintenanceResource
			result, crName := client.CreateMaintenanceResource(context.Background(), healthEventDoc)
			assert.Equal(t, tt.shouldSucceed, result)
			if tt.shouldSucceed && !tt.dryRun {
				assert.NotEmpty(t, crName, "CR name should be returned on success")
			}
			assert.Equal(t, tt.shouldCreate, createCalled, "Create function call expectation mismatch")
		})
	}
}

func TestRunLogCollectorJob(t *testing.T) {
	tests := []struct {
		name           string
		nodeName       string
		expectedResult bool
		description    string
	}{
		{
			name:           "Missing manifest file",
			nodeName:       "test-node-no-manifest",
			expectedResult: false,
			description:    "Should return false when log collector manifest file is missing",
		},
		{
			name:           "Dry run mode",
			nodeName:       "test-node-dry-run",
			expectedResult: false,
			description:    "Should return false in dry run mode since no manifest file exists",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create a fake Kubernetes client
			fakeClient := fake.NewSimpleClientset()

			// Create FaultRemediationClient with fake client
			client := &FaultRemediationClient{
				kubeClient: fakeClient,
			}

			ctx := context.Background()

			// Test RunLogCollectorJob - this will fail because manifest file doesn't exist in test
			result := client.RunLogCollectorJob(ctx, tt.nodeName)

			// Since manifest file doesn't exist in test environment, it should return an error
			if tt.expectedResult {
				assert.NoError(t, result, tt.description)
			} else {
				assert.Error(t, result, tt.description)
			}
		})
	}
}

func TestLogCollectorJobErrorHandling(t *testing.T) {
	tests := []struct {
		name        string
		nodeName    string
		description string
	}{
		{
			name:        "Invalid node name",
			nodeName:    "",
			description: "Should handle empty node name gracefully",
		},
		{
			name:        "Long node name",
			nodeName:    "very-long-node-name-that-exceeds-kubernetes-limits-for-testing-purposes-and-should-be-handled-gracefully",
			description: "Should handle very long node names",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create a fake Kubernetes client
			fakeClient := fake.NewSimpleClientset()

			// Create FaultRemediationClient
			client := &FaultRemediationClient{
				kubeClient: fakeClient,
			}

			ctx := context.Background()

			// Test RunLogCollectorJob with edge cases
			result := client.RunLogCollectorJob(ctx, tt.nodeName)

			// Should return error since manifest file doesn't exist in test environment
			assert.Error(t, result, tt.description)
		})
	}
}

func TestRunLogCollectorJobDryRun(t *testing.T) {
	// Create a fake Kubernetes client
	fakeClient := fake.NewSimpleClientset()

	// Create FaultRemediationClient with dry run mode
	client := &FaultRemediationClient{
		kubeClient: fakeClient,
		dryRunMode: []string{metav1.DryRunAll},
	}

	ctx := context.Background()
	result := client.RunLogCollectorJob(ctx, "test-node-dry-run")

	// In dry run mode, it returns nil (no error) as it skips execution
	// but doesn't actually create the job
	assert.NoError(t, result, "Dry run should return no error as it skips execution")
}
