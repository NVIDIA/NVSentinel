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

package remediation

import (
	"context"
	"github.com/google/uuid"
	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/fault-remediation/pkg/common"
	"github.com/nvidia/nvsentinel/fault-remediation/pkg/config"
	"github.com/nvidia/nvsentinel/fault-remediation/pkg/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"os"
	"path/filepath"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"testing"
	"text/template"
	"time"
)

func TestNewRemediationClient(t *testing.T) {
	tests := []struct {
		name        string
		client      client.Client
		dryRun      bool
		wantErr     bool
		templateDir string
	}{
		{
			name:        "file does not exist",
			templateDir: "does-not-exist",
			dryRun:      false,
			wantErr:     true,
		},
		{
			name:        "file does exist",
			templateDir: "templates",
			dryRun:      false,
			wantErr:     false,
		},
		{
			name:        "file does exist & dry-run",
			templateDir: "templates",
			dryRun:      true,
			wantErr:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			testConfig := config.TomlConfig{
				Template: config.Template{
					MountPath: tt.templateDir,
				},
				RemediationActions: map[string]config.MaintenanceResource{
					protos.RecommendedAction_RESTART_BM.String(): {
						Namespace:             "dgxc-janitor",
						Version:               "v1alpha1",
						ApiGroup:              "janitor.dgxc.nvidia.com",
						Kind:                  "RebootNode",
						CompleteConditionType: "NodeReady",
						TemplateFileName:      "rebootnode-template.yaml",
					},
					protos.RecommendedAction_COMPONENT_RESET.String(): {
						Namespace:             "dgxc-janitor",
						Version:               "v1alpha1",
						ApiGroup:              "janitor.dgxc.nvidia.com",
						Kind:                  "RebootNode",
						CompleteConditionType: "NodeReady",
						TemplateFileName:      "rebootnode-template.yaml",
					},
				},
			}
			result, err := NewRemediationClient(tt.client, tt.dryRun, testConfig)
			if tt.wantErr {
				assert.Error(t, err)
				assert.Nil(t, result)
			} else {
				assert.NoError(t, err)
				assert.NotNil(t, result)
				if tt.dryRun {
					assert.Equal(t, []string{metav1.DryRunAll}, result.dryRunMode)
				} else {
					assert.Empty(t, result.dryRunMode)
				}
			}
		})
	}
}

