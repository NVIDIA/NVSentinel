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
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/nvidia/nvsentinel/commons/pkg/kubeclient"
	"github.com/nvidia/nvsentinel/commons/pkg/labels"
	"github.com/nvidia/nvsentinel/lifecycle-manager/api/v1alpha1"
)

const uncordonReasonValidationSucceeded = "ValidationSucceeded"

const annotationQuarantineHealthEvent = "quarantineHealthEvent"

type nodesQuarantinedError struct {
	nodeNames        []string
	unsafeRetryNodes map[string]bool
}

func (e *nodesQuarantinedError) Error() string {
	return fmt.Sprintf("nodes %v became quarantined before validation release", e.nodeNames)
}

// validationStateLabelKey is a node label that mirrors the validation-session and
// active-validation-request annotations
const validationStateLabelKey = "nvsentinel.nvidia.com/validation-state"

type validationStateLabelValue string

const (
	validatingLabelValue        validationStateLabelValue = "validating"
	validationPendingLabelValue validationStateLabelValue = "validation-pending"
	validationFailedLabelValue  validationStateLabelValue = "validation-failed"
)

type sessionEntry struct {
	Name            string    `json:"name"`
	Tests           []string  `json:"tests,omitempty"`
	Failed          bool      `json:"failed,omitempty"`
	NodeUID         types.UID `json:"nodeUID,omitempty"`
	RequestUID      types.UID `json:"requestUID,omitempty"`
	ClaimID         string    `json:"claimID,omitempty"`
	CordonOwnership string    `json:"cordonOwnership,omitempty"`
	PreserveCordon  bool      `json:"preserveCordon,omitempty"`
}

func reconcileValidationStateLabel(node *corev1.Node) (bool, error) {
	entries, err := parseSessionAnnotation(node.Annotations[annotationValidationSession])
	if err != nil {
		return false, err
	}

	newValue := deriveValidationStateLabel(node.Annotations[annotationActiveValidationRequest], entries)

	current, exists := node.Labels[validationStateLabelKey]

	if len(newValue) == 0 {
		if !exists {
			return false, nil
		}

		delete(node.Labels, validationStateLabelKey)

		return true, nil
	}

	if exists && current == string(newValue) {
		return false, nil
	}

	if node.Labels == nil {
		node.Labels = make(map[string]string)
	}

	node.Labels[validationStateLabelKey] = string(newValue)

	return true, nil
}

func deriveValidationStateLabel(activeValidationRequest string, entries []sessionEntry) validationStateLabelValue {
	if len(entries) == 0 {
		return ""
	}

	if len(activeValidationRequest) != 0 {
		return validatingLabelValue
	}

	for _, e := range entries {
		if !e.Failed {
			return validationPendingLabelValue
		}
	}

	return validationFailedLabelValue
}

func marshalSessionAnnotation(entries []sessionEntry) (string, error) {
	b, err := json.Marshal(entries)
	if err != nil {
		return "", fmt.Errorf("marshal %q annotation: %w", annotationValidationSession, err)
	}

	return string(b), nil
}

func parseSessionAnnotation(sessionAnnotation string) ([]sessionEntry, error) {
	if len(sessionAnnotation) == 0 {
		return nil, nil
	}

	var entries []sessionEntry

	if err := json.Unmarshal([]byte(sessionAnnotation), &entries); err != nil {
		return nil, fmt.Errorf("unmarshal %q annotation: %w", annotationValidationSession, err)
	}

	return entries, nil
}

// ownedRegistrationRetryInterval requeues a request whose session registration
// was refused. Such a request is not in the node session yet, so Node watch
// events do not reach it.
const ownedRegistrationRetryInterval = 30 * time.Second

