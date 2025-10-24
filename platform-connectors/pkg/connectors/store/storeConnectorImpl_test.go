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

package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	platformconnector "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/ringbuffer"
	"github.com/stretchr/testify/require"
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

func TestGetEnvAsInt(t *testing.T) {
	tests := []struct {
		name         string
		envName      string
		envValue     string
		defaultValue int
		expectedVal  int
		expectError  bool
	}{
		{
			name:         "Environment variable not set, use default",
			envName:      "TEST_ENV_NOT_SET",
			envValue:     "",
			defaultValue: 100,
			expectedVal:  100,
			expectError:  false,
		},
		{
			name:         "Environment variable set to valid positive integer",
			envName:      "TEST_ENV_VALID",
			envValue:     "250",
			defaultValue: 100,
			expectedVal:  250,
			expectError:  false,
		},
		{
			name:         "Environment variable set to invalid string",
			envName:      "TEST_ENV_INVALID",
			envValue:     "not_a_number",
			defaultValue: 100,
			expectedVal:  0,
			expectError:  true,
		},
		{
			name:         "Environment variable set to zero",
			envName:      "TEST_ENV_ZERO",
			envValue:     "0",
			defaultValue: 100,
			expectedVal:  0,
			expectError:  true,
		},
		{
			name:         "Environment variable set to negative number",
			envName:      "TEST_ENV_NEGATIVE",
			envValue:     "-10",
			defaultValue: 100,
			expectedVal:  0,
			expectError:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Set or unset the environment variable
			if tt.envValue != "" {
				os.Setenv(tt.envName, tt.envValue)
				defer os.Unsetenv(tt.envName)
			} else {
				os.Unsetenv(tt.envName)
			}

			result, err := getEnvAsInt(tt.envName, tt.defaultValue)

			if tt.expectError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				require.Equal(t, tt.expectedVal, result)
			}
		})
	}
}

func TestConfirmConnectivityWithDBAndCollection(t *testing.T) {
	mtOpts := mtest.NewOptions().ClientType(mtest.Mock).ClientOptions(options.Client().SetRetryWrites(false))
	mt := mtest.New(t, mtOpts)

	mt.Run("successful connectivity", func(mt *mtest.T) {
		mt.AddMockResponses(
			mtest.CreateSuccessResponse(),
			mtest.CreateCursorResponse(0, "testdb.$cmd.listCollections", mtest.FirstBatch, bson.D{
				{Key: "name", Value: "testcollection"},
			}),
		)

		ctx := context.Background()

		err := confirmConnectivityWithDBAndCollection(ctx, mt.Client, "testdb", "testcollection", 1*time.Second, 100*time.Millisecond)
		require.NoError(mt, err)
	})

	mt.Run("ping fails and times out", func(mt *mtest.T) {
		mt.AddMockResponses(
			mtest.CreateCommandErrorResponse(mtest.CommandError{
				Message: "ping failed",
				Name:    "NetworkError",
			}),
		)

		ctx := context.Background()

		err := confirmConnectivityWithDBAndCollection(ctx, mt.Client, "testdb", "testcollection", 500*time.Millisecond, 100*time.Millisecond)
		require.Error(mt, err)
		require.Contains(mt, err.Error(), "retrying ping to database testdb timed out")
	})

	mt.Run("collection not found", func(mt *mtest.T) {
		mt.AddMockResponses(
			mtest.CreateSuccessResponse(),
			mtest.CreateCursorResponse(0, "testdb.$cmd.listCollections", mtest.FirstBatch),
		)

		ctx := context.Background()

		err := confirmConnectivityWithDBAndCollection(ctx, mt.Client, "testdb", "testcollection", 1*time.Second, 100*time.Millisecond)
		require.Error(mt, err)
		require.Contains(mt, err.Error(), "no collection with name testcollection for DB testdb was found")
	})

	mt.Run("multiple collections with same name", func(mt *mtest.T) {
		mt.AddMockResponses(
			mtest.CreateSuccessResponse(),
			mtest.CreateCursorResponse(0, "testdb.$cmd.listCollections", mtest.FirstBatch,
				bson.D{{Key: "name", Value: "testcollection"}},
				bson.D{{Key: "name", Value: "testcollection"}},
			),
		)

		ctx := context.Background()

		err := confirmConnectivityWithDBAndCollection(ctx, mt.Client, "testdb", "testcollection", 1*time.Second, 100*time.Millisecond)
		require.Error(mt, err)
		require.Contains(mt, err.Error(), "more than one collection with name testcollection for DB testdb was found")
	})

	mt.Run("list collections error", func(mt *mtest.T) {
		mt.AddMockResponses(
			mtest.CreateSuccessResponse(),
			mtest.CreateCommandErrorResponse(mtest.CommandError{
				Code:    13,
				Message: "not authorized",
			}),
		)

		ctx := context.Background()

		err := confirmConnectivityWithDBAndCollection(ctx, mt.Client, "testdb", "testcollection", 1*time.Second, 100*time.Millisecond)
		require.Error(mt, err)
		require.Contains(mt, err.Error(), "unable to get list of collections for DB testdb")
	})
}

