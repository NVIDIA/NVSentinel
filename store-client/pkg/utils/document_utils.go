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

package utils

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
)

// ExtractDocumentID extracts the document ID from a raw event
func ExtractDocumentID(event map[string]interface{}) (string, error) {
	// Try MongoDB ObjectId format first
	if id, exists := event["_id"]; exists {
		return convertIDToString(id), nil
	}

	// Try PostgreSQL uuid format
	if id, exists := event["id"]; exists {
		return convertIDToString(id), nil
	}

	// Try in fullDocument for change stream events
	if fullDoc, exists := event["fullDocument"]; exists {
		if id, err := extractIDFromFullDocument(fullDoc); err == nil {
			return id, nil
		}
	}

	return "", datastore.NewValidationError(
		"",
		"no document ID found in event",
		nil,
	).WithMetadata("event", event)
}

// extractIDFromDocument extracts ID from a document interface, trying both MongoDB and PostgreSQL formats
func extractIDFromDocument(doc interface{}) interface{} {
	docMap, ok := doc.(map[string]interface{})
	if !ok {
		return nil
	}

	// Try MongoDB _id format first
	if id, exists := docMap["_id"]; exists {
		return id
	}

	// Try PostgreSQL id format
	if id, exists := docMap["id"]; exists {
		return id
	}

	return nil
}

// ExtractDocumentIDNative extracts the document ID from a raw event while preserving its native type.
// This is useful for database operations that require the native ID type (e.g., MongoDB ObjectID).
// Unlike ExtractDocumentID which converts to string, this preserves the original type for queries.
func ExtractDocumentIDNative(event map[string]interface{}) (interface{}, error) {
	// Try MongoDB _id format first (preserves ObjectID type)
	if id, exists := event["_id"]; exists {
		return id, nil
	}

	// Try PostgreSQL id/uuid format
	if id, exists := event["id"]; exists {
		return id, nil
	}

	// Try in fullDocument for change stream events
	if fullDoc, exists := event["fullDocument"]; exists {
		if id := extractIDFromDocument(fullDoc); id != nil {
			return id, nil
		}
	}

	return nil, datastore.NewValidationError(
		"",
		"no document ID found in event",
		nil,
	).WithMetadata("event", event)
}

// extractIDFromFullDocument extracts document ID from fullDocument field
// This helper function reduces complexity in ExtractDocumentID
func extractIDFromFullDocument(fullDoc interface{}) (string, error) {
	doc, ok := fullDoc.(map[string]interface{})
	if !ok {
		return "", fmt.Errorf("fullDocument is not a map")
	}

	// Try MongoDB _id format first
	if id, exists := doc["_id"]; exists {
		return convertIDToString(id), nil
	}

	// Try PostgreSQL id format
	if id, exists := doc["id"]; exists {
		return convertIDToString(id), nil
	}

	return "", fmt.Errorf("no ID found in fullDocument")
}

// convertIDToString converts various ID types to their string representation
// Handles MongoDB ObjectID, PostgreSQL UUID, and other formats
func convertIDToString(id interface{}) string {
	// Check if it's a MongoDB ObjectID (has Hex() method)
	if objectID, ok := id.(interface{ Hex() string }); ok {
		return objectID.Hex()
	}

	// Fall back to standard string conversion for other types
	return fmt.Sprintf("%v", id)
}

// NormalizeFieldNamesForMongoDB converts protobuf-generated JSON structures to use lowercase field names.
// This is necessary because:
// 1. Protobuf JSON marshaling uses camelCase (e.g., entityType, entityValue)
// 2. MongoDB stores and queries fields based on JSON tag names (camelCase)
// 3. MongoDB aggregation pipeline filters expect lowercase field references (e.g., $$this.entitytype)
// 4. When embedding resolved values in pipelines, field names must match MongoDB's lowercase convention
//
// This function ensures that protobuf values embedded in MongoDB aggregation pipelines
// have lowercase field names matching the pipeline filter expectations.
//
// Example:
//
//	Input:  []*protos.Entity marshals to [{"entityType": "GPU", "entityValue": "0"}]
//	Output: [{"entitytype": "GPU", "entityvalue": "0"}]
//
// Usage: Call this on any protobuf values before embedding them in MongoDB aggregation pipelines.
func NormalizeFieldNamesForMongoDB(value interface{}) interface{} {
	// First, marshal to JSON (converts protobuf to JSON with camelCase)
	jsonBytes, err := json.Marshal(value)
	if err != nil {
		// If marshaling fails, return original value
		return value
	}

	// Unmarshal to map[string]interface{} or []interface{}
	var intermediate interface{}
	if err := json.Unmarshal(jsonBytes, &intermediate); err != nil {
		// If unmarshaling fails, return original value
		return value
	}

	// Recursively lowercase all field names
	return lowercaseKeys(intermediate)
}

// lowercaseKeys recursively converts all map keys to lowercase
func lowercaseKeys(value interface{}) interface{} {
	switch v := value.(type) {
	case map[string]interface{}:
		result := make(map[string]interface{})
		for key, val := range v {
			// Convert key to lowercase and recursively process value
			result[strings.ToLower(key)] = lowercaseKeys(val)
		}

		return result
	case []interface{}:
		result := make([]interface{}, len(v))
		for i, val := range v {
			result[i] = lowercaseKeys(val)
		}

		return result
	default:
		// Primitive values (string, number, bool, null) remain unchanged
		return v
	}
}
