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

	config "github.com/nvidia/nvsentinel/health-events-analyzer/pkg/config"
	"github.com/nvidia/nvsentinel/health-events-analyzer/pkg/publisher"
	storeconnector "github.com/nvidia/nvsentinel/platform-connectors/pkg/connectors/store"
	platform_connectors "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"
)

// Mock Publisher
type mockPublisher struct {
	mock.Mock
}

func (m *mockPublisher) HealthEventOccuredV1(ctx context.Context, events *platform_connectors.HealthEvents, opts ...grpc.CallOption) (*emptypb.Empty, error) {
	args := m.Called(ctx, events)
	return args.Get(0).(*emptypb.Empty), args.Error(1)
}

// Mock CollectionClient
type mockCollectionClient struct {
	mock.Mock
}

func (m *mockCollectionClient) Aggregate(ctx context.Context, pipeline interface{}, opts ...*options.AggregateOptions) (*mongo.Cursor, error) {
	args := m.Called(ctx, pipeline, opts)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*mongo.Cursor), args.Error(1)
}

func createMockCursor(docs []bson.M) (*mongo.Cursor, error) {
	var docsInterface []interface{}
	for _, doc := range docs {
		docsInterface = append(docsInterface, doc)
	}
	return mongo.NewCursorFromDocuments(docsInterface, nil, nil)
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
	healthEvent = storeconnector.HealthEventWithStatus{
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
	ctx := context.TODO()

	mockClient := new(mockCollectionClient)

	reconciler := &Reconciler{
		config: HealthEventsAnalyzerReconcilerConfig{
			CollectionClient: mockClient,
		},
	}

	t.Run("rule1 matches", func(t *testing.T) {
		mockCursor, _ := createMockCursor([]bson.M{{"ruleMatched": true}})
		mockClient.On("Aggregate", ctx, mock.Anything, mock.Anything).Return(mockCursor, nil).Once()
		result := reconciler.evaluateRule(ctx, rules[0], healthEvent)
		assert.True(t, result)
		mockClient.AssertExpectations(t)
	})

	t.Run("rule2 does not match", func(t *testing.T) {
		mockCursor, _ := createMockCursor([]bson.M{{"ruleMatched": false}})
		mockClient.On("Aggregate", ctx, mock.Anything, mock.Anything).Return(mockCursor, nil).Once()
		result := reconciler.evaluateRule(ctx, rules[1], healthEvent)
		assert.False(t, result)
		mockClient.AssertExpectations(t)
	})

	t.Run("aggregation fails", func(t *testing.T) {
		mockClient.On("Aggregate", ctx, mock.Anything, mock.Anything).Return(nil, errors.New("aggregation failed")).Once()
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
		mockClient := new(mockCollectionClient)
		mockPublisher := &mockPublisher{}
		expectedHealthEvents := &platform_connectors.HealthEvents{
			Version: 1,
			Events:  []*platform_connectors.HealthEvent{healthEvent.HealthEvent},
		}
		mockPublisher.On("HealthEventOccuredV1", ctx, expectedHealthEvents).Return(&emptypb.Empty{}, nil)

		reconciler := Reconciler{
			config: HealthEventsAnalyzerReconcilerConfig{
				HealthEventsAnalyzerRules: &config.TomlConfig{Rules: rules},
				CollectionClient:          mockClient,
				Publisher:                 publisher.NewPublisher(mockPublisher),
			},
		}

		mockCursor, _ := createMockCursor([]bson.M{{"ruleMatched": true}})
		mockClient.On("Aggregate", ctx, mock.Anything, mock.Anything).Return(mockCursor, nil)

		published, err := reconciler.handleEvent(ctx, &healthEvent)
		assert.NoError(t, err)
		assert.True(t, published)
		mockClient.AssertExpectations(t)
		mockPublisher.AssertExpectations(t)
	})

	t.Run("no rules match", func(t *testing.T) {
		healthEvent = storeconnector.HealthEventWithStatus{
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

		mockClient := new(mockCollectionClient)
		mockPublisher := &mockPublisher{}

		reconciler := Reconciler{
			config: HealthEventsAnalyzerReconcilerConfig{
				HealthEventsAnalyzerRules: &config.TomlConfig{Rules: rules},
				CollectionClient:          mockClient,
				Publisher:                 publisher.NewPublisher(mockPublisher),
			},
		}

		published, err := reconciler.handleEvent(ctx, &healthEvent)
		assert.NoError(t, err)
		assert.False(t, published)
		mockClient.AssertNotCalled(t, "Aggregate")
		mockPublisher.AssertNotCalled(t, "HealthEventOccuredV1")
	})

	t.Run("one sequence matched", func(t *testing.T) {
		healthEvent = storeconnector.HealthEventWithStatus{
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

		mockClient := new(mockCollectionClient)
		mockPublisher := &mockPublisher{}

		reconciler := Reconciler{
			config: HealthEventsAnalyzerReconcilerConfig{
				HealthEventsAnalyzerRules: &config.TomlConfig{Rules: rules},
				CollectionClient:          mockClient,
				Publisher:                 publisher.NewPublisher(mockPublisher),
			},
		}

		mockCursor, _ := createMockCursor([]bson.M{{"ruleMatched": false}})
		mockClient.On("Aggregate", ctx, mock.Anything, mock.Anything).Return(mockCursor, nil)

		published, err := reconciler.handleEvent(ctx, &healthEvent)
		assert.NoError(t, err)
		assert.False(t, published)
		mockClient.AssertExpectations(t)
		mockPublisher.AssertNotCalled(t, "HealthEventOccuredV1")
	})

	t.Run("empty rules list", func(t *testing.T) {
		mockClient := new(mockCollectionClient)
		mockPublisher := &mockPublisher{}

		reconciler := Reconciler{
			config: HealthEventsAnalyzerReconcilerConfig{
				HealthEventsAnalyzerRules: &config.TomlConfig{Rules: []config.HealthEventsAnalyzerRule{}},
				CollectionClient:          mockClient,
				Publisher:                 publisher.NewPublisher(mockPublisher),
			},
		}

		published, err := reconciler.handleEvent(ctx, &healthEvent)
		assert.NoError(t, err)
		assert.False(t, published)
		mockClient.AssertNotCalled(t, "Aggregate")
		mockPublisher.AssertNotCalled(t, "HealthEventOccuredV1")
	})
}
