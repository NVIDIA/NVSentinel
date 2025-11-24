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
	"fmt"
	"testing"
	"time"

	datamodels "github.com/nvidia/nvsentinel/data-models/pkg/model"
	protos "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	config "github.com/nvidia/nvsentinel/health-events-analyzer/pkg/config"
	"github.com/nvidia/nvsentinel/health-events-analyzer/pkg/publisher"
	"github.com/nvidia/nvsentinel/store-client/pkg/client"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Mock Publisher
type mockPublisher struct {
	mock.Mock
}

func (m *mockPublisher) HealthEventOccurredV1(ctx context.Context, events *protos.HealthEvents, opts ...grpc.CallOption) (*emptypb.Empty, error) {
	args := m.Called(ctx, events)
	return args.Get(0).(*emptypb.Empty), args.Error(1)
}

// Mock DatabaseClient
type mockDatabaseClient struct {
	mock.Mock
}

func (m *mockDatabaseClient) InsertMany(ctx context.Context, documents []interface{}) (*client.InsertManyResult, error) {
	args := m.Called(ctx, documents)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*client.InsertManyResult), args.Error(1)
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

func (m *mockDatabaseClient) UpdateManyDocuments(ctx context.Context, filter interface{}, update interface{}) (*client.UpdateResult, error) {
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

// Mock cursor for tests
type mockCursor struct {
	mock.Mock
	data []map[string]interface{}
	pos  int
}

func createMockCursor(data []map[string]interface{}) (*mockCursor, error) {
	return &mockCursor{data: data, pos: -1}, nil
}

func (m *mockCursor) Next(ctx context.Context) bool {
	m.pos++
	return m.pos < len(m.data)
}

func (m *mockCursor) Decode(v interface{}) error {
	if m.pos >= 0 && m.pos < len(m.data) {
		if doc, ok := v.(*map[string]interface{}); ok {
			*doc = m.data[m.pos]
		}
	}
	return nil
}

func (m *mockCursor) Close(ctx context.Context) error {
	return nil
}

func (m *mockCursor) All(ctx context.Context, results interface{}) error {
	if resultsSlice, ok := results.(*[]map[string]interface{}); ok {
		*resultsSlice = m.data
	}

	return nil
}

func (m *mockCursor) Err() error {
	return nil
}

var (
	rules = []config.HealthEventsAnalyzerRule{
		{
			Name:        "rule1",
			Description: "check multiple remediations are completed within 2 minutes",
			Stage: []string{
				`{"$match": {"$expr": {"$gte": ["$healthevent.generatedtimestamp.seconds", {"$subtract": [{"$divide": [{"$toLong": "$$NOW"}, 1000]}, 120]}]}}}`,
				`{"$match": {"healtheventstatus.faultremediated": true, "healthevent.nodename": "this.healthevent.nodename"}}`,
				`{"$count": "count"}`,
				`{"$match": {"count": {"$gte": 5}}}`,
			},
			RecommendedAction: "CONTACT_SUPPORT",
		},
		{
			Name:        "rule2",
			Description: "check the occurrence of XID error 13",
			Stage: []string{
				`{"$match": {"$expr": {"$gte": ["$healthevent.generatedtimestamp.seconds", {"$subtract": [{"$divide": [{"$toLong": "$$NOW"}, 1000]}, 120]}]}}}`,
				`{"$match": {"healthevent.entitiesimpacted.0.entitytype": "GPU", "healthevent.entitiesimpacted.0.entityvalue": "this.healthevent.entitiesimpacted.0.entityvalue", "healthevent.errorcode.0": "13", "healthevent.nodename": "this.healthevent.nodename", "healthevent.checkname": {"$ne": "HealthEventsAnalyzer"}}}`,
				`{"$count": "count"}`,
				`{"$match": {"count": {"$gte": 3}}}`,
			},
			RecommendedAction: "CONTACT_SUPPORT",
		},
		{
			Name:        "rule3",
			Description: "check the occurrence of XID errors (13 or 31)",
			Stage: []string{
				`{"$match": {"$expr": {"$gte": ["$healthevent.generatedtimestamp.seconds", {"$subtract": [{"$divide": [{"$toLong": "$$NOW"}, 1000]}, 180]}]}}}`,
				`{"$match": {"healthevent.entitiesimpacted.0.entitytype": "GPU", "healthevent.entitiesimpacted.0.entityvalue": "this.healthevent.entitiesimpacted.0.entityvalue", "healthevent.errorcode.0": {"$in": ["13", "31"]}, "healthevent.nodename": "this.healthevent.nodename"}}`,
				`{"$count": "count"}`,
				`{"$match": {"count": {"$gte": 1}}}`,
			},
			RecommendedAction: "CONTACT_SUPPORT",
		},
	}
	healthEvent_13 = datamodels.HealthEventWithStatus{
		CreatedAt: time.Now(),
		HealthEvent: &protos.HealthEvent{
			Version:        1,
			Agent:          "gpu-health-monitor",
			ComponentClass: "GPU",
			CheckName:      "GpuXidError",
			IsFatal:        true,
			IsHealthy:      false,
			Message:        "XID error occurred",
			ErrorCode:      []string{"13"},
			EntitiesImpacted: []*protos.Entity{{
				EntityType:  "GPU",
				EntityValue: "1",
			}},
			Metadata: map[string]string{
				"SerialNumber": "1655322004581",
			},
			GeneratedTimestamp: &timestamppb.Timestamp{
				Seconds: time.Now().Unix(),
				Nanos:   0,
			},
			NodeName: "node1",
		},
		HealthEventStatus: datamodels.HealthEventStatus{},
	}
	healthEvent_48 = datamodels.HealthEventWithStatus{
		CreatedAt: time.Now(),
		HealthEvent: &protos.HealthEvent{
			Version:        1,
			Agent:          "gpu-health-monitor",
			ComponentClass: "GPU",
			CheckName:      "GpuXidError",
			IsFatal:        true,
			IsHealthy:      false,
			Message:        "XID error occurred",
			ErrorCode:      []string{"48"},
			EntitiesImpacted: []*protos.Entity{{
				EntityType:  "GPU",
				EntityValue: "1",
			}},
			Metadata: map[string]string{
				"SerialNumber": "1655322004581",
			},
			GeneratedTimestamp: &timestamppb.Timestamp{
				Seconds: time.Now().Unix(),
				Nanos:   0,
			},
			NodeName: "node1",
		},
		HealthEventStatus: datamodels.HealthEventStatus{},
	}
)

func TestCheckRule(t *testing.T) {
	ctx := context.Background()

	mockClient := new(mockDatabaseClient)

	reconciler := &Reconciler{
		databaseClient: mockClient,
	}

	t.Run("rule1 matches", func(t *testing.T) {
		// Rule1 requires 5 occurrences, so return ruleMatched: true
		cursor, _ := createMockCursor([]map[string]interface{}{
			{"ruleMatched": true},
		})
		mockClient.On("Aggregate", ctx, mock.Anything).Return(cursor, nil).Once()
		result, err := reconciler.validateAllSequenceCriteria(ctx, rules[0], healthEvent_13)
		assert.NoError(t, err)
		assert.True(t, result)
		mockClient.AssertExpectations(t)
	})

	t.Run("rule2 does not match", func(t *testing.T) {
		// Rule2 requires 3 occurrences for the sequence, return ruleMatched: false
		cursor, _ := createMockCursor([]map[string]interface{}{
			{"ruleMatched": false},
		})
		mockClient.On("Aggregate", ctx, mock.Anything).Return(cursor, nil).Once()
		result, err := reconciler.validateAllSequenceCriteria(ctx, rules[1], healthEvent_13)
		assert.NoError(t, err)
		assert.False(t, result)
		mockClient.AssertExpectations(t)
	})

	t.Run("aggregate fails", func(t *testing.T) {
		mockClient.On("Aggregate", ctx, mock.Anything).Return((*mockCursor)(nil), fmt.Errorf("aggregate failed")).Once()
		result, err := reconciler.validateAllSequenceCriteria(ctx, rules[0], healthEvent_13)
		assert.Error(t, err)
		assert.False(t, result)
		mockClient.AssertExpectations(t)
	})

	t.Run("invalid stage format", func(t *testing.T) {
		invalidRule := rules[0]
		invalidRule.Stage = []string{"invalid json"}
		result, err := reconciler.validateAllSequenceCriteria(ctx, invalidRule, healthEvent_13)
		assert.Error(t, err)
		assert.False(t, result)
	})
}

func TestHandleEvent(t *testing.T) {
	ctx := context.Background()

	t.Run("rule matches and event is published", func(t *testing.T) {
		mockClient := new(mockDatabaseClient)
		mockPublisher := &mockPublisher{}

		reconciler := &Reconciler{
			config: HealthEventsAnalyzerReconcilerConfig{
				HealthEventsAnalyzerRules: &config.TomlConfig{Rules: []config.HealthEventsAnalyzerRule{rules[1]}},
				Publisher:                 publisher.NewPublisher(mockPublisher),
			},
			databaseClient: mockClient,
		}

		// Create the expected health event that the publisher will create (transformed)
		expectedTransformedEvent := &protos.HealthEvent{
			Version:            healthEvent_13.HealthEvent.Version,
			Agent:              "health-events-analyzer", // Publisher sets this
			CheckName:          "rule2",                  // Publisher sets this to ruleName
			ComponentClass:     healthEvent_13.HealthEvent.ComponentClass,
			Message:            healthEvent_13.HealthEvent.Message,
			RecommendedAction:  protos.RecommendedAction_CONTACT_SUPPORT, // From rule2
			ErrorCode:          healthEvent_13.HealthEvent.ErrorCode,
			IsHealthy:          false, // Publisher sets this
			IsFatal:            true,  // Publisher sets this
			EntitiesImpacted:   healthEvent_13.HealthEvent.EntitiesImpacted,
			Metadata:           healthEvent_13.HealthEvent.Metadata,
			GeneratedTimestamp: healthEvent_13.HealthEvent.GeneratedTimestamp,
			NodeName:           healthEvent_13.HealthEvent.NodeName,
		}
		expectedHealthEvents := &protos.HealthEvents{
			Version: 1,
			Events:  []*protos.HealthEvent{expectedTransformedEvent},
		}

		mockPublisher.On("HealthEventOccurredV1", ctx, expectedHealthEvents).Return(&emptypb.Empty{}, nil)

		// Rule2 requires 3 occurrences, so return ruleMatched: true
		cursor, _ := createMockCursor([]map[string]interface{}{
			{"ruleMatched": true},
		})
		mockClient.On("Aggregate", ctx, mock.Anything).Return(cursor, nil)

		published, _ := reconciler.handleEvent(ctx, &healthEvent_13)
		assert.True(t, published)
		mockClient.AssertExpectations(t)
		mockPublisher.AssertExpectations(t)
	})

	t.Run("no rules match", func(t *testing.T) {
		mockClient := new(mockDatabaseClient)
		mockPublisher := &mockPublisher{}
		cfg := HealthEventsAnalyzerReconcilerConfig{
			HealthEventsAnalyzerRules: &config.TomlConfig{Rules: rules},
			Publisher:                 publisher.NewPublisher(mockPublisher),
		}
		reconciler := NewReconciler(cfg)
		reconciler.databaseClient = mockClient

		// Create a copy of the event with error code that doesn't match any rules
		testEvent := healthEvent_13
		testEventCopy := datamodels.HealthEventWithStatus{
			CreatedAt: testEvent.CreatedAt,
			HealthEvent: &protos.HealthEvent{
				Version:            testEvent.HealthEvent.Version,
				Agent:              testEvent.HealthEvent.Agent,
				ComponentClass:     testEvent.HealthEvent.ComponentClass,
				CheckName:          testEvent.HealthEvent.CheckName,
				IsFatal:            testEvent.HealthEvent.IsFatal,
				IsHealthy:          testEvent.HealthEvent.IsHealthy,
				Message:            testEvent.HealthEvent.Message,
				ErrorCode:          []string{"43"}, // Set error code that doesn't match any specific rule
				EntitiesImpacted:   testEvent.HealthEvent.EntitiesImpacted,
				Metadata:           testEvent.HealthEvent.Metadata,
				GeneratedTimestamp: testEvent.HealthEvent.GeneratedTimestamp,
				NodeName:           testEvent.HealthEvent.NodeName,
			},
			HealthEventStatus: testEvent.HealthEventStatus,
		}

		// Rule1 will still call Aggregate (since it doesn't check error codes), return ruleMatched: false so it doesn't match
		// Rules 2 and 3 might also call Aggregate for their criteria checks
		cursor, _ := createMockCursor([]map[string]interface{}{
			{"ruleMatched": false},
		})
		mockClient.On("Aggregate", ctx, mock.Anything).Return(cursor, nil).Maybe()

		published, err := reconciler.handleEvent(ctx, &testEventCopy)
		assert.NoError(t, err)
		assert.False(t, published)
		mockClient.AssertExpectations(t)
		mockPublisher.AssertNotCalled(t, "HealthEventOccurredV1")
	})

	t.Run("multisequence rule partially matches", func(t *testing.T) {
		mockClient := new(mockDatabaseClient)
		mockPublisher := &mockPublisher{}
		cfg := HealthEventsAnalyzerReconcilerConfig{
			HealthEventsAnalyzerRules: &config.TomlConfig{Rules: rules},
			Publisher:                 publisher.NewPublisher(mockPublisher),
		}
		reconciler := NewReconciler(cfg)
		reconciler.databaseClient = mockClient

		// Multiple rules will be processed:
		// - Rule1: will call Aggregate (doesn't check error codes)
		// - Rule2: will call Aggregate (matches error code 13)
		// - Rule3: has two sequences, return ruleMatched: false so it doesn't match
		cursor, _ := createMockCursor([]map[string]interface{}{
			{"ruleMatched": false},
		})
		mockClient.On("Aggregate", ctx, mock.Anything).Return(cursor, nil).Maybe() // Rule1 and other calls

		published, _ := reconciler.handleEvent(ctx, &healthEvent_13)
		assert.False(t, published)
		mockClient.AssertExpectations(t)
		mockPublisher.AssertNotCalled(t, "HealthEventOccurredV1")
	})

	t.Run("empty rules list", func(t *testing.T) {
		mockClient := new(mockDatabaseClient)
		mockPublisher := &mockPublisher{}
		cfg := HealthEventsAnalyzerReconcilerConfig{
			HealthEventsAnalyzerRules: &config.TomlConfig{Rules: []config.HealthEventsAnalyzerRule{}},
			Publisher:                 publisher.NewPublisher(mockPublisher),
		}
		reconciler := NewReconciler(cfg)
		reconciler.databaseClient = mockClient

		published, err := reconciler.handleEvent(ctx, &healthEvent_13)
		assert.NoError(t, err)
		assert.False(t, published)
		mockClient.AssertNotCalled(t, "Aggregate")
		mockPublisher.AssertNotCalled(t, "HealthEventOccurredV1")
	})
}

func TestGetPipelineStages(t *testing.T) {
	ctx := context.Background()
	reconciler := &Reconciler{
		config: HealthEventsAnalyzerReconcilerConfig{},
	}

	t.Run("simple stages with this references", func(t *testing.T) {
		rule := config.HealthEventsAnalyzerRule{
			Name: "test-rule",
			Stage: []string{
				`{"$match": {"healthevent.nodename": "this.healthevent.nodename"}}`,
				`{"$count": "total"}`,
			},
		}
		event := datamodels.HealthEventWithStatus{
			HealthEvent: &protos.HealthEvent{
				NodeName: "test-node-1",
			},
		}

		pipeline, err := reconciler.getPipelineStages(rule, event)
		assert.NoError(t, err)
		// Agent filter (1) + configured stages (2) = 3 total
		assert.Len(t, pipeline, 3)

		// Check first stage is agent filter
		firstStage := pipeline[0]
		agentMatch, ok := firstStage["$match"].(map[string]interface{})
		assert.True(t, ok)
		_, hasAgentFilter := agentMatch["healthevent.agent"]
		assert.True(t, hasAgentFilter, "First stage must be agent filter")

		// Check second stage (first configured stage)
		secondStage := pipeline[1]
		matchStage, ok := secondStage["$match"].(map[string]interface{})
		assert.True(t, ok)
		assert.Equal(t, "test-node-1", matchStage["healthevent.nodename"])

		// Check third stage (second configured stage)
		thirdStage := pipeline[2]
		assert.Equal(t, "total", thirdStage["$count"])
	})

	t.Run("complex stages with nested this references", func(t *testing.T) {
		rule := config.HealthEventsAnalyzerRule{
			Name: "complex-rule",
			Stage: []string{
				`{"$match": {"healthevent.nodename": "this.healthevent.nodename", "healthevent.errorcode.0": "this.healthevent.errorcode.0"}}`,
				`{"$match": {"healthevent.isfatal": true}}`,
				`{"$count": "count"}`,
				`{"$match": {"count": {"$gte": 5}}}`,
			},
		}
		event := datamodels.HealthEventWithStatus{
			HealthEvent: &protos.HealthEvent{
				NodeName:  "gpu-node-2",
				ErrorCode: []string{"13"},
				IsFatal:   true,
			},
		}

		pipeline, err := reconciler.getPipelineStages(rule, event)
		assert.NoError(t, err)
		// Agent filter (1) + configured stages (4) = 5 total
		assert.Len(t, pipeline, 5)

		// Check that this references are resolved in second stage (first configured stage)
		secondStage := pipeline[1]
		matchStage := secondStage["$match"].(map[string]interface{})
		assert.Equal(t, "gpu-node-2", matchStage["healthevent.nodename"])
		assert.Equal(t, "13", matchStage["healthevent.errorcode.0"])
	})

	t.Run("stages with entity array access", func(t *testing.T) {
		rule := config.HealthEventsAnalyzerRule{
			Name: "entity-rule",
			Stage: []string{
				`{"$match": {"healthevent.entitiesimpacted.0.entityvalue": "this.healthevent.entitiesimpacted.0.entityvalue"}}`,
			},
		}
		event := datamodels.HealthEventWithStatus{
			HealthEvent: &protos.HealthEvent{
				EntitiesImpacted: []*protos.Entity{
					{EntityType: "GPU", EntityValue: "GPU-123"},
				},
			},
		}

		pipeline, err := reconciler.getPipelineStages(rule, event)
		assert.NoError(t, err)
		// Agent filter (1) + configured stages (1) = 2 total
		assert.Len(t, pipeline, 2)

		secondStage := pipeline[1]
		matchStage := secondStage["$match"].(map[string]interface{})
		assert.Equal(t, "GPU-123", matchStage["healthevent.entitiesimpacted.0.entityvalue"])
	})

	t.Run("invalid JSON in stage", func(t *testing.T) {
		rule := config.HealthEventsAnalyzerRule{
			Name: "invalid-rule",
			Stage: []string{
				`{invalid json}`,
			},
		}
		event := datamodels.HealthEventWithStatus{
			HealthEvent: &protos.HealthEvent{
				NodeName: "test",
			},
		}

		pipeline, err := reconciler.getPipelineStages(rule, event)
		assert.Error(t, err)
		assert.Nil(t, pipeline)
		assert.Contains(t, err.Error(), "failed to parse stage 0")
	})

	t.Run("invalid this reference in stage", func(t *testing.T) {
		rule := config.HealthEventsAnalyzerRule{
			Name: "bad-ref-rule",
			Stage: []string{
				`{"$match": {"field": "this.healthevent.nonexistent"}}`,
			},
		}
		event := datamodels.HealthEventWithStatus{
			HealthEvent: &protos.HealthEvent{
				NodeName: "test",
			},
		}

		pipeline, err := reconciler.getPipelineStages(rule, event)
		assert.Error(t, err)
		assert.Nil(t, pipeline)
	})

	t.Run("empty stage list", func(t *testing.T) {
		rule := config.HealthEventsAnalyzerRule{
			Name:  "empty-rule",
			Stage: []string{},
		}
		event := datamodels.HealthEventWithStatus{
			HealthEvent: &protos.HealthEvent{
				NodeName: "test",
			},
		}

		pipeline, err := reconciler.getPipelineStages(rule, event)
		assert.NoError(t, err)
		// Even with empty stages, agent filter is always present
		assert.Len(t, pipeline, 1)
	})

	t.Run("stages without any this references", func(t *testing.T) {
		rule := config.HealthEventsAnalyzerRule{
			Name: "static-rule",
			Stage: []string{
				`{"$match": {"healthevent.isfatal": true}}`,
				`{"$count": "total"}`,
			},
		}
		event := datamodels.HealthEventWithStatus{
			HealthEvent: &protos.HealthEvent{
				NodeName: "test",
			},
		}

		pipeline, err := reconciler.getPipelineStages(rule, event)
		assert.NoError(t, err)
		// Agent filter (1) + configured stages (2) = 3 total
		assert.Len(t, pipeline, 3)

		// Verify configured stages remain unchanged (second stage is first configured)
		secondStage := pipeline[1]
		matchStage := secondStage["$match"].(map[string]interface{})
		assert.Equal(t, true, matchStage["healthevent.isfatal"])
	})

	t.Run("stages with MongoDB operators", func(t *testing.T) {
		rule := config.HealthEventsAnalyzerRule{
			Name: "operator-rule",
			Stage: []string{
				`{"$match": {"$expr": {"$gte": ["$healthevent.generatedtimestamp.seconds", 1000]}, "healthevent.nodename": "this.healthevent.nodename"}}`,
			},
		}
		event := datamodels.HealthEventWithStatus{
			HealthEvent: &protos.HealthEvent{
				NodeName: "operator-node",
			},
		}

		pipeline, err := reconciler.getPipelineStages(rule, event)
		assert.NoError(t, err)
		// Agent filter (1) + configured stages (1) = 2 total
		assert.Len(t, pipeline, 2)

		secondStage := pipeline[1]
		matchStage := secondStage["$match"].(map[string]interface{})
		assert.Equal(t, "operator-node", matchStage["healthevent.nodename"])

		// Verify $expr is preserved
		exprStage, ok := matchStage["$expr"].(map[string]interface{})
		assert.True(t, ok)
		assert.NotNil(t, exprStage["$gte"])
	})

	_ = ctx // suppress unused warning if any
}

// TestGetPipelineStages_ReturnTypeCompatibility ensures the pipeline return type
// is []map[string]interface{} (not []interface{}) to maintain compatibility with
// MongoDB's Aggregate function. This test prevents regression of the bug where
// changing the return type to []interface{} broke aggregation pipeline processing.
//
// Context: In commit 7be1a34, the return type was mistakenly changed to []interface{},
// which caused MongoDB BSON marshaling to behave differently, resulting in queries
// returning no results. The correct type is []map[string]interface{} which MongoDB
// can properly convert to []bson.D for aggregation operations.
func TestGetPipelineStages_ReturnTypeCompatibility(t *testing.T) {
	reconciler := &Reconciler{
		config: HealthEventsAnalyzerReconcilerConfig{},
	}

	rule := config.HealthEventsAnalyzerRule{
		Name: "type-check-rule",
		Stage: []string{
			`{"$match": {"healthevent.nodename": "test-node"}}`,
			`{"$count": "total"}`,
		},
	}

	event := datamodels.HealthEventWithStatus{
		HealthEvent: &protos.HealthEvent{
			NodeName: "test-node",
		},
	}

	pipeline, err := reconciler.getPipelineStages(rule, event)
	assert.NoError(t, err)

	// CRITICAL: Verify the return type is []map[string]interface{}
	// This should compile and not panic
	var typedPipeline []map[string]interface{}
	typedPipeline = pipeline
	assert.NotNil(t, typedPipeline)

	// Verify each element in the pipeline is map[string]interface{}
	// Since the return type is []map[string]interface{}, each stage is guaranteed to be the correct type
	for i, stage := range pipeline {
		assert.NotNil(t, stage, "Stage %d must not be nil", i)
		// Verify it's actually a map by checking it has keys
		assert.Greater(t, len(stage), 0, "Stage %d must have at least one key", i)
	}

	// Verify this type can be used with MongoDB's Aggregate method
	// by checking it matches the signature that store-client expects
	mockDB := &mockDatabaseClient{}
	mockCursor := &mockCursor{}
	
	// The Aggregate method expects interface{} but should work with []map[string]interface{}
	// This ensures backward compatibility with MongoDB driver
	mockDB.On("Aggregate", mock.Anything, pipeline).Return(mockCursor, nil)
	
	cursor, err := mockDB.Aggregate(context.Background(), pipeline)
	assert.NoError(t, err)
	assert.NotNil(t, cursor)
	mockDB.AssertExpectations(t)
}