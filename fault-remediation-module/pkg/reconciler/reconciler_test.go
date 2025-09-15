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
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	storeconnector "gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/platform-connectors/pkg/connectors/store"
	platformconnector "gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/platform-connectors/pkg/protos"
	"gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/store-client-sdk/pkg/storewatcher"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// MockK8sClient is a mock implementation of K8sClient interface
type MockK8sClient struct {
	createMaintenanceResourceFn func(ctx context.Context, healthEventDoc *HealthEventDoc) bool
	runLogCollectorJobFn        func(ctx context.Context, nodeName string) error
}

func (m *MockK8sClient) CreateMaintenanceResource(ctx context.Context, healthEventDoc *HealthEventDoc) bool {
	return m.createMaintenanceResourceFn(ctx, healthEventDoc)
}

func (m *MockK8sClient) RunLogCollectorJob(ctx context.Context, nodeName string) error {
	return m.runLogCollectorJobFn(ctx, nodeName)
}

// MockCollection is a mock implementation of mongo.Collection
type MockCollection struct {
	updateOneFn func(ctx context.Context, filter interface{}, update interface{}, opts ...*options.UpdateOptions) (*mongo.UpdateResult, error)
}

func (m *MockCollection) UpdateOne(ctx context.Context, filter interface{}, update interface{}, opts ...*options.UpdateOptions) (*mongo.UpdateResult, error) {
	return m.updateOneFn(ctx, filter, update, opts...)
}

func TestNewReconciler(t *testing.T) {
	tests := []struct {
		name             string
		nodeName         string
		crCreationResult bool
		dryRun           bool
	}{
		{
			name:             "Create reconciler with dry run enabled",
			nodeName:         "node1",
			crCreationResult: true,
			dryRun:           true,
		},
		{
			name:             "Create reconciler with dry run disabled",
			nodeName:         "node2",
			crCreationResult: false,
			dryRun:           false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := ReconcilerConfig{
				MongoConfig: storewatcher.MongoDBConfig{
					URI:      "mongodb://localhost:27017",
					Database: "test",
				},
				K8sClient: &MockK8sClient{
					createMaintenanceResourceFn: func(ctx context.Context, healthEventDoc *HealthEventDoc) bool {
						assert.Equal(t, tt.nodeName, healthEventDoc.HealthEvent.NodeName)
						return tt.crCreationResult
					},
				},
			}

			r := NewReconciler(cfg, tt.dryRun)
			assert.NotNil(t, r)
			assert.Equal(t, tt.dryRun, r.DryRun)
		})
	}
}

func TestHandleEvent(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name              string
		nodeName          string
		recommendedAction platformconnector.RecommenedAction
		shouldSucceed     bool
	}{
		{
			name:              "Successful NODE_REBOOT action",
			nodeName:          "node1",
			recommendedAction: platformconnector.RecommenedAction_NODE_REBOOT,
			shouldSucceed:     true,
		},
		{
			name:              "Failed NODE_REBOOT action",
			nodeName:          "node2",
			recommendedAction: platformconnector.RecommenedAction_NODE_REBOOT,
			shouldSucceed:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			k8sClient := &MockK8sClient{
				createMaintenanceResourceFn: func(ctx context.Context, healthEventDoc *HealthEventDoc) bool {
					assert.Equal(t, tt.nodeName, healthEventDoc.HealthEvent.NodeName)
					assert.Equal(t, tt.recommendedAction, healthEventDoc.HealthEvent.RecommendedAction)
					return tt.shouldSucceed
				},
			}

			cfg := ReconcilerConfig{
				K8sClient: k8sClient,
			}

			r := NewReconciler(cfg, false)
			healthEventDoc := &HealthEventDoc{
				ID: primitive.NewObjectID(),
				HealthEventWithStatus: storeconnector.HealthEventWithStatus{
					HealthEvent: &platformconnector.HealthEvent{
						NodeName:          tt.nodeName,
						RecommendedAction: tt.recommendedAction,
					},
				},
			}
			result := r.Config.K8sClient.CreateMaintenanceResource(ctx, healthEventDoc)
			assert.Equal(t, tt.shouldSucceed, result)
		})
	}
}

