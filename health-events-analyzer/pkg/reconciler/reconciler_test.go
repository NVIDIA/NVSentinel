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
	"errors"
	"testing"
	"time"

	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	platform_connectors "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	config "github.com/nvidia/nvsentinel/health-events-analyzer/pkg/config"
	"github.com/nvidia/nvsentinel/health-events-analyzer/pkg/publisher"
	"github.com/nvidia/nvsentinel/store-client-sdk/pkg/client"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"
)

// Mock Publisher
type mockPublisher struct {
	mock.Mock
}

func (m *mockPublisher) HealthEventOccurredV1(ctx context.Context, events *platform_connectors.HealthEvents, opts ...grpc.CallOption) (*emptypb.Empty, error) {
	args := m.Called(ctx, events)
	return args.Get(0).(*emptypb.Empty), args.Error(1)
}

// Mock DatabaseClient
type mockDatabaseClient struct {
	mock.Mock
}

func (m *mockDatabaseClient) UpdateDocumentStatus(ctx context.Context, documentID string, statusPath string, status interface{}) error {
	args := m.Called(ctx, documentID, statusPath, status)
	return args.Error(0)
}

func (m *mockDatabaseClient) CountDocuments(ctx context.Context, filter interface{}, options *client.CountOptions) (int64, error) {
	args := m.Called(ctx, filter, options)
	return args.Get(0).(int64), args.Error(1)
}

func (m *mockDatabaseClient) UpdateDocument(ctx context.Context, filter interface{}, update interface{}) (*client.UpdateResult, error) {
	args := m.Called(ctx, filter, update)
	return args.Get(0).(*client.UpdateResult), args.Error(1)
}

func (m *mockDatabaseClient) UpsertDocument(ctx context.Context, filter interface{}, document interface{}) (*client.UpdateResult, error) {
	args := m.Called(ctx, filter, document)
	return args.Get(0).(*client.UpdateResult), args.Error(1)
}

func (m *mockDatabaseClient) FindOne(ctx context.Context, filter interface{}, options *client.FindOneOptions) (client.SingleResult, error) {
	args := m.Called(ctx, filter, options)
	return args.Get(0).(client.SingleResult), args.Error(1)
}

func (m *mockDatabaseClient) Find(ctx context.Context, filter interface{}, options *client.FindOptions) (client.Cursor, error) {
	args := m.Called(ctx, filter, options)
	return args.Get(0).(client.Cursor), args.Error(1)
}

func (m *mockDatabaseClient) Aggregate(ctx context.Context, pipeline interface{}) (client.Cursor, error) {
	args := m.Called(ctx, pipeline)
	return args.Get(0).(client.Cursor), args.Error(1)
}

func (m *mockDatabaseClient) WithTransaction(ctx context.Context, fn func(client.SessionContext) error) error {
	args := m.Called(ctx, fn)
	return args.Error(0)
}

func (m *mockDatabaseClient) Ping(ctx context.Context) error {
	args := m.Called(ctx)
	return args.Error(0)
}

func (m *mockDatabaseClient) NewChangeStreamWatcher(ctx context.Context, tokenConfig client.TokenConfig, pipeline interface{}) (client.ChangeStreamWatcher, error) {
	args := m.Called(ctx, tokenConfig, pipeline)
	return args.Get(0).(client.ChangeStreamWatcher), args.Error(1)
}

func (m *mockDatabaseClient) Close(ctx context.Context) error {
	args := m.Called(ctx)
	return args.Error(0)
}

