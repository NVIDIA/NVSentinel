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
	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
)

// MongoDBPipelineBuilder creates pipelines optimized for MongoDB change streams
// MongoDB change streams emit events when documents are inserted, updated, or deleted.
// For status changes, MongoDB only emits UPDATE events (never INSERT with status already set).
type MongoDBPipelineBuilder struct{}

// NodeQuarantined status field path in health event documents (MongoDB and PostgreSQL).
const nodeQuarantinedStatusField = "healtheventstatus.nodequarantined"

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
	fieldName := nodeQuarantinedStatusField
	getFieldExpr := datastore.D(
		datastore.E("$getField", datastore.D(
			datastore.E("field", fieldName),
			datastore.E("input", "$updateDescription.updatedFields"),
		)),
	)
	makeExprEq := func(status string) datastore.Document {
		return datastore.D(datastore.E("$expr", datastore.D(
			datastore.E("$eq", datastore.A(getFieldExpr, status)),
		)))
	}

	return datastore.ToPipeline(
		datastore.D(
			datastore.E(opMatch, datastore.D(
				datastore.E(fieldOperationType, "update"),
				datastore.E("$or", datastore.A(
					makeExprEq(string(model.Quarantined)),
					makeExprEq(string(model.AlreadyQuarantined)),
					makeExprEq(string(model.UnQuarantined)),
					makeExprEq(string(model.Cancelled)),
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
			datastore.E(opMatch, datastore.D(
				datastore.E(fieldOperationType, datastore.D(
					datastore.E(opIn, datastore.A(opTypeInsert)),
				)),
			)),
		),
	)
}

// BuildProcessableHealthEventInsertsPipeline creates a pipeline that watches for
// all EXECUTE_REMEDIATION health event inserts.
//
// Backward Compatibility: This pipeline uses $or to match events where processingstrategy is either:
//   - EXECUTE_REMEDIATION (new events from NVSentinel health monitors)
//   - Missing/null (old events created before upgrade, custom monitors, or circuit breaker backlog)
func (b *MongoDBPipelineBuilder) BuildProcessableHealthEventInsertsPipeline() datastore.Pipeline {
	return datastore.ToPipeline(
		datastore.D(
			datastore.E(opMatch, datastore.D(
				datastore.E(fieldOperationType, datastore.D(
					datastore.E(opIn, datastore.A(opTypeInsert)),
				)),
				datastore.E("$or", datastore.A(
					datastore.D(datastore.E(
						"fullDocument.healthevent.processingstrategy",
						int32(protos.ProcessingStrategy_EXECUTE_REMEDIATION),
					)),
					datastore.D(datastore.E(
						"fullDocument.healthevent.processingstrategy",
						datastore.D(datastore.E("$exists", false)),
					)),
				)),
			)),
		),
	)
}

// BuildProcessableNonFatalUnhealthyInsertsPipeline creates a pipeline for non-fatal,
// unhealthy event inserts. This is used by health-events-analyzer for pattern analysis.
// STORE_AND_ANALYSE source events are included as analyzer input; STORE_ONLY events
// remain observability-only and are excluded.
func (b *MongoDBPipelineBuilder) BuildProcessableNonFatalUnhealthyInsertsPipeline() datastore.Pipeline {
	return datastore.ToPipeline(
		datastore.D(
			datastore.E(opMatch, datastore.D(
				datastore.E(fieldOperationType, opTypeInsert),
				datastore.E("fullDocument.healthevent.agent", datastore.D(datastore.E(opNE, "health-events-analyzer"))),
				datastore.E("fullDocument.healthevent.ishealthy", false),
				datastore.E("$or", datastore.A(
					datastore.D(datastore.E(
						"fullDocument.healthevent.processingstrategy",
						int32(protos.ProcessingStrategy_EXECUTE_REMEDIATION),
					)),
					datastore.D(datastore.E(
						"fullDocument.healthevent.processingstrategy",
						int32(protos.ProcessingStrategy_STORE_AND_ANALYSE),
					)),
					datastore.D(datastore.E(
						"fullDocument.healthevent.processingstrategy",
						datastore.D(datastore.E("$exists", false)),
					)),
				)),
			)),
		),
	)
}

// userPodsEvictionStatusField is the pod eviction status of a health event.
const userPodsEvictionStatusField = "healtheventstatus.userpodsevictionstatus"

// evictionStatusWritten matches an update that set the pod eviction status to one of
// statuses. MongoDB reports a $set of the whole status under its own key. Amazon DocumentDB
// reports only the fields whose value changed, and evaluates $objectToArray of
// updatedFields to null, so the status is read with $getField under both keys.
func evictionStatusWritten(statuses ...string) datastore.Element {
	written := func(field string) datastore.Document {
		return datastore.D(datastore.E("$getField", datastore.D(
			datastore.E("field", field),
			datastore.E("input", "$updateDescription.updatedFields"),
		)))
	}
	statusOf := func(evictionStatus datastore.Document) datastore.Document {
		return datastore.D(datastore.E("$getField", datastore.D(
			datastore.E("field", "status"),
			datastore.E("input", evictionStatus),
		)))
	}

	matches := datastore.A()
	for _, status := range statuses {
		matches = append(matches,
			datastore.D(datastore.E("$eq", datastore.A(statusOf(written(userPodsEvictionStatusField)), status))),
			datastore.D(datastore.E("$eq", datastore.A(written(userPodsEvictionStatusField+".status"), status))),
		)
	}

	return datastore.E("$expr", datastore.D(datastore.E("$or", matches)))
}

// BuildQuarantinedAndDrainedNodesPipeline creates a pipeline for remediation-ready nodes
// This watches for insert/update events where both quarantine and eviction status indicate the
// node is ready for reboot, or where the node has been unquarantined and needs cleanup, or where
// quarantine was cancelled.
func (b *MongoDBPipelineBuilder) BuildQuarantinedAndDrainedNodesPipeline() datastore.Pipeline {
	return datastore.ToPipeline(
		datastore.D(
			datastore.E(opMatch, datastore.D(
				datastore.E("$or", datastore.A(
					// ============================================================
					// Case 1: UPDATE operations (PRIMARY PATH)
					// ============================================================
					datastore.D(
						datastore.E(fieldOperationType, "update"),
						datastore.E("$or", datastore.A(
							datastore.D(
								evictionStatusWritten(string(model.StatusSucceeded), string(model.AlreadyDrained)),
								datastore.E("fullDocument.healtheventstatus.userpodsevictionstatus.status", datastore.D(
									datastore.E(opIn, datastore.A(string(model.StatusSucceeded), string(model.AlreadyDrained))),
								)),
								datastore.E("fullDocument.healtheventstatus.nodequarantined", datastore.D(
									datastore.E(opIn, datastore.A(string(model.Quarantined), string(model.AlreadyQuarantined))),
								)),
							),
							// Watch for unquarantine events (for annotation cleanup)
							datastore.D(
								evictionStatusWritten(string(model.StatusSucceeded)),
								datastore.E("fullDocument.healtheventstatus.nodequarantined", string(model.UnQuarantined)),
								datastore.E("fullDocument.healtheventstatus.userpodsevictionstatus.status", string(model.StatusSucceeded)),
							),
							// Watch for cancelled quarantine events (for annotation cleanup)
							datastore.D(
								datastore.E(fieldUpdatedFields, datastore.D(
									datastore.E(nodeQuarantinedStatusField, string(model.Cancelled)),
								)),
							),
						)),
					),

					// ============================================================
					// Case 2: INSERT operations (FAILSAFE PATH)
					// ============================================================
					// Match if event is inserted with remediation-ready status already set
					datastore.D(
						datastore.E(fieldOperationType, opTypeInsert),
						datastore.E("$or", datastore.A(
							// Watch for quarantine events (for remediation)
							datastore.D(
								datastore.E("fullDocument.healtheventstatus.userpodsevictionstatus.status", datastore.D(
									datastore.E(opIn, datastore.A(string(model.StatusSucceeded), string(model.AlreadyDrained))),
								)),
								datastore.E("fullDocument.healtheventstatus.nodequarantined", datastore.D(
									datastore.E(opIn, datastore.A(string(model.Quarantined), string(model.AlreadyQuarantined))),
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
					),
				)),
			)),
		),
	)
}
