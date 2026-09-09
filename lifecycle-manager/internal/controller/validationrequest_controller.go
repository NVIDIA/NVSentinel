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
	"fmt"
	"time"

	"github.com/google/cel-go/cel"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/nvidia/nvsentinel/lifecycle-manager/api/v1alpha1"
	"github.com/nvidia/nvsentinel/lifecycle-manager/pkg/config"
)

const (
	annotationActiveValidationRequest = "nvsentinel.nvidia.com/active-validation-request"
	annotationValidationSession       = "nvsentinel.nvidia.com/validation-session"
	finalizerName                     = "nvsentinel.nvidia.com/validation-request"
)

type ValidationRequestReconciler struct {
	client.Client
	APIReader         client.Reader
	Scheme            *runtime.Scheme
	Config            *config.Config
	Namespace         string
	ReadinessPrograms map[string]cel.Program
}

func NewValidationRequestReconciler(cl client.Client, apiReader client.Reader, scheme *runtime.Scheme,
	cfg *config.Config, namespace string) (*ValidationRequestReconciler, error) {
	r := &ValidationRequestReconciler{
		Client:    cl,
		APIReader: apiReader,
		Scheme:    scheme,
		Config:    cfg,
		Namespace: namespace,
	}

	if cfg != nil && cfg.Validation != nil {
		programs, err := buildReadinessPrograms(cfg.Validation.Spec.ReadinessCriteria)
		if err != nil {
			return nil, fmt.Errorf("build readiness criteria programs: %w", err)
		}

		r.ReadinessPrograms = programs
	}

	return r, nil
}

func (r *ValidationRequestReconciler) SetupWithManager(mgr ctrl.Manager) error {
	controllerManager := ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.ValidationRequest{}).
		Watches(&corev1.Node{}, handler.EnqueueRequestsFromMapFunc(r.nodeToValidationRequest),
			builder.WithPredicates(predicate.NewPredicateFuncs(func(obj client.Object) bool {
				_, ok := obj.GetAnnotations()[annotationValidationSession]
				return ok
			}))).
		Named("validationrequest")

	// We need to reference the dynamic types from the TestProviders in the ValidationConfiguration. Normally, you can
	// specify a static type in Owns like this: Owns(&batchv1.Job{})
	for _, provider := range r.Config.Validation.Spec.Providers {
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(schema.GroupVersionKind{
			Group:   provider.APIGroup,
			Version: provider.Version,
			Kind:    provider.Kind,
		})
		// Namespaced-scoped TestProvider resources will only be monitored in the same namespace which is running
		// the lifecycle-manager pod from the Cache setting based to NewManager.
		controllerManager = controllerManager.Owns(u)
	}

	return controllerManager.Complete(r)
}

func (r *ValidationRequestReconciler) nodeToValidationRequest(ctx context.Context,
	obj client.Object) []reconcile.Request {
	entries, err := parseSessionAnnotation(obj.GetAnnotations()[annotationValidationSession])
	if err != nil {
		logf.FromContext(ctx).Error(err, "Failed to parse validation-session annotation", "node", obj.GetName())
		return nil
	}

	if len(entries) == 0 {
		return nil
	}

	requests := make([]reconcile.Request, len(entries))
	for i, e := range entries {
		requests[i] = reconcile.Request{NamespacedName: types.NamespacedName{Name: e.Name}}
	}

	return requests
}