func TestNewRemediationClient_MissingTemplateFile_E2E(t *testing.T) {
	// Create a temporary directory for test files

	tempDir := t.TempDir()

	t.Run("MissingTemplateFileName", func(t *testing.T) {
		// Create the template file for RESTART_BM so we can test COMPONENT_RESET missing template
		templateContent := `apiVersion: janitor.dgxc.nvidia.com/v1alpha1
kind: RebootNode
metadata:
  name: test-{{ .HealthEvent.NodeName }}-{{ .HealthEventID }}
spec:
  nodeName: {{ .HealthEvent.NodeName }}
  force: false`

		require.NoError(t, os.WriteFile(filepath.Join(tempDir, "rebootnode-template.yaml"), []byte(templateContent), 0644))

		testConfig := config.TomlConfig{
			Template: config.Template{
				MountPath: tempDir,
			},
			RemediationActions: map[string]config.MaintenanceResource{
				protos.RecommendedAction_RESTART_BM.String(): {
					Namespace:             "dgxc-janitor",
					Version:               "v1alpha1",
					ApiGroup:              "janitor.dgxc.nvidia.com",
					Kind:                  "RebootNode",
					CompleteConditionType: "NodeReady",
					TemplateFileName:      "rebootnode-template.yaml",
				},
				protos.RecommendedAction_COMPONENT_RESET.String(): {
					Namespace:             "dgxc-janitor",
					Version:               "v1alpha1",
					ApiGroup:              "janitor.dgxc.nvidia.com",
					Kind:                  "RebootNode",
					CompleteConditionType: "NodeReady",
					// Missing TemplateFileName - this should cause initialization to fail
				},
			},
		}
		resultClient, err := NewRemediationClient(nil, false, testConfig)
		// Should fail with specific error about missing template file configuration
		assert.Error(t, err)
		assert.Nil(t, resultClient)
		assert.Contains(t, err.Error(), "is missing template file configuration")
		assert.Contains(t, err.Error(), protos.RecommendedAction_COMPONENT_RESET.String())
	})

	t.Run("EmptyTemplateFileName", func(t *testing.T) {
		// Create the template file for RESTART_BM so we can test COMPONENT_RESET empty template

		templateContent := `apiVersion: janitor.dgxc.nvidia.com/v1alpha1
kind: RebootNode
metadata:
  name: test-{{ .NodeName }}-{{ .HealthEventID }}
spec:
  nodeName: {{ .NodeName }}
  force: false`

		assert.NoError(t, os.WriteFile(filepath.Join(tempDir, "rebootnode-template.yaml"), []byte(templateContent), 0644))

		testConfig := config.TomlConfig{
			Template: config.Template{
				MountPath: tempDir,
			},

			RemediationActions: map[string]config.MaintenanceResource{
				protos.RecommendedAction_RESTART_BM.String(): {
					Namespace:             "dgxc-janitor",
					Version:               "v1alpha1",
					ApiGroup:              "janitor.dgxc.nvidia.com",
					Kind:                  "RebootNode",
					CompleteConditionType: "NodeReady",
					TemplateFileName:      "rebootnode-template.yaml",
				},
				protos.RecommendedAction_COMPONENT_RESET.String(): {
					Namespace:             "dgxc-janitor",
					Version:               "v1alpha1",
					ApiGroup:              "janitor.dgxc.nvidia.com",
					Kind:                  "RebootNode",
					CompleteConditionType: "NodeReady",
					TemplateFileName:      "", // Empty TemplateFileName - should cause failure
				},
			},
		}

		resultClient, err := NewRemediationClient(nil, false, testConfig)

		// Should fail with specific error about missing template file configuration
		assert.Error(t, err)
		assert.Nil(t, resultClient)
		assert.Contains(t, err.Error(), "is missing template file configuration")
		assert.Contains(t, err.Error(), protos.RecommendedAction_COMPONENT_RESET.String())
	})
}

