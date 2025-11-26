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
	"fmt"
	"testing"

	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
)

func TestConvertAgnosticPipelineToMongo_InsertOnly(t *testing.T) {
	// Create an insert-only pipeline using the agnostic builder
	agnosticPipeline := BuildAllHealthEventInsertsPipeline()

	// Convert to MongoDB pipeline
	result, err := ConvertAgnosticPipelineToMongo(agnosticPipeline)
	require.NoError(t, err)

	// Verify it's a mongo.Pipeline
	mongoPipeline, ok := result.(mongo.Pipeline)
	require.True(t, ok, "Result should be mongo.Pipeline")
	require.Len(t, mongoPipeline, 1, "Pipeline should have 1 stage")

	// Verify the structure
	stage := mongoPipeline[0]
	require.Len(t, stage, 1, "Stage should have 1 element")
	require.Equal(t, "$match", stage[0].Key)

	// Extract the match document
	matchDoc, ok := stage[0].Value.(bson.D)
	require.True(t, ok, "Match value should be bson.D")

	// Find operationType
	var foundOpType bool
	for _, elem := range matchDoc {
		if elem.Key == "operationType" {
			foundOpType = true
			// It should be a nested bson.D with $in operator
			opTypeValue, ok := elem.Value.(bson.D)
			require.True(t, ok, "operationType value should be bson.D")
			require.Len(t, opTypeValue, 1)
			require.Equal(t, "$in", opTypeValue[0].Key)
		}
	}
	require.True(t, foundOpType, "Should have operationType field")
}

func TestConvertAgnosticPipelineToMongo_QuarantineUpdate(t *testing.T) {
	// Create a quarantine update pipeline using the agnostic builder
	agnosticPipeline := BuildNodeQuarantineStatusUpdatesPipeline()

	// Convert to MongoDB pipeline
	result, err := ConvertAgnosticPipelineToMongo(agnosticPipeline)
	require.NoError(t, err)

	// Verify it's a mongo.Pipeline
	mongoPipeline, ok := result.(mongo.Pipeline)
	require.True(t, ok, "Result should be mongo.Pipeline")
	require.Len(t, mongoPipeline, 1, "Pipeline should have 1 stage")

	// Verify the structure
	stage := mongoPipeline[0]
	require.Len(t, stage, 1, "Stage should have 1 element")
	require.Equal(t, "$match", stage[0].Key)

	// Extract the match document
	matchDoc, ok := stage[0].Value.(bson.D)
	require.True(t, ok, "Match value should be bson.D, got %T", stage[0].Value)

	// Verify it has operationType and $or
	var foundOpType, foundOr bool
	for _, elem := range matchDoc {
		if elem.Key == "operationType" {
			foundOpType = true
			assert.Equal(t, "update", elem.Value)
		}
		if elem.Key == "$or" {
			foundOr = true
			// Verify $or is a bson.A (array)
			orArray, ok := elem.Value.(bson.A)
			require.True(t, ok, "$or value should be bson.A")
			require.Len(t, orArray, 4, "$or should have 4 conditions (Quarantined, AlreadyQuarantined, UnQuarantined, Cancelled)")

			// Check first condition
			firstCond, ok := orArray[0].(bson.D)
			require.True(t, ok, "First $or condition should be bson.D")
			require.Len(t, firstCond, 1, "First condition should have 1 element")
			require.Equal(t, "updateDescription.updatedFields", firstCond[0].Key)

			// Verify nested structure
			nestedDoc, ok := firstCond[0].Value.(bson.D)
			require.True(t, ok, "Nested value should be bson.D")
			require.Len(t, nestedDoc, 1)
			require.Equal(t, "healtheventstatus.nodequarantined", nestedDoc[0].Key)
			require.Equal(t, "Quarantined", nestedDoc[0].Value)
		}
	}

	require.True(t, foundOpType, "Should have operationType field")
	require.True(t, foundOr, "Should have $or field")
}

