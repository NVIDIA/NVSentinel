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
	"testing"

	"github.com/stretchr/testify/assert"
	"gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/store-client-sdk/pkg/storewatcher"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// MockK8sClient is a mock implementation of K8sClient interface
type MockK8sClient struct {
	createMaintenanceResourceFn func(ctx context.Context, nodeName string) bool
}

func (m *MockK8sClient) CreateMaintenanceResource(ctx context.Context, nodeName string) bool {
	return m.createMaintenanceResourceFn(ctx, nodeName)
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
					createMaintenanceResourceFn: func(ctx context.Context, nodeName string) bool {
						assert.Equal(t, tt.nodeName, nodeName)
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
		name          string
		nodeName      string
		shouldSucceed bool
		expectedError bool
	}{
		{
			name:          "Successful maintenance creation",
			nodeName:      "node1",
			shouldSucceed: true,
			expectedError: false,
		},
		{
			name:          "Failed maintenance creation",
			nodeName:      "node2",
			shouldSucceed: false,
			expectedError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			k8sClient := &MockK8sClient{
				createMaintenanceResourceFn: func(ctx context.Context, nodeName string) bool {
					assert.Equal(t, tt.nodeName, nodeName)
					return tt.shouldSucceed
				},
			}

			cfg := ReconcilerConfig{
				K8sClient: k8sClient,
			}

			r := NewReconciler(cfg, false)
			result := r.handleEvent(ctx, tt.nodeName)
			assert.Equal(t, tt.shouldSucceed, result)
		})
	}
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
					assert.Equal(t, tt.nodeRemediated, updateDoc["$set"].(bson.M)["healtheventstatus.noderemediated"])

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