/*
Outside of controller start-up or a SyncPeriod, we expect the Reconcile function to be triggered by edge-based signals
for ValidationRequest, nodes, or test provider resources. This is configured above in SetupWithManager:
- ValidationRequest CREATE, UPDATE, and DELETE events will fire the reconciler.
- Node CREATE, UPDATE, and DELETE events where the node has the validation-session annotation will fire the reconciler
for each ValidationRequest listed in the session annotation. In practice, CREATE events will never fire due to the
annotation not existing. DELETE events still fire, since the last-known cached object retains the annotation, and
this is the only signal that drives reconciling a ValidationRequest after one of its nodes is deleted.
- TestProvider resource CREATE, UPDATE, and DELETE events which have an OwnerReference for a ValidationRequest.

The only place where we rely on a level-based signal to trigger reconciling is to detect test provider
timeouts where we specify an explicit RequeueAfter time that is after the configured test provider timeout.

ValidationRequest phase transitions:
1. Init -> Pending -> Succeeded/Failed
2. Init -> Pending -> Running -> Succeeded/Failed

TestGroup transitions within a ValidationRequest:
1. Without any retries: Pending -> Running -> Succeeded/Failed
2. With 1 retry: Pending -> Running -> Pending -> Running -> Succeeded/Failed
3. Built directly as Failed with no attempts, if it cannot meet its batch minimum when initial TestGroups are built.
4. Pending -> Failed/Succeeded with no attempts, if a deleted node drops it below its batch minimum before it starts.
*/
func (r *ValidationRequestReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var validationRequest v1alpha1.ValidationRequest
	if err := r.Get(ctx, req.NamespacedName, &validationRequest); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(fmt.Errorf("get ValidationRequest %q: %w", req.Name, err))
	}

	if !validationRequest.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &validationRequest)
	}

	switch validationRequest.Status.Phase {
	case "":
		return r.reconcileInit(ctx, &validationRequest)
	case v1alpha1.PhasePending:
		return r.reconcilePending(ctx, &validationRequest)
	case v1alpha1.PhaseRunning:
		return r.reconcileRunning(ctx, &validationRequest)
	case v1alpha1.PhaseSucceeded, v1alpha1.PhaseFailed:
		return ctrl.Result{}, nil
	}

	return ctrl.Result{}, nil
}

func (r *ValidationRequestReconciler) reconcileInit(ctx context.Context,
	validationRequest *v1alpha1.ValidationRequest) (ctrl.Result, error) {
	if controllerutil.AddFinalizer(validationRequest, finalizerName) {
		if err := r.Update(ctx, validationRequest); err != nil {
			return ctrl.Result{}, fmt.Errorf("add finalizer to ValidationRequest %q: %w", validationRequest.Name, err)
		}
	}

	now := metav1.Now()
	validationRequest.Status.Phase = v1alpha1.PhasePending
	validationRequest.Status.StartTime = &now

	if err := r.Status().Update(ctx, validationRequest); err != nil {
		return ctrl.Result{}, fmt.Errorf("update ValidationRequest %q status to pending: %w",
			validationRequest.Name, err)
	}

	return ctrl.Result{}, nil
}

