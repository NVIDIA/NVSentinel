// Copyright (c) 2026, NVIDIA CORPORATION. All rights reserved.
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

// Package quarantinetaints maintains the taint records shared by quarantine and validation.
package quarantinetaints

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

const AnnotationKey = "quarantineHealthEventAppliedTaints"

type record struct {
	Key         string             `json:"key"`
	Value       string             `json:"value"`
	Effect      corev1.TaintEffect `json:"effect"`
	PreExisting bool               `json:"preExisting"`
}

// Prune retires records whose exact taint no longer exists. Once quarantine and
// validation finish, retainPreExisting=false also retires records of taints that
// belonged to another owner. The taints themselves are never changed.
func Prune(node *corev1.Node, retainPreExisting bool) (bool, error) {
	raw, present := node.Annotations[AnnotationKey]
	if !present {
		return false, nil
	}

	if strings.TrimSpace(raw) == "" {
		delete(node.Annotations, AnnotationKey)
		return true, nil
	}

	var records []json.RawMessage
	if err := json.Unmarshal([]byte(raw), &records); err != nil {
		return false, fmt.Errorf("parse quarantine taint records on node %s: %w", node.Name, err)
	}

	remaining, err := retainedRecords(node, records, retainPreExisting)
	if err != nil {
		return false, err
	}

	if len(remaining) == 0 {
		delete(node.Annotations, AnnotationKey)
		return true, nil
	}

	if len(remaining) == len(records) {
		return false, nil
	}

	encoded, err := json.Marshal(remaining)
	if err != nil {
		return false, fmt.Errorf("encode retained quarantine taint records on node %s: %w", node.Name, err)
	}

	node.Annotations[AnnotationKey] = string(encoded)

	return true, nil
}

func retainedRecords(node *corev1.Node, records []json.RawMessage, retainPreExisting bool) ([]json.RawMessage, error) {
	remaining := make([]json.RawMessage, 0, len(records))
	for _, encoded := range records {
		var taint record
		if err := json.Unmarshal(encoded, &taint); err != nil {
			return nil, fmt.Errorf("parse quarantine taint record on node %s: %w", node.Name, err)
		}

		if taint.PreExisting && !retainPreExisting {
			continue
		}

		if slices.ContainsFunc(node.Spec.Taints, func(actual corev1.Taint) bool {
			return actual.Key == taint.Key && actual.Value == taint.Value && actual.Effect == taint.Effect
		}) {
			remaining = append(remaining, encoded)
		}
	}

	return remaining, nil
}
