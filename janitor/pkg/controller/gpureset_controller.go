// Copyright (c) 2025, NVIDIA CORPORATION. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// nolint:wsl,gocognit,cyclop,gocyclo,nestif // Business logic migrated from old code
package controller

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"sort"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/nvidia/nvsentinel/janitor/api/v1alpha1"
	"github.com/nvidia/nvsentinel/janitor/pkg/config"
	"github.com/nvidia/nvsentinel/janitor/pkg/distributedlock"
	"github.com/nvidia/nvsentinel/janitor/pkg/gpuservices"
	"github.com/nvidia/nvsentinel/janitor/pkg/metrics"
)

const (
	// gpuResetFinalizer is the name of the finalizer added to GPUReset objects.
	gpuResetFinalizer = "janitor.dgxc.nvidia.com/finalizer"
	// jobOwnerKey is used to index Jobs by their owner (the GPUReset resource).
	jobOwnerKey = "metadata.controller"
	// gpuResetsEnvVar is the name of the environment variable set in the GPU reset Job
	// containing a comma-separated list of GPU UUIDs and/or PCI Bus IDs to reset.
	gpuResetsEnvVar = "NVIDIA_GPU_RESETS"
	// jobNameSuffix is the static string appended to the GPUReset name to form the Job's name.
	jobNameSuffix = "-reset-job"
	// podNameSuffixLength is the 6-character suffix (e.g., "-abc12") added by K8s to a Job's name to create its Pods.
	podNameSuffixLength = 6
	// maxJobNameBaseLength calculates the maximum allowed length for the base name (derived from the GPUReset name)
	// to ensure the final name for Pods created by the Job does not exceed the 63-character limit.
	maxJobNameBaseLength = validation.DNS1123LabelMaxLength - len(jobNameSuffix) - 1 - 8 - podNameSuffixLength
)

// checkPodsTerminatedFn is a function signature used for checking if managed service pods have terminated.
type checkPodsTerminatedFn func(ctx context.Context, nodeName string) (bool, error)

// checkPodsReadyFn is a function signature used for checking if managed service pods are ready after restoration.
type checkPodsReadyFn func(ctx context.Context, nodeName string) (bool, error)

// GPUResetReconciler reconciles a GPUReset object
type GPUResetReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// FieldIndexer enables client-side field indexing, allowing for  mocking in tests.
	FieldIndexer client.FieldIndexer
	// Config holds controller-specific configuration.
	Config *config.GPUResetControllerConfig
	// serviceManager is the configuration for the managed GPU services that should be torn down and restored during
	// the reset workflow.
	serviceManager gpuservices.Manager
	// checkPodsTerminatedFn is a function pointer for checking pod termination status, allowing for mocking in tests.
	checkPodsTerminatedFn checkPodsTerminatedFn
	// checkPodsReadyFn is a function pointer for checking pod readiness status, allowing for mocking in tests.
	checkPodsReadyFn checkPodsReadyFn
	// NodeLock provides node-level locking across Janitor controllers
	NodeLock      distributedlock.NodeLock
	LockNamespace string
}

// +kubebuilder:rbac:groups=janitor.dgxc.nvidia.com,resources=gpuresets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=janitor.dgxc.nvidia.com,resources=gpuresets/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=janitor.dgxc.nvidia.com,resources=gpuresets/finalizers,verbs=update

// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch;patch;update
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups="coordination.k8s.io",resources=leases,verbs=get;list;watch;create;delete

// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;delete;patch

func (r *GPUResetReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var gpuReset v1alpha1.GPUReset
	if err := r.Get(ctx, req.NamespacedName, &gpuReset); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}

		return ctrl.Result{}, fmt.Errorf("failed to get GPUReset %s: %w", req.NamespacedName, err)
	}

	if !gpuReset.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &gpuReset)
	}

	completedReconciling := gpuReset.Status.CompletionTime != nil
	if !completedReconciling {
		locked := r.NodeLock.LockNode(ctx, &gpuReset, gpuReset.Spec.NodeName)
		if !locked {
			return ctrl.Result{RequeueAfter: time.Second * 2}, nil
		}

		result, err := r.reconcileHelper(ctx, &gpuReset)
		// We will always re-queue the object and check if Unlock is needed on the next reconcile rather than
		// re-fetch the object or require reconcileHelper to specify it completed reconciling. If the controller
		// forces a re-queue by returning an error or setting a RequeueAfter, we will respect that re-queue behavior,
		// else we will manually set RequeueAfter. As a result, if the controller doesn't force a re-queue or forces
		// a re-queue by setting Requeue, we will set RequeueAfter. Setting Requeue to true is deprecated
		// and can lead to long reconcile times so we will enforce that RequeueAfter is used.
		if err != nil || result.RequeueAfter > 0 {
			return result, err
		}

		return ctrl.Result{RequeueAfter: time.Second * 2}, nil
	}

	retryUnlock := r.NodeLock.CheckUnlock(ctx, &gpuReset, gpuReset.Spec.NodeName)
	if retryUnlock {
		return ctrl.Result{RequeueAfter: time.Second * 2}, nil
	}

	return ctrl.Result{}, nil
}

func (r *GPUResetReconciler) reconcileHelper(ctx context.Context, gr *v1alpha1.GPUReset) (ctrl.Result, error) {
	if len(gr.Status.Conditions) == 0 {
		return r.initialize(ctx, gr)
	}

	if !meta.IsStatusConditionTrue(gr.Status.Conditions, string(v1alpha1.Ready)) {
		return r.isReady(ctx, gr)
	}

	if !meta.IsStatusConditionTrue(gr.Status.Conditions, string(v1alpha1.ServicesTornDown)) {
		return r.tearDownServices(ctx, gr)
	}

	if !meta.IsStatusConditionTrue(gr.Status.Conditions, string(v1alpha1.ResetJobCreated)) {
		return r.createJob(ctx, gr)
	}

	if !meta.IsStatusConditionTrue(gr.Status.Conditions, string(v1alpha1.ResetJobCompleted)) {
		return r.checkJobStatus(ctx, gr)
	}

	if !meta.IsStatusConditionTrue(gr.Status.Conditions, string(v1alpha1.ServicesRestored)) {
		return r.restoreServices(ctx, gr)
	}

	return r.reconcileCompletion(ctx, gr)
}

