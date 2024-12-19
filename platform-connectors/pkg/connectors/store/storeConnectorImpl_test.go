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

package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	platformconnector "gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/platform-connectors/pkg/protos"
	"gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/platform-connectors/pkg/ringbuffer"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo/integration/mtest"
	"go.mongodb.org/mongo-driver/mongo/options"
)

func TestInsertHealthEvents(t *testing.T) {
	mtOpts := mtest.NewOptions().ClientType(mtest.Mock).ClientOptions(options.Client().SetRetryWrites(false))
	mt := mtest.New(t, mtOpts)

	ringBuffer := ringbuffer.NewRingBuffer("testRingBuffer", context.Background())
	nodeName := "testNode"

	mt.Run("successful insertion", func(mt *mtest.T) {
		mt.AddMockResponses(
			mtest.CreateSuccessResponse(), // StartTransaction
			mtest.CreateSuccessResponse(), // InsertMany
		)

		connector := &MongoDbStoreConnector{
			client:     mt.Client,
			ringBuffer: ringBuffer,
			nodeName:   nodeName,
			collection: mt.Coll,
		}

		healthEvents := &platformconnector.HealthEvents{
			Events: []*platformconnector.HealthEvent{{ComponentClass: "abc"}},
		}

		err := connector.insertHealthEvents(context.Background(), healthEvents)
		require.NoError(mt, err)
	})

	mt.Run("insertion failure", func(mt *mtest.T) {
		mt.AddMockResponses(
			// InsertMany fails
			mtest.CreateCommandErrorResponse(mtest.CommandError{
				Message: "duplicate key error",
				Name:    "DuplicateKey",
			}),
		)

		connector := &MongoDbStoreConnector{
			client:     mt.Client,
			ringBuffer: ringBuffer,
			nodeName:   nodeName,
			collection: mt.Coll,
		}

		healthEvents := &platformconnector.HealthEvents{
			Events: []*platformconnector.HealthEvent{{ComponentClass: "abc"}},
		}

		err := connector.insertHealthEvents(context.Background(), healthEvents)
		require.Error(mt, err)
		require.Contains(mt, err.Error(), "duplicate key error", "error message should contain 'duplicate key error'")
	})
}

func TestFetchAndProcessHealthMetric(t *testing.T) {
	mtOpts := mtest.NewOptions().ClientType(mtest.Mock).ClientOptions(options.Client().SetRetryWrites(false))
	mt := mtest.New(t, mtOpts)

	mt.Run("process health metrics", func(mt *mtest.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		ringBuffer := ringbuffer.NewRingBuffer("testRingBuffer1", ctx)
		nodeName := "testNode1"

		connector := &MongoDbStoreConnector{
			client:     mt.Client,
			ringBuffer: ringBuffer,
			nodeName:   nodeName,
			collection: mt.Coll,
		}

		// mock responses for insertHealthEvents
		mt.AddMockResponses(
			mtest.CreateSuccessResponse(), // StartTransaction
			mtest.CreateSuccessResponse(), // InsertMany
		)

		healthEvent := &platformconnector.HealthEvent{}

		healthEvents := &platformconnector.HealthEvents{
			Events: []*platformconnector.HealthEvent{healthEvent},
		}

		ringBuffer.Enqueue(healthEvents)

		require.Equal(t, 1, ringBuffer.CurrentLength())

		go connector.FetchAndProcessHealthMetric(ctx)

		time.Sleep(100 * time.Millisecond)

		// check that the event has been dequeued
		require.Equal(t, 0, ringBuffer.CurrentLength())

		cancel()
	})

	mt.Run("process health metrics when insert fails", func(mt *mtest.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		ringBuffer := ringbuffer.NewRingBuffer("testRingBuffer2", ctx)
		nodeName := "testNode2"

		connector := &MongoDbStoreConnector{
			client:     mt.Client,
			ringBuffer: ringBuffer,
			nodeName:   nodeName,
			collection: mt.Coll,
		}

		// mock responses for insertHealthEvents
		mt.AddMockResponses(
			mtest.CreateSuccessResponse(), // StartTransaction
			// InsertMany fails
			mtest.CreateCommandErrorResponse(mtest.CommandError{
				Message: "duplicate key error",
				Name:    "DuplicateKey",
			}),
		)

		healthEvent := &platformconnector.HealthEvent{}

		healthEvents := &platformconnector.HealthEvents{
			Events: []*platformconnector.HealthEvent{healthEvent},
		}

		ringBuffer.Enqueue(healthEvents)

		require.Equal(t, 1, ringBuffer.CurrentLength())

		go connector.FetchAndProcessHealthMetric(ctx)

		time.Sleep(100 * time.Millisecond)

		// check that the event has been dequeued
		require.Equal(t, 0, ringBuffer.CurrentLength())

		cancel()
	})
}

