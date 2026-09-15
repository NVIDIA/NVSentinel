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

package distributedlock

import (
	"context"
	"fmt"
	"log/slog"

	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// LockMetrics is an optional interface for recording lock/unlock failures.
// Pass nil to NewNodeLock to disable metrics.
type LockMetrics interface {
	IncLockFailure(nodeName string)
	IncUnlockFailure(nodeName string)
}

/*
The NodeLock interface can be used in Reconcile functions to add node-level locking functionality across all
controllers which are leveraging the lock. Prior to reconciling a maintenance resource, any controller leveraging
NodeLock must first acquire the node-level lock for the given node specified in the maintenance object. This node lock
is implemented with a lease object per node. A controller must successfully create the node lock prior proceeding with
reconciling and release the node lock by deleting the lease after it reaches a terminal state. If the lease lock for
a given node already exists when a competing object needs reconciled, it will re-queue the object and retry acquiring
the lock. The lock is held by a given maintenance resource as long as the lease object exists with an owner reference
that matches the current maintenance resource. Initially acquiring the lock requires successful creation whereas
subsequent reconcile loops only require fetching the existing object and checking the owner reference matches the
current maintenance resource.

Controller + CRD requirements for leveraging NodeLock:
 1. Check when locking and unlocking is required: controllers leveraging NodeLock should only call LockNode() while
    the current maintenance resource has not reached a terminal state. It is safe to call LockNode() if the current
    maintenance resource already has acquired the lock on a previous reconcile loop. After acquiring the lock and
    reconciling, maintenance resources should be re-queued to allow for unlocking on a subsequent reconcile by calling
    CheckUnlock.
 2. Reconcile to terminal: controllers must reconcile CRD objects to a terminal status. An object will hold the
    node lock until the terminal condition is met, signaling that the node lock can be released.
 3. RBAC requirements: controllers leveraging NodeLock need to have the following resources and permissions:
    resources=leases, verbs=get;list;watch;create;delete

Metrics: callers may inject a LockMetrics implementation to record lock/unlock failures. Pass nil to disable metrics.
*/
type NodeLock interface {
	LockNode(ctx context.Context, maintenanceObject client.Object, nodeName string) bool
	GetHolder(ctx context.Context, nodeName string) (*metav1.OwnerReference, error)
	CheckUnlock(ctx context.Context, maintenanceObject client.Object, nodeName string) (retryUnlock bool)
}

// NewNodeLock creates a new NodeLock instance. scheme is used to resolve
// the GVK of maintenance objects for owner references. metrics may be nil.
func NewNodeLock(c client.Client, scheme *runtime.Scheme, namespace string, metrics LockMetrics) NodeLock {
	return &nodeLock{
		Client:    c,
		scheme:    scheme,
		namespace: namespace,
		metrics:   metrics,
	}
}

type nodeLock struct {
	client.Client
	scheme    *runtime.Scheme
	namespace string
	metrics   LockMetrics
}

func (lock *nodeLock) LockNode(ctx context.Context, maintenanceObject client.Object, nodeName string) bool {
	nodeLockName, lease, err := lock.getNodeLockLease(ctx, nodeName)
	if err == nil {
		slog.DebugContext(ctx, "Node lock already exists, checking if current maintenance resource is the holder",
			"maintenanceResource", maintenanceObject.GetName(), "nodeLockName", nodeLockName)

		return lease.GetOwnerReferences()[0].UID == maintenanceObject.GetUID()
	}

	if !apierrors.IsNotFound(err) {
		slog.ErrorContext(ctx, "Got an error fetching node lock lease",
			"error", err, "maintenanceResource", maintenanceObject.GetName(), "nodeLockName", nodeLockName)
		lock.incLockFailure(nodeName)

		return false
	}

	slog.DebugContext(ctx, "Node lock lease does not exist, attempting to acquire the lock",
		"maintenanceResource", maintenanceObject.GetName(), "nodeLockName", nodeLockName)

	apiVersion, kind := lock.resolveGVK(maintenanceObject)

	lease = &coordinationv1.Lease{
		Name:      nodeLockName,
		Namespace: lock.namespace,
		OwnerReferences: []metav1.OwnerReference{
			{
				APIVersion:         apiVersion,
				Kind:               kind,
				Name:               maintenanceObject.GetName(),
				UID:                maintenanceObject.GetUID(),
				BlockOwnerDeletion: new(true),
			},
		},
	}

	err = lock.Create(ctx, lease)
	if err != nil {
		if !apierrors.IsAlreadyExists(err) {
			slog.ErrorContext(ctx, "Got an error creating node lock lease, failed to acquire the lock",
				"error", err, "maintenanceResource", maintenanceObject.GetName(), "nodeLockName", nodeLockName)
			lock.incLockFailure(nodeName)
		} else {
			slog.DebugContext(ctx, "Node lock lease already exists, failed to acquire the lock",
				"maintenanceResource", maintenanceObject.GetName(), "nodeLockName", nodeLockName)
		}

		return false
	}

	slog.InfoContext(ctx, "Successfully created node lock lease and acquired lock",
		"maintenanceResource", maintenanceObject.GetName(), "nodeLockName", nodeLockName)

	return true
}

// GetHolder returns the owner of the node's lock lease. Controllers use this
// after a failed LockNode call to distinguish duplicate work from cross-kind
// maintenance contention.
func (lock *nodeLock) GetHolder(ctx context.Context, nodeName string) (*metav1.OwnerReference, error) {
	_, lease, err := lock.getNodeLockLease(ctx, nodeName)
	if err != nil {
		return nil, fmt.Errorf("getting node lock holder for node %q: %w", nodeName, err)
	}

	owner := lease.GetOwnerReferences()[0]

	return &owner, nil
}

/*
Internal cases for how NodeLock releases node-level locks:
 1. In the default case, the node lock lease will be deleted on the first reconciliation loop after the terminal
    condition is met. A maintenance resource will be re-queued when the controller needs to do additional work,
    when the terminal condition is set, or when the node lock lease fails to be deleted.

External cases for how NodeLock releases node-level locks:
 1. Maintenance resource deletion: if a maintenance object is deleted after it acquires the lock but before the
    terminal condition is met and the lock is deleted, the owner reference set on the lease object by the owning
    resource will ensure that K8s garbage collection will delete the lock.
 2. Node resource deletion: NVSentinel sets an OwnerReference on maintenance resources for the node they correspond
    to. This ensures that the lease objects are cleaned up indirectly if the corresponding node is deleted.
*/
func (lock *nodeLock) CheckUnlock(ctx context.Context,
	maintenanceObject client.Object, nodeName string,
) (retryUnlock bool) {
	slog.DebugContext(ctx, "Terminal condition met for maintenance resource, checking if lock needs released",
		"maintenanceResource", maintenanceObject.GetName())

	nodeLockName, lease, err := lock.getNodeLockLease(ctx, nodeName)
	if err != nil {
		return lock.handleNotFoundError(err, nodeLockName, nodeName)
	}

	slog.DebugContext(ctx, "Node lock already exists, checking if current maintenance resource is the holder",
		"maintenanceResource", maintenanceObject.GetName(), "nodeLockName", nodeLockName)

	if lease.GetOwnerReferences()[0].UID == maintenanceObject.GetUID() {
		slog.DebugContext(ctx, "Node lock needs released for maintenance resource, attempting to delete lock",
			"maintenanceResource", maintenanceObject.GetName(), "nodeLockName", lease.GetName())

		err = lock.Delete(ctx, lease)
		if err != nil {
			return lock.handleNotFoundError(err, nodeName, nodeName)
		}

		slog.InfoContext(ctx, "Node lock successfully released for maintenance resource",
			"maintenanceResource", maintenanceObject.GetName(), "nodeLockName", lease.GetName())
	} else {
		slog.DebugContext(ctx, "Node lock already exists but the current maintenance resource isn't the owner",
			"maintenanceResource", maintenanceObject.GetName())
	}

	return false
}

func (lock *nodeLock) getNodeLockLease(
	ctx context.Context, nodeName string,
) (string, *coordinationv1.Lease, error) {
	nodeLockNamespaceName := types.NamespacedName{
		Name:      nodeName,
		Namespace: lock.namespace,
	}

	var lease coordinationv1.Lease

	err := lock.Get(ctx, nodeLockNamespaceName, &lease)
	if err != nil {
		return nodeName, nil, err
	}

	ownerReferences := lease.GetOwnerReferences()
	if len(ownerReferences) != 1 {
		return "", nil, fmt.Errorf(
			"found an unexpected number of owner references on lock %s: %d",
			nodeName, len(ownerReferences))
	}

	return nodeName, &lease, err
}

// resolveGVK extracts the API version and kind from a maintenance object.
// It first checks the object's GVK; if that is empty (common in tests or
// when GVK is not set by the client), it falls back to the scheme.
func (lock *nodeLock) resolveGVK(obj client.Object) (apiVersion, kind string) {
	gvk := obj.GetObjectKind().GroupVersionKind()
	if gvk.Kind != "" && gvk.GroupVersion().String() != "" {
		return gvk.GroupVersion().String(), gvk.Kind
	}

	if lock.scheme != nil {
		gvks, _, err := lock.scheme.ObjectKinds(obj)
		if err == nil && len(gvks) > 0 {
			return gvks[0].GroupVersion().String(), gvks[0].Kind
		}
	}

	return "", ""
}

func (lock *nodeLock) incLockFailure(nodeName string) {
	if lock.metrics != nil {
		lock.metrics.IncLockFailure(nodeName)
	}
}

func (lock *nodeLock) handleNotFoundError(
	err error, resourceName string, nodeName string,
) (retryUnlock bool) {
	if apierrors.IsNotFound(err) {
		slog.Debug("Resource does not exist, completing reconciling", "resourceName", resourceName)

		return false
	}

	slog.Error("Got an error operating on resource, need to retry reconciling",
		"error", err, "resourceName", resourceName)

	if lock.metrics != nil {
		lock.metrics.IncUnlockFailure(nodeName)
	}

	return true
}
