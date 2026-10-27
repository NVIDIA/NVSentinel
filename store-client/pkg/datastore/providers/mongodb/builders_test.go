// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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
	"testing"

	"github.com/stretchr/testify/assert"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestMongoFilterBuilder(t *testing.T) {
	cutoff := 10
	clause := bson.M{"a": 1}

	filter := NewMongoBuilderFactory().NewFilterBuilder().
		Eq("node", "node-1").
		Ne("status", nil).
		Gt("count", 1).
		Lte("createdAt", cutoff).
		In("type", []string{"x", "y"}).
		And(clause).
		Build()

	assert.Equal(t, bson.M{
		"node":      "node-1",
		"status":    bson.M{"$ne": nil},
		"count":     bson.M{"$gt": 1},
		"createdAt": bson.M{"$lte": cutoff},
		"type":      bson.M{"$in": []string{"x", "y"}},
		"$and":      []any{clause},
	}, filter)
}