func (r *ValidationRequestReconciler) addToSessionAndCheckEligibility(ctx context.Context, ns v1alpha1.NodeSpec,
	validationRequest *v1alpha1.ValidationRequest, criteria []v1alpha1.CriteriaSpec,
	resolvedTests []string) (exists bool, ready bool, retry bool, err error) {
	var node corev1.Node
	if err := r.Get(ctx, client.ObjectKey{Name: ns.Name}, &node); err != nil {
		if apierrors.IsNotFound(err) {
			return false, false, false, nil
		}

		return false, false, false, fmt.Errorf("get node %q: %w", ns.Name, err)
	}

	if r.onlyOwnedCordon() {
		refusal, found, err := r.ownedRegistrationRefusal(ctx, &node, validationRequest)
		if err != nil {
			return false, false, false, err
		}

		if !found {
			return false, false, false, nil
		}

		if refusal != "" {
			slog.InfoContext(ctx, "Validation request cannot join the node session yet",
				"validationRequest", validationRequest.Name, "node", ns.Name, "reason", refusal)

			return true, false, true, nil
		}
	}

	if err := r.updateValidationSession(ctx, ns.Name, validationRequest, resolvedTests); err != nil {
		return true, false, false, fmt.Errorf("ensure session on node %q: %w", ns.Name, err)
	}

	ready, err = r.nodeReadyForValidation(ctx, &node, validationRequest, criteria)

	return true, ready, false, err
}

func (r *ValidationRequestReconciler) nodeReadyForValidation(ctx context.Context, node *corev1.Node,
	validationRequest *v1alpha1.ValidationRequest, criteria []v1alpha1.CriteriaSpec) (bool, error) {
	activeValidationRequest := node.Annotations[annotationActiveValidationRequest]
	if len(activeValidationRequest) != 0 && activeValidationRequest != validationRequest.Name {
		slog.InfoContext(ctx, "Node claimed by another validation request, waiting", "node", node.Name,
			"activeValidationRequest", activeValidationRequest)

		return false, nil
	}

	failedCriteria, err := evaluateCriteria(ctx, r.Client, node, criteria, r.ReadinessPrograms,
		r.ResourceSliceWatch.Enabled)
	if err != nil {
		slog.ErrorContext(ctx, "Failed to check node readiness", "node", node.Name, "error", err)
		return false, fmt.Errorf("failed to check node readiness: %w", err)
	}

	if len(failedCriteria) != 0 {
		slog.InfoContext(ctx, "Node not ready, waiting", "node", node.Name, "criterion", failedCriteria)
		return false, nil
	}

	return true, nil
}

// ownedRegistrationRefusal decides registration on the cached Node and, when
// that refuses, on the live Node: the cache may not show the producer's
// handoff yet. found is false when the Node no longer exists.
func (r *ValidationRequestReconciler) ownedRegistrationRefusal(ctx context.Context, cached *corev1.Node,
	validationRequest *v1alpha1.ValidationRequest) (refusal string, found bool, err error) {
	refusal = ownedRegistrationRefusal(cached, validationRequest)
	if refusal == "" {
		return "", true, nil
	}

	var live corev1.Node
	if err := r.APIReader.Get(ctx, client.ObjectKey{Name: cached.Name}, &live); err != nil {
		if apierrors.IsNotFound(err) {
			return "", false, nil
		}

		return "", false, fmt.Errorf("get node %q: %w", cached.Name, err)
	}

	return ownedRegistrationRefusal(&live, validationRequest), true, nil
}

func (r *ValidationRequestReconciler) updateValidationSession(ctx context.Context, nodeName string,
	validationRequest *v1alpha1.ValidationRequest, tests []string) error {
	return r.patchNodeWithRetry(ctx, nodeName, func(node *corev1.Node) (bool, error) {
		changed, err := r.registerValidationSession(node, validationRequest, tests)
		if err != nil {
			return false, err
		}

		if r.onlyOwnedCordon() && !ownedSessionMatches(node, validationRequest) {
			return changed, nil
		}

		consumed := consumeValidationHandoff(node, validationRequest)

		return changed || consumed, nil
	})
}

func (r *ValidationRequestReconciler) registerValidationSession(node *corev1.Node,
	validationRequest *v1alpha1.ValidationRequest, tests []string) (bool, error) {
	if r.onlyOwnedCordon() {
		return updateOwnedValidationSession(node, validationRequest, tests)
	}

	entries, err := parseSessionAnnotation(node.Annotations[annotationValidationSession])
	if err != nil {
		return false, err
	}

	for _, e := range entries {
		if e.Name == validationRequest.Name {
			return false, nil
		}
	}

	entries = append(entries, sessionEntry{Name: validationRequest.Name, Tests: tests})

	return storeOwnedSession(node, entries)
}