/*
What causes a pending ValidationRequest to be blocked?
- A node referenced in the request has a NodeReadinessViolation
- A node referenced in the request already has a running request

What causes a pending ValidationRequest to transition to a terminal status (and never enter running)?
- If a given test has a BatchMinimumNotMet failure and the BatchFailurePolicy is fail, the request will be failed.
- If a given test has a BatchMinimumNotMet failure and the BatchFailurePolicy is ignore, the given test will be skipped.
- If all nodes are deleted, the entire request will be marked as successful (regardless of the BatchFailurePolicy).
- If all tests are skipped, the entire request will be marked as successful.

What happens if a node referenced in a pending ValidationRequest is deleted?
- The node will be skipped and removed from consideration. If this results in a BatchMinimumNotMet error,
refer to the section above.

How are we ensuring that nodes only have 1 running ValidationRequest?
- If a given node requires multiple tests that cannot be batched, reconcileRunning below ensures that a given node
does not have multiple running TestGroups.
- It is the responsibility of reconcilePending to ensure that multiple ValidationRequest CRs targeting the same node
are all added to the validation-session for a given node. However, only 1 of those requests may be running at a given
time.
- Since MaxConcurrentReconciles is 1 in this controller, we can directly check if the active-validation-request
annotation is present in a given node prior to marking the request as running. If the controller supported multiple
threads, we would need to implement node-level locking within the controller by using the K8s API. This could be
accomplished by using a get and update/patch that is conditional on the latest observed ResourceVersion to check for a
conflicting update.
- For tracking if a given node has an active ValidationRequest, we are leveraging the active-validation-request
annotation. An alternative would be for the controller to list all running requests and check for node overlap prior
to starting a new request.

How are validation-sessions managed across ValidationRequests?
- A given request is added to a node's validation-session annotation when it is pending.
- A given request is removed from a node's validation-session once the same request transitions to successful, if the
same request is deleted, or if the request transitioned to failed but an equivalent request transitions to successful.
We consider a request equivalent if it covers the same set of tests for the given node. Note that an equivalent request
is only permitted to remove failed requests from the validation-session, and it is not permitted to skip any pending or
running requests.
- A validation-session annotation is removed as soon as all requests are removed. At this point, any cordon or taint
specified in the validation request is also removed from the node.
*/
func (r *ValidationRequestReconciler) reconcilePending(ctx context.Context,
	validationRequest *v1alpha1.ValidationRequest) (ctrl.Result, error) {
	criteria := r.Config.Validation.Spec.ReadinessCriteria
	resolvedTests := resolveValidationRequestTests(validationRequest, r.Config)

	existingNodes := make(map[string]bool, len(validationRequest.Spec.Nodes))

	var deletedNodes []string

	allReady := true

	for _, ns := range validationRequest.Spec.Nodes {
		exists, ready, err := r.addToSessionAndCheckEligibility(ctx, ns, validationRequest, criteria, resolvedTests)
		if err != nil {
			return ctrl.Result{}, err
		}

		switch {
		case !exists:
			// Deleted nodes will be marked as skipped
			deletedNodes = append(deletedNodes, ns.Name)
		case !ready:
			// Any node with a NodeReadinessViolation will keep the request in Pending. We don't return early here so
			// that every remaining node still gets added to the validation-session annotation
			allReady = false
		default:
			existingNodes[ns.Name] = true
		}
	}

	if !allReady {
		return ctrl.Result{}, nil
	}

	// If every node has been deleted, mark the request as successful
	if len(existingNodes) == 0 {
		return r.completeValidationRequest(ctx, validationRequest, v1alpha1.PhaseSucceeded, deletedNodes, nil)
	}

	testGroups, failedGroupsPresent, skippedTests := buildInitialTestGroups(validationRequest, r.Config, existingNodes)

	// If all tests are skipped from BatchMinimumNotMet failures but all tests specify a BatchFailurePolicy of ignore,
	// mark the request as successful
	if len(testGroups) == 0 {
		return r.completeValidationRequest(ctx, validationRequest, v1alpha1.PhaseSucceeded, deletedNodes, skippedTests)
	}

	validationRequest.Status.TestGroups = testGroups

	// If there's at least 1 test with a BatchMinimumNotMet failure that specifies a BatchFailurePolicy of fail,
	// mark the request as failed
	if failedGroupsPresent {
		return r.completeValidationRequest(ctx, validationRequest, v1alpha1.PhaseFailed, deletedNodes, skippedTests)
	}

	// If we have at least 1 existing node and there's at least 1 test which has not been skipped, mark the
	// request as running and start the eligible TestGroups.
	return r.transitionValidationRequestToRunning(ctx, validationRequest, existingNodes, deletedNodes, skippedTests)
}