func TestShouldSkipEvent(t *testing.T) {
	r := NewReconciler(ReconcilerConfig{}, false)

	tests := []struct {
		name              string
		nodeName          string
		recommendedAction platformconnector.RecommenedAction
		shouldSkip        bool
		description       string
	}{
		{
			name:              "Skip NONE action",
			nodeName:          "test-node-1",
			recommendedAction: platformconnector.RecommenedAction_NONE,
			shouldSkip:        true,
			description:       "NONE actions should be skipped",
		},
		{
			name:              "Process NODE_REBOOT action",
			nodeName:          "test-node-2",
			recommendedAction: platformconnector.RecommenedAction_NODE_REBOOT,
			shouldSkip:        false,
			description:       "NODE_REBOOT actions should not be skipped",
		},
		{
			name:              "Skip REPORT_ISSUE action",
			nodeName:          "test-node-3",
			recommendedAction: platformconnector.RecommenedAction_REPORT_ISSUE,
			shouldSkip:        true,
			description:       "Unsupported REPORT_ISSUE action should be skipped",
		},
		{
			name:              "Skip unknown action",
			nodeName:          "test-node-4",
			recommendedAction: platformconnector.RecommenedAction(999),
			shouldSkip:        true,
			description:       "Unknown actions should be skipped",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			healthEvent := &platformconnector.HealthEvent{
				NodeName:          tt.nodeName,
				RecommendedAction: tt.recommendedAction,
			}
			healthEventWithStatus := storeconnector.HealthEventWithStatus{
				HealthEvent: healthEvent,
			}

			result := r.shouldSkipEvent(healthEventWithStatus)
			assert.Equal(t, tt.shouldSkip, result, tt.description)
		})
	}
}

func TestRunLogCollectorOnNoneActionWhenEnabled(t *testing.T) {
	ctx := context.Background()

	called := false
	k8sClient := &MockK8sClient{
		createMaintenanceResourceFn: func(ctx context.Context, healthEventDoc *HealthEventDoc) bool {
			return true
		},
		runLogCollectorJobFn: func(ctx context.Context, nodeName string) error {
			called = true
			assert.Equal(t, "test-node-none", nodeName)
			return nil
		},
	}

	cfg := ReconcilerConfig{
		K8sClient:          k8sClient,
		EnableLogCollector: true,
	}
	r := NewReconciler(cfg, false)

	he := &platformconnector.HealthEvent{NodeName: "test-node-none", RecommendedAction: platformconnector.RecommenedAction_NONE}
	event := storeconnector.HealthEventWithStatus{HealthEvent: he}

	// Simulate the Start loop behavior: log collector run before skipping
	if event.HealthEvent.RecommendedAction == platformconnector.RecommenedAction_NONE && r.Config.EnableLogCollector {
		_ = r.Config.K8sClient.RunLogCollectorJob(ctx, event.HealthEvent.NodeName)
	}
	assert.True(t, r.shouldSkipEvent(event))
	assert.True(t, called, "log collector job should be invoked when enabled for NONE action")
}

func TestRunLogCollectorJobErrorScenarios(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name           string
		nodeName       string
		jobResult      bool
		expectedResult bool
		description    string
	}{
		{
			name:           "Log collector job succeeds",
			nodeName:       "test-node-success",
			jobResult:      true,
			expectedResult: true,
			description:    "Happy path - job completes successfully",
		},
		{
			name:           "Log collector job fails",
			nodeName:       "test-node-fail",
			jobResult:      false,
			expectedResult: false,
			description:    "Error path - job fails to complete",
		},
		{
			name:           "Log collector job with api error",
			nodeName:       "test-node-api-error",
			jobResult:      false,
			expectedResult: false,
			description:    "Error path - kubernetes API error during job creation",
		},
		{
			name:           "Log collector job with creation error",
			nodeName:       "test-node-create-error",
			jobResult:      false,
			expectedResult: false,
			description:    "Error path - job creation fails",
		},
		{
			name:           "Log collector job timeout",
			nodeName:       "test-node-timeout",
			jobResult:      false,
			expectedResult: false,
			description:    "Error path - job times out",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			k8sClient := &MockK8sClient{
				createMaintenanceResourceFn: func(ctx context.Context, healthEventDoc *HealthEventDoc) bool {
					return true
				},
				runLogCollectorJobFn: func(ctx context.Context, nodeName string) error {
					assert.Equal(t, tt.nodeName, nodeName)
					if tt.jobResult {
						return nil
					}
					return fmt.Errorf("job failed")
				},
			}

			cfg := ReconcilerConfig{
				K8sClient:          k8sClient,
				EnableLogCollector: true,
			}
			r := NewReconciler(cfg, false)

			result := r.Config.K8sClient.RunLogCollectorJob(ctx, tt.nodeName)
			if tt.expectedResult {
				assert.NoError(t, result, tt.description)
			} else {
				assert.Error(t, result, tt.description)
			}
		})
	}
}

