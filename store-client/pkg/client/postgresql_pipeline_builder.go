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

// PostgreSQLPipelineBuilder creates pipelines optimized for PostgreSQL changelog-based change detection
// PostgreSQL uses database triggers to capture INSERT/UPDATE/DELETE operations in a changelog table.
// The pipelines are evaluated client-side via PipelineFilter, not server-side like MongoDB.
type PostgreSQLPipelineBuilder struct{}

// NewPostgreSQLPipelineBuilder creates a new PostgreSQL pipeline builder
func NewPostgreSQLPipelineBuilder() *PostgreSQLPipelineBuilder {
	return &PostgreSQLPipelineBuilder{}
}

// BuildNodeQuarantineStatusPipeline creates a pipeline for PostgreSQL changestreams
//
// PostgreSQL behavior:
//   - Health events are inserted WITHOUT quarantine status
//   - fault-quarantine UPDATES the node_quarantined column
//   - PostgreSQL trigger should emit UPDATE event in changelog
//   - We also watch INSERT events as a failsafe in case of trigger issues
//
// This pipeline matches BOTH UPDATE and INSERT operations to be defensive:
//  1. UPDATE operations (primary path) - when fault-quarantine updates existing events
//  2. INSERT operations with status already set (failsafe path) - for edge cases
func (b *PostgreSQLPipelineBuilder) BuildNodeQuarantineStatusPipeline() datastore.Pipeline {
	return datastore.ToPipeline(
		datastore.D(
			datastore.E("$match", datastore.D(
				datastore.E("$or", datastore.A(
					// ============================================================
					// Case 1: UPDATE operations (PRIMARY PATH)
					// ============================================================
					// This matches when quarantine status is CHANGED via UPDATE.
					// This is the expected path for both MongoDB and PostgreSQL.
					datastore.D(
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
					),

					// ============================================================
					// Case 2: INSERT operations with status already set (FAILSAFE PATH)
					// ============================================================
					// This matches when an event is INSERTED with quarantine status already present.
					// This is a PostgreSQL-specific failsafe in case:
					//   - The UPDATE changelog trigger doesn't fire correctly
					//   - There are timing issues with trigger execution
					//   - Events are inserted with status pre-populated (edge case)
					datastore.D(
						datastore.E("operationType", "insert"),
						datastore.E("fullDocument.healtheventstatus.nodequarantined", datastore.D(
							datastore.E("$in", datastore.A(
								string(model.Quarantined),
								string(model.AlreadyQuarantined),
								string(model.UnQuarantined),
								string(model.Cancelled),
							)),
						)),
					),
				)),
			)),
		),
	)
}

// BuildAllHealthEventInsertsPipeline creates a pipeline that watches for all health event inserts
// Same behavior as MongoDB - INSERT operations only.
func (b *PostgreSQLPipelineBuilder) BuildAllHealthEventInsertsPipeline() datastore.Pipeline {
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
// For PostgreSQL, we need to handle both INSERT and UPDATE operations because platform-connectors
// may insert a record and then immediately update it, causing the trigger to fire UPDATE events.
func (b *PostgreSQLPipelineBuilder) BuildNonFatalUnhealthyInsertsPipeline() datastore.Pipeline {
	return datastore.ToPipeline(
		datastore.D(
			datastore.E("$match", datastore.D(
				datastore.E("operationType", datastore.D(datastore.E("$in", datastore.A("insert", "update")))),
				datastore.E("fullDocument.healthevent.agent", datastore.D(datastore.E("$ne", "health-events-analyzer"))),
				datastore.E("fullDocument.healthevent.ishealthy", false),
			)),
		),
	)
}

// BuildQuarantinedAndDrainedNodesPipeline creates a pipeline for remediation-ready nodes
// Similar to BuildNodeQuarantineStatusPipeline, this supports both UPDATE and INSERT operations
// to be defensive against PostgreSQL trigger edge cases.
func (b *PostgreSQLPipelineBuilder) BuildQuarantinedAndDrainedNodesPipeline() datastore.Pipeline {
	return datastore.ToPipeline(
		datastore.D(
			datastore.E("$match", datastore.D(
				datastore.E("$or", datastore.A(
					// ============================================================
					// Case 1: UPDATE operations (PRIMARY PATH)
					// ============================================================
					datastore.D(
						datastore.E("operationType", "update"),
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
							// Watch for cancelled quarantine events
							datastore.D(
								datastore.E("fullDocument.healtheventstatus.nodequarantined", string(model.Cancelled)),
							),
						)),
					),

					// ============================================================
					// Case 2: INSERT operations with ready status (FAILSAFE PATH)
					// ============================================================
					// Match if event is inserted with remediation-ready status already set
					datastore.D(
						datastore.E("operationType", "insert"),
						datastore.E("$or", datastore.A(
							datastore.D(
								datastore.E("fullDocument.healtheventstatus.userpodsevictionstatus.status", datastore.D(
									datastore.E("$in", datastore.A(string(model.StatusSucceeded), string(model.AlreadyDrained))),
								)),
								datastore.E("fullDocument.healtheventstatus.nodequarantined", datastore.D(
									datastore.E("$in", datastore.A(string(model.Quarantined), string(model.AlreadyQuarantined))),
								)),
							),
							datastore.D(
								datastore.E("fullDocument.healtheventstatus.nodequarantined", string(model.UnQuarantined)),
								datastore.E("fullDocument.healtheventstatus.userpodsevictionstatus.status", string(model.StatusSucceeded)),
							),
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
