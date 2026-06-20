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

// Package condition adapts proto Condition (wire format) to metav1.Condition
// (what controller-runtime's meta.SetStatusCondition operates on).
package condition

import (
	"google.golang.org/protobuf/types/known/timestamppb"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	protos "github.com/nvidia/nvsentinel/data-models/pkg/protos"
)

func ToMetav1(c *protos.Condition) metav1.Condition {
	if c == nil {
		return metav1.Condition{}
	}

	var lastTransition metav1.Time
	if c.LastTransitionTime != nil {
		lastTransition = metav1.NewTime(c.LastTransitionTime.AsTime())
	}

	return metav1.Condition{
		Type:               c.Type,
		Status:             metav1.ConditionStatus(c.Status),
		ObservedGeneration: c.ObservedGeneration,
		LastTransitionTime: lastTransition,
		Reason:             c.Reason,
		Message:            c.Message,
	}
}

func FromMetav1(c metav1.Condition) *protos.Condition {
	var lastTransition *timestamppb.Timestamp
	if !c.LastTransitionTime.IsZero() {
		lastTransition = timestamppb.New(c.LastTransitionTime.Time)
	}

	return &protos.Condition{
		Type:               c.Type,
		Status:             string(c.Status),
		ObservedGeneration: c.ObservedGeneration,
		LastTransitionTime: lastTransition,
		Reason:             c.Reason,
		Message:            c.Message,
	}
}

// ToMetav1Slice skips nil entries.
func ToMetav1Slice(in []*protos.Condition) []metav1.Condition {
	if in == nil {
		return nil
	}

	out := make([]metav1.Condition, 0, len(in))
	for _, c := range in {
		if c == nil {
			continue
		}

		out = append(out, ToMetav1(c))
	}

	return out
}

func FromMetav1Slice(in []metav1.Condition) []*protos.Condition {
	if in == nil {
		return nil
	}

	out := make([]*protos.Condition, 0, len(in))
	for i := range in {
		out = append(out, FromMetav1(in[i]))
	}

	return out
}
