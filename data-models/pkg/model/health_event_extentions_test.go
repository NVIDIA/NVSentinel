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

package model

import (
	"testing"
	"time"

	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"go.mongodb.org/mongo-driver/bson"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestHealthEventWithStatus_MarshalBSON(t *testing.T) {
	healthEvent := &protos.HealthEvent{
		Version:       1,
		Message:       "Test event",
		NodeName:      "test-node",
		GeneratedTimestamp: timestamppb.Now(),
		Metadata: map[string]string{
			"test_key": "test_value",
			"providerID": "test-provider",
		},
	}

	wrapper := &HealthEventWithStatus{
		CreatedAt:   time.Now(),
		HealthEvent: healthEvent,
	}

	data, err := bson.Marshal(wrapper)
	if err != nil {
		t.Fatalf("MarshalBSON failed: %v", err)
	}

	if len(data) == 0 {
		t.Fatal("Marshaled data is empty")
	}

	var result bson.M
	err = bson.Unmarshal(data, &result)
	if err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	t.Logf("Marshaled document keys: %v", getKeys(result))

	if _, ok := result["healthevent"]; !ok {
		t.Error("Missing 'healthevent' field")
	}

	if _, ok := result["createdAt"]; !ok {
		t.Error("Missing 'createdAt' field")
	}

	if heDoc, ok := result["healthevent"].(bson.M); ok {
		t.Logf("healthevent keys: %v", getKeys(heDoc))
		
		if _, ok := heDoc["metadata"]; !ok {
			t.Error("Missing 'metadata' in healthevent")
		}
		
		if msg, ok := heDoc["message"].(string); !ok || msg != "Test event" {
			t.Errorf("Expected message='Test event', got: %v", heDoc["message"])
		}
	} else {
		t.Error("healthevent is not a BSON document")
	}
}

func getKeys(m bson.M) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}