func (r *ValidationRequestReconciler) releaseNodesFromSuccessfulRequest(ctx context.Context,
	validationRequest *v1alpha1.ValidationRequest, supersedeTests []string) error {
	// Check the whole request before releasing any node. The check inside each
	// patch still protects against quarantine starting after this precheck.
	unsafeRetryNodes, err := r.checkNodesBeforeRelease(ctx, validationRequest)
	if err != nil {
		return err
	}

	for _, ns := range validationRequest.Spec.Nodes {
		nodeExists := false

		err := r.patchNodeWithRetry(ctx, ns.Name, func(node *corev1.Node) (bool, error) {
			nodeExists = true

			return r.releaseSuccessfulNode(ctx, node, validationRequest, supersedeTests, unsafeRetryNodes)
		})
		if err != nil {
			return fmt.Errorf("release node %q from successful request: %w", ns.Name, err)
		}

		// Only groups containing a released member are unsafe to retry.
		if nodeExists {
			unsafeRetryNodes[ns.Name] = true
		}
	}

	return nil
}

func (r *ValidationRequestReconciler) releaseSuccessfulNode(ctx context.Context, node *corev1.Node,
	validationRequest *v1alpha1.ValidationRequest, supersedeTests []string,
	unsafeRetryNodes map[string]bool) (bool, error) {
	if node.Annotations[annotationActiveValidationRequest] != validationRequest.Name {
		unsafeRetryNodes[node.Name] = true
	}

	if _, quarantined := node.Annotations[annotationQuarantineHealthEvent]; quarantined {
		return false, &nodesQuarantinedError{nodeNames: []string{node.Name}, unsafeRetryNodes: unsafeRetryNodes}
	}

	if !r.onlyOwnedCordon() {
		return r.releaseSuccessfulNodeSession(node, validationRequest, supersedeTests)
	}

	if hasPendingValidationHandoff(node) {
		return false, &validationHandoffPendingError{nodeName: node.Name}
	}

	refusal := ownedReleaseRefusal(node, validationRequest)

	changed, err := r.releaseSuccessfulNodeSession(node, validationRequest, supersedeTests)
	if err == nil && refusal != "" && node.Spec.Unschedulable {
		slog.WarnContext(ctx, "Validation succeeded but the cordon was retained",
			"validationRequest", validationRequest.Name, "node", node.Name, "reason", refusal)
	}

	return changed, err
}

// releaseDeletedOwnedNode removes the deleted request's association and reports
// whether the node stays cordoned with no association left.
func (r *ValidationRequestReconciler) releaseDeletedOwnedNode(node *corev1.Node,
	request *v1alpha1.ValidationRequest) (changed, retainedCordon bool, err error) {
	changed, err = releaseOwnedNodeFromSession(node, request, nil, false, r.Config.Validation.Spec.SchedulingGate)
	if err != nil {
		return false, false, err
	}

	retainedCordon = node.Spec.Unschedulable && node.Annotations[annotationValidationSession] == ""

	return changed, retainedCordon, nil
}

func (r *ValidationRequestReconciler) releaseSuccessfulNodeSession(node *corev1.Node,
	request *v1alpha1.ValidationRequest, supersedeTests []string) (bool, error) {
	if r.onlyOwnedCordon() {
		return releaseOwnedNodeFromSession(node, request, supersedeTests, true,
			r.Config.Validation.Spec.SchedulingGate)
	}

	changed, err := releaseNodeFromSession(node, request.Name, supersedeTests,
		r.Config.Validation.Spec.SchedulingGate)
	if err != nil {
		return false, err
	}

	return retireReleasedLegacyClaim(node) || changed, nil
}

func (r *ValidationRequestReconciler) checkNodesBeforeRelease(ctx context.Context,
	validationRequest *v1alpha1.ValidationRequest) (map[string]bool, error) {
	unsafeRetryNodes := make(map[string]bool)

	var quarantinedNodes []string

	var pendingHandoff *validationHandoffPendingError

	for _, ns := range validationRequest.Spec.Nodes {
		var node corev1.Node
		if err := r.APIReader.Get(ctx, client.ObjectKey{Name: ns.Name}, &node); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}

			return nil, fmt.Errorf("check node %q before validation release: %w", ns.Name, err)
		}

		// This also detects a partial release from an earlier reconcile, whose
		// status update may have failed or whose controller may have restarted.
		if node.Annotations[annotationActiveValidationRequest] != validationRequest.Name {
			unsafeRetryNodes[ns.Name] = true
		}

		if _, quarantined := node.Annotations[annotationQuarantineHealthEvent]; quarantined {
			quarantinedNodes = append(quarantinedNodes, ns.Name)
		}

		if r.onlyOwnedCordon() && hasPendingValidationHandoff(&node) {
			pendingHandoff = &validationHandoffPendingError{nodeName: node.Name}
		}
	}

	if len(quarantinedNodes) > 0 {
		return nil, &nodesQuarantinedError{nodeNames: quarantinedNodes, unsafeRetryNodes: unsafeRetryNodes}
	}

	if pendingHandoff != nil {
		return nil, pendingHandoff
	}

	return unsafeRetryNodes, nil
}

