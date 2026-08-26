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

package datastore

import (
	"encoding/json"
	"fmt"

	"go.mongodb.org/mongo-driver/v2/bson"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// MarshalBSON preserves the public *bool API while writing nullable booleans in
// the protobuf BoolValue shape expected by proto-based consumers.
func (h HealthEventStatus) MarshalBSON() ([]byte, error) {
	return bson.Marshal(healthEventStatusBSON{
		NodeQuarantined:           h.NodeQuarantined,
		QuarantineFinishTimestamp: h.QuarantineFinishTimestamp,
		UserPodsEvictionStatus:    h.UserPodsEvictionStatus,
		DrainFinishTimestamp:      h.DrainFinishTimestamp,
		FaultRemediated:           encodeWrappedBool(h.FaultRemediated),
		LastRemediationTimestamp:  h.LastRemediationTimestamp,
		SpanIds:                   h.SpanIds,
	})
}

// UnmarshalBSON accepts both historical and current on-disk encodings for
// nullable boolean fields while preserving the public datastore API.
func (h *HealthEventStatus) UnmarshalBSON(data []byte) error {
	var raw bson.D
	if err := bson.Unmarshal(data, &raw); err != nil {
		return err
	}

	var faultRemediated *bool

	remaining := make(bson.D, 0, len(raw))

	for _, elem := range raw {
		if elem.Key == "faultremediated" {
			value, err := decodeBSONBoolValue(elem.Value)
			if err != nil {
				return fmt.Errorf("failed to decode faultremediated: %w", err)
			}

			faultRemediated = value

			continue
		}

		remaining = append(remaining, elem)
	}

	remainingBytes, err := bson.Marshal(remaining)
	if err != nil {
		return err
	}

	type alias HealthEventStatus

	var decoded alias
	if err := bson.Unmarshal(remainingBytes, &decoded); err != nil {
		return err
	}

	*h = HealthEventStatus(decoded)
	h.FaultRemediated = faultRemediated

	return nil
}

// MarshalJSON preserves the public *bool API while writing nullable booleans in
// the protobuf BoolValue shape expected by proto-based consumers.
func (h HealthEventStatus) MarshalJSON() ([]byte, error) {
	return json.Marshal(healthEventStatusJSON{
		NodeQuarantined:           h.NodeQuarantined,
		QuarantineFinishTimestamp: h.QuarantineFinishTimestamp,
		UserPodsEvictionStatus:    h.UserPodsEvictionStatus,
		DrainFinishTimestamp:      h.DrainFinishTimestamp,
		FaultRemediated:           encodeWrappedBool(h.FaultRemediated),
		LastRemediationTimestamp:  h.LastRemediationTimestamp,
		SpanIds:                   h.SpanIds,
	})
}

// UnmarshalJSON accepts both historical and current JSON encodings for
// nullable boolean fields while preserving the public datastore API.
func (h *HealthEventStatus) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	var faultRemediated *bool

	if value, ok := raw["faultremediated"]; ok {
		decoded, err := decodeJSONBoolValue(value)
		if err != nil {
			return fmt.Errorf("failed to decode faultremediated: %w", err)
		}

		faultRemediated = decoded

		delete(raw, "faultremediated")
	}

	remainingBytes, err := json.Marshal(raw)
	if err != nil {
		return fmt.Errorf("failed to marshal remaining JSON fields: %w", err)
	}

	type alias HealthEventStatus

	var decoded alias
	if err := json.Unmarshal(remainingBytes, &decoded); err != nil {
		return fmt.Errorf("failed to unmarshal remaining JSON fields: %w", err)
	}

	*h = HealthEventStatus(decoded)
	h.FaultRemediated = faultRemediated

	return nil
}

type healthEventStatusBSON struct {
	NodeQuarantined           *Status                `bson:"nodequarantined"`
	QuarantineFinishTimestamp *timestamppb.Timestamp `bson:"quarantinefinishtimestamp,omitempty"`
	UserPodsEvictionStatus    OperationStatus        `bson:"userpodsevictionstatus"`
	DrainFinishTimestamp      *timestamppb.Timestamp `bson:"drainfinishtimestamp,omitempty"`
	FaultRemediated           interface{}            `bson:"faultremediated"`
	LastRemediationTimestamp  *timestamppb.Timestamp `bson:"lastremediationtimestamp,omitempty"`
	SpanIds                   map[string]string      `bson:"spanids,omitempty"`
}

type healthEventStatusJSON struct {
	NodeQuarantined           *Status                `json:"nodequarantined"`
	QuarantineFinishTimestamp *timestamppb.Timestamp `json:"quarantinefinishtimestamp,omitempty"`
	UserPodsEvictionStatus    OperationStatus        `json:"userpodsevictionstatus"`
	DrainFinishTimestamp      *timestamppb.Timestamp `json:"drainfinishtimestamp,omitempty"`
	FaultRemediated           interface{}            `json:"faultremediated"`
	LastRemediationTimestamp  *timestamppb.Timestamp `json:"lastremediationtimestamp,omitempty"`
	SpanIds                   map[string]string      `json:"spanids,omitempty"`
}

func encodeWrappedBool(value *bool) interface{} {
	if value == nil {
		return nil
	}

	return map[string]bool{"value": *value}
}

func decodeBSONBoolValue(value interface{}) (*bool, error) {
	switch v := value.(type) {
	case nil:
		return nil, nil
	case bool:
		return &v, nil
	case bson.D:
		for _, elem := range v {
			if elem.Key != "value" {
				continue
			}

			boolValue, ok := elem.Value.(bool)
			if !ok {
				return nil, fmt.Errorf("value field is %T, not bool", elem.Value)
			}

			return &boolValue, nil
		}

		return nil, nil
	default:
		return nil, fmt.Errorf("unsupported BSON type %T", value)
	}
}

func decodeJSONBoolValue(data json.RawMessage) (*bool, error) {
	if string(data) == "null" {
		return nil, nil
	}

	var plain bool
	if err := json.Unmarshal(data, &plain); err == nil {
		return &plain, nil
	}

	var wrapped struct {
		Value *bool `json:"value"`
	}

	if err := json.Unmarshal(data, &wrapped); err != nil {
		return nil, err
	}

	return wrapped.Value, nil
}