/*
A given ValidationRequest will be marked as Succeeded if all TestGroups are marked as Succeeded, or if any Failed
TestGroups only cover nodes that have all been skipped (see terminalPhase and allNodesSkipped below).
- TestGroups can run concurrently if they have no overlapping nodes and do not exceed the maxConcurrentGroups limit
- A failed TestGroup will block any pending TestGroup from starting. Any existing running TestGroup will be allowed to
complete. A TestGroup will only be marked as failed after all retries are exhausted for retryable failures. See the
section below for what causes TestGroup failures and which are retryable.
- After the ValidationRequest reaches a terminal status and transitions out of running, the active-validation-request
annotation will be removed from each node in the request.
- If the request succeeded, the request will be removed from the validation-session for each node, and it will be
permitted to clear any equivalent failed requests. If the request failed, it will persist in the validation-session and
removing it will require either the request being deleted or an equivalent request succeeding.

What causes a running ValidationRequest to fail?
- If any individual TestGroup is marked as Failed.
- Any TestGroup failure that can be retried will cause the TestGroup to transition from Running -> Pending.
- If the failure is not retryable or if all retries have been exhausted, we will transition from Running -> Failed.
- All failures will result in the current test provider resource being deleted prior to the ValidationRequest status,
including the new TestGroup phase, being persisted.
- Retryable failures: TestFailed, TestTimeout, NodeReadinessViolation, NodeDeleted
- BatchMinimumNotMet is never a Running TestGroup's failure reason. It can only occur while a ValidationRequest is
pending, during buildInitialTestGroups. Once running, a deleted node that drops a TestGroup below its batch minimum
still records NodeDeleted as the attempt's failure reason, even though the resulting TestGroup phase may be Failed. In
this sense, BatchMinimumNotMet is a non-retryable failure.
- In the case of a NodeReadinessViolation, we will fail the current attempt before waiting to retry until the node
passes the criteria.

What causes a running ValidationRequest to be blocked (meaning that a TestGroup is stuck pending)?
- A node referenced in the TestGroup has a NodeReadinessViolation.
- A node referenced in the TestGroup overlaps with a running TestGroup.
- The MaxConcurrentGroups limit, which specifies the maximum number of running TestGroups, has been reached.
- Note that we check if nodes are deleted or not ready prior to starting new TestGroups so it's possible that a pending
TestGroup is marked as Failed without ever starting an attempt, if removing its deleted nodes drops it below the
minimum required for a test with a BatchFailurePolicy of fail. No attempt is created in this case, so no failure
reason (NodeDeleted or otherwise) is recorded, only the TestGroup phase is set to Failed.

What happens if a node referenced in a running ValidationRequest is deleted?
- Any Running TestGroup will have its attempt marked as Failed due to a NodeDeleted error.
- The node will be moved to the skipped section on the ValidationRequest and we will always re-evaluate the node
minimums for each of its tests.
- If the node minimums are still met for the given tests, the TestGroup will be permitted to be retried, provided
retries have not been exhausted (since NodeDeleted is a retryable failure).
- If the node minimums are no longer met for a given test and the BatchFailurePolicy is fail, the TestGroup will be
marked as Failed (with a failure reason of NodeDeleted) which will result in the overall request failing.
- If the node minimums are no longer met for a given test and the BatchFailurePolicy is ignore, the given test will be
skipped. If this TestGroup has other batched tests, the remaining tests will be retried. If all tests are skipped, the
TestGroup will be marked as Successful.
- If all tests are skipped, each individual TestGroup will always be marked as successful. However, it's possible that
all nodes were deleted and all TestGroups failed with a NodeDeleted failure reason because a test had a
BatchFailurePolicy of fail. As a result, we do a final check if all nodes were deleted prior to marking an overall
ValidationRequest as failed or successful.
*/
func (r *ValidationRequestReconciler) reconcileRunning(ctx context.Context,
	validationRequest *v1alpha1.ValidationRequest) (ctrl.Result, error) {
	hasFailedTestGroups, hasRunningTestGroups, hasPendingTestGroups, err := r.reconcileTestGroups(ctx, validationRequest)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Only start new TestGroups if we don't have failed TestGroups
	if !hasFailedTestGroups {
		if err := r.startPendingTestGroups(ctx, validationRequest); err != nil {
			return ctrl.Result{}, err
		}
	}

	// A ValidationRequest is considered terminal if it has no running TestGroups and if it has either:
	// 1. It has no pending TestGroups
	// 2. It has pending TestGroups but there's at least 1 failed TestGroup
	// Note that if we discover a failed TestGroup, we'll allow any running test group to complete before marking the
	// ValidationRequest with a terminal status.
	allTerminal := !hasRunningTestGroups && (!hasPendingTestGroups || hasFailedTestGroups)

	if allTerminal {
		phase := terminalPhase(hasFailedTestGroups, validationRequest)
		return r.completeValidationRequest(ctx, validationRequest, phase, nil, nil)
	}

	if err := r.Status().Update(ctx, validationRequest); err != nil {
		return ctrl.Result{}, fmt.Errorf("update ValidationRequest %q status while running: %w",
			validationRequest.Name, err)
	}

	// This is our only level-based re-queue which allows us to catch TestTimeout failures for TestGroups.
	if next := r.getNextTestGroupTimeout(validationRequest); next > 0 {
		return ctrl.Result{RequeueAfter: next}, nil
	}

	return ctrl.Result{}, nil
}

/*
The reconcileDelete function for ValidationRequest will ensure that any running TestGroup will have its test provider
resource cleaned up prior to finalizer removal and ValidationRequest deletion. Additionally, the given request will
be removed from the validation-session for the given node (and uncordoned or untainted if it was the last request in
the session). If the ValidationRequest was running, the active-validation-request annotation will also be removed for
the given node.
*/
func (r *ValidationRequestReconciler) reconcileDelete(ctx context.Context,
	validationRequest *v1alpha1.ValidationRequest) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(validationRequest, finalizerName) {
		return ctrl.Result{}, nil
	}

	for i := range validationRequest.Status.TestGroups {
		g := &validationRequest.Status.TestGroups[i]
		if len(g.Attempts) > 0 {
			a := g.Attempts[len(g.Attempts)-1]
			if a.Phase == v1alpha1.PhaseRunning {
				if err := r.deleteTestGroupObject(ctx, g, a.ObjectName); err != nil {
					return ctrl.Result{}, err
				}
			}
		}
	}

	if err := r.releaseNodesFromSuccessfulRequest(ctx, validationRequest, nil); err != nil {
		return ctrl.Result{}, err
	}

	controllerutil.RemoveFinalizer(validationRequest, finalizerName)

	if err := r.Update(ctx, validationRequest); err != nil {
		return ctrl.Result{}, fmt.Errorf("remove finalizer from ValidationRequest %q: %w", validationRequest.Name, err)
	}

	return ctrl.Result{}, nil
}

func (r *ValidationRequestReconciler) completeValidationRequest(ctx context.Context,
	validationRequest *v1alpha1.ValidationRequest, phase v1alpha1.Phase,
	skippedNodes, skippedTests []string) (ctrl.Result, error) {
	if phase == v1alpha1.PhaseSucceeded {
		err := r.releaseNodesFromSuccessfulRequest(ctx, validationRequest,
			resolveValidationRequestTests(validationRequest, r.Config))
		if err != nil {
			return ctrl.Result{}, err
		}
	} else if err := r.releaseNodesFromFailedRequest(ctx, validationRequest); err != nil {
		return ctrl.Result{}, err
	}

	validationRequest.Status.Skipped = buildSkippedStatus(validationRequest.Status.Skipped, skippedNodes, skippedTests)

	now := metav1.Now()
	validationRequest.Status.Phase = phase
	validationRequest.Status.CompletionTime = &now

	if err := r.Status().Update(ctx, validationRequest); err != nil {
		return ctrl.Result{}, fmt.Errorf("update ValidationRequest %q status to %s: %w",
			validationRequest.Name, phase, err)
	}

	return ctrl.Result{}, nil
}

