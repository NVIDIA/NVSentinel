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

// RuleEvaluationResult represents the result of a rule evaluation
type RuleEvaluationResult int

const (
	RuleEvaluationSuccess RuleEvaluationResult = iota
	RuleEvaluationFailed
)

const (
	// Annotation keys for storing event on node which causes node to be cordoned or tainted
	QuarantineHealthEventAnnotationKey                 = "quarantineHealthEvent"
	QuarantineHealthEventAppliedTaintsAnnotationKey    = "quarantineHealthEventAppliedTaints"
	QuarantineHealthEventIsCordonedAnnotationKey       = "quarantineHealthEventIsCordoned"
	QuarantineHealthEventIsCordonedAnnotationValueTrue = "True"
	QuarantinedNodeUncordonedManuallyAnnotationKey     = "quarantinedNodeUncordonedManually"
	QuarantinedNodeUncordonedManuallyAnnotationValue   = "True"
	QuarantinedNodeIsUntaintedManuallyAnnotationKey    = "quarantinedNodeUntaintedManually"
	QuarantinedNodeIsUntaintedManuallyAnnotationValue  = "True"

	ServiceName = "NVSentinel"
)
