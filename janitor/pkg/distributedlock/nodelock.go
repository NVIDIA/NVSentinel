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
	"strings"

	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/nvidia/nvsentinel/janitor/pkg/metrics"
)

/*
The NodeLock interface can be used in Reconcile functions to add node-level locking functionality across all
controllers which are leveraging the lock. Prior to reconciling a maintenance resource, any controller leveraging
NodeLock must first acquire the node-level lock for the given node specified in the maintenance object. This node lock
is implemented with a lease object per node. A controller must successfully create the node lock prior proceeding with
reconciling and release the node lock by deleting the lease after it sets a completion timestamp. If the lease lock for
a given node already exists when a competing object needs reconciled, it will re-queue the object and retry acquiring
the lock. The lock is held by a given maintenance resource as long as the lease object exists with an owner reference
that matches the current maintenance resource. Initially acquiring the lock requires successful creation whereas
subsequent reconcile loops only require fetching the existing object and checking the owner reference matches the
current maintenance resource.

Controller + CRD requirements for leveraging NodeLock:
1. Check when locking and unlocking is required: controllers leveraging NodeLock should only call LockNode() if the
current maintenance resource has not completed reconciling (meaning its CompletionTime is not set). It is safe to
call LockNode() if the current maintenance resource already has acquired the lock on a previous reconcile loop. After
acquiring the lock and reconciling, maintenance resources should be re-queued to allow for unlocking on a
subsequent reconcile by calling CheckUnlock. Forcing re-processing prevents controllers needing to either indicate if
they set the CompletionTime on the current reconcile or having to re-fetch the resource at the end of reconciling.
When the maintenance resource is re-processed with its CompletionTime set, it is safe to call CheckUnlock whether it
still held the lock or was previously released. See rebootnode_controller.go for an example implementation.
2. Reconcile until CompletionTime is set: controllers must reconcile CRD objects to a terminal status which is signaled
by having a CompletionTime status populated. In most cases, an object will require several reconciliations and that
object will hold the node lock until the CompletionTime is set signaling that the node lock can be released.
3. RBAC requirements: controllers leveraging NodeLock need to have the following resources and permissions:
  - resources=leases, verbs=get;list;watch;create;delete

Metrics: we will emit a metric for janitor_actions_count with action_type lock or unlock and status failed when we run
into an unexpected failure with acquiring or releasing a lock. Any failures to lock or unlock that result from conflicts
with other maintenance resources will not emit a metric (specifically not found or already exists errors from the K8s
API). We are not tracking the time it takes to acquire the lock or how long a lock is held for a particular resource
and are relying on the underlying controller to reconcile each maintenance resource which holds the lock to a terminal
status.
*/
type NodeLock interface {
	LockNode(ctx context.Context, maintenanceObject client.Object, nodeName string) bool
	CheckUnlock(ctx context.Context, maintenanceObject client.Object, nodeName string) (retryUnlock bool)
}

// NewNodeLock creates a new NodeLock instance
func NewNodeLock(client client.Client, namespace string) NodeLock {
	return &nodeLock{
		Client:    client,
		namespace: namespace,
	}
}

type nodeLock struct {
	client.Client
	namespace string
}

func (lock *nodeLock) LockNode(ctx context.Context, maintenanceObject client.Object, nodeName string) bool {
	nodeLockName, lease, err := lock.getNodeLockLease(ctx, nodeName)
	if err == nil {
		slog.Debug("Node lock already exists, checking if current maintenance resource is the holder",
			"maintenanceResource", maintenanceObject.GetName(), "nodeLockName", nodeLockName)

		return lease.GetOwnerReferences()[0].UID == maintenanceObject.GetUID()
	}

	if !apierrors.IsNotFound(err) {
		slog.Error("Got an error fetching node lock lease",
			"error", err, "maintenanceResource", maintenanceObject.GetName(), "nodeLockName", nodeLockName)
		metrics.IncActionCount(metrics.ActionTypeLock, metrics.StatusFailed, nodeName)

		return false
	}

	slog.Debug("Node lock lease does not exist, attempting to acquire the lock",
		"maintenanceResource", maintenanceObject.GetName(), "nodeLockName", nodeLockName)

	// Get GVK from the object - if empty, infer from object type
	gvk := maintenanceObject.GetObjectKind().GroupVersionKind()
	apiVersion := gvk.GroupVersion().String()
	kind := gvk.Kind

	// If GVK is not set (common in tests), try to get it from the object's type
	if apiVersion == "" || kind == "" {
		// Try to extract from the object's type name
		typeName := fmt.Sprintf("%T", maintenanceObject)

		if apiVersion == "" {
			apiVersion = "janitor.dgxc.nvidia.com/v1alpha1"
		}

		if kind == "" {
			// Extract kind from type name (e.g., "*v1alpha1.RebootNode" -> "RebootNode")
			if idx := strings.LastIndex(typeName, "."); idx != -1 {
				kind = strings.TrimPrefix(typeName[idx+1:], "*")
			}
		}
	}

	lease = &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{
			Name:      nodeLockName,
			Namespace: lock.namespace,
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion:         apiVersion,
					Kind:               kind,
					Name:               maintenanceObject.GetName(),
					UID:                maintenanceObject.GetUID(),
					BlockOwnerDeletion: ptr.To(true),
				},
			},
		},
		// There's no need to populate a HolderIdentity, LeaseDurationSeconds, nor RenewTime in Spec, we only need to identify
		// the corresponding node and maintenance resource which is accomplished by the lease name and ownerReference.
	}

	err = lock.Create(ctx, lease)
	if err != nil {
		if !apierrors.IsAlreadyExists(err) {
			slog.Error("Got an error creating node lock lease, failed to acquire the lock",
				"error", err, "maintenanceResource", maintenanceObject.GetName(), "nodeLockName", nodeLockName)
			metrics.IncActionCount(metrics.ActionTypeLock, metrics.StatusFailed, nodeName)
		} else {
			slog.Debug("Node lock lease already exists, failed to acquire the lock",
				"maintenanceResource", maintenanceObject.GetName(), "nodeLockName", nodeLockName)
		}

		return false
	}

	slog.Info("Successfully created node lock lease and acquired lock",
		"maintenanceResource", maintenanceObject.GetName(), "nodeLockName", nodeLockName)

	return true
}