func (r *ValidationRequestReconciler) transitionValidationRequestToRunning(ctx context.Context,
	validationRequest *v1alpha1.ValidationRequest, existingNodes map[string]bool, deletedNodes []string,
	skippedTests []string) (ctrl.Result, error) {
	for n := range existingNodes {
		if err := r.setActiveValidationRequest(ctx, n, validationRequest.Name); err != nil {
			return ctrl.Result{}, fmt.Errorf("set active annotation on node %q: %w", n, err)
		}
	}

	validationRequest.Status.Phase = v1alpha1.PhaseRunning
	validationRequest.Status.Skipped = buildSkippedStatus(validationRequest.Status.Skipped, deletedNodes, skippedTests)

	if err := r.startPendingTestGroups(ctx, validationRequest); err != nil {
		return ctrl.Result{}, err
	}

	// If we successfully create a TestGroup resource but fail to update the ValidationRequest to track that object, we'll
	// discover the object already exists because we use fixed resource naming per TestGroup attempt. In this case,
	// we'll inherit the existing resource on the re-queue and retry the status update for the already-existing object.
	if err := r.Status().Update(ctx, validationRequest); err != nil {
		return ctrl.Result{}, fmt.Errorf("update ValidationRequest %q status to running: %w",
			validationRequest.Name, err)
	}

	return ctrl.Result{}, nil
}

func (r *ValidationRequestReconciler) reconcileTestGroups(ctx context.Context,
	validationRequest *v1alpha1.ValidationRequest) (hasFailed, hasRunning, hasPending bool, err error) {
	for i := range validationRequest.Status.TestGroups {
		currentTestGroup := &validationRequest.Status.TestGroups[i]
		// It's possible we keep a running TestGroup running, move to failed, or move to pending. As a result, we should
		// call reconcileRunningTestGroup prior to checking its current phase.
		if currentTestGroup.Phase == v1alpha1.PhaseRunning {
			if err := r.reconcileRunningTestGroup(ctx, validationRequest, currentTestGroup); err != nil {
				return false, false, false, err
			}
		}

		switch currentTestGroup.Phase {
		case v1alpha1.PhaseFailed:
			hasFailed = true
		case v1alpha1.PhaseRunning:
			hasRunning = true
		case v1alpha1.PhasePending:
			hasPending = true
		case v1alpha1.PhaseSucceeded:
		}
	}

	return hasFailed, hasRunning, hasPending, nil
}