// SetupWithManager sets up the controller with the Manager. It configures watches
// for GPUResets and owned Jobs, and adds field indexers for efficient lookups
// of GPUResets by node name and Jobs by their controlling owner.
func (r *GPUResetReconciler) SetupWithManager(mgr ctrl.Manager) error {
	gpuServiceManager, err := gpuservices.NewManager(r.Config.ServiceManager.Name, r.Config.ServiceManager.Spec)
	if err != nil {
		return fmt.Errorf("failed to construct GPU service manager: %w", err)
	}

	r.serviceManager = gpuServiceManager

	if r.Config.ResolvedJobTemplate == nil {
		return fmt.Errorf("failed to get valid reset job template")
	}
	// Assign the manager's field indexer, which can be overridden in tests.
	r.FieldIndexer = mgr.GetFieldIndexer()
	// Set concrete implementations for checking pod status, which can be overridden in tests.
	r.checkPodsTerminatedFn = r.checkPodsTerminated
	r.checkPodsReadyFn = r.checkPodsReady

	// Initialize NodeLock for distributed locking across maintenance operations
	r.NodeLock = distributedlock.NewNodeLock(mgr.GetClient(), r.LockNamespace)

	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.GPUReset{}).
		// Trigger a reconciliation for the owning GPUReset resource whenever a Job owned by a GPUReset resource
		// changes state.
		Owns(&batchv1.Job{}).
		Named("gpureset").
		Complete(r)
}

// initialize prepares a new GPUReset resource for reconciliation. It adds the finalizer,
// records the start time, and sets the initial 'Ready' condition to Pending.
//
// It also increments the total requests metric.
func (r *GPUResetReconciler) initialize(ctx context.Context, gr *v1alpha1.GPUReset) (ctrl.Result, error) {
	isNewRequest := gr.Status.StartTime == nil
	updatedGR := gr.DeepCopy()

	if !controllerutil.ContainsFinalizer(updatedGR, gpuResetFinalizer) {
		controllerutil.AddFinalizer(updatedGR, gpuResetFinalizer)

		if err := r.Update(ctx, updatedGR); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to add finalizer to %s: %w", gr.Name, err)
		}
	}

	if isNewRequest {
		now := metav1.Now()
		updatedGR.Status.StartTime = &now
	}

	if meta.FindStatusCondition(updatedGR.Status.Conditions, string(v1alpha1.Ready)) == nil {
		updatedGR.Status.Phase = v1alpha1.ResetPending
		pendingCond := NewCondition(v1alpha1.Ready, metav1.ConditionFalse, v1alpha1.ReasonResetPending,
			"GPU reset pending")
		meta.SetStatusCondition(&updatedGR.Status.Conditions, pendingCond)
	}

	if err := r.updateStatus(ctx, gr, updatedGR.Status); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to initialize status for GPUReset %s: %w", gr.Name, err)
	}

	if isNewRequest {
		metrics.GPUResetRequestsTotal.WithLabelValues(gr.Spec.NodeName).Inc()
	}

	return ctrl.Result{}, nil
}

// reconcileDelete handles the finalization logic for a GPUReset resource,
// ensuring managed services are restored before the resource is fully deleted.
func (r *GPUResetReconciler) reconcileDelete(ctx context.Context, gr *v1alpha1.GPUReset) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	nodeName := gr.Spec.NodeName
	managerName := r.serviceManager.Name

	if controllerutil.ContainsFinalizer(gr, gpuResetFinalizer) {
		if len(r.serviceManager.Spec.Apps) == 0 {
			log.V(1).Info("GPU services manager has no apps specified, skipping service restoration", "manager",
				managerName, "node", nodeName)
			controllerutil.RemoveFinalizer(gr, gpuResetFinalizer)

			if err := r.Update(ctx, gr); err != nil {
				return ctrl.Result{}, fmt.Errorf("failed to remove finalizer from %s: %w", gr.Name, err)
			}

			return ctrl.Result{}, nil
		}

		if !meta.IsStatusConditionTrue(gr.Status.Conditions, string(v1alpha1.Terminating)) {
			log.Info("Running finalizer to restore managed services before deletion", "manager", managerName, "node",
				nodeName)

			if err := r.updateCondition(ctx, gr, v1alpha1.Terminating, metav1.ConditionTrue,
				v1alpha1.ReasonFinalizerRestoringServices, "Restoring managed services before deletion"); err != nil {
				return ctrl.Result{}, err
			}
		}

		if res, err := r.restoreServices(ctx, gr); err != nil || res.RequeueAfter > 0 {
			log.V(1).Info("Waiting for managed service restoration to complete before removing finalizer", "manager",
				managerName, "node", nodeName)

			return res, err
		}

		log.Info("Managed service restoration complete, removing finalizer", "manager", managerName, "node", nodeName)
		controllerutil.RemoveFinalizer(gr, gpuResetFinalizer)

		if err := r.Update(ctx, gr); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to remove finalizer from %s: %w", gr.Name, err)
		}
	}

	return ctrl.Result{}, nil
}

