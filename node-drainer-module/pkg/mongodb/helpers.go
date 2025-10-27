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

package mongodb

import (
	"context"
	"errors"
	"fmt"

	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	"github.com/nvidia/nvsentinel/node-drainer-module/pkg/queue"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

func IsNodeAlreadyDrained(ctx context.Context, collection queue.MongoCollectionAPI,
	nodeName string) (bool, error) {
	filter := bson.M{
		"healthevent.nodename": nodeName,
		"healtheventstatus.nodequarantined": bson.M{
			"$in": []string{string(model.Quarantined), string(model.UnQuarantined)},
		},
	}

	// ObjectID contains timestamp, sort descending to get latest
	opts := options.FindOne().SetSort(bson.D{bson.E{Key: "_id", Value: -1}})

	var result bson.M
	if err := collection.FindOne(ctx, filter, opts).Decode(&result); err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return false, nil
		}

		return false, fmt.Errorf("failed to query latest event for node %s: %w", nodeName, err)
	}

	healthEventStatus, ok := result["healtheventstatus"].(bson.M)
	if !ok {
		return false, fmt.Errorf("invalid healtheventstatus format for node %s", nodeName)
	}

	nodeQuarantined, ok := healthEventStatus["nodequarantined"].(string)
	if !ok {
		return false, fmt.Errorf("invalid nodequarantined format for node %s", nodeName)
	}

	if nodeQuarantined == string(model.UnQuarantined) {
		return false, nil
	}

	userPodsEvictionStatus, ok := healthEventStatus["userpodsevictionstatus"].(bson.M)
	if !ok {
		return false, nil
	}

	drainStatus, ok := userPodsEvictionStatus["status"].(string)
	if !ok {
		return false, nil
	}

	return drainStatus == string(model.StatusSucceeded), nil
}
