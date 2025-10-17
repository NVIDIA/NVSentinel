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

package queue

import (
	"context"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"k8s.io/client-go/util/workqueue"
)

type NodeEvent struct {
	NodeName   string
	Event      *bson.M
	Collection MongoCollectionAPI
}

type MongoCollectionAPI interface {
	UpdateOne(ctx context.Context, filter interface{}, update interface{},
		opts ...*options.UpdateOptions) (*mongo.UpdateResult, error)
	FindOne(ctx context.Context, filter interface{}, opts ...*options.FindOneOptions) *mongo.SingleResult
	Find(ctx context.Context, filter interface{}, opts ...*options.FindOptions) (*mongo.Cursor, error)
}

const (
	maxRetries = 5
)

type EventQueueManager interface {
	EnqueueEvent(ctx context.Context, nodeName string, event bson.M, collection MongoCollectionAPI) error
	Start(ctx context.Context)
	Shutdown()
	SetEventProcessor(processor EventProcessor)
}

type eventQueueManager struct {
	queue          workqueue.TypedRateLimitingInterface[NodeEvent]
	eventProcessor EventProcessor
	shutdown       chan struct{}
}