var (
	rules = []config.HealthEventsAnalyzerRule{
		{
			Name:        "rule1",
			Description: "check the occurrence of XID error 13",
			TimeWindow:  "2m",
			Sequence: []config.SequenceStep{{
				Criteria: map[string]interface{}{
					"healthevent.entitiesimpacted.0.entitytype":  "GPU",
					"healthevent.entitiesimpacted.0.entityvalue": "1",
					"healthevent.errorcode.0":                    "13",
					"healthevent.nodename":                       "this.healthevent.nodename",
				},
				ErrorCount: 3,
			}},
			RecommendedAction: "REPORT_ERROR",
		},
		{
			Name:        "rule2",
			Description: "check the occurrence of XID error 13 and XID error 31",
			TimeWindow:  "3m",
			Sequence: []config.SequenceStep{
				{
					Criteria: map[string]interface{}{
						"healthevent.entitiesimpacted.0.entitytype":  "GPU",
						"healthevent.entitiesimpacted.0.entityvalue": "1",
						"healthevent.errorcode.0":                    "13",
						"healthevent.nodename":                       "this.healthevent.nodename",
					},
					ErrorCount: 1,
				},
				{
					Criteria: map[string]interface{}{
						"healthevent.entitiesimpacted.0.entitytype":  "GPU",
						"healthevent.entitiesimpacted.0.entityvalue": "1",
						"healthevent.errorcode.0":                    "31",
						"healthevent.nodename":                       "this.healthevent.nodename",
					},
					ErrorCount: 1,
				}},
			RecommendedAction: "COMPONENT_RESET",
		},
	}
	healthEvent = model.HealthEventWithStatus{
		CreatedAt: time.Now(),
		HealthEvent: &platform_connectors.HealthEvent{
			NodeName: "node1",
			EntitiesImpacted: []*platform_connectors.Entity{{
				EntityType:  "GPU",
				EntityValue: "1",
			}},
			ErrorCode: []string{"13"},
			CheckName: "GpuXidError",
		},
	}
)

func TestCheckRule(t *testing.T) {
	ctx := context.Background()

	mockClient := new(mockDatabaseClient)

	reconciler := &Reconciler{
		databaseClient: mockClient,
	}

	t.Run("rule1 matches", func(t *testing.T) {
		// Rule1 requires 3 occurrences, so return 3
		mockClient.On("CountDocuments", ctx, mock.Anything, mock.Anything).Return(int64(3), nil).Once()
		result := reconciler.evaluateRule(ctx, rules[0], healthEvent)
		assert.True(t, result)
		mockClient.AssertExpectations(t)
	})

	t.Run("rule2 does not match", func(t *testing.T) {
		// Rule2 requires 1 occurrence for each sequence, return 0 for first sequence
		mockClient.On("CountDocuments", ctx, mock.Anything, mock.Anything).Return(int64(0), nil).Once()
		result := reconciler.evaluateRule(ctx, rules[1], healthEvent)
		assert.False(t, result)
		mockClient.AssertExpectations(t)
	})

	t.Run("count documents fails", func(t *testing.T) {
		mockClient.On("CountDocuments", ctx, mock.Anything, mock.Anything).Return(int64(0), errors.New("count failed")).Once()
		result := reconciler.evaluateRule(ctx, rules[0], healthEvent)
		assert.False(t, result)
		mockClient.AssertExpectations(t)
	})

	t.Run("invalid time window", func(t *testing.T) {
		invalidRule := rules[0]
		invalidRule.TimeWindow = "invalid"
		result := reconciler.evaluateRule(ctx, invalidRule, healthEvent)
		assert.False(t, result)
	})
}