func (r *ValidationRequestReconciler) releaseNodesFromDeletedRequest(ctx context.Context,
	validationRequest *v1alpha1.ValidationRequest) error {
	for _, ns := range validationRequest.Spec.Nodes {
		retainedGate := false
		retainedCordon := false

		err := r.patchNodeWithRetry(ctx, ns.Name, func(node *corev1.Node) (bool, error) {
			if r.onlyOwnedCordon() {
				changed, retained, err := r.releaseDeletedOwnedNode(node, validationRequest)
				retainedCordon = retained

				return changed, err
			}

			retainedGate = false
			hadSession := len(node.Annotations[annotationValidationSession]) > 0

			changed, err := releaseNodeFromSession(node, validationRequest.Name, nil, r.Config.Validation.Spec.SchedulingGate)
			if err != nil {
				return false, err
			}

			if _, quarantined := node.Annotations[annotationQuarantineHealthEvent]; quarantined &&
				hadSession && len(node.Annotations[annotationValidationSession]) == 0 {
				retainedGate = applySchedulingGateRelease(node.DeepCopy(), r.Config.Validation.Spec.SchedulingGate)
			}

			return changed, nil
		})
		if err != nil {
			return fmt.Errorf("remove deleted request %q from node %q: %w", validationRequest.Name, ns.Name, err)
		}

		if retainedGate {
			slog.WarnContext(ctx, "Deleted last validation request during quarantine; scheduling gate retained",
				"validationRequest", validationRequest.Name, "node", ns.Name,
				"recovery", "after remediation, create a new ValidationRequest or manually review the retained gate")
		}

		if retainedCordon {
			slog.WarnContext(ctx, "Deleted last validation request; cordon retained because deletion grants no release",
				"validationRequest", validationRequest.Name, "node", ns.Name,
				"recovery", "review the node and uncordon it explicitly, or validate it with a new ValidationRequest")
		}
	}

	return nil
}

func releaseNodeFromSession(node *corev1.Node, validationRequestName string, supersedeTests []string,
	schedulingGate *v1alpha1.SchedulingGateConfig) (bool, error) {
	entries, err := parseSessionAnnotation(node.Annotations[annotationValidationSession])
	if err != nil {
		return false, err
	}

	remaining, sessionBecameEmpty := removeSessionEntry(entries, validationRequestName, supersedeTests)

	changed := false

	if len(remaining) != len(entries) {
		if sessionBecameEmpty {
			delete(node.Annotations, annotationValidationSession)
		} else {
			annotation, err := marshalSessionAnnotation(remaining)
			if err != nil {
				return false, err
			}

			node.Annotations[annotationValidationSession] = annotation
		}

		changed = true
	}

	if node.Annotations[annotationActiveValidationRequest] == validationRequestName {
		delete(node.Annotations, annotationActiveValidationRequest)

		changed = true
	}

	// Deleting a request removes its session entry, but must not remove a new
	// quarantine. Successful completion checks this condition before cleanup.
	_, quarantined := node.Annotations[annotationQuarantineHealthEvent]
	if sessionBecameEmpty && !quarantined && applySchedulingGateRelease(node, schedulingGate) {
		changed = true
	}

	return changed, nil
}

func markNodeSessionEntryFailed(node *corev1.Node, validationRequestName string) (bool, error) {
	entries, err := parseSessionAnnotation(node.Annotations[annotationValidationSession])
	if err != nil {
		return false, err
	}

	changed := false

	for i := range entries {
		if entries[i].Name == validationRequestName && !entries[i].Failed {
			entries[i].Failed = true
			changed = true
		}
	}

	if changed {
		annotation, err := marshalSessionAnnotation(entries)
		if err != nil {
			return false, err
		}

		node.Annotations[annotationValidationSession] = annotation
	}

	if node.Annotations[annotationActiveValidationRequest] == validationRequestName {
		delete(node.Annotations, annotationActiveValidationRequest)

		changed = true
	}

	return changed, nil
}