func (r *ValidationRequestReconciler) reconcileRunningTestGroup(ctx context.Context,
	validationRequest *v1alpha1.ValidationRequest, currentTestGroup *v1alpha1.TestGroupStatus) error {
	testGroupAttempt := &currentTestGroup.Attempts[len(currentTestGroup.Attempts)-1]

	testProviderConfigForGroup, ok := r.Config.Validation.Spec.Providers[currentTestGroup.Provider]
	if !ok {
		return fmt.Errorf("provider %q referenced by test group %q not found in ValidationConfiguration",
			currentTestGroup.Provider, currentTestGroup.Name)
	}

	timedOut := time.Since(testGroupAttempt.StartTime.Time) >=
		time.Duration(testProviderConfigForGroup.TimeoutSeconds)*time.Second

	newTestGroupPhaseIfAttemptFailed := newTestGroupPhaseAfterAttempt(len(currentTestGroup.Attempts),
		testProviderConfigForGroup.Retries)

	deletedNodes, nodesFailingReadiness, err := r.fetchDeletedAndNotReadyNodes(ctx, currentTestGroup)
	if err != nil {
		return fmt.Errorf("checking group nodes: %w", err)
	}

	testGroupObjectSucceeded, testGroupObjectFailed, err := r.checkTestGroupObjectStatus(ctx, currentTestGroup,
		testGroupAttempt.ObjectName)
	if err != nil {
		return fmt.Errorf("checking provider resource: %w", err)
	}

	var failedNodes []string

	var newGroupPhase, attemptPhase v1alpha1.Phase

	var attemptFailureReason v1alpha1.FailureReason

	// Ordering of checks:
	// 1. NodeDeleted must be checked first. If we have exhausted all retries, the deleted nodes cause us to drop below
	// MinimumNodesPerBatch, the BatchFailurePolicy is ignore, and there's no remaining tests, we will mark the overall
	// TestGroup as successful. In other words, the deleted nodes case is the only case where a failed attempt could
	// result in the TestGroup phase transitioning to successful rather than failed or back to pending. Note that a
	// BatchMinimumNotMet failure reason is not possible for any TestGroup after the ValidationRequest transitions to
	// running and this reason will only be reflected during buildInitialTestGroups. As a result, any running or
	// pending TestGroup that encounters a deleted node will show the NodeDeleted reason regardless of the batch
	// failure policy.
	// 2. Succeeded must be checked before TestTimeout. TestTimeout failures occur when the duration between the
	// attempt's start time and the current time is greater than the provider's timeout. Reconciling delays could result
	// in a duration that exceeds the provider timeout even if the TestGroup attempt completed successfully within
	// the timeout.
	switch {
	case len(deletedNodes) > 0:
		failedNodes = deletedNodes
		newGroupPhase = removeDeletedNodesFromTestGroup(validationRequest, currentTestGroup, deletedNodes, r.Config)
		attemptPhase = v1alpha1.PhaseFailed
		attemptFailureReason = v1alpha1.FailureReasonNodeDeleted
	case len(nodesFailingReadiness) > 0:
		failedNodes = nodesFailingReadiness
		newGroupPhase = newTestGroupPhaseIfAttemptFailed
		attemptPhase = v1alpha1.PhaseFailed
		attemptFailureReason = v1alpha1.FailureReasonNodeReadinessViolation
	case testGroupObjectSucceeded:
		newGroupPhase = v1alpha1.PhaseSucceeded
		attemptPhase = v1alpha1.PhaseSucceeded
	case timedOut:
		newGroupPhase = newTestGroupPhaseIfAttemptFailed
		attemptPhase = v1alpha1.PhaseFailed
		attemptFailureReason = v1alpha1.FailureReasonTestTimeout
	case testGroupObjectFailed:
		newGroupPhase = newTestGroupPhaseIfAttemptFailed
		attemptPhase = v1alpha1.PhaseFailed
		attemptFailureReason = v1alpha1.FailureReasonTestFailed
	default:
		return nil
	}

	currentTestGroup.Phase = newGroupPhase
	testGroupAttempt.Phase = attemptPhase
	testGroupAttempt.FailureReason = attemptFailureReason
	now := metav1.Now()
	testGroupAttempt.EndTime = &now
	testGroupAttempt.FailedNodes = failedNodes

	return r.deleteTestGroupObject(ctx, currentTestGroup, testGroupAttempt.ObjectName)
}

func (r *ValidationRequestReconciler) startPendingTestGroups(ctx context.Context,
	validationRequest *v1alpha1.ValidationRequest) error {
	maxConcurrentRunningTestGroups := r.Config.Validation.Spec.MaxConcurrentGroups
	runningTestGroupsCount := 0
	runningTestGroupNodes := make(map[string]bool)

	var pendingTestGroups []int

	for i, currentTestGroup := range validationRequest.Status.TestGroups {
		switch currentTestGroup.Phase {
		case v1alpha1.PhaseRunning:
			runningTestGroupsCount++

			for _, n := range currentTestGroup.Nodes {
				runningTestGroupNodes[n] = true
			}
		case v1alpha1.PhasePending:
			pendingTestGroups = append(pendingTestGroups, i)
		case v1alpha1.PhaseSucceeded, v1alpha1.PhaseFailed:
		}
	}

	for _, pendingGroupIndex := range pendingTestGroups {
		if runningTestGroupsCount >= maxConcurrentRunningTestGroups {
			return nil
		}

		currentPendingTestGroup := &validationRequest.Status.TestGroups[pendingGroupIndex]

		started, err := r.startPendingTestGroup(ctx, validationRequest, currentPendingTestGroup, runningTestGroupNodes)
		if err != nil {
			return err
		}

		if started {
			runningTestGroupsCount++
		}
	}

	return nil
}