func TestHandleEvent(t *testing.T) {

	ctx := context.Background()

	t.Run("rule matches and event is published", func(t *testing.T) {
		mockClient := new(mockDatabaseClient)
		mockPublisher := &mockPublisher{}
		expectedHealthEvents := &platform_connectors.HealthEvents{
			Version: 1,
			Events:  []*platform_connectors.HealthEvent{healthEvent.HealthEvent},
		}
		mockPublisher.On("HealthEventOccurredV1", ctx, expectedHealthEvents).Return(&emptypb.Empty{}, nil)

		reconciler := Reconciler{
			config: HealthEventsAnalyzerReconcilerConfig{
				HealthEventsAnalyzerRules: &config.TomlConfig{Rules: rules},
				Publisher:                 publisher.NewPublisher(mockPublisher),
			},
			databaseClient: mockClient,
		}

		// Rule1 requires 3 occurrences, so return 3
		mockClient.On("CountDocuments", ctx, mock.Anything, mock.Anything).Return(int64(3), nil)

		published, err := reconciler.handleEvent(ctx, &healthEvent)
		assert.NoError(t, err)
		assert.True(t, published)
		mockClient.AssertExpectations(t)
		mockPublisher.AssertExpectations(t)
	})

	t.Run("no rules match", func(t *testing.T) {
		healthEvent = model.HealthEventWithStatus{
			CreatedAt: time.Now(),
			HealthEvent: &platform_connectors.HealthEvent{
				NodeName: "node1",
				EntitiesImpacted: []*platform_connectors.Entity{{
					EntityType:  "GPU",
					EntityValue: "0",
				}},
				ErrorCode: []string{"43"},
				CheckName: "GpuXidError",
			},
		}

		mockClient := new(mockDatabaseClient)
		mockPublisher := &mockPublisher{}

		reconciler := Reconciler{
			config: HealthEventsAnalyzerReconcilerConfig{
				HealthEventsAnalyzerRules: &config.TomlConfig{Rules: rules},
				Publisher:                 publisher.NewPublisher(mockPublisher),
			},
			databaseClient: mockClient,
		}

		published, err := reconciler.handleEvent(ctx, &healthEvent)
		assert.NoError(t, err)
		assert.False(t, published)
		mockClient.AssertNotCalled(t, "CountDocuments")
		mockPublisher.AssertNotCalled(t, "HealthEventOccurredV1")
	})

	t.Run("one sequence matched", func(t *testing.T) {
		healthEvent = model.HealthEventWithStatus{
			CreatedAt: time.Now(),
			HealthEvent: &platform_connectors.HealthEvent{
				NodeName: "node1",
				EntitiesImpacted: []*platform_connectors.Entity{{
					EntityType:  "GPU",
					EntityValue: "1",
				}},
				ErrorCode: []string{"31"},
				CheckName: "GpuXidError",
			},
		}

		mockClient := new(mockDatabaseClient)
		mockPublisher := &mockPublisher{}

		reconciler := Reconciler{
			config: HealthEventsAnalyzerReconcilerConfig{
				HealthEventsAnalyzerRules: &config.TomlConfig{Rules: rules},
				Publisher:                 publisher.NewPublisher(mockPublisher),
			},
			databaseClient: mockClient,
		}

		// Rule2 has two sequences, return enough for first but not second
		mockClient.On("CountDocuments", ctx, mock.Anything, mock.Anything).Return(int64(1), nil).Once() // First sequence passes
		mockClient.On("CountDocuments", ctx, mock.Anything, mock.Anything).Return(int64(0), nil).Once() // Second sequence fails

		published, err := reconciler.handleEvent(ctx, &healthEvent)
		assert.NoError(t, err)
		assert.False(t, published)
		mockClient.AssertExpectations(t)
		mockPublisher.AssertNotCalled(t, "HealthEventOccurredV1")
	})

	t.Run("empty rules list", func(t *testing.T) {
		mockClient := new(mockDatabaseClient)
		mockPublisher := &mockPublisher{}

		reconciler := Reconciler{
			config: HealthEventsAnalyzerReconcilerConfig{
				HealthEventsAnalyzerRules: &config.TomlConfig{Rules: []config.HealthEventsAnalyzerRule{}},
				Publisher:                 publisher.NewPublisher(mockPublisher),
			},
			databaseClient: mockClient,
		}

		published, err := reconciler.handleEvent(ctx, &healthEvent)
		assert.NoError(t, err)
		assert.False(t, published)
		mockClient.AssertNotCalled(t, "CountDocuments")
		mockPublisher.AssertNotCalled(t, "HealthEventOccurredV1")
	})
}
