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

// Package validation evaluates HealthEvents against the fault-quarantine
// validation.ruleSets and, once a quarantine session's unhealthy events all
// recover, creates a ValidationRequest CRD for any post-remediation validation
// tests that were requested. See docs/designs/049-node-validation.md.

package validation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"text/template"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/nvidia/nvsentinel/commons/pkg/drain"
	"github.com/nvidia/nvsentinel/commons/pkg/templates"
	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/common"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/config"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/evaluator"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/informer"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
)

type ValidationClient struct {
	k8sClient        *informer.FaultQuarantineClient
	healthEventStore datastore.HealthEventStore

	validationRuleSets        []evaluator.RuleSetEvaluatorIface
	validationRuleSetTestsMap map[string][]string

	resourceTemplate *template.Template
	resourceGVR      schema.GroupVersionResource
}

type templateData struct {
	NodeName  string
	SessionID string
	Tests     []string
	TraceID   string
	SpanID    string
}

// As part of the quarantineValidationHealthEvent annotation, we keep track of HealthEvents which contributed the
// tests and the tests themselves. We will use the HealthEvent ID to check if the node was fully drained as part
// of the quarantine session prior to requesting validation for the given set of tests.
type healthEventWithTests struct {
	*protos.HealthEvent
	Tests []string `json:"tests,omitempty"`
}

func NewValidationClient(cfg config.TomlConfig, k8sClient *informer.FaultQuarantineClient,
	healthEventStore datastore.HealthEventStore) (*ValidationClient, error) {
	if !cfg.Validation.Enabled || len(cfg.Validation.RuleSets) == 0 {
		return nil, nil
	}

	validationRuleSets, err := evaluator.InitializeRuleSetEvaluators(cfg.Validation.RuleSets, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize validation rule set evaluators: %w", err)
	}

	validationRuleSetTestsMap := make(map[string][]string, len(cfg.Validation.RuleSets))
	for _, ruleSet := range cfg.Validation.RuleSets {
		validationRuleSetTestsMap[ruleSet.Name] = ruleSet.Tests
	}

	validationTemplate, err := templates.LoadTemplate(cfg.Validation.TemplateMountPath, cfg.Validation.TemplateFileName,
		"validation")
	if err != nil {
		return nil, fmt.Errorf("failed to load ValidationRequest template: %w", err)
	}

	gvr := schema.GroupVersionResource{
		Group:    cfg.Validation.ApiGroup,
		Version:  cfg.Validation.Version,
		Resource: cfg.Validation.Resource,
	}

	return &ValidationClient{
		validationRuleSets:        validationRuleSets,
		validationRuleSetTestsMap: validationRuleSetTestsMap,
		k8sClient:                 k8sClient,
		healthEventStore:          healthEventStore,
		resourceTemplate:          validationTemplate,
		resourceGVR:               gvr,
	}, nil
}

/*
This is called whenever a new unhealthy HealthEvent is added to the quarantineHealthEvent annotation. Specifically,
if a new HealthEvent is added to the quarantineHealthEvent annotation from either a Quarantined or AlreadyQuarantined
event, we will also evaluate if that unhealthy HealthEvent requires a validation test. If one of these events
requires one or more validation tests, we will add the event to the quarantineValidationHealthEvent annotation.

1. We inherit the same de-duping behavior which is provided by the quarantineHealthEvent annotation to only track
HealthEvents with a unique HealthEventKey (which is a subset of HealthEvent properties). In other words, even if we
process all new unhealthy HealthEvents, we will only consider it for validation tests if it resulted in us updating
our annotation on either a Quarantined or AlreadyQuarantined event. As a result, we will not capture any tests from
duplicated events which are derived from properties outside of what's included in HealthEventKey.

Recall that we do not remove events from quarantineValidationHealthEvent throughout the quarantine session compared
to the quarantineHealthEvent where we remove events as soon as a matching healthy event clears it. This could
result in us tracking duplicated events for the same HealthEventKey if an event recovers and goes back to unhealthy
as part of the same session. To avoid this, we would need quarantineValidationHealthEvent to maintain its own
de-duplicating on the HealthEventKey in the future.

2. Related to the above, since we only track unhealthy HealthEvents which result in Quarantined or AlreadyQuarantined
events, we will not allow events to contribute tests from the validation rule-set if they do not satisfy the primary
fault-quarantine rule-set.

3. We have multiple levels of de-duplication to track
a. HealthEvent de-duplication: as discussed above, we de-duplicate HealthEvents with the same HealthEventKey by
leveraging the quarantineHealthEvent.
b. Test de-duplication within 1 HealthEvent: if a validation rule-set evaluation results in the same test being
required multiple times for the same fault, we will ensure that only unique tests are requested per event.
c. Test de-duplication across HealthEvents: this isn't relevant to the node annotation update here and is covered
in FetchValidationTestsFromQuarantineSession when the node is unquarantined. When processing all HealthEvents
which required a validation test, we will build up a unique list of tests requested across all events to include
as part of a single validation test.
*/
func (c *ValidationClient) UpdateQuarantineValidationAnnotation(ctx context.Context,
	existingQuarantineValidationAnnotation string, event *protos.HealthEvent) (string, error) {
	tests := c.getTestsForHealthEvent(ctx, event)
	if len(tests) == 0 {
		return existingQuarantineValidationAnnotation, nil
	}

	var events []healthEventWithTests
	if len(existingQuarantineValidationAnnotation) != 0 {
		if err := json.Unmarshal([]byte(existingQuarantineValidationAnnotation), &events); err != nil {
			return "", fmt.Errorf("failed to parse validation annotation: %w", err)
		}
	}

	events = append(events, healthEventWithTests{HealthEvent: event, Tests: tests})

	data, err := json.Marshal(events)
	if err != nil {
		return "", fmt.Errorf("failed to marshal validation annotation for node %s: %w", event.NodeName, err)
	}

	return string(data), nil
}