func TestCreateRebootNodeResource(t *testing.T) {
	tests := []struct {
		name              string
		nodeName          string
		dryRun            bool
		recommendedAction protos.RecommendedAction
		expectedError     bool
		existingObjects   []client.Object
	}{
		{
			name:              "Node does not exist, returns error",
			nodeName:          "test-node-1",
			dryRun:            false,
			recommendedAction: protos.RecommendedAction_RESTART_BM,
			expectedError:     true,
		},
		{
			name:              "Successful rebootnode creation",
			nodeName:          "test-node-1",
			dryRun:            false,
			recommendedAction: protos.RecommendedAction_RESTART_BM,
			expectedError:     false,
			existingObjects: []client.Object{
				&corev1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: "test-node-1",
					},
				},
			},
		},
		{
			name:              "dry run",
			nodeName:          "test-node-1",
			dryRun:            true,
			recommendedAction: protos.RecommendedAction_RESTART_BM,
			expectedError:     false,
			existingObjects: []client.Object{
				&corev1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: "test-node-1",
					},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fakeClient := fake.NewClientBuilder().
				WithObjects(tt.existingObjects...).
				Build()

			// Create Template
			tmpl := template.New("rebootnode")
			tmpl, err := tmpl.Parse(`apiVersion: {{.ApiGroup}}/{{.Version}}
kind: RebootNode
metadata:
  name: maintenance-{{.NodeName}}-{{.HealthEventID}}
spec:
  nodeName: {{.NodeName}}`)
			assert.NoError(t, err)

			remediationConfig := config.TomlConfig{
				RemediationActions: map[string]config.MaintenanceResource{
					protos.RecommendedAction_RESTART_BM.String(): {
						Version:          "v1alpha1",
						ApiGroup:         "janitor.dgxc.nvidia.com",
						Kind:             "RebootNode",
						TemplateFileName: "test.yaml",
					},
					protos.RecommendedAction_COMPONENT_RESET.String(): {
						Version:          "v1alpha1",
						ApiGroup:         "janitor.dgxc.nvidia.com",
						Kind:             "RebootNode",
						TemplateFileName: "gpu-reset.yaml",
					},
				},
			}
			// Create templates map
			templates := map[string]*template.Template{
				protos.RecommendedAction_RESTART_BM.String():      tmpl,
				protos.RecommendedAction_COMPONENT_RESET.String(): tmpl,
			}

			remediationClient := &FaultRemediationClient{
				client:            fakeClient,
				dryRunMode:        []string{},
				remediationConfig: remediationConfig,
				templates:         templates,
			}
			if tt.dryRun {
				remediationClient.dryRunMode = []string{metav1.DryRunAll}
			}

			// Create a HealthEventData object
			healthEventDoc := &events.HealthEventData{
				ID: uuid.New().String(),
				HealthEventWithStatus: model.HealthEventWithStatus{
					HealthEvent: &protos.HealthEvent{
						NodeName:          tt.nodeName,
						RecommendedAction: tt.recommendedAction,
					},
				},
			}
			groupConfig, err := common.GetGroupConfigForEvent(remediationConfig.RemediationActions,
				healthEventDoc.HealthEvent)
			assert.NoError(t, err)

			// Test CreateMaintenanceResource
			crName, err := remediationClient.CreateMaintenanceResource(context.Background(), healthEventDoc, groupConfig)
			if tt.expectedError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}

			if !tt.expectedError && !tt.dryRun {
				assert.NotEmpty(t, crName, "CR name should be returned on success")

				// validate object was created
				gvk := schema.GroupVersionKind{
					Group:   "janitor.dgxc.nvidia.com",
					Version: "v1alpha1",
					Kind:    "RebootNode",
				}

				createdObject := &unstructured.Unstructured{}
				createdObject.SetGroupVersionKind(gvk)
				err = fakeClient.Get(context.Background(), types.NamespacedName{
					Name: crName,
				}, createdObject)

				assert.NoError(t, err)
			}
			if tt.dryRun {
				// validate object was created
				gvk := schema.GroupVersionKind{
					Group:   "janitor.dgxc.nvidia.com",
					Version: "v1alpha1",
					Kind:    "RebootNode",
				}

				createdObject := &unstructured.Unstructured{}
				createdObject.SetGroupVersionKind(gvk)
				err = fakeClient.Get(context.Background(), types.NamespacedName{
					Name: crName,
				}, createdObject)
				assert.Error(t, err)
				assert.True(t, apierrors.IsNotFound(err))
			}
		})
	}
}

