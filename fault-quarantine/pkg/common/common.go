// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package common

import "github.com/nvidia/nvsentinel/data-models/pkg/protos"

// As part of the quarantineValidationHealthEvent annotation, we keep track of HealthEvents which contributed the
// tests and the tests themselves. We will use the HealthEvent ID to check if the node was fully drained as part
// of the quarantine session prior to requesting validation for the given set of tests.
type HealthEventWithTests struct {
	*protos.HealthEvent
	Tests []string `json:"tests,omitempty"`
}

// RuleEvaluationResult represents the result of a rule evaluation
type RuleEvaluationResult int

const (
	RuleEvaluationSuccess RuleEvaluationResult = iota
	RuleEvaluationFailed
)

const (
	// Annotation keys for storing event on node which causes node to be cordoned or tainted
	QuarantineHealthEventAnnotationKey                    = "quarantineHealthEvent"
	QuarantineHealthEventAppliedTaintsAnnotationKey       = "quarantineHealthEventAppliedTaints"
	QuarantineHealthEventAppliedLabelsAnnotationKey       = "quarantineHealthEventAppliedLabels"
	QuarantineHealthEventIsCordonedAnnotationKey          = "quarantineHealthEventIsCordoned"
	QuarantineHealthEventIsCordonedAnnotationValueTrue    = "True"
	QuarantineHealthEventCordonPreExistingAnnotationKey   = "quarantineHealthEventCordonPreExisting"
	QuarantineHealthEventCordonPreExistingAnnotationValue = "True"
	QuarantinedNodeUncordonedManuallyAnnotationKey        = "quarantinedNodeUncordonedManually"
	QuarantinedNodeUncordonedManuallyAnnotationValue      = "True"
	QuarantinedNodeIsUntaintedManuallyAnnotationKey       = "quarantinedNodeUntaintedManually"
	QuarantinedNodeIsUntaintedManuallyAnnotationValue     = "True"
	QuarantineValidationHealthEventAnnotationKey          = "quarantineValidationHealthEvent"

	ServiceName = "NVSentinel"
)