/*
Internal cases for how NodeLock releases node-level locks:
1. In the default case, the node lock lease will be deleted on the first reconciliation loop after the CompletionTime
is set. In most cases, the corresponding controller will not re-queue the object when it sets the CompletionTime,
however, we are forcing the object to be re-reconciled until it releases the node lock lease. As a result, a
maintenance resource will be re-queued when the controller needs to do additional work, when the controller set the
CompletionTime, or when the node lock lease fails to be deleted.

External cases for how NodeLock releases node-level locks:
1. Maintenance resource deletion: if a maintenance object is deleted after it acquires the lock but before it sets
a completion timestamp and deletes the lock, the owner reference set on the lease object by the owning resource will
ensure that K8s garbage collection will delete the lock.
2. Node resource deletion: NVSentinel sets an OwnerReference on maintenance resources for the node they correspond to.
This ensures that the lease objects are cleaned up indirectly if the corresponding node is deleted. In this case,
we won't ever need to re-acquire the lock since the node is gone, however, it ensures that stale leases do not persist.
*/
func (lock *nodeLock) CheckUnlock(ctx context.Context,
	maintenanceObject client.Object, nodeName string) (retryUnlock bool) {
	slog.Debug("Completion timestamp set for maintenance resource, checking if lock needs released",
		"maintenanceResource", maintenanceObject.GetName())

	nodeLockName, lease, err := lock.getNodeLockLease(ctx, nodeName)
	if err != nil {
		return handleNotFoundError(err, nodeLockName, nodeName)
	}

	slog.Debug("Node lock already exists, checking if current maintenance resource is the holder",
		"maintenanceResource", maintenanceObject.GetName(), "nodeLockName", nodeLockName)
	// The current maintenance object will always have the lock for the second invocation of checkUnlock in
	// ReconcileWrapper because it is called after lockNode, however, this check is required on the first invocation
	// of the function where it is not known if the current maintenance object is the holder of the lock.
	if lease.GetOwnerReferences()[0].UID == maintenanceObject.GetUID() {
		slog.Debug("Node lock needs released for maintenance resource, attempting to delete lock",
			"maintenanceResource", maintenanceObject.GetName(), "nodeLockName", lease.GetName())

		err = lock.Delete(ctx, lease)
		if err != nil {
			return handleNotFoundError(err, nodeName, nodeName)
		}

		slog.Info("Node lock successfully released for maintenance resource",
			"maintenanceResource", maintenanceObject.GetName(), "nodeLockName", lease.GetName())
	} else {
		slog.Debug("Node lock already exists but the current maintenance resource isn't the owner",
			"maintenanceResource", maintenanceObject.GetName())
	}

	return false
}

func (lock *nodeLock) getNodeLockLease(ctx context.Context, nodeName string) (string, *coordinationv1.Lease, error) {
	nodeLockNamespaceName := types.NamespacedName{
		Name:      nodeName,
		Namespace: lock.namespace,
	}

	var lease coordinationv1.Lease

	err := lock.Get(ctx, nodeLockNamespaceName, &lease)
	if err != nil {
		// we rely on the nodeLockName being returned if we do receive a 404 not found error
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

func handleNotFoundError(err error, resourceName string, nodeName string) (retryUnlock bool) {
	if apierrors.IsNotFound(err) {
		slog.Debug("Resource does not exist, completing reconciling", "resourceName", resourceName)

		return false
	}

	slog.Error("Got an error operating on resource, need to retry reconciling", "error", err, "resourceName", resourceName)
	metrics.IncActionCount(metrics.ActionTypeUnlock, metrics.StatusFailed, nodeName)

	return true
}
