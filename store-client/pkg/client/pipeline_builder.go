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
	"os"

	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
)

// PipelineBuilder creates database-agnostic pipelines for change stream filtering
// Different database providers can implement this interface to provide optimized pipelines
// based on their specific change stream behavior and capabilities.
type PipelineBuilder interface {
	// BuildNodeQuarantineStatusPipeline creates a pipeline that watches for node quarantine status changes
	// Used by: node-drainer to detect when nodes need draining
	BuildNodeQuarantineStatusPipeline() datastore.Pipeline

	// BuildAllHealthEventInsertsPipeline creates a pipeline that watches for all health event inserts
	// Used by: fault-quarantine to detect new health events
	BuildAllHealthEventInsertsPipeline() datastore.Pipeline

	// BuildNonFatalUnhealthyInsertsPipeline creates a pipeline for non-fatal unhealthy events
	// Used by: health-events-analyzer for pattern analysis
	BuildNonFatalUnhealthyInsertsPipeline() datastore.Pipeline

	// BuildQuarantinedAndDrainedNodesPipeline creates a pipeline for remediation-ready nodes
	// Used by: fault-remediation to detect when nodes are ready for reboot
	BuildQuarantinedAndDrainedNodesPipeline() datastore.Pipeline
}

// GetPipelineBuilder returns the appropriate pipeline builder for the current database provider
// The builder is selected based on the DATASTORE_PROVIDER environment variable.
func GetPipelineBuilder() PipelineBuilder {
	provider := os.Getenv("DATASTORE_PROVIDER")

	switch provider {
	case string(datastore.ProviderPostgreSQL):
		return NewPostgreSQLPipelineBuilder()
	case string(datastore.ProviderMongoDB), "":
		// Default to MongoDB for backward compatibility
		return NewMongoDBPipelineBuilder()
	default:
		// Fallback to MongoDB for unknown providers
		return NewMongoDBPipelineBuilder()
	}
}