func TestQuarantinedAndDrainedPipelineMatchesMainBranch(t *testing.T) {
	// This test ensures our agnostic pipeline produces the expected structure for cross-database compatibility
	// Updated to accept both INSERT and UPDATE operations for PostgreSQL compatibility
	expectedPipeline := mongo.Pipeline{
		bson.D{
			bson.E{Key: "$match", Value: bson.D{
				bson.E{Key: "operationType", Value: bson.D{
					bson.E{Key: "$in", Value: bson.A{"insert", "update"}},
				}},
				bson.E{Key: "$or", Value: bson.A{
					// Watch for quarantine events (for remediation)
					bson.D{
						bson.E{Key: "fullDocument.healtheventstatus.userpodsevictionstatus.status", Value: bson.D{
							bson.E{Key: "$in", Value: bson.A{model.StatusSucceeded, model.AlreadyDrained}},
						}},
						bson.E{Key: "fullDocument.healtheventstatus.nodequarantined", Value: bson.D{
							bson.E{Key: "$in", Value: bson.A{model.Quarantined, model.AlreadyQuarantined}},
						}},
					},
					// Watch for unquarantine events (for annotation cleanup)
					bson.D{
						bson.E{Key: "fullDocument.healtheventstatus.nodequarantined", Value: model.UnQuarantined},
						bson.E{Key: "fullDocument.healtheventstatus.userpodsevictionstatus.status", Value: model.StatusSucceeded},
					},
					// Watch for cancelled quarantine events (for annotation cleanup)
					bson.D{
						bson.E{Key: "fullDocument.healtheventstatus.nodequarantined", Value: model.Cancelled},
					},
				}},
			}},
		},
	}

	// Our branch agnostic pipeline (converted to mongo)
	agnosticPipeline := BuildQuarantinedAndDrainedNodesPipeline()
	convertedInterface, err := ConvertAgnosticPipelineToMongo(agnosticPipeline)
	require.NoError(t, err)

	ourBranchPipeline, ok := convertedInterface.(mongo.Pipeline)
	require.True(t, ok, "Converted pipeline should be mongo.Pipeline")

	// Compare structure
	require.Equal(t, len(expectedPipeline), len(ourBranchPipeline), "Pipeline lengths should match")

	// Compare using string representation for debugging
	expectedStr := fmt.Sprintf("%+v", expectedPipeline)
	actualStr := fmt.Sprintf("%+v", ourBranchPipeline)

	if expectedStr != actualStr {
		t.Log("Expected pipeline:")
		t.Logf("%s", expectedStr)
		t.Log("\nActual pipeline:")
		t.Logf("%s", actualStr)
		t.Fatal("Pipelines do not match")
	}

	// Also verify the structure manually
	expectedStage := expectedPipeline[0]
	actualStage := ourBranchPipeline[0]

	require.Equal(t, len(expectedStage), len(actualStage), "Stage lengths should match")
	require.Equal(t, "$match", expectedStage[0].Key)
	require.Equal(t, "$match", actualStage[0].Key)
}

func TestConvertAgnosticPipelineToMongo_CompareWithDeprecated(t *testing.T) {
	// Create both versions of the quarantine pipeline
	agnosticPipeline := BuildNodeQuarantineStatusUpdatesPipeline()
	deprecatedPipeline := BuildQuarantineUpdatePipeline()

	// Convert the agnostic version
	convertedResult, err := ConvertAgnosticPipelineToMongo(agnosticPipeline)
	require.NoError(t, err)

	convertedPipeline, ok := convertedResult.(mongo.Pipeline)
	require.True(t, ok)

	directPipeline, ok := deprecatedPipeline.(mongo.Pipeline)
	require.True(t, ok)

	// Both should have the same structure
	require.Len(t, convertedPipeline, len(directPipeline), "Pipeline lengths should match")

	// Compare the $match stage structure
	convertedStage := convertedPipeline[0]
	directStage := directPipeline[0]

	require.Equal(t, len(directStage), len(convertedStage), "Stage element counts should match")
	require.Equal(t, directStage[0].Key, convertedStage[0].Key, "Both should have $match")

	// Both match documents should have the same number of fields
	convertedMatch := convertedStage[0].Value.(bson.D)
	directMatch := directStage[0].Value.(bson.D)

	require.Equal(t, len(directMatch), len(convertedMatch),
		"Match documents should have same number of fields")
}