func TestInsertHealthEvents_ErrorScenarios(t *testing.T) {
	mtOpts := mtest.NewOptions().ClientType(mtest.Mock).ClientOptions(options.Client().SetRetryWrites(false))
	mt := mtest.New(t, mtOpts)

	mt.Run("session start fails", func(mt *mtest.T) {
		ctx := context.Background()
		ringBuffer := ringbuffer.NewRingBuffer("testRingBufferSessionFail", ctx)
		nodeName := "testNode"

		connector := &MongoDbStoreConnector{
			client:     mt.Client,
			ringBuffer: ringBuffer,
			nodeName:   nodeName,
			collection: mt.Coll,
		}

		healthEvents := &platformconnector.HealthEvents{
			Events: []*platformconnector.HealthEvent{
				{
					ComponentClass: "gpu",
					CheckName:      "GpuXidError",
					NodeName:       nodeName,
				},
			},
		}

		mt.AddMockResponses(
			mtest.CreateCommandErrorResponse(mtest.CommandError{
				Code:    244,
				Message: "Cannot start session",
			}),
		)

		err := connector.insertHealthEvents(ctx, healthEvents)
		require.Error(t, err)
	})

	mt.Run("transaction fails", func(mt *mtest.T) {
		ctx := context.Background()
		ringBuffer := ringbuffer.NewRingBuffer("testRingBufferTxnFail", ctx)
		nodeName := "testNode"

		connector := &MongoDbStoreConnector{
			client:     mt.Client,
			ringBuffer: ringBuffer,
			nodeName:   nodeName,
			collection: mt.Coll,
		}

		healthEvents := &platformconnector.HealthEvents{
			Events: []*platformconnector.HealthEvent{
				{
					ComponentClass: "gpu",
					CheckName:      "GpuXidError",
					NodeName:       nodeName,
				},
			},
		}

		mt.AddMockResponses(
			mtest.CreateSuccessResponse(), // StartSession
			mtest.CreateCommandErrorResponse(mtest.CommandError{
				Code:    251,
				Message: "Transaction aborted",
			}),
		)

		err := connector.insertHealthEvents(ctx, healthEvents)
		require.Error(t, err)
	})
}

func TestPollTillCACertIsSuccessfullyMounted(t *testing.T) {
	t.Run("cert available immediately", func(t *testing.T) {
		tempDir, err := os.MkdirTemp("", "test_cert_poll")
		require.NoError(t, err)
		defer os.RemoveAll(tempDir)

		certPath := filepath.Join(tempDir, "ca.crt")
		certContent := []byte("test certificate content")
		err = os.WriteFile(certPath, certContent, 0644)
		require.NoError(t, err)

		result, err := pollTillCACertIsMountedSuccessfully(certPath, 5*time.Second, 1*time.Second)
		require.NoError(t, err)
		require.Equal(t, certContent, result)
	})

	t.Run("cert not available and times out", func(t *testing.T) {
		certPath := "/nonexistent/path/ca.crt"

		start := time.Now()
		result, err := pollTillCACertIsMountedSuccessfully(certPath, 2*time.Second, 500*time.Millisecond)
		elapsed := time.Since(start)

		require.Error(t, err)
		require.Nil(t, result)
		require.Contains(t, err.Error(), "retrying reading CA cert from")
		require.GreaterOrEqual(t, elapsed, 2*time.Second)
	})

	t.Run("cert becomes available after retry", func(t *testing.T) {
		tempDir, err := os.MkdirTemp("", "test_cert_delayed")
		require.NoError(t, err)
		defer os.RemoveAll(tempDir)

		certPath := filepath.Join(tempDir, "ca.crt")
		certContent := []byte("delayed certificate content")

		go func() {
			time.Sleep(1 * time.Second)
			os.WriteFile(certPath, certContent, 0644)
		}()

		result, err := pollTillCACertIsMountedSuccessfully(certPath, 5*time.Second, 500*time.Millisecond)
		require.NoError(t, err)
		require.Equal(t, certContent, result)
	})
}