func (r *ValidationRequestReconciler) startPendingTestGroup(ctx context.Context,
	validationRequest *v1alpha1.ValidationRequest, currentPendingTestGroup *v1alpha1.TestGroupStatus,
	runningTestGroupNodes map[string]bool) (bool, error) {
	for _, n := range currentPendingTestGroup.Nodes {
		if runningTestGroupNodes[n] {
			return false, nil
		}
	}

	deletedNodes, nodesFailingReadiness, err := r.fetchDeletedAndNotReadyNodes(ctx, currentPendingTestGroup)
	if err != nil {
		return false, fmt.Errorf("checking group nodes for %q: %w", currentPendingTestGroup.Name, err)
	}

	if len(deletedNodes) > 0 {
		nextPhase := removeDeletedNodesFromTestGroup(validationRequest, currentPendingTestGroup, deletedNodes, r.Config)
		if nextPhase != v1alpha1.PhasePending {
			return false, nil
		}
	}

	if len(nodesFailingReadiness) > 0 {
		return false, nil
	}

	objectName := attemptObjectName(validationRequest.Name, currentPendingTestGroup.Name,
		len(currentPendingTestGroup.Attempts)+1)
	if err := r.createTestGroupObject(ctx, validationRequest, currentPendingTestGroup, objectName); err != nil {
		return false, fmt.Errorf("creating provider resource for group %q: %w", currentPendingTestGroup.Name, err)
	}

	now := metav1.Now()
	currentPendingTestGroup.Attempts = append(currentPendingTestGroup.Attempts, v1alpha1.AttemptStatus{
		ObjectName: objectName,
		Phase:      v1alpha1.PhaseRunning,
		StartTime:  &now,
	})
	currentPendingTestGroup.Phase = v1alpha1.PhaseRunning

	for _, n := range currentPendingTestGroup.Nodes {
		runningTestGroupNodes[n] = true
	}

	return true, nil
}

func terminalPhase(hasFailedTestGroups bool, validationRequest *v1alpha1.ValidationRequest) v1alpha1.Phase {
	if hasFailedTestGroups && !allNodesSkipped(validationRequest) {
		return v1alpha1.PhaseFailed
	}

	return v1alpha1.PhaseSucceeded
}

func (r *ValidationRequestReconciler) getNextTestGroupTimeout(
	validationRequest *v1alpha1.ValidationRequest) time.Duration {
	var nextTestGroupTimeout time.Duration

	for _, currentTestGroup := range validationRequest.Status.TestGroups {
		remaining := r.testGroupTimeoutRemaining(currentTestGroup)
		if remaining > 0 && (nextTestGroupTimeout == 0 || remaining < nextTestGroupTimeout) {
			nextTestGroupTimeout = remaining
		}
	}

	return nextTestGroupTimeout
}

func (r *ValidationRequestReconciler) testGroupTimeoutRemaining(g v1alpha1.TestGroupStatus) time.Duration {
	if g.Phase != v1alpha1.PhaseRunning {
		return 0
	}

	providerCfg := r.Config.Validation.Spec.Providers[g.Provider]
	if providerCfg.TimeoutSeconds <= 0 {
		return 0
	}

	timeout := time.Duration(providerCfg.TimeoutSeconds) * time.Second

	attempt := g.Attempts[len(g.Attempts)-1]

	remaining := time.Until(attempt.StartTime.Add(timeout))
	if remaining <= 0 {
		remaining = time.Second
	}

	return remaining
}