// isReady ensures only one GPUReset is executed at a time per node. It finds
// all pending and in-progress resets for the target node and only allows the
// oldest one (by creation timestamp) to proceed. All other resets for that node
// are put into a waiting state with a 'ResourceContention' reason.
//
// It also enforces the 'pending' and 'active' gauge metrics based on the current cluster state.
func (r *GPUResetReconciler) isReady(ctx context.Context, gr *v1alpha1.GPUReset) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	nodeName := gr.Spec.NodeName

	var gpuResetsForNode v1alpha1.GPUResetList
	if err := r.List(ctx, &gpuResetsForNode, client.MatchingFields{"spec.nodeName": nodeName}); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to list GPUResets for node %s: %w", nodeName, err)
	}

	var gpuResetCandidates []*v1alpha1.GPUReset

	for i := range gpuResetsForNode.Items {
		gpuReset := &gpuResetsForNode.Items[i]
		if gpuReset.Status.CompletionTime == nil {
			gpuResetCandidates = append(gpuResetCandidates, gpuReset)
		}
	}

	if len(gpuResetCandidates) == 0 {
		// update metrics: enforce the absolute state (Active=0, Pending=0)
		metrics.GPUResetActiveRequests.WithLabelValues(nodeName).Set(0)
		metrics.GPUResetPendingRequests.WithLabelValues(nodeName).Set(0)

		log.V(1).Info("No pending GPU resets found for node, will recheck", "node", nodeName,
			"recheck_after", 5*time.Second)

		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	sort.Slice(gpuResetCandidates, func(i, j int) bool {
		return gpuResetCandidates[i].CreationTimestamp.Before(&gpuResetCandidates[j].CreationTimestamp)
	})

	var reason v1alpha1.GPUResetReason

	var message string

	var status metav1.ConditionStatus

	isCandidateToRun := gpuResetCandidates[0].UID == gr.UID
	ready := false

	if isCandidateToRun {
		status = metav1.ConditionTrue
		reason = v1alpha1.ReasonReadyForReset
		message = "Node ready for GPU reset"
		ready = true
	} else {
		status = metav1.ConditionFalse
		reason = v1alpha1.ReasonResourceContention
		message = fmt.Sprintf("Waiting for %s to complete before starting", gpuResetCandidates[0].Name)
	}

	if err := r.updateCondition(ctx, gr, v1alpha1.Ready, status, reason, message); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to update GPUReset %s: %w", gr.Name, err)
	}

	activeRequestsCount := 1.0
	pendingRequestsCount := float64(len(gpuResetCandidates) - 1)
	// update metrics: enforce the absolute state (Active=1, Pending=Total-1)
	metrics.GPUResetActiveRequests.WithLabelValues(nodeName).Set(activeRequestsCount)
	metrics.GPUResetPendingRequests.WithLabelValues(nodeName).Set(pendingRequestsCount)

	if !ready {
		log.V(1).Info("Pending completion of active GPU reset on node", "node", nodeName, "active_reset",
			gpuResetCandidates[0].Name)

		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	return ctrl.Result{}, nil
}

// tearDownServices disables the managed services. It disables them by setting node labels
// to their disabled state and waits for the associated pods to terminate before proceeding.
func (r *GPUResetReconciler) tearDownServices(ctx context.Context, gr *v1alpha1.GPUReset) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	managerName := r.serviceManager.Name

	if len(r.serviceManager.Spec.Apps) == 0 {
		log.V(1).Info("GPU services manager has no apps specified, skipping service teardown", "manager", managerName,
			"node", gr.Spec.NodeName)

		if err := r.updateCondition(ctx, gr, v1alpha1.ServicesTornDown, metav1.ConditionTrue, v1alpha1.ReasonSkipped,
			"No services manager configured, or has no managed services specified"); err != nil {
			return ctrl.Result{}, err
		}

		return ctrl.Result{}, nil
	}

	currentCond := meta.FindStatusCondition(gr.Status.Conditions, string(v1alpha1.ServicesTornDown))
	if currentCond == nil {
		if err := r.updateCondition(ctx, gr, v1alpha1.ServicesTornDown, metav1.ConditionFalse,
			v1alpha1.ReasonTearingDownServices, fmt.Sprintf("Removing %s managed services", managerName)); err != nil {
			return ctrl.Result{}, err
		}

		return ctrl.Result{}, nil
	}

	teardownTimeout := r.serviceManager.Spec.TeardownTimeout

	timeSinceInProgress := 0 * time.Minute
	if currentCond.Reason == string(v1alpha1.ReasonTearingDownServices) {
		timeSinceInProgress = time.Since(currentCond.LastTransitionTime.Time)
	}

	if timeSinceInProgress > teardownTimeout {
		log.Error(nil, "Managed service teardown timeout exceeded", "manager", managerName, "timeout", teardownTimeout)

		return r.reconcileTerminalFailure(ctx, gr, v1alpha1.ReasonServiceTeardownTimeoutExceeded,
			fmt.Sprintf("Failed to teardown %s managed services within the timeout period", managerName))
	}

	node, err := r.getNode(gr.Spec.NodeName)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return r.reconcileTerminalFailure(ctx, gr, v1alpha1.NodeNotFound, "Target node for GPU reset was not found")
		}

		// Likely transient, errors during node retrieval
		log.V(1).Info("Failed to get node for service teardown", "node", gr.Spec.NodeName)

		return ctrl.Result{}, fmt.Errorf("failed to get node %s for service teardown: %w", gr.Spec.NodeName, err)
	}

	nodeToUpdate := node.DeepCopy()
	nodeUpdated := false

	// Set node labels to disable managed services
	for _, app := range r.serviceManager.Spec.Apps {
		if value, exists := nodeToUpdate.Labels[app.NodeLabel]; !exists || value != app.DisabledValue {
			log.V(1).Info("Setting node label to disable managed services", "node", node.Name, "manager", managerName,
				"label", app.NodeLabel, "value", app.DisabledValue)

			if nodeToUpdate.Labels == nil {
				nodeToUpdate.Labels = make(map[string]string)
			}

			nodeToUpdate.Labels[app.NodeLabel] = app.DisabledValue
			nodeUpdated = true
		}
	}

	if nodeUpdated {
		if err := r.Patch(ctx, nodeToUpdate, client.MergeFrom(node)); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to patch node %s to disable %s managed services: %w",
				node.Name, managerName, err)
		}

		log.Info("Managed services disabled", "node", node.Name, "manager", managerName)

		return ctrl.Result{RequeueAfter: time.Second * 2}, nil
	}

	// Wait for pods to terminate
	podsAreGone, err := r.checkPodsTerminatedFn(ctx, gr.Spec.NodeName)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to check pod termination status for node %s: %w",
			gr.Spec.NodeName, err)
	}

	if !podsAreGone {
		log.V(1).Info("Waiting for managed service pods to be terminated", "node", gr.Spec.NodeName, "manager",
			managerName, "recheck_after", 3*time.Second)

		return ctrl.Result{RequeueAfter: 3 * time.Second}, nil
	}

	if currentCond.Status != metav1.ConditionTrue {
		if err := r.updateCondition(ctx, gr, v1alpha1.ServicesTornDown, metav1.ConditionTrue,
			v1alpha1.ReasonServiceTeardownSucceeded, fmt.Sprintf("%s managed services have been removed",
				managerName)); err != nil {
			return ctrl.Result{}, err
		}

		return ctrl.Result{}, nil
	}

	log.Info("Teardown of managed service pods complete", "node", gr.Spec.NodeName, "manager", managerName)

	return ctrl.Result{}, nil
}