func TestRunLogCollectorJob(t *testing.T) {
	eventId := "12345"
	jobNamespace := "test"
	nodeName := "test-node-1"
	labels := map[string]string{
		logCollectorNodeLabel:  nodeName,
		logCollectorEventLabel: eventId,
	}

	tests := []struct {
		name             string
		templateDir      string
		dryRun           bool
		expectedError    bool
		expectedJobCount int
		requeueTime      time.Duration
		existingObjects  []client.Object
	}{
		{
			name:          "No template file, returns error",
			dryRun:        false,
			expectedError: true,
		},
		{
			name:             "Successful job creation",
			dryRun:           false,
			expectedError:    false,
			expectedJobCount: 1,
			templateDir:      "templates",
			requeueTime:      10 * time.Second,
		},
		{
			name:          "Skip creation with dry run",
			dryRun:        true,
			expectedError: false,
		},
		{
			name:             "error state if multiple jobs exist for same event and node",
			dryRun:           false,
			templateDir:      "templates",
			expectedJobCount: 2,
			expectedError:    true,
			existingObjects: []client.Object{
				&batchv1.Job{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test1",
						Namespace: jobNamespace,
						Labels:    labels,
					},
				},
				&batchv1.Job{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test2",
						Namespace: jobNamespace,
						Labels:    labels,
					},
				},
			},
		},
		{
			name:             "job already exists, not completed",
			dryRun:           false,
			templateDir:      "templates",
			expectedJobCount: 1,
			expectedError:    false,
			existingObjects: []client.Object{
				&batchv1.Job{
					ObjectMeta: metav1.ObjectMeta{
						Name:              "test1",
						Namespace:         jobNamespace,
						Labels:            labels,
						CreationTimestamp: metav1.Now(),
					},
				},
			},
			requeueTime: 10 * time.Second,
		},
		{
			name:             "job already exists and succeeded",
			dryRun:           false,
			templateDir:      "templates",
			expectedJobCount: 1,
			expectedError:    false,
			existingObjects: []client.Object{
				&batchv1.Job{
					ObjectMeta: metav1.ObjectMeta{
						Name:              "test1",
						Namespace:         jobNamespace,
						Labels:            labels,
						CreationTimestamp: metav1.Now(),
					},
					Status: batchv1.JobStatus{
						Conditions: []batchv1.JobCondition{
							{
								Type:   batchv1.JobComplete,
								Status: corev1.ConditionTrue,
							},
						},
						CompletionTime: ptr.To(metav1.Now()),
						StartTime:      ptr.To(metav1.Now()),
					},
				},
			},
		},
		{
			name:             "job already exists and failed",
			dryRun:           false,
			templateDir:      "templates",
			expectedJobCount: 1,
			expectedError:    false,
			existingObjects: []client.Object{
				&batchv1.Job{
					ObjectMeta: metav1.ObjectMeta{
						Name:              "test1",
						Namespace:         jobNamespace,
						Labels:            labels,
						CreationTimestamp: metav1.Now(),
					},
					Status: batchv1.JobStatus{
						Conditions: []batchv1.JobCondition{
							{
								Type:   batchv1.JobFailed,
								Status: corev1.ConditionTrue,
							},
						},
						CompletionTime: ptr.To(metav1.Now()),
						StartTime:      ptr.To(metav1.Now()),
					},
				},
			},
		},
		{
			name:             "job timed out",
			dryRun:           false,
			templateDir:      "templates",
			expectedJobCount: 1,
			expectedError:    false,
			existingObjects: []client.Object{
				&batchv1.Job{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test1",
						Namespace: jobNamespace,
						Labels:    labels,
						CreationTimestamp: metav1.Time{
							Time: metav1.Now().Add(-15 * time.Minute),
						},
					},
					Status: batchv1.JobStatus{
						CompletionTime: ptr.To(metav1.Now()),
						StartTime:      ptr.To(metav1.Now()),
					},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {

			fakeClient := fake.NewClientBuilder().
				WithObjects(tt.existingObjects...).
				WithStatusSubresource(tt.existingObjects...).
				Build()

			remediationClient := &FaultRemediationClient{
				client:            fakeClient,
				dryRunMode:        []string{},
				templateMountPath: tt.templateDir,
			}
			if tt.dryRun {
				remediationClient.dryRunMode = []string{metav1.DryRunAll}
			}

			// Test Run Log Collector
			result, err := remediationClient.RunLogCollectorJob(context.Background(), nodeName, eventId)
			if tt.expectedError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
			assert.Equal(t, result.RequeueAfter, tt.requeueTime)

			existingJobs := &batchv1.JobList{}
			err = fakeClient.List(
				context.TODO(),
				existingJobs,
				client.MatchingLabels(labels),
				client.InNamespace(jobNamespace),
			)
			assert.NoError(t, err)

			assert.Len(t, existingJobs.Items, tt.expectedJobCount)

		})
	}
}