func TestConvertDocumentToBsonD_SimpleFields(t *testing.T) {
	doc := datastore.D(
		datastore.E("field1", "value1"),
		datastore.E("field2", 123),
		datastore.E("field3", true),
	)

	result, err := convertDocumentToBsonD(doc)
	require.NoError(t, err)
	require.Len(t, result, 3)

	assert.Equal(t, "field1", result[0].Key)
	assert.Equal(t, "value1", result[0].Value)
	assert.Equal(t, "field2", result[1].Key)
	assert.Equal(t, 123, result[1].Value)
	assert.Equal(t, "field3", result[2].Key)
	assert.Equal(t, true, result[2].Value)
}

func TestConvertDocumentToBsonD_NestedDocument(t *testing.T) {
	doc := datastore.D(
		datastore.E("outer", datastore.D(
			datastore.E("inner", "value"),
		)),
	)

	result, err := convertDocumentToBsonD(doc)
	require.NoError(t, err)
	require.Len(t, result, 1)

	assert.Equal(t, "outer", result[0].Key)

	innerDoc, ok := result[0].Value.(bson.D)
	require.True(t, ok, "Inner value should be bson.D")
	require.Len(t, innerDoc, 1)
	assert.Equal(t, "inner", innerDoc[0].Key)
	assert.Equal(t, "value", innerDoc[0].Value)
}

func TestConvertDocumentToBsonD_Array(t *testing.T) {
	doc := datastore.D(
		datastore.E("arrayField", datastore.A("item1", "item2", "item3")),
	)

	result, err := convertDocumentToBsonD(doc)
	require.NoError(t, err)
	require.Len(t, result, 1)

	assert.Equal(t, "arrayField", result[0].Key)

	array, ok := result[0].Value.(bson.A)
	require.True(t, ok, "Value should be bson.A")
	require.Len(t, array, 3)
	assert.Equal(t, "item1", array[0])
	assert.Equal(t, "item2", array[1])
	assert.Equal(t, "item3", array[2])
}

func TestConvertDocumentToBsonD_ComplexNesting(t *testing.T) {
	// Simulate the quarantine pipeline structure
	doc := datastore.D(
		datastore.E("$match", datastore.D(
			datastore.E("operationType", "update"),
			datastore.E("$or", datastore.A(
				datastore.D(
					datastore.E("updateDescription.updatedFields", datastore.D(
						datastore.E("healtheventstatus.nodequarantined", "Quarantined"),
					)),
				),
			)),
		)),
	)

	result, err := convertDocumentToBsonD(doc)
	require.NoError(t, err)
	require.Len(t, result, 1)
	require.Equal(t, "$match", result[0].Key)

	matchDoc, ok := result[0].Value.(bson.D)
	require.True(t, ok, "Match value should be bson.D")
	require.Len(t, matchDoc, 2) // operationType and $or

	// Verify operationType
	assert.Equal(t, "operationType", matchDoc[0].Key)
	assert.Equal(t, "update", matchDoc[0].Value)

	// Verify $or
	assert.Equal(t, "$or", matchDoc[1].Key)
	orArray, ok := matchDoc[1].Value.(bson.A)
	require.True(t, ok, "$or value should be bson.A")
	require.Len(t, orArray, 1)

	// Verify the condition inside $or
	condition, ok := orArray[0].(bson.D)
	require.True(t, ok, "Condition should be bson.D")
	require.Len(t, condition, 1)
	assert.Equal(t, "updateDescription.updatedFields", condition[0].Key)

	// Verify the nested field
	nestedDoc, ok := condition[0].Value.(bson.D)
	require.True(t, ok, "Nested document should be bson.D")
	require.Len(t, nestedDoc, 1)
	assert.Equal(t, "healtheventstatus.nodequarantined", nestedDoc[0].Key)
	assert.Equal(t, "Quarantined", nestedDoc[0].Value)
}