// createJob ensures the GPU reset Job is claimed in the status. It first sets
// the .Status.JobRef field. On subsequent reconciles, it delegates to
// getOrCreateJob to ensure the Job resource exists.
func (r *GPUResetReconciler) createJob(ctx context.Context, gr *v1alpha1.GPUReset) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	jobName := r.expectedJobName(gr)
	jobNamespace := r.Config.ResolvedJobTemplate.Namespace

	if gr.Status.JobRef != nil {
		if gr.Status.JobRef.Name != jobName {
			return r.reconcileTerminalFailure(ctx, gr, v1alpha1.ReasonInternalError,
				fmt.Sprintf("JobRef name %s does not match expected name %s", gr.Status.JobRef.Name, jobName))
		}

		return r.getOrCreateJob(ctx, gr)
	}

	log.V(1).Info("Attempting to claim GPU reset job", "job", jobName, "namespace", jobNamespace)

	updatedGR := gr.DeepCopy()
	updatedGR.Status.JobRef = &corev1.ObjectReference{Kind: "Job", Name: jobName, Namespace: jobNamespace}
	updatedGR.Status.Phase = v1alpha1.ResetInProgress

	if meta.FindStatusCondition(updatedGR.Status.Conditions, string(v1alpha1.ResetJobCreated)) == nil {
		jobCreatedCond := NewCondition(v1alpha1.ResetJobCreated, metav1.ConditionFalse, v1alpha1.ReasonCreatingResetJob,
			"Claiming GPU reset job")
		meta.SetStatusCondition(&updatedGR.Status.Conditions, jobCreatedCond)
	}

	if err := r.Status().Patch(ctx, updatedGR, client.MergeFrom(gr)); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to patch GPUReset %s status: %w", gr.Name, err)
	}

	log.V(1).Info("Successfully claimed GPU reset job", "job", jobName, "namespace", jobNamespace)

	return ctrl.Result{}, nil
}

// checkJobStatus monitors the created Job for completion (Succeeded or Failed).
func (r *GPUResetReconciler) checkJobStatus(ctx context.Context, gr *v1alpha1.GPUReset) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	currentCond := meta.FindStatusCondition(gr.Status.Conditions, string(v1alpha1.ResetJobCompleted))
	if currentCond == nil {
		if err := r.updateCondition(ctx, gr, v1alpha1.ResetJobCompleted, metav1.ConditionFalse,
			v1alpha1.ReasonResetJobRunning, "GPU reset in progress"); err != nil {
			return ctrl.Result{}, err
		}

		return ctrl.Result{}, nil
	}

	if gr.Status.JobRef == nil {
		log.Info("Job reference not found in status, unable to check status", "job", r.expectedJobName(gr), "namespace",
			r.Config.ResolvedJobTemplate.Namespace)

		return r.reconcileTerminalFailure(ctx, gr, v1alpha1.ReasonInternalError, "Job reference is missing")
	}

	jobNamespace := gr.Status.JobRef.Namespace
	jobName := gr.Status.JobRef.Name

	job := &batchv1.Job{}

	err := r.Get(ctx, client.ObjectKey{Namespace: jobNamespace, Name: jobName}, job)
	if err != nil {
		if apierrors.IsNotFound(err) {
			log.Error(err, "GPU reset job not found, it may have been deleted", "job", jobName, "namespace", jobNamespace)
			return r.reconcileTerminalFailure(ctx, gr, v1alpha1.ReasonResetJobNotFound, "Job for GPU reset was not found")
		}

		return ctrl.Result{}, fmt.Errorf("failed to get Job %s/%s: %w", jobNamespace, jobName, err)
	}

	if job.Status.Succeeded > 0 {
		log.V(1).Info("GPU reset job completed successfully", "job", job.Name, "namespace", job.Namespace)

		if err := r.updateCondition(ctx, gr, v1alpha1.ResetJobCompleted, metav1.ConditionTrue,
			v1alpha1.ReasonResetJobSucceeded, "GPU(s) reset successfully"); err != nil {
			return ctrl.Result{}, err
		}

		return ctrl.Result{}, nil
	}

	if job.Status.Failed > 0 {
		log.Info("GPU reset job failed", "job", job.Name, "namespace", job.Namespace)

		if err := r.updateCondition(ctx, gr, v1alpha1.ResetJobCompleted, metav1.ConditionTrue,
			v1alpha1.ReasonResetJobFailed, "GPU reset failed"); err != nil {
			return ctrl.Result{}, err
		}

		return ctrl.Result{}, nil
	}

	log.V(1).Info("Waiting for GPU reset job to complete", "job", job.Name, "namespace", job.Namespace)

	return ctrl.Result{}, nil
}

