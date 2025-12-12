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
	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
)

// MongoDBPipelineBuilder creates pipelines optimized for MongoDB change streams
// MongoDB change streams emit events when documents are inserted, updated, or deleted.
// For status changes, MongoDB only emits UPDATE events (never INSERT with status already set).
type MongoDBPipelineBuilder struct{}

// NewMongoDBPipelineBuilder creates a new MongoDB pipeline builder
func NewMongoDBPipelineBuilder() *MongoDBPipelineBuilder {
	return &MongoDBPipelineBuilder{}
}

// BuildNodeQuarantineStatusPipeline creates a pipeline for MongoDB change streams
//
// MongoDB behavior:
//   - Health events are inserted WITHOUT quarantine status
//   - fault-quarantine UPDATES the document to set nodequarantined field
//   - MongoDB change stream emits UPDATE event with updateDescription.updatedFields
//
// Therefore, we only need to watch for UPDATE operations with updateDescription.
func (b *MongoDBPipelineBuilder) BuildNodeQuarantineStatusPipeline() datastore.Pipeline {
	return datastore.ToPipeline(
		datastore.D(
			datastore.E("$match", datastore.D(
				datastore.E("operationType", "update"),
				datastore.E("$or", datastore.A(
					datastore.D(
						datastore.E("updateDescription.updatedFields", datastore.D(
							datastore.E("healtheventstatus.nodequarantined", string(model.Quarantined)),
						)),
					),
					datastore.D(
						datastore.E("updateDescription.updatedFields", datastore.D(
							datastore.E("healtheventstatus.nodequarantined", string(model.AlreadyQuarantined)),
						)),
					),
					datastore.D(
						datastore.E("updateDescription.updatedFields", datastore.D(
							datastore.E("healtheventstatus.nodequarantined", string(model.UnQuarantined)),
						)),
					),
					datastore.D(
						datastore.E("updateDescription.updatedFields", datastore.D(
							datastore.E("healtheventstatus.nodequarantined", string(model.Cancelled)),
						)),
					),
				)),
			)),
		),
	)
}

// BuildAllHealthEventInsertsPipeline creates a pipeline that watches for all health event inserts
// This is used by fault-quarantine to detect all new health events without filtering.
func (b *MongoDBPipelineBuilder) BuildAllHealthEventInsertsPipeline() datastore.Pipeline {
	return datastore.ToPipeline(
		datastore.D(
			datastore.E("$match", datastore.D(
				datastore.E("operationType", datastore.D(
					datastore.E("$in", datastore.A("insert")),
				)),
			)),
		),
	)
}

// BuildNonFatalUnhealthyInsertsPipeline creates a pipeline for non-fatal, unhealthy event inserts
// This is used by health-events-analyzer to detect warning-level health events for pattern analysis.
func (b *MongoDBPipelineBuilder) BuildNonFatalUnhealthyInsertsPipeline() datastore.Pipeline {
	return datastore.ToPipeline(
		datastore.D(
			datastore.E("$match", datastore.D(
				datastore.E("operationType", "insert"),
				datastore.E("fullDocument.healthevent.agent", datastore.D(datastore.E("$ne", "health-events-analyzer"))),
				datastore.E("fullDocument.healthevent.ishealthy", false),
			)),
		),
	)
}

// BuildQuarantinedAndDrainedNodesPipeline creates a pipeline for remediation-ready nodes
// This watches for insert/update events where both quarantine and eviction status indicate the
// node is ready for reboot, or where the node has been unquarantined and needs cleanup, or where
// quarantine was cancelled. Note: Both INSERT and UPDATE operations are supported to handle
// database-specific semantics (MongoDB uses "update" for replaceOne operations, PostgreSQL uses
// "insert" for new healthy events).
func (b *MongoDBPipelineBuilder) BuildQuarantinedAndDrainedNodesPipeline() datastore.Pipeline {
	return datastore.ToPipeline(
		datastore.D(
			datastore.E("$match", datastore.D(
				datastore.E("operationType", datastore.D(
					datastore.E("$in", datastore.A("insert", "update")),
				)),
				datastore.E("$or", datastore.A(
					// Watch for quarantine events (for remediation)
					datastore.D(
						datastore.E("fullDocument.healtheventstatus.userpodsevictionstatus.status", datastore.D(
							datastore.E("$in", datastore.A(string(model.StatusSucceeded), string(model.AlreadyDrained))),
						)),
						datastore.E("fullDocument.healtheventstatus.nodequarantined", datastore.D(
							datastore.E("$in", datastore.A(string(model.Quarantined), string(model.AlreadyQuarantined))),
						)),
					),
					// Watch for unquarantine events (for annotation cleanup)
					datastore.D(
						datastore.E("fullDocument.healtheventstatus.nodequarantined", string(model.UnQuarantined)),
						datastore.E("fullDocument.healtheventstatus.userpodsevictionstatus.status", string(model.StatusSucceeded)),
					),
					// Watch for cancelled quarantine events (for annotation cleanup)
					datastore.D(
						datastore.E("fullDocument.healtheventstatus.nodequarantined", string(model.Cancelled)),
					),
				)),
			)),
		),
	)
}