func (r *ValidationRequestReconciler) releaseNodesFromFailedRequest(ctx context.Context,
	validationRequest *v1alpha1.ValidationRequest) error {
	for _, ns := range validationRequest.Spec.Nodes {
		err := r.patchNodeWithRetry(ctx, ns.Name, func(node *corev1.Node) (bool, error) {
			if r.onlyOwnedCordon() && !ownedSessionMatches(node, validationRequest) {
				return false, nil
			}

			return markNodeSessionEntryFailed(node, validationRequest.Name)
		})
		if err != nil {
			return fmt.Errorf("mark node %q failed in session: %w", ns.Name, err)
		}
	}

	return nil
}

func (r *ValidationRequestReconciler) setActiveValidationRequest(ctx context.Context,
	nodeName string, validationRequest *v1alpha1.ValidationRequest) error {
	return r.patchNodeWithRetry(ctx, nodeName, func(node *corev1.Node) (bool, error) {
		if r.onlyOwnedCordon() && !ownedSessionMatches(node, validationRequest) {
			return false, fmt.Errorf("validation request %q no longer matches node %q identity",
				validationRequest.Name, nodeName)
		}

		validationRequestName := validationRequest.Name
		if node.Annotations[annotationActiveValidationRequest] == validationRequestName {
			return false, nil
		}

		if node.Annotations == nil {
			node.Annotations = make(map[string]string)
		}

		node.Annotations[annotationActiveValidationRequest] = validationRequestName

		return true, nil
	})
}

func (r *ValidationRequestReconciler) fetchDeletedAndNotReadyNodes(ctx context.Context,
	g *v1alpha1.TestGroupStatus) ([]string, []string, error) {
	var deletedNodes, nodesFailingReadiness []string

	for _, nodeName := range g.Nodes {
		var node corev1.Node
		if err := r.Get(ctx, client.ObjectKey{Name: nodeName}, &node); err != nil {
			if apierrors.IsNotFound(err) {
				deletedNodes = append(deletedNodes, nodeName)
				continue
			}

			return nil, nil, fmt.Errorf("get node %q: %w", nodeName, err)
		}

		failedCriteria, err := evaluateCriteria(ctx, r.Client, &node, r.Config.Validation.Spec.ReadinessCriteria,
			r.ReadinessPrograms, r.ResourceSliceWatch.Enabled)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to check node readiness: %w", err)
		}

		if len(failedCriteria) != 0 {
			nodesFailingReadiness = append(nodesFailingReadiness, nodeName)
		}
	}

	return deletedNodes, nodesFailingReadiness, nil
}

func (r *ValidationRequestReconciler) groupBlockedByLiveNodeState(ctx context.Context,
	group *v1alpha1.TestGroupStatus, validationRequest *v1alpha1.ValidationRequest) (bool, error) {
	for _, nodeName := range group.Nodes {
		var node corev1.Node
		if err := r.APIReader.Get(ctx, client.ObjectKey{Name: nodeName}, &node); err != nil {
			if apierrors.IsNotFound(err) {
				// Wait for the Node watch to drive the existing deleted-node handling.
				return true, nil
			}

			return false, fmt.Errorf("check live node %q before starting validation: %w", nodeName, err)
		}

		if _, quarantined := node.Annotations[annotationQuarantineHealthEvent]; quarantined {
			return true, nil
		}

		if r.onlyOwnedCordon() && !ownedSessionMatches(&node, validationRequest) {
			return true, nil
		}

		if r.onlyOwnedCordon() && hasPendingValidationHandoff(&node) {
			return true, nil
		}
	}

	return false, nil
}

func (r *ValidationRequestReconciler) patchNodeWithRetry(ctx context.Context, nodeName string,
	mutate func(*corev1.Node) (changed bool, err error)) error {
	return kubeclient.RetryNodePatch(func() error {
		var node corev1.Node
		if err := r.APIReader.Get(ctx, client.ObjectKey{Name: nodeName}, &node); err != nil {
			return client.IgnoreNotFound(err)
		}

		original := node.DeepCopy()

		changed, err := mutate(&node)
		if err != nil {
			return err
		}

		// Deriving the validation-state label here prevents us from needing to call reconcileValidationStateLabel
		// each time we modify either the active-validation-request or validation-session annotations.
		labelChanged, err := reconcileValidationStateLabel(&node)
		if err != nil {
			return err
		}

		changed = changed || labelChanged

		if !changed {
			return nil
		}

		patch, err := validationNodeMergePatch(original, &node)
		if err != nil {
			return fmt.Errorf("build merge patch for node %q: %w", nodeName, err)
		}

		if patch == nil {
			return nil
		}

		return r.Patch(ctx, &node, client.RawPatch(types.MergePatchType, patch))
	})
}