// restoreServices re-enables the managed services. It enables them by setting node labels
// to their enabled state and waits for the associated pods to become ready.
func (r *GPUResetReconciler) restoreServices(ctx context.Context, gr *v1alpha1.GPUReset) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	nodeName := gr.Spec.NodeName
	managerName := r.serviceManager.Name

	if len(r.serviceManager.Spec.Apps) == 0 {
		log.V(1).Info("GPU services manager has no apps specified, skipping service restoration", "manager",
			managerName, "node", nodeName)

		if err := r.updateCondition(ctx, gr, v1alpha1.ServicesRestored, metav1.ConditionTrue, v1alpha1.ReasonSkipped,
			"No services manager configured, or has no managed services specified"); err != nil {
			return ctrl.Result{}, err
		}

		return ctrl.Result{}, nil
	}

	currentCond := meta.FindStatusCondition(gr.Status.Conditions, string(v1alpha1.ServicesRestored))
	if currentCond == nil {
		if err := r.updateCondition(ctx, gr, v1alpha1.ServicesRestored, metav1.ConditionFalse,
			v1alpha1.ReasonRestoringServices, fmt.Sprintf("Re-deploying %s managed services", managerName)); err != nil {
			return ctrl.Result{}, err
		}

		return ctrl.Result{}, nil
	}

	restoreTimeout := r.serviceManager.Spec.RestoreTimeout

	timeSinceInProgress := 0 * time.Minute
	if currentCond.Reason == string(v1alpha1.ReasonRestoringServices) {
		timeSinceInProgress = time.Since(currentCond.LastTransitionTime.Time)
	}

	if timeSinceInProgress > restoreTimeout {
		log.Error(nil, "Managed service restoration timeout exceeded", "manager", managerName, "timeout", restoreTimeout)

		return r.reconcileTerminalFailure(ctx, gr, v1alpha1.ReasonRestoreTimeoutExceeded,
			fmt.Sprintf("failed to restore %s managed services within the timeout period", managerName))
	}

	node, err := r.getNode(nodeName)
	if err != nil {
		log.V(1).Info("Failed to get node for service restoration, will retry", "node", nodeName, "error", err)
		return ctrl.Result{}, fmt.Errorf("failed to get node %s for service restoration: %w", nodeName, err)
	}

	nodeToUpdate := node.DeepCopy()
	nodeUpdated := false

	// Set node labels back to enabled value to restore services
	for _, app := range r.serviceManager.Spec.Apps {
		if value, exists := nodeToUpdate.Labels[app.NodeLabel]; !exists || value != app.EnabledValue {
			log.V(1).Info("Setting node label to enable managed service", "node", node.Name, "manager", managerName,
				"label", app.NodeLabel, "value", app.EnabledValue)

			if nodeToUpdate.Labels == nil {
				nodeToUpdate.Labels = make(map[string]string)
			}

			nodeToUpdate.Labels[app.NodeLabel] = app.EnabledValue
			nodeUpdated = true
		}
	}

	if nodeUpdated {
		if err := r.Patch(ctx, nodeToUpdate, client.MergeFrom(node)); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to patch node %s to re-enable %s managed services: %w",
				node.Name, managerName, err)
		}

		log.Info("Managed services re-enabled", "node", node.Name, "manager", managerName)

		return ctrl.Result{RequeueAfter: time.Second * 2}, nil
	}

	// Wait for pods to become ready
	podsReady, err := r.checkPodsReadyFn(ctx, node.Name)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to check %s managed service pods readiness for node %s: %w",
			managerName, node.Name, err)
	}

	if !podsReady {
		log.V(1).Info("Waiting for managed service pods to become Ready", "node", node.Name, "manager", managerName,
			"recheck_after", 2*time.Second)

		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	}

	if currentCond.Status != metav1.ConditionTrue {
		if err := r.updateCondition(ctx, gr, v1alpha1.ServicesRestored, metav1.ConditionTrue,
			v1alpha1.ReasonServiceRestoreSucceeded, fmt.Sprintf("%s managed services are Ready", managerName)); err != nil {
			return ctrl.Result{}, err
		}

		return ctrl.Result{}, nil
	}

	return ctrl.Result{}, nil
}

// reconcileCompletion updates the GPUReset status to a completed state
// and sets the CompletionTime.
//
// It also records all success-related metrics (duration, completion total, active count).
func (r *GPUResetReconciler) reconcileCompletion(ctx context.Context, gr *v1alpha1.GPUReset) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	nodeName := gr.Spec.NodeName

	jobFinishedCond := meta.FindStatusCondition(gr.Status.Conditions, string(v1alpha1.ResetJobCompleted))
	if jobFinishedCond.Reason == string(v1alpha1.ReasonResetJobFailed) {
		log.Info("GPU reset failed", "node", nodeName)
		return r.reconcileTerminalFailure(ctx, gr, v1alpha1.ReasonResetJobFailed, jobFinishedCond.Message)
	}

	log.Info("GPU reset successful", "node", nodeName)

	updatedGR := gr.DeepCopy()
	now := metav1.Now()
	updatedGR.Status.CompletionTime = &now
	updatedGR.Status.Phase = v1alpha1.ResetSucceeded

	succeededCond := NewCondition(v1alpha1.Complete, metav1.ConditionTrue, v1alpha1.ReasonGPUResetSucceeded,
		jobFinishedCond.Message)
	meta.SetStatusCondition(&updatedGR.Status.Conditions, succeededCond)

	if err := r.updateStatus(ctx, gr, updatedGR.Status); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to set final status for GPUReset %s: %w", gr.Name, err)
	}

	// update metrics: enforce the absolute state (Active=0)
	metrics.GPUResetActiveRequests.WithLabelValues(nodeName).Set(0)
	metrics.GPUResetRequestsCompletedTotal.WithLabelValues(nodeName, "success").Inc()

	if gr.Status.StartTime != nil {
		duration := updatedGR.Status.CompletionTime.Sub(gr.Status.StartTime.Time)
		metrics.GPUResetDurationSeconds.WithLabelValues(nodeName, "success").Observe(duration.Seconds())
	}

	return ctrl.Result{}, nil
}

// expectedJobName returns a valid and deterministic name for a Job. It ensures
// the name does not exceed 63 characters to avoid issues with derived Pod names.
func (r *GPUResetReconciler) expectedJobName(gr *v1alpha1.GPUReset) string {
	baseName := gr.Name

	if len(baseName)+len(jobNameSuffix) <= validation.DNS1123LabelMaxLength {
		return baseName + jobNameSuffix
	}

	if len(baseName) > maxJobNameBaseLength {
		baseName = baseName[0:maxJobNameBaseLength]
	}

	hash := sha256.Sum256([]byte(gr.Name))
	shortHash := fmt.Sprintf("%x", hash)[:8]

	return fmt.Sprintf("%s-%s%s", baseName, shortHash, jobNameSuffix)
}