func TestConvertValueToBson_PreservesTypes(t *testing.T) {
	tests := []struct {
		name     string
		input    interface{}
		expected interface{}
	}{
		{
			name:     "string",
			input:    "test",
			expected: "test",
		},
		{
			name:     "int",
			input:    42,
			expected: 42,
		},
		{
			name:     "bool",
			input:    true,
			expected: true,
		},
		{
			name:     "float",
			input:    3.14,
			expected: 3.14,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := convertValueToBson(tt.input)
			require.NoError(t, err)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestBuildQuarantinedAndDrainedNodesPipeline(t *testing.T) {
	// Create the remediation pipeline
	agnosticPipeline := BuildQuarantinedAndDrainedNodesPipeline()

	// Convert to MongoDB pipeline
	result, err := ConvertAgnosticPipelineToMongo(agnosticPipeline)
	require.NoError(t, err)

	// Verify it's a mongo.Pipeline
	mongoPipeline, ok := result.(mongo.Pipeline)
	require.True(t, ok, "Result should be mongo.Pipeline")
	require.Len(t, mongoPipeline, 1, "Pipeline should have 1 stage")

	// Verify the structure
	stage := mongoPipeline[0]
	require.Len(t, stage, 1, "Stage should have 1 element")
	require.Equal(t, "$match", stage[0].Key)

	// Extract the match document
	matchDoc, ok := stage[0].Value.(bson.D)
	require.True(t, ok, "Match value should be bson.D")

	// Verify it has operationType and $or
	var foundOpType, foundOr bool
	for _, elem := range matchDoc {
		if elem.Key == "operationType" {
			foundOpType = true
			// Should be a document with $in operator containing both "insert" and "update"
			opTypeDoc, ok := elem.Value.(bson.D)
			require.True(t, ok, "operationType should be a document with $in operator")
			require.Len(t, opTypeDoc, 1, "operationType document should have 1 element")
			require.Equal(t, "$in", opTypeDoc[0].Key)

			// Verify $in contains both "insert" and "update"
			inArray, ok := opTypeDoc[0].Value.(bson.A)
			require.True(t, ok, "$in value should be an array")
			require.Len(t, inArray, 2, "$in should contain 2 operation types")
			require.Contains(t, inArray, "insert", "Should include 'insert' operation type")
			require.Contains(t, inArray, "update", "Should include 'update' operation type")
		}
		if elem.Key == "$or" {
			foundOr = true
			// Verify $or is a bson.A (array)
			orArray, ok := elem.Value.(bson.A)
			require.True(t, ok, "$or value should be bson.A")
			require.Len(t, orArray, 3, "$or should have 3 conditions (quarantine, unquarantine, and cancelled)")

			// Check first condition (quarantine with both status checks)
			firstCond, ok := orArray[0].(bson.D)
			require.True(t, ok, "First $or condition should be bson.D")
			require.Len(t, firstCond, 2, "First condition should have 2 elements (eviction status and quarantine status)")

			// Verify it has both fullDocument fields
			var foundEvictionStatus, foundQuarantineStatus bool
			for _, field := range firstCond {
				if field.Key == "fullDocument.healtheventstatus.userpodsevictionstatus.status" {
					foundEvictionStatus = true
					// Should have $in operator
					inDoc, ok := field.Value.(bson.D)
					require.True(t, ok, "Eviction status should use $in operator")
					require.Equal(t, "$in", inDoc[0].Key)
				}
				if field.Key == "fullDocument.healtheventstatus.nodequarantined" {
					foundQuarantineStatus = true
					// Should have $in operator
					inDoc, ok := field.Value.(bson.D)
					require.True(t, ok, "Quarantine status should use $in operator")
					require.Equal(t, "$in", inDoc[0].Key)
				}
			}
			require.True(t, foundEvictionStatus, "First condition should have eviction status field")
			require.True(t, foundQuarantineStatus, "First condition should have quarantine status field")

			// Check second condition (unquarantine)
			secondCond, ok := orArray[1].(bson.D)
			require.True(t, ok, "Second $or condition should be bson.D")
			require.Len(t, secondCond, 2, "Second condition should have 2 elements")
		}
	}

	require.True(t, foundOpType, "Should have operationType field")
	require.True(t, foundOr, "Should have $or field")
}