// Session membership and handoff consumption require the same optimistic
// concurrency protection as spec changes, including annotation-only updates.
func validationNodeMergePatch(original, modified *corev1.Node) ([]byte, error) {
	patch, err := kubeclient.NodeMergePatch(original, modified)
	if err != nil || patch == nil {
		return patch, err
	}

	var document map[string]any
	if err := json.Unmarshal(patch, &document); err != nil {
		return nil, fmt.Errorf("decode node patch: %w", err)
	}

	metadata, _ := document["metadata"].(map[string]any)
	if metadata == nil {
		metadata = make(map[string]any)
		document["metadata"] = metadata
	}

	metadata["uid"] = original.UID
	metadata["resourceVersion"] = original.ResourceVersion

	return json.Marshal(document)
}

func removeSessionEntry(entries []sessionEntry, validationRequestName string,
	supersedeTests []string) (remaining []sessionEntry, sessionBecameEmpty bool) {
	for _, e := range entries {
		if e.Name == validationRequestName {
			continue
		}

		if supersedeTests != nil && e.Failed && sameTests(e.Tests, supersedeTests) {
			continue
		}

		remaining = append(remaining, e)
	}

	return remaining, len(entries) > 0 && len(remaining) == 0
}

func applySchedulingGateRelease(node *corev1.Node, schedulingGate *v1alpha1.SchedulingGateConfig) bool {
	if schedulingGate == nil {
		return false
	}

	changed := false

	if schedulingGate.Cordon.Remove && node.Spec.Unschedulable {
		node.Spec.Unschedulable = false
		changed = true

		if len(schedulingGate.Cordon.LabelPrefix) != 0 {
			releaseCordonLabels(node, schedulingGate.Cordon.LabelPrefix)
		}
	}

	return releaseConfiguredTaints(node, schedulingGate) || changed
}

func releaseConfiguredTaints(node *corev1.Node, schedulingGate *v1alpha1.SchedulingGateConfig) bool {
	if schedulingGate == nil {
		return false
	}

	remainingTaints, taintsChanged := removeConfiguredTaints(node.Spec.Taints, schedulingGate.Taints)
	node.Spec.Taints = remainingTaints

	return taintsChanged
}

func releaseCordonLabels(node *corev1.Node, prefix string) {
	if node.Labels == nil {
		node.Labels = map[string]string{}
	}

	delete(node.Labels, labels.Key(prefix, labels.CordonedBySuffix))
	delete(node.Labels, labels.Key(prefix, labels.CordonedReasonSuffix))
	delete(node.Labels, labels.Key(prefix, labels.CordonedTimestampSuffix))

	node.Labels[labels.Key(prefix, labels.UncordonedBySuffix)] = labels.ServiceName
	node.Labels[labels.Key(prefix, labels.UncordonedTimestampSuffix)] = time.Now().UTC().Format(labels.TimestampFormat)
	node.Labels[labels.Key(prefix, labels.UncordonedReasonSuffix)] = uncordonReasonValidationSucceeded
}

func removeConfiguredTaints(taints []corev1.Taint, configuredTaints []v1alpha1.TaintConfig) ([]corev1.Taint, bool) {
	remaining := slices.DeleteFunc(slices.Clone(taints), func(t corev1.Taint) bool {
		return slices.ContainsFunc(configuredTaints, func(cfg v1alpha1.TaintConfig) bool {
			return cfg.Remove && taintMatches(t, cfg)
		})
	})

	return remaining, len(remaining) != len(taints)
}

func taintMatches(nodeTaint corev1.Taint, taintCfg v1alpha1.TaintConfig) bool {
	return nodeTaint.Key == taintCfg.Key &&
		nodeTaint.Value == taintCfg.Value &&
		nodeTaint.Effect == taintCfg.Effect
}

func sameTests(a, b []string) bool {
	sortedA := append([]string{}, a...)
	sortedB := append([]string{}, b...)

	sort.Strings(sortedA)
	sort.Strings(sortedB)

	return slices.Equal(sortedA, sortedB)
}