func TestBuildCacheFromDB(t *testing.T) {
	mtOpts := mtest.NewOptions().ClientType(mtest.Mock)
	mt := mtest.New(t, mtOpts)

	mt.Run("successful cache build", func(mt *mtest.T) {
		// Mock the aggregation pipeline response with two documents in the first batch
		aggResponse := mtest.CreateCursorResponse(1, "testdb.testcollection", mtest.FirstBatch,
			bson.D{
				{Key: "_id", Value: bson.D{
					{Key: "version", Value: int64(1)},
					{Key: "agent", Value: "agent1"},
					{Key: "componentClass", Value: "class1"},
					{Key: "checkName", Value: "check1"},
					{Key: "entityType", Value: "type1"},
					{Key: "entityValue", Value: "value1"},
				}},
				{Key: "doc", Value: bson.D{
					{Key: "healthevent", Value: bson.D{
						{Key: "isfatal", Value: true},
						{Key: "ishealthy", Value: false},
					}},
				}},
			},
			bson.D{
				{Key: "_id", Value: bson.D{
					{Key: "version", Value: int64(2)},
					{Key: "agent", Value: "agent2"},
					{Key: "componentClass", Value: "class2"},
					{Key: "checkName", Value: "check2"},
					{Key: "entityType", Value: "type2"},
					{Key: "entityValue", Value: "value2"},
				}},
				{Key: "doc", Value: bson.D{
					{Key: "healthevent", Value: bson.D{
						{Key: "isfatal", Value: false},
						{Key: "ishealthy", Value: true},
					}},
				}},
			},
		)

		aggCursor := mtest.CreateCursorResponse(0, "testdb.testcollection", mtest.NextBatch)

		mt.AddMockResponses(aggResponse, aggCursor)

		connector := &MongoDbStoreConnector{
			client:     mt.Client,
			collection: mt.Coll,
		}

		cache, err := connector.buildCacheFromDB(context.Background(), "testNode")
		require.NoError(t, err)
		require.Len(t, cache, 2)

		expectedKey1 := buildCacheKey("1", "agent1", "class1", "check1", "type1", "value1")
		expectedKey2 := buildCacheKey("2", "agent2", "class2", "check2", "type2", "value2")

		require.Equal(t, cachedEntityState{IsFatal: true, IsHealthy: false}, cache[expectedKey1])
		require.Equal(t, cachedEntityState{IsFatal: false, IsHealthy: true}, cache[expectedKey2])
	})

	mt.Run("aggregation failure", func(mt *mtest.T) {
		commandError := mtest.CommandError{
			Code:    1,
			Name:    "AggregationError",
			Message: "Aggregation pipeline failed",
		}
		mt.AddMockResponses(mtest.CreateCommandErrorResponse(commandError))

		connector := &MongoDbStoreConnector{
			client:     mt.Client,
			collection: mt.Coll,
		}

		_, err := connector.buildCacheFromDB(context.Background(), "testNode")
		require.Error(t, err)
		require.Contains(t, err.Error(), "failed to run aggregation pipeline")
	})
}

func TestUpdateCache(t *testing.T) {
	changedEvents := []*platformconnector.HealthEvent{
		{
			Version:        1,
			Agent:          "agent1",
			ComponentClass: "class1",
			CheckName:      "check1",
			IsFatal:        false,
			IsHealthy:      true,
			EntitiesImpacted: []*platformconnector.Entity{
				{
					EntityType:  "type1",
					EntityValue: "value1",
				},
			},
		},
	}

	connector := &MongoDbStoreConnector{
		entityCache: map[string]cachedEntityState{
			"1|agent1|class1|check1|type1|value1": {IsFatal: true, IsHealthy: false},
		},
	}

	connector.updateCache(changedEvents)

	expectedKey := buildCacheKey("1", "agent1", "class1", "check1", "type1", "value1")
	expectedState := cachedEntityState{IsFatal: false, IsHealthy: true}

	require.Equal(t, expectedState, connector.entityCache[expectedKey], "Cache should be updated with new state")
}

func TestFilterStateChangedEvents(t *testing.T) {
	events := []*platformconnector.HealthEvent{
		{
			Version:        1,
			Agent:          "agent1",
			ComponentClass: "class1",
			CheckName:      "check1",
			IsFatal:        true,
			IsHealthy:      false,
			EntitiesImpacted: []*platformconnector.Entity{
				{
					EntityType:  "type1",
					EntityValue: "value1",
				},
			},
		},
		{
			Version:        2,
			Agent:          "agent2",
			ComponentClass: "class2",
			CheckName:      "check2",
			IsFatal:        true,
			IsHealthy:      true,
			EntitiesImpacted: []*platformconnector.Entity{
				{
					EntityType:  "type2",
					EntityValue: "value2",
				},
			},
		},
		{
			Version:        3,
			Agent:          "agent3",
			ComponentClass: "class3",
			CheckName:      "check3",
			IsFatal:        false,
			IsHealthy:      true,
			EntitiesImpacted: []*platformconnector.Entity{
				{
					EntityType:  "type3",
					EntityValue: "value3",
				},
			},
		},
	}

	connector := &MongoDbStoreConnector{
		entityCache: map[string]cachedEntityState{
			"1|agent1|class1|check1|type1|value1": {IsFatal: true, IsHealthy: false},
			"2|agent2|class2|check2|type2|value2": {IsFatal: false, IsHealthy: true},
		},
	}

	changedEvents := connector.filterStateChangedEvents(events)

	require.Len(t, changedEvents, 2, "Two events should be marked as changed")

	require.Equal(t, uint32(2), changedEvents[0].Version)
	require.Equal(t, "agent2", changedEvents[0].Agent)

	require.Equal(t, uint32(3), changedEvents[1].Version)
	require.Equal(t, "agent3", changedEvents[1].Agent)
}
