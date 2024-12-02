package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	platformconnector "gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/platform-connectors/pkg/protos"
	"gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/platform-connectors/pkg/ringbuffer"
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
