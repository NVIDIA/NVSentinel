/*
Copyright (c) 2024, NVIDIA CORPORATION.  All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package pkg

import (
	"context"
	"errors"
	"testing"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
)

type MockMongoDBClient struct {
	QueryHealthEventsFunc func(ctx context.Context, filter bson.M, limit int) ([]bson.M, error)
}

func (m *MockMongoDBClient) QueryHealthEvents(ctx context.Context, filter bson.M, limit int) ([]bson.M, error) {
	if m.QueryHealthEventsFunc != nil {
		return m.QueryHealthEventsFunc(ctx, filter, limit)
	}
	return nil, errors.New("QueryHealthEventsFunc not set")
}

func (m *MockMongoDBClient) Connect(ctx context.Context) error    { return nil }
func (m *MockMongoDBClient) Disconnect(ctx context.Context) error { return nil }
func (m *MockMongoDBClient) FindDocumentsByNodeName(ctx context.Context, nodeName string) ([]bson.M, error) {
	return nil, nil
}
func (m *MockMongoDBClient) GetCollection() *mongo.Collection { return nil }
func (m *MockMongoDBClient) GetClient() *mongo.Client         { return nil }
func (m *MockMongoDBClient) IsConnected() bool                { return true }

func TestCreateHealthEvent(t *testing.T) {
	// Create a test manager
	service := &HealthEventManager{}

	// Test configuration
	config := &Config{
		NodeName:          "test-node",
		ErrorCode:         "TEST_ERROR",
		Reason:            "Test reason",
		IsHealthy:         false,
		RecommendedAction: 1,
		CreatorID:         "test-creator",
		Force:             false,
		SkipQuarantine:    false,
		SkipDrain:         false,
		SocketPath:        "/tmp/test.sock",
	}

	// Test creating health event
	healthEvent, err := service.CreateHealthEvent(config)
	if err != nil {
		t.Fatalf("Failed to create health event: %v", err)
	}

	// Verify the health event fields
	if healthEvent.NodeName != config.NodeName {
		t.Errorf("Expected NodeName %s, got %s", config.NodeName, healthEvent.NodeName)
	}

	if healthEvent.CheckName != config.ErrorCode {
		t.Errorf("Expected CheckName %s, got %s", config.ErrorCode, healthEvent.CheckName)
	}

	if healthEvent.IsHealthy != config.IsHealthy {
		t.Errorf("Expected IsHealthy %t, got %t", config.IsHealthy, healthEvent.IsHealthy)
	}

	if healthEvent.RecommendedAction != 1 {
		t.Errorf("Expected RecommendedAction 1, got %d", healthEvent.RecommendedAction)
	}

	// Verify quarantine overrides
	if !healthEvent.QuarantineOverrides.Force {
		t.Error("Expected QuarantineOverrides.Force to be true")
	}

	if healthEvent.QuarantineOverrides.Skip != config.SkipQuarantine {
		t.Errorf("Expected QuarantineOverrides.Skip %t, got %t", config.SkipQuarantine, healthEvent.QuarantineOverrides.Skip)
	}

	// Verify drain overrides
	if healthEvent.DrainOverrides.Force != config.Force {
		t.Errorf("Expected DrainOverrides.Force %t, got %t", config.Force, healthEvent.DrainOverrides.Force)
	}

	if healthEvent.DrainOverrides.Skip != config.SkipDrain {
		t.Errorf("Expected DrainOverrides.Skip %t, got %t", config.SkipDrain, healthEvent.DrainOverrides.Skip)
	}
}

func TestCreateHealthEventWithForce(t *testing.T) {
	service := &HealthEventManager{}

	config := &Config{
		NodeName:          "test-node",
		ErrorCode:         "TEST_ERROR",
		Reason:            "Test reason",
		IsHealthy:         false,
		RecommendedAction: 1,
		CreatorID:         "test-creator",
		Force:             true, // Test force flag
		SkipQuarantine:    false,
		SkipDrain:         false,
		SocketPath:        "/tmp/test.sock",
	}

	healthEvent, err := service.CreateHealthEvent(config)
	if err != nil {
		t.Fatalf("Failed to create health event: %v", err)
	}

	// Verify force flag affects drain overrides
	if !healthEvent.DrainOverrides.Force {
		t.Error("Expected DrainOverrides.Force to be true when Force is true")
	}
}

func TestQueryHealthEvents(t *testing.T) {
	tests := []struct {
		name           string
		filter         bson.M
		limit          int
		mockResponse   []bson.M
		mockError      error
		expectedEvents int
		expectedError  bool
	}{
		{
			name:   "successful_query_with_events",
			filter: bson.M{"healthevent.nodename": "test-node"},
			limit:  10,
			mockResponse: []bson.M{
				{
					"_id": primitive.NewObjectID(),
					"healthevent": bson.M{
						"nodename":          "test-node",
						"errorcode":         []string{"TEST_ERROR"},
						"message":           "Test message",
						"checkname":         "TEST_CHECK",
						"recommendedaction": 1,
						"metadata": bson.M{
							"creator_id": "test-user",
						},
					},
					"healtheventstatus": bson.M{
						"nodequarantined": "Quarantined",
						"userpodsevictionstatus": bson.M{
							"status": "InProgress",
						},
						"faultremediated": false,
					},
					"createdat": primitive.NewDateTimeFromTime(primitive.NewObjectID().Timestamp()),
				},
			},
			mockError:      nil,
			expectedEvents: 1,
			expectedError:  false,
		},
		{
			name:           "successful_query_no_events",
			filter:         bson.M{"healthevent.nodename": "non-existent-node"},
			limit:          10,
			mockResponse:   []bson.M{},
			mockError:      nil,
			expectedEvents: 0,
			expectedError:  false,
		},
		{
			name:           "mongodb_client_nil",
			filter:         bson.M{"healthevent.nodename": "test-node"},
			limit:          10,
			mockResponse:   nil,
			mockError:      nil,
			expectedEvents: 0,
			expectedError:  true,
		},
		{
			name:           "mongodb_query_error",
			filter:         bson.M{"healthevent.nodename": "test-node"},
			limit:          10,
			mockResponse:   nil,
			mockError:      errors.New("database connection failed"),
			expectedEvents: 0,
			expectedError:  true,
		},
		{
			name:   "multiple_events_success",
			filter: bson.M{"healthevent.nodename": "test-node"},
			limit:  10,
			mockResponse: []bson.M{
				{
					"_id": primitive.NewObjectID(),
					"healthevent": bson.M{
						"nodename":          "test-node",
						"errorcode":         []string{"TEST_ERROR"},
						"message":           "Test message",
						"checkname":         "TEST_CHECK",
						"recommendedaction": 1,
						"metadata": bson.M{
							"creator_id": "test-user",
						},
					},
					"healtheventstatus": bson.M{
						"nodequarantined": "Quarantined",
						"userpodsevictionstatus": bson.M{
							"status": "InProgress",
						},
						"faultremediated": false,
					},
					"createdat": primitive.NewDateTimeFromTime(primitive.NewObjectID().Timestamp()),
				},
				{
					"_id": primitive.NewObjectID(),
					"healthevent": bson.M{
						"nodename":          "test-node-2",
						"errorcode":         []string{"TEST_ERROR_2"},
						"message":           "Test message 2",
						"checkname":         "TEST_CHECK_2",
						"recommendedaction": 1,
						"metadata": bson.M{
							"creator_id": "test-user-2",
						},
					},
					"healtheventstatus": bson.M{
						"nodequarantined": "Quarantined",
						"userpodsevictionstatus": bson.M{
							"status": "InProgress",
						},
						"faultremediated": false,
					},
					"createdat": primitive.NewDateTimeFromTime(primitive.NewObjectID().Timestamp()),
				},
			},
			mockError:      nil,
			expectedEvents: 2, // Both events should be parsed successfully
			expectedError:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create a test manager
			service := &HealthEventManager{}

			// Set up mock MongoDB client if not testing nil client
			if tt.name != "mongodb_client_nil" {
				mockClient := &MockMongoDBClient{
					QueryHealthEventsFunc: func(ctx context.Context, filter bson.M, limit int) ([]bson.M, error) {
						// Verify the parameters passed to the mock
						if filter == nil {
							t.Error("Expected filter to be passed to QueryHealthEvents")
						}
						if limit != tt.limit {
							t.Errorf("Expected limit %d, got %d", tt.limit, limit)
						}
						return tt.mockResponse, tt.mockError
					},
				}
				service.mongoClient = mockClient
			}

			// Call the method under test
			ctx := context.Background()
			events, err := service.QueryHealthEvents(ctx, tt.filter, tt.limit)

			// Check error expectations
			if tt.expectedError {
				if err == nil {
					t.Error("Expected error, got nil")
				}
				return
			}

			if err != nil {
				t.Errorf("Unexpected error: %v", err)
				return
			}

			// Check events count
			if len(events) != tt.expectedEvents {
				t.Errorf("Expected %d events, got %d", tt.expectedEvents, len(events))
			}

			// If we expect events, verify the first event structure
			if tt.expectedEvents > 0 && len(events) > 0 {
				event := events[0]
				if event.HealthEventWithStatus.HealthEvent.NodeName != "test-node" {
					t.Errorf("Expected node name 'test-node', got '%s'", event.HealthEventWithStatus.HealthEvent.NodeName)
				}
				if event.HealthEventWithStatus.HealthEvent.CheckName != "TEST_CHECK" {
					t.Errorf("Expected check name 'TEST_CHECK', got '%s'", event.HealthEventWithStatus.HealthEvent.CheckName)
				}
			}
		})
	}
}
