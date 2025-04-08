// Copyright (c) 2024, NVIDIA CORPORATION.  All rights reserved.
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
	"os"
	"path/filepath"
	"testing"
	"text/template"

	"github.com/stretchr/testify/assert"
	platformconnector "gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/platform-connectors/pkg/protos"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
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

type MockResourceInterface struct {
	dynamic.ResourceInterface
	createFunc func(gvr schema.GroupVersionResource, obj *unstructured.Unstructured, opts metav1.CreateOptions) (*unstructured.Unstructured, error)
}

func (m *MockResourceInterface) Create(ctx context.Context, obj *unstructured.Unstructured, opts metav1.CreateOptions, subresources ...string) (*unstructured.Unstructured, error) {
	gvr := schema.GroupVersionResource{
		Group:    "janitor.dgxc.nvidia.com",
		Version:  "v1alpha1",
		Resource: "maintenances",
	}
	return m.createFunc(gvr, obj, opts)
}

func TestNewK8sClient(t *testing.T) {
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
			client, err := NewK8sClient(tt.kubeconfig, tt.dryRun, TemplateData{
				Namespace:         "dgxc-janitor",
				Version:           "v1alpha1",
				ApiGroup:          "janitor.dgxc.nvidia.com",
				TemplateMountPath: "templates",
				TemplateFileName:  "maintenance-template.yaml",
			})
			if tt.wantErr {
				assert.Error(t, err)
				assert.Nil(t, client)
			} else {
				assert.NoError(t, err)
				assert.NotNil(t, client)
				if tt.dryRun {
					assert.Equal(t, []string{metav1.DryRunAll}, client.dryRunMode)
				} else {
					assert.Empty(t, client.dryRunMode)
				}
			}
		})
	}
}

func TestCreateMaintenanceResource(t *testing.T) {
	tests := []struct {
		name          string
		nodeName      string
		dryRun        bool
		shouldSucceed bool
		expectedError bool
	}{
		{
			name:          "Successful maintenance creation",
			nodeName:      "test-node-1",
			dryRun:        false,
			shouldSucceed: true,
			expectedError: false,
		},
		{
			name:          "Successful maintenance creation with dry run",
			nodeName:      "test-node-2",
			dryRun:        true,
			shouldSucceed: true,
			expectedError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create a fake dynamic client
			mockClient := &MockDynamicClient{
				createFunc: func(gvr schema.GroupVersionResource, obj *unstructured.Unstructured, opts metav1.CreateOptions) (*unstructured.Unstructured, error) {
					// Verify the maintenance resource structure
					assert.Equal(t, "janitor.dgxc.nvidia.com", gvr.Group)
					assert.Equal(t, "v1alpha1", gvr.Version)
					assert.Equal(t, "maintenances", gvr.Resource)

					// Verify the object structure
					metadata, found, err := unstructured.NestedMap(obj.Object, "metadata")
					assert.NoError(t, err)
					assert.True(t, found)
					assert.Contains(t, metadata["generateName"], tt.nodeName)
					assert.Equal(t, "dgxc-janitor", metadata["namespace"])
					return obj, nil
				},
			}

			templatePath := filepath.Join("templates", "maintenance-template.yaml")
			templateContent, err := os.ReadFile(templatePath)
			assert.NoError(t, err)

			tmpl, err := template.New("maintenance").Parse(string(templateContent))
			assert.NoError(t, err)

			// Create K8sClient with mock
			client := &FaultRemediationClient{
				clientset:  mockClient,
				dryRunMode: []string{},
				template:   tmpl,
				templateData: TemplateData{
					Namespace: "dgxc-janitor",
					Version:   "v1alpha1",
					ApiGroup:  "janitor.dgxc.nvidia.com",
				},
			}
			if tt.dryRun {
				client.dryRunMode = []string{metav1.DryRunAll}
			}

			// Create a HealthEvent object
			healthEvent := &platformconnector.HealthEvent{
				NodeName: tt.nodeName,
			}

			// Test CreateMaintenanceResource
			result := client.CreateMaintenanceResource(context.Background(), healthEvent)
			assert.Equal(t, tt.shouldSucceed, result)
		})
	}
}