// getOrCreateJob ensures the GPU reset job exists for the given GPUReset.
// It first checks if the job already exists. If not, it runs a pre-flight drift check
// to ensure managed services are still torn down before creating a Job responsible for performing the reset.
func (r *GPUResetReconciler) getOrCreateJob(ctx context.Context, gr *v1alpha1.GPUReset) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	jobName := r.expectedJobName(gr)

	job, err := r.getJob(ctx, gr)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to get job %s/%s for GPUReset %s: %w",
			r.Config.ResolvedJobTemplate.Namespace, jobName, gr.Name, err)
	}

	if job != nil {
		if !meta.IsStatusConditionTrue(gr.Status.Conditions, string(v1alpha1.ResetJobCreated)) {
			if err := r.updateCondition(ctx, gr, v1alpha1.ResetJobCreated, metav1.ConditionTrue,
				v1alpha1.ReasonResetJobCreationSucceeded, "GPU reset job created"); err != nil {
				return ctrl.Result{}, err
			}

			return ctrl.Result{}, nil
		}

		return ctrl.Result{}, nil
	}

	// Job NOT found. Create it.

	// Drift check: verify managed service pods are NOT running
	nodeName := gr.Spec.NodeName
	managerName := r.serviceManager.Name

	podsAreGone, err := r.checkPodsTerminatedFn(ctx, nodeName)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to run %s managed service drift check on node %s: %w",
			managerName, nodeName, err)
	}

	if !podsAreGone {
		log.Info("Drift detected: managed service pods are still running, re-initiating teardown.", "node",
			nodeName, "manager", managerName)

		if err := r.updateCondition(ctx, gr, v1alpha1.ServicesTornDown,
			metav1.ConditionFalse, v1alpha1.ReasonTearingDownServices,
			fmt.Sprintf("Drift detected: %s managed service pods are still running", managerName)); err != nil {
			return ctrl.Result{}, err
		}

		return ctrl.Result{}, nil
	}

	gpuResetJob, err := r.newGpuResetJob(ctx, gr)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to construct job %s/%s for GPUReset %s: %w",
			r.Config.ResolvedJobTemplate.Namespace, jobName, gr.Name, err)
	}

	if err := r.Create(ctx, gpuResetJob); err != nil {
		if apierrors.IsAlreadyExists(err) {
			log.V(1).Info("GPU reset job already exists", "job", gpuResetJob.Name, "namespace", gpuResetJob.Namespace)
			return ctrl.Result{}, nil
		}

		return ctrl.Result{}, fmt.Errorf("failed to create job %s/%s for GPUReset %s: %w", gpuResetJob.Namespace,
			gpuResetJob.Name, gr.Name, err)
	}

	log.V(1).Info("GPU reset job submitted for creation", "job", gpuResetJob.Name, "namespace", gpuResetJob.Namespace)

	if err := r.updateCondition(ctx, gr, v1alpha1.ResetJobCreated, metav1.ConditionTrue,
		v1alpha1.ReasonResetJobCreationSucceeded, "GPU reset job created"); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// getJob retrieves the GPU reset job owned by the given GPUReset.
// An error is returned if a job with the expected name exists but has a different owner.
func (r *GPUResetReconciler) getJob(ctx context.Context, gr *v1alpha1.GPUReset) (*batchv1.Job, error) {
	jobName := r.expectedJobName(gr)

	job := &batchv1.Job{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: r.Config.ResolvedJobTemplate.Namespace, Name: jobName},
		job); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}

		return nil, fmt.Errorf("failed to get job %s/%s: %w", r.Config.ResolvedJobTemplate.Namespace, jobName, err)
	}

	if metav1.IsControlledBy(job, gr) {
		return job, nil
	}

	return nil, fmt.Errorf("unexpected error: job %s/%s exists but owned by %s, expected %s", job.Namespace,
		job.Name, metav1.GetControllerOf(job), gr.Name)
}

// newGpuResetJob constructs the Kubernetes Job object responsible for performing the GPU reset.
func (r *GPUResetReconciler) newGpuResetJob(ctx context.Context, gr *v1alpha1.GPUReset) (*batchv1.Job, error) {
	log := log.FromContext(ctx)

	jobName := r.expectedJobName(gr)
	jobNamespace := r.Config.ResolvedJobTemplate.Namespace
	log.V(1).Info("Constructing new GPU reset job", "job", jobName, "namespace", jobNamespace)

	jobMeta := *r.Config.ResolvedJobTemplate.ObjectMeta.DeepCopy()
	jobMeta.Name = jobName
	jobMeta.Namespace = jobNamespace

	if jobMeta.Labels == nil {
		jobMeta.Labels = make(map[string]string)
	}

	if len(gr.Name) <= validation.DNS1123LabelMaxLength {
		jobMeta.Labels["gpureset-name"] = gr.Name
		jobMeta.Labels[jobOwnerKey] = gr.Name
	}

	jobSpec := *r.Config.ResolvedJobTemplate.Spec.DeepCopy()

	log.V(1).Info("Setting NodeName", "job", jobName, "namespace", jobNamespace, "node", gr.Spec.NodeName)
	jobSpec.Template.Spec.NodeName = gr.Spec.NodeName

	if jobSpec.Template.Spec.RestartPolicy == "" {
		log.V(1).Info("RestartPolicy not found, setting to 'OnFailure'", "job", jobName, "namespace", jobNamespace)

		jobSpec.Template.Spec.RestartPolicy = corev1.RestartPolicyOnFailure
	}

	var gpuIDs []string
	if gr.Spec.Selector != nil {
		gpuIDs = append(gpuIDs, gr.Spec.Selector.UUIDs...)
		gpuIDs = append(gpuIDs, gr.Spec.Selector.PCIBusIDs...)
	}

	gpuIDString := ""
	if len(gpuIDs) > 0 {
		gpuIDString = strings.Join(gpuIDs, ",")
	}

	gpuResetEnvVar := corev1.EnvVar{
		Name:  gpuResetsEnvVar,
		Value: gpuIDString,
	}

	for i, container := range jobSpec.Template.Spec.Containers {
		envVarIndex := -1

		for j, env := range jobSpec.Template.Spec.Containers[i].Env {
			if env.Name == gpuResetEnvVar.Name {
				envVarIndex = j
				break
			}
		}

		if envVarIndex >= 0 {
			oldValue := jobSpec.Template.Spec.Containers[i].Env[envVarIndex]
			log.V(1).Info("Overriding existing environment variable", "env_var", gpuResetEnvVar.Name, "job", jobName,
				"namespace", jobNamespace, "container", container.Name, "new_value", gpuResetEnvVar.Value, "old_value",
				oldValue.Value)
			jobSpec.Template.Spec.Containers[i].Env[envVarIndex] = gpuResetEnvVar
		} else {
			log.V(1).Info("Adding environment variable", "env_var", gpuResetEnvVar.Name, "job", jobName, "namespace",
				jobNamespace, "container", container.Name, "value", gpuResetEnvVar.Value)
			jobSpec.Template.Spec.Containers[i].Env = append(jobSpec.Template.Spec.Containers[i].Env, gpuResetEnvVar)
		}
	}

	job := &batchv1.Job{
		ObjectMeta: jobMeta,
		Spec:       jobSpec,
	}

	if err := ctrl.SetControllerReference(gr, job, r.Scheme); err != nil {
		return nil, fmt.Errorf("failed to set controller reference on job %s/%s: %w", jobNamespace, jobName, err)
	}

	return job, nil
}