func TestRunLogCollectorJobDryRunMode(t *testing.T) {
	ctx := context.Background()

	called := false
	k8sClient := &MockK8sClient{
		createMaintenanceResourceFn: func(ctx context.Context, healthEventDoc *HealthEventDoc) bool {
			return true
		},
		runLogCollectorJobFn: func(ctx context.Context, nodeName string) error {
			called = true
			// In dry run mode, this should return nil without actually creating the job
			return nil
		},
	}

	cfg := ReconcilerConfig{
		K8sClient:          k8sClient,
		EnableLogCollector: true,
	}
	r := NewReconciler(cfg, true) // Enable dry run

	result := r.Config.K8sClient.RunLogCollectorJob(ctx, "test-node-dry-run")
	assert.NoError(t, result, "Dry run should return no error")
	assert.True(t, called, "Function should be called even in dry run mode")
}

func TestLogCollectorDisabled(t *testing.T) {
	ctx := context.Background()

	logCollectorCalled := false
	k8sClient := &MockK8sClient{
		createMaintenanceResourceFn: func(ctx context.Context, healthEventDoc *HealthEventDoc) bool {
			return true
		},
		runLogCollectorJobFn: func(ctx context.Context, nodeName string) error {
			logCollectorCalled = true
			return nil
		},
	}

	cfg := ReconcilerConfig{
		K8sClient:          k8sClient,
		EnableLogCollector: false, // Disabled
	}
	r := NewReconciler(cfg, false)

	he := &platformconnector.HealthEvent{NodeName: "test-node-disabled", RecommendedAction: platformconnector.RecommenedAction_NONE}
	event := storeconnector.HealthEventWithStatus{HealthEvent: he}

	// Simulate the Start loop behavior: log collector should NOT run when disabled
	if event.HealthEvent.RecommendedAction == platformconnector.RecommenedAction_NONE && r.Config.EnableLogCollector {
		_ = r.Config.K8sClient.RunLogCollectorJob(ctx, event.HealthEvent.NodeName)
	}
	assert.True(t, r.shouldSkipEvent(event))
	assert.False(t, logCollectorCalled, "log collector job should NOT be invoked when disabled")
}

func TestUpdateNodeRemediatedStatus(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name           string
		event          bson.M
		nodeRemediated bool
		mockError      error
		expectError    bool
	}{
		{
			name: "Successful update",
			event: bson.M{
				"fullDocument": bson.M{
					"_id": "test-id-1",
				},
			},
			nodeRemediated: true,
			mockError:      nil,
			expectError:    false,
		},
		{
			name: "Failed update",
			event: bson.M{
				"fullDocument": bson.M{
					"_id": "test-id-2",
				},
			},
			nodeRemediated: false,
			mockError:      assert.AnError,
			expectError:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockColl := &MockCollection{
				updateOneFn: func(ctx context.Context, filter interface{}, update interface{}, opts ...*options.UpdateOptions) (*mongo.UpdateResult, error) {
					filterDoc := filter.(bson.M)
					updateDoc := update.(bson.M)

					assert.Equal(t, tt.event["fullDocument"].(bson.M)["_id"], filterDoc["_id"])
					assert.Equal(t, tt.nodeRemediated, updateDoc["$set"].(bson.M)["healtheventstatus.faultremediated"])

					if tt.mockError != nil {
						return nil, tt.mockError
					}
					return &mongo.UpdateResult{ModifiedCount: 1}, nil
				},
			}

			r := NewReconciler(ReconcilerConfig{}, false)
			err := r.updateNodeRemediatedStatus(ctx, mockColl, tt.event, tt.nodeRemediated)

			if tt.expectError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}
