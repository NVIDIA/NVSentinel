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

package controller

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/util/validation"
)

func TestGroupNameAndAttemptObjectName(t *testing.T) {
	testCases := []struct {
		name string
		// If set, groupName is passed directly to attemptObjectName, and we bypass calling the groupName function
		groupName          string
		tests              []string
		index              int
		vrName             string
		attemptNumber      int
		expectedObjectName string
	}{
		{
			name:               "no truncation or hashing",
			tests:              []string{"dcgm-level4"},
			index:              1,
			vrName:             "vr-abc123",
			attemptNumber:      1,
			expectedObjectName: "vr-abc123-dcgm-level4-group-1-1",
		},
		{
			name:               "vrName exact fit",
			tests:              []string{"basic"},
			index:              1,
			vrName:             strings.Repeat("v", 47),
			attemptNumber:      1,
			expectedObjectName: strings.Repeat("v", 47) + "-basic-group-1-1",
		},
		{
			name:               "vrName truncated",
			tests:              []string{"basic"},
			index:              1,
			vrName:             strings.Repeat("v", 250),
			attemptNumber:      5,
			expectedObjectName: strings.Repeat("v", 47) + "-basic-group-1-5",
		},
		{
			name:               "test names hashed",
			tests:              []string{strings.Repeat("x", 60), strings.Repeat("y", 60), strings.Repeat("z", 60)},
			index:              3,
			vrName:             strings.Repeat("v", 80),
			attemptNumber:      2,
			expectedObjectName: strings.Repeat("x", 43) + "-3611b5f3-group-3-2",
		},
		{
			name:               "test names hashed, vrName dropped",
			tests:              []string{strings.Repeat("a", 60)},
			index:              1,
			vrName:             "vr",
			attemptNumber:      1,
			expectedObjectName: strings.Repeat("a", 43) + "-92b9e111-group-1-1",
		},
		{
			name:               "groupName truncated, trailing hyphen trimmed",
			groupName:          strings.Repeat("a", 62),
			vrName:             "vr",
			attemptNumber:      1,
			expectedObjectName: strings.Repeat("a", 62),
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			grp := tc.groupName
			if len(grp) == 0 {
				grp = groupName(tc.tests, tc.index)
				if errs := validation.IsDNS1123Label(grp); len(errs) != 0 {
					t.Fatalf("group name %q is not a valid DNS-1123 label: %v", grp, errs)
				}
			}

			objectName := attemptObjectName(tc.vrName, grp, tc.attemptNumber)

			if len(objectName) > validation.DNS1123LabelMaxLength {
				t.Fatalf("object name %q exceeds %d characters", objectName, validation.DNS1123LabelMaxLength)
			}

			if errs := validation.IsDNS1123Label(objectName); len(errs) != 0 {
				t.Fatalf("object name %q is not a valid DNS-1123 label: %v", objectName, errs)
			}

			if len(tc.expectedObjectName) != 0 && objectName != tc.expectedObjectName {
				t.Fatalf("got object name %q, want %q", objectName, tc.expectedObjectName)
			}
		})
	}
}