// checkPodsTerminated verifies that all pods belonging to the managed services
// have been successfully terminated on the target node.
func (r *GPUResetReconciler) checkPodsTerminated(ctx context.Context, nodeName string) (bool, error) {
	log := log.FromContext(ctx)

	if len(r.serviceManager.Spec.Apps) == 0 {
		return true, nil
	}

	managerName := r.serviceManager.Name

	for _, app := range r.serviceManager.Spec.Apps {
		finalSelectorMap := make(map[string]string)
		maps.Copy(finalSelectorMap, r.serviceManager.Spec.ManagerSelector)
		maps.Copy(finalSelectorMap, app.AppSelector)

		listOptions := []client.ListOption{
			client.InNamespace(r.serviceManager.Spec.Namespace),
			client.MatchingLabels(finalSelectorMap),
			client.MatchingFields{"spec.nodeName": nodeName},
		}

		pods := &corev1.PodList{}
		if err := r.List(ctx, pods, listOptions...); err != nil {
			return false, fmt.Errorf("failed to list %s pods for termination check for node %s: %w",
				managerName, nodeName, err)
		}

		if len(pods.Items) > 0 {
			log.V(1).Info("Managed service pod still running on node", "node", nodeName, "manager", managerName,
				"count", len(pods.Items), "selector", finalSelectorMap)

			return false, nil
		}
	}

	return true, nil
}

// checkPodsReady verifies that all pods belonging to the managed services
// have been successfully re-deployed and are in a Ready state on the target node.
func (r *GPUResetReconciler) checkPodsReady(ctx context.Context, nodeName string) (bool, error) {
	log := log.FromContext(ctx)

	if len(r.serviceManager.Spec.Apps) == 0 {
		return true, nil
	}

	managerName := r.serviceManager.Name

	for _, app := range r.serviceManager.Spec.Apps {
		finalSelectorMap := make(map[string]string)
		maps.Copy(finalSelectorMap, r.serviceManager.Spec.ManagerSelector)
		maps.Copy(finalSelectorMap, app.AppSelector)

		listOptions := []client.ListOption{
			client.InNamespace(r.serviceManager.Spec.Namespace),
			client.MatchingLabels(finalSelectorMap),
			client.MatchingFields{"spec.nodeName": nodeName},
		}

		pods := &corev1.PodList{}
		if err := r.List(ctx, pods, listOptions...); err != nil {
			return false, fmt.Errorf("failed to list %s managed service pods for ready check for node %s: %w",
				managerName, nodeName, err)
		}

		if len(pods.Items) == 0 {
			log.V(1).Info("Waiting for managed service pod to be created", "node", nodeName, "manager", managerName,
				"selector", finalSelectorMap)

			return false, nil
		}

		// Check if at least one pod is ready for the given app selector
		for _, pod := range pods.Items {
			isReady := false

			for _, cond := range pod.Status.Conditions {
				if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
					isReady = true
					break
				}
			}

			if !isReady {
				log.V(1).Info("Waiting for managed service pod to become Ready", "node", nodeName,
					"manager", managerName, "pod", pod.Name, "phase", pod.Status.Phase)

				return false, nil
			}
		}
	}

	return true, nil
}

// reconcileTerminalFailure updates the GPUReset status to a terminal failed state
// and sets the CompletionTime.
//
// It also records all failure-related metrics, ensuring to decrement the 'pending' count
// or reset the 'active' gauge to zero.
func (r *GPUResetReconciler) reconcileTerminalFailure(
	ctx context.Context,
	gr *v1alpha1.GPUReset,
	reason v1alpha1.GPUResetReason,
	message string,
) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	nodeName := gr.Spec.NodeName

	log.Error(errors.New(message), "terminal failure", "node", nodeName, "reason", reason)

	updatedGR := gr.DeepCopy()

	if updatedGR.Status.CompletionTime == nil {
		now := metav1.Now()
		updatedGR.Status.CompletionTime = &now
		updatedGR.Status.Phase = v1alpha1.ResetFailed

		completeCond := NewCondition(v1alpha1.Complete, metav1.ConditionTrue, reason, message)
		meta.SetStatusCondition(&updatedGR.Status.Conditions, completeCond)

		if err := r.updateStatus(ctx, gr, updatedGR.Status); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to set terminal failure status for GPUReset %s: %w",
				gr.Name, err)
		}

		// update metrics
		readyCond := meta.FindStatusCondition(gr.Status.Conditions, string(v1alpha1.Ready))
		if readyCond != nil && readyCond.Status == metav1.ConditionTrue {
			// update metrics: enforce the absolute state (Active=0)
			metrics.GPUResetActiveRequests.WithLabelValues(nodeName).Set(0)
		} else {
			metrics.GPUResetPendingRequests.WithLabelValues(nodeName).Dec()
		}

		metrics.GPUResetRequestsCompletedTotal.WithLabelValues(nodeName, "failure").Inc()
		metrics.GPUResetFailureReasonsTotal.WithLabelValues(nodeName, string(reason)).Inc()

		if gr.Status.StartTime != nil {
			duration := updatedGR.Status.CompletionTime.Sub(gr.Status.StartTime.Time)
			metrics.GPUResetDurationSeconds.WithLabelValues(nodeName, "failure").Observe(duration.Seconds())
		}
	}

	return ctrl.Result{}, nil
}