/*
When a healthy HealthEvent removes the last entry from the node's quarantineHealthEvent, which results in an
UnQuarantined event, this function is called to compute the final list of tests required for validation. If no
unhealthy events required validation tests during the quarantine session or if none of these unhealthy events
successfully completed a full drain, we will skip creating a ValidationRequest targeting the given node
and list of tests. Recall that we compute a unique set of tests across all unhealthy events during the session.

Example quarantineValidationHealthEvent annotation:

[

	{
	  "version": 1,
	  "agent": "syslog-health-monitor",
	  "componentClass": "GPU",
	  "checkName": "GpuFallenOffBus",
	  "isFatal": true,
	  "recommendedAction": 24,
	  "entitiesImpacted": [
	    { "entityType": "GPU_UUID", "entityValue": "GPU-5e6f7a8b-0000-0000-0000-000000000000" }
	  ],
	  "nodeName": "node-123",
	  "id": "70bd4fc9ffa9f5eca91c340d",
	  "generatedTimestamp": "2026-08-28T10:22:00Z",
	  "tests": ["dcgm-diag-test"]
	}

]

Resulting ValidationRequest:

	{
	  "apiVersion": "nvsentinel.nvidia.com/v1alpha1",
	  "kind": "ValidationRequest",
	  "metadata": {
	    "name": "node-123-validation-88d1268a2995",
	    "labels": {
	      "app.kubernetes.io/managed-by": "nvsentinel"
	    }
	  },
	  "spec": {
	    "nodes": [
	      { "name": "node-123" }
	    ],
	    "tests": [
	      "dcgm-diag-test"
	    ]
	  }
	}
*/
func (c *ValidationClient) FetchValidationTestsFromQuarantineSession(ctx context.Context, nodeName,
	quarantineValidationAnnotation string) ([]string, string, error) {
	if len(quarantineValidationAnnotation) == 0 {
		return nil, "", nil
	}

	var eventsWithTests []healthEventWithTests
	if err := json.Unmarshal([]byte(quarantineValidationAnnotation), &eventsWithTests); err != nil {
		return nil, "", fmt.Errorf("failed to parse validation annotation for node %s: %w", nodeName, err)
	}

	events := make([]*protos.HealthEvent, 0, len(eventsWithTests))
	seenTests := make(map[string]struct{})

	var tests, eventIDs []string

	for _, event := range eventsWithTests {
		if event.HealthEvent == nil {
			continue
		}

		events = append(events, event.HealthEvent)

		if len(event.Tests) == 0 {
			continue
		}

		eventIDs = append(eventIDs, event.Id)

		for _, test := range event.Tests {
			if _, ok := seenTests[test]; !ok {
				seenTests[test] = struct{}{}
				tests = append(tests, test)
			}
		}
	}

	isDrained, err := drain.IsNodeDrained(ctx, c.healthEventStore, nodeName, events, "", nil, drain.PartialDrainEntity)
	if err != nil {
		return nil, "", fmt.Errorf("failed to look up drain status for node %s: %w", nodeName, err)
	}

	if !isDrained {
		slog.InfoContext(ctx, "Skipping validation: no contributing event completed its drain", "node", nodeName)

		return nil, "", nil
	}

	sort.Strings(tests)
	sort.Strings(eventIDs)

	return tests, sessionID(eventIDs), nil
}

func (c *ValidationClient) CreateValidationRequest(ctx context.Context, node *corev1.Node, tests []string, sessionID,
	traceID, spanID string) error {
	data := templateData{
		NodeName:  node.Name,
		SessionID: sessionID,
		Tests:     tests,
		TraceID:   traceID,
		SpanID:    spanID,
	}

	validationObject, _, err := templates.Render(c.resourceTemplate, data)
	if err != nil {
		return fmt.Errorf("failed to render ValidationRequest for node %s: %w", node.Name, err)
	}

	templates.SetNodeOwnerRef(validationObject, node)

	return c.k8sClient.CreateValidationRequestResource(ctx, c.resourceGVR, validationObject)
}

func (c *ValidationClient) getTestsForHealthEvent(ctx context.Context, event *protos.HealthEvent) []string {
	seen := make(map[string]struct{})

	var tests []string

	for _, eval := range c.validationRuleSets {
		result, err := eval.Evaluate(ctx, event)
		if err != nil {
			slog.WarnContext(ctx, "Error evaluating validation ruleset", "ruleset", eval.GetName(), "error", err)
			continue
		}

		if result != common.RuleEvaluationSuccess {
			continue
		}

		for _, test := range c.validationRuleSetTestsMap[eval.GetName()] {
			if _, ok := seen[test]; ok {
				continue
			}

			seen[test] = struct{}{}

			tests = append(tests, test)
		}
	}

	return tests
}

// sessionID derives a deterministic name suffix from the sorted event IDs that contributed tests.
func sessionID(eventIDs []string) string {
	sum := sha256.Sum256([]byte(strings.Join(eventIDs, ",")))
	return hex.EncodeToString(sum[:])[:12]
}
