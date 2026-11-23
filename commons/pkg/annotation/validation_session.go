// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

package annotation

import (
	"encoding/json"
	"fmt"
)

// ValidationSessionKey is the node annotation in which lifecycle-manager lists the
// ValidationRequests that hold the node: pending, running, or failed. The annotation is removed
// when the last request leaves the session.
const ValidationSessionKey = "nvsentinel.nvidia.com/validation-session"

// ValidationSessionEntry is one ValidationRequest in a node's validation session.
type ValidationSessionEntry struct {
	Name   string   `json:"name"`
	Tests  []string `json:"tests,omitempty"`
	Failed bool     `json:"failed,omitempty"`
}

// ParseValidationSession decodes the value of the validation session annotation. An empty value
// is an empty session.
func ParseValidationSession(value string) ([]ValidationSessionEntry, error) {
	if len(value) == 0 {
		return nil, nil
	}

	var entries []ValidationSessionEntry

	if err := json.Unmarshal([]byte(value), &entries); err != nil {
		return nil, fmt.Errorf("unmarshal %q annotation: %w", ValidationSessionKey, err)
	}

	return entries, nil
}

// MarshalValidationSession encodes the entries as the value of the validation session annotation.
func MarshalValidationSession(entries []ValidationSessionEntry) (string, error) {
	b, err := json.Marshal(entries)
	if err != nil {
		return "", fmt.Errorf("marshal %q annotation: %w", ValidationSessionKey, err)
	}

	return string(b), nil
}