// reconcilePhase maps a detailed GPUResetReason to its high-level GPUResetPhase.
func reconcilePhase(reason v1alpha1.GPUResetReason) v1alpha1.GPUResetPhase {
	switch reason {
	// Pending
	case v1alpha1.ReasonResetPending:
		return v1alpha1.ResetPending
	case v1alpha1.ReasonReadyForReset:
		return v1alpha1.ResetPending
	case v1alpha1.ReasonResourceContention:
		return v1alpha1.ResetPending

	// InProgress
	case v1alpha1.ReasonSkipped:
		return v1alpha1.ResetInProgress
	case v1alpha1.ReasonTearingDownServices:
		return v1alpha1.ResetInProgress
	case v1alpha1.ReasonServiceTeardownSucceeded:
		return v1alpha1.ResetInProgress
	case v1alpha1.ReasonCreatingResetJob:
		return v1alpha1.ResetInProgress
	case v1alpha1.ReasonResetInProgress:
		return v1alpha1.ResetInProgress
	case v1alpha1.ReasonResetJobRunning:
		return v1alpha1.ResetInProgress
	case v1alpha1.ReasonRestoringServices:
		return v1alpha1.ResetInProgress
	case v1alpha1.ReasonFinalizerRestoringServices:
		return v1alpha1.ResetTerminating
	case v1alpha1.ReasonServiceRestoreSucceeded:
		return v1alpha1.ResetInProgress
	case v1alpha1.ReasonResetJobCreationSucceeded:
		return v1alpha1.ResetInProgress
	case v1alpha1.ReasonResetJobSucceeded:
		return v1alpha1.ResetInProgress

	// Completed - Succeeded
	case v1alpha1.ReasonGPUResetSucceeded:
		return v1alpha1.ResetSucceeded

	// Completed - Failed
	case v1alpha1.NodeNotFound:
		return v1alpha1.ResetFailed
	case v1alpha1.ReasonInvalidConfig:
		return v1alpha1.ResetFailed
	case v1alpha1.ReasonServiceTeardownTimeoutExceeded:
		return v1alpha1.ResetFailed
	case v1alpha1.ReasonResetJobCreationFailed:
		return v1alpha1.ResetFailed
	case v1alpha1.ReasonResetJobNotFound:
		return v1alpha1.ResetFailed
	case v1alpha1.ReasonResetJobFailed:
		return v1alpha1.ResetFailed
	case v1alpha1.ReasonServiceRestoreFailed:
		return v1alpha1.ResetFailed
	case v1alpha1.ReasonRestoreTimeoutExceeded:
		return v1alpha1.ResetFailed
	case v1alpha1.ReasonInternalError:
		return v1alpha1.ResetFailed
	default:
		return v1alpha1.ResetUnknown
	}
}

// updateCondition updates the status condition of the GPUReset object and triggers a status patch.
func (r *GPUResetReconciler) updateCondition(
	ctx context.Context,
	gr *v1alpha1.GPUReset,
	condType v1alpha1.GPUResetConditionType,
	status metav1.ConditionStatus,
	reason v1alpha1.GPUResetReason,
	message string,
) error {
	updatedGR := gr.DeepCopy()
	newCond := NewCondition(condType, status, reason, message)
	meta.SetStatusCondition(&updatedGR.Status.Conditions, newCond)

	updatedGR.Status.Phase = reconcilePhase(reason)

	if err := r.updateStatus(ctx, gr, updatedGR.Status); err != nil {
		return fmt.Errorf("failed to update GPUReset %s condition %s: %w", gr.Name, condType, err)
	}

	return nil
}

// updateStatus performs a status-only patch on the GPUReset resource if the status has changed.
func (r *GPUResetReconciler) updateStatus(
	ctx context.Context,
	gr *v1alpha1.GPUReset,
	newStatus v1alpha1.GPUResetStatus,
) error {
	log := log.FromContext(ctx)

	grName := gr.Name

	var latest v1alpha1.GPUReset
	if err := r.Get(ctx, client.ObjectKey{Name: grName, Namespace: gr.Namespace}, &latest); err != nil {
		return fmt.Errorf("failed to get latest GPUReset %s for status update: %w", grName, err)
	}

	updated := latest.DeepCopy()
	updated.Status = newStatus

	// Avoid unnecessary status updates
	if reflect.DeepEqual(latest.Status, updated.Status) {
		return nil
	}

	oldPhase := latest.Status.Phase
	newPhase := newStatus.Phase

	if oldPhase != newPhase {
		log.V(1).Info("GPUReset phase changed", "node", gr.Spec.NodeName, "old", oldPhase, "new", newPhase)
	}

	if err := r.Client.Status().Patch(ctx, updated, client.MergeFrom(&latest)); err != nil {
		return fmt.Errorf("failed to patch GPUReset %s status: %w", grName, err)
	}

	return nil
}

// NewCondition creates a new Condition object.
func NewCondition(
	condType v1alpha1.GPUResetConditionType,
	status metav1.ConditionStatus,
	reason v1alpha1.GPUResetReason,
	msg string,
) metav1.Condition {
	return metav1.Condition{
		Type:    string(condType),
		Status:  status,
		Reason:  string(reason),
		Message: msg,
	}
}

func (r *GPUResetReconciler) getNode(name string) (*corev1.Node, error) {
	var node corev1.Node

	nodeKey := client.ObjectKey{Name: name}
	if err := r.Get(context.TODO(), nodeKey, &node); err != nil {
		return nil, err
	}

	return &node, nil
}
