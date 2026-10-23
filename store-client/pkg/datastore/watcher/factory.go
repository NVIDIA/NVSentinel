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

package watcher

// Config holds configuration for creating change stream watchers
type Config struct {
	// ClientName is a unique identifier for the change stream client (used for resume tokens)
	ClientName string

	// TableName is the table/collection name to watch for changes
	TableName string

	// Pipeline is an optional custom aggregation pipeline for filtering change stream events
	// The type is interface{} to support different providers (e.g., mongo.Pipeline for MongoDB)
	Pipeline any
}
