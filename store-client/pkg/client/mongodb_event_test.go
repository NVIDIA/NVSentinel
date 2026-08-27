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

package client

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"

	mongoWatcher "github.com/nvidia/nvsentinel/store-client/pkg/datastore/providers/mongodb/watcher"
)

func TestMongoEvent_GetResumeToken(t *testing.T) {
	tokenBytes, err := bson.Marshal(bson.D{{Key: "_data", Value: "8264ABCDEF"}})
	require.NoError(t, err)

	t.Run("returns bson.Raw token injected by the watcher", func(t *testing.T) {
		e := &mongoEvent{rawEvent: mongoWatcher.Event{
			"_resumeToken": bson.Raw(tokenBytes),
		}}

		assert.Equal(t, tokenBytes, e.GetResumeToken())
	})

	t.Run("returns plain byte slice token", func(t *testing.T) {
		e := &mongoEvent{rawEvent: mongoWatcher.Event{
			"_resumeToken": tokenBytes,
		}}

		assert.Equal(t, tokenBytes, e.GetResumeToken())
	})

	t.Run("returns empty token when field is missing", func(t *testing.T) {
		e := &mongoEvent{rawEvent: mongoWatcher.Event{
			"operationType": "insert",
		}}

		assert.Empty(t, e.GetResumeToken())
	})
}
