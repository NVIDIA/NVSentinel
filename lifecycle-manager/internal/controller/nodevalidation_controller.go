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
	"log/slog"
	"sort"
	"time"

	"github.com/google/cel-go/cel"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/nvidia/nvsentinel/commons/pkg/kubeclient"
	"github.com/nvidia/nvsentinel/lifecycle-manager/api/v1alpha1"
	"github.com/nvidia/nvsentinel/lifecycle-manager/pkg/config"
	"github.com/nvidia/nvsentinel/lifecycle-manager/pkg/metrics"
)

const newNodeValidationReason = "ValidationRequestCreated"

type NodeValidationReconciler struct {
	client.Client
	APIReader        client.Reader
	Scheme           *runtime.Scheme
	Config           *config.Config
	CriteriaPrograms map[string]cel.Program

	nodesInBatch map[string]bool
	batchEndTime time.Time
}

/*
A NodeValidationReconciler is only registered if spec.newNodeValidation is set in the active ValidationConfiguration.
As a result, in order to enable this controller, the lifecycle-manager component must enable the ValidationRequest
controller and ensure spec.newNodeValidation is set (there is not a separate toggle in the Helm chart for only this
controller).

Note that MaxConcurrentReconciles is set to 1 which prevents us from needing to implement locking for the shared
nodesInBatch and batchEndTime state within this controller.
*/
func NewNodeValidationReconciler(cl client.Client, apiReader client.Reader, scheme *runtime.Scheme,
	cfg *config.Config) (*NodeValidationReconciler, error) {
	programs, err := buildReadinessPrograms(cfg.Validation.Spec.NewNodeValidation.Criteria)
	if err != nil {
		return nil, fmt.Errorf("build newNodeValidation criteria programs: %w", err)
	}

	return &NodeValidationReconciler{
		Client:           cl,
		APIReader:        apiReader,
		Scheme:           scheme,
		Config:           cfg,
		CriteriaPrograms: programs,
	}, nil
}

func (r *NodeValidationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Node{}).
		Named("nodevalidation").
		Complete(r)
}

/*
This controller batches all nodes which satisfy spec.newNodeValidation.criteria within the configured batchPeriodSeconds
into a single ValidationRequest which targets the given nodes using spec.newNodeValidation.newNodeTests. To ensure
that each node is only included in a single batch, this controller will set the spec.newNodeValidation.condition after
successfully creating a ValidationRequest for the batch to ensure that an already-validated new node which may still
satisfy the spec.newNodeValidation.criteria is not included in a duplicate batch. As a result, a node needs to both
satisfy spec.newNodeValidation.criteria and not have the spec.newNodeValidation.condition set in order to be eligible
to join an active batch.

Batch management:
- The controller tracks nodesInBatch and batchEndTime as in-memory state and does not persist any pending batch state
within the K8s API. If the controller restarts, it will be able to exactly reconstruct nodesInBatch on start-up
(with a new batchEndTime).
- The batch period starts as soon as the first eligible node joins a group.
- A node which satisfies the newNodeValidation.criteria and does not have the newNodeValidation.condition set will join
the pending batch.
- A node which no longer satisfies the newNodeValidation.criteria will be removed from an active batch (a node
would also be removed from the batch if it had the newNodeValidation.condition set but no external actor should be
modifying this property). Additionally, any deleted node will also be removed from an active batch.
- When the batch flush time is met, we do 1 final check for readiness criteria and will remove any nodes from the batch
prior to creating the ValidationRequest. This allows a final check on newNodeValidation.criteria which may not be
evaluated from node events (for example, a node may violate the creationTimestamp criteria if it is in a batch for too
long which does not correspond to any change to the node object which would trigger an event). This also protects
against a situation where we update the newNodeValidation.condition for a node and it is immediately added to the next
batch (from a stale cache read that did not detect our latest write). As long as our batch period is longer than the
cache delay, we will drop the node from the batch during this check.
- At batch trigger time, it's possible all eligible nodes have become ineligible. We will skip creating a
ValidationRequest if no nodes exist within the batch.

Triggering reconciling:
- Edge-based triggers: the controller is primarily triggered by writes to node objects which ensures eligible nodes
join the batch and ensures ineligible nodes are removed from the batch.
- Level-based triggers: when a new batch is started by the first node joining, we will re-queue that node to trigger
a reconcile exactly at the batch flush time to ensure that we have an opportunity to create a ValidationRequest for
all nodes in the batch.

Handling failures: it's possible that either we fail to create a ValidationRequest for a batch or we fail to set the
newNodeValidation.condition for one or more nodes within that batch. We need to retry both of these requests while
ensuring that a duplicated ValidationRequest is not created.
- To ensure that we do not create duplicated ValidationRequests for the same batch, we will use a consistent
ValidationRequest name per batch period. Specifically, we will use the batchEndTime within the ValidationRequest name
and only clear the batch after its processing has completed. If a create VR request fails client-side but succeeds
server-side, and we retry, we will gracefully handle the AlreadyExists error and proceed to node status updates.
- When we retry processing a batch, it is possible that some nodes which were originally eligible are now ineligible.
If the ValidationRequest does not currently exist, we will proceed with targeting the nodes currently in the batch.
If the ValidationRequest does already exist, we will inherit the nodes that were included in the ValidationRequest
and override our in-memory state. At this point, we will update the newNodeValidation.condition for all nodes
which were included in the ValidationRequest. If any of these requests fail, we will re-execute the steps above.
*/
func (r *NodeValidationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	if len(r.nodesInBatch) > 0 && time.Now().After(r.batchEndTime) {
		if err := r.createValidationRequestForNewNodes(ctx); err != nil {
			return ctrl.Result{}, fmt.Errorf("flush new node validation batch: %w", err)
		}
	}

	var node corev1.Node
	if err := r.Get(ctx, req.NamespacedName, &node); err != nil {
		if apierrors.IsNotFound(err) {
			r.removeNodeFromPendingBatch(req.Name)
			return ctrl.Result{}, nil
		}

		return ctrl.Result{}, fmt.Errorf("get node %q: %w", req.Name, err)
	}

	eligible, err := r.isNodeEligibleForBatch(&node)
	if err != nil {
		return ctrl.Result{}, err
	}

	if !eligible {
		r.removeNodeFromPendingBatch(node.Name)
		return ctrl.Result{}, nil
	}

	requeueDurationForBatch := r.addNodeToPendingBatch(node.Name)

	return ctrl.Result{RequeueAfter: requeueDurationForBatch}, nil
}

func (r *NodeValidationReconciler) createValidationRequestForNewNodes(ctx context.Context) error {
	candidateNames := make([]string, 0, len(r.nodesInBatch))
	for n := range r.nodesInBatch {
		candidateNames = append(candidateNames, n)
	}

	eligibleNames, err := r.getEligibleNodesInBatch(ctx, candidateNames)
	if err != nil {
		metrics.NewNodeValidationBatchesTotal.WithLabelValues(metrics.StatusFailure).Inc()
		return err
	}

	validationRequestName, targetedNodes, err := r.createValidationRequest(ctx, eligibleNames)
	if err != nil {
		metrics.NewNodeValidationBatchesTotal.WithLabelValues(metrics.StatusFailure).Inc()
		return err
	}

	if len(validationRequestName) != 0 {
		slog.InfoContext(ctx, "Created ValidationRequest for new nodes", "validationRequest", validationRequestName,
			"nodes", targetedNodes)

		for _, name := range targetedNodes {
			if err := r.setNewNodeValidationCondition(ctx, name, validationRequestName); err != nil {
				metrics.NewNodeValidationBatchesTotal.WithLabelValues(metrics.StatusFailure).Inc()
				return fmt.Errorf("mark node %q validated for %q: %w", name, validationRequestName, err)
			}
		}
	}

	r.nodesInBatch = nil

	metrics.NewNodeValidationBatchesTotal.WithLabelValues(metrics.StatusSuccess).Inc()

	if len(validationRequestName) != 0 {
		metrics.NewNodeValidationBatchSize.Observe(float64(len(targetedNodes)))
	}

	return nil
}

func (r *NodeValidationReconciler) removeNodeFromPendingBatch(nodeName string) {
	delete(r.nodesInBatch, nodeName)
}

func (r *NodeValidationReconciler) addNodeToPendingBatch(nodeName string) time.Duration {
	if r.nodesInBatch == nil {
		r.nodesInBatch = make(map[string]bool)
	}

	isFirstNodeInBatch := len(r.nodesInBatch) == 0
	r.nodesInBatch[nodeName] = true

	if !isFirstNodeInBatch {
		return 0
	}

	batchPeriod := time.Duration(r.Config.Validation.Spec.NewNodeValidation.BatchPeriodSeconds) * time.Second
	r.batchEndTime = time.Now().Add(batchPeriod)

	return time.Until(r.batchEndTime)
}

func (r *NodeValidationReconciler) isNodeEligibleForBatch(node *corev1.Node) (bool, error) {
	cfg := r.Config.Validation.Spec.NewNodeValidation

	if isNodeConditionTrue(node, cfg.Condition) {
		return false, nil
	}

	failedCriterion, err := evaluateCriteria(node, cfg.Criteria, r.CriteriaPrograms)
	if err != nil {
		return false, fmt.Errorf("evaluate newNodeValidation criteria for node %q: %w", node.Name, err)
	}

	return len(failedCriterion) == 0, nil
}

func (r *NodeValidationReconciler) getEligibleNodesInBatch(ctx context.Context, names []string) ([]string, error) {
	sorted := append([]string{}, names...)
	sort.Strings(sorted)

	var eligible []string

	for _, name := range sorted {
		var node corev1.Node
		if err := r.Get(ctx, client.ObjectKey{Name: name}, &node); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}

			return nil, fmt.Errorf("get node %q: %w", name, err)
		}

		ok, err := r.isNodeEligibleForBatch(&node)
		if err != nil {
			return nil, err
		}

		if ok {
			eligible = append(eligible, name)
		}
	}

	return eligible, nil
}

func (r *NodeValidationReconciler) createValidationRequest(ctx context.Context, eligibleNames []string) (string,
	[]string, error) {
	if len(eligibleNames) == 0 {
		return "", nil, nil
	}

	nodes := make([]v1alpha1.NodeSpec, len(eligibleNames))
	for i, name := range eligibleNames {
		nodes[i] = v1alpha1.NodeSpec{Name: name}
	}

	name := fmt.Sprintf("validation-%d", r.batchEndTime.UnixNano())

	validationRequest := &v1alpha1.ValidationRequest{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by":     "nvsentinel",
				"nvsentinel.nvidia.com/created-by": "lifecycle-manager",
			},
		},
		Spec: v1alpha1.ValidationRequestSpec{
			Nodes: nodes,
			Tests: r.Config.Validation.Spec.NewNodeValidation.NewNodeTests,
		},
	}

	if err := r.Create(ctx, validationRequest); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return "", nil, fmt.Errorf("create ValidationRequest for new nodes %v: %w", eligibleNames, err)
		}

		existing := &v1alpha1.ValidationRequest{}
		if err := r.Get(ctx, client.ObjectKey{Name: name}, existing); err != nil {
			return "", nil, fmt.Errorf("get existing ValidationRequest %q for new nodes %v: %w", name,
				eligibleNames, err)
		}

		existingNames := make([]string, len(existing.Spec.Nodes))
		for i, n := range existing.Spec.Nodes {
			existingNames[i] = n.Name
		}

		return existing.Name, existingNames, nil
	}

	return validationRequest.Name, eligibleNames, nil
}

func (r *NodeValidationReconciler) setNewNodeValidationCondition(ctx context.Context, nodeName,
	validationRequestName string) error {
	conditionType := r.Config.Validation.Spec.NewNodeValidation.Condition
	message := fmt.Sprintf("New node validation requested via ValidationRequest %q", validationRequestName)

	return kubeclient.RetryNodePatch(func() error {
		var node corev1.Node
		if err := r.APIReader.Get(ctx, client.ObjectKey{Name: nodeName}, &node); err != nil {
			return client.IgnoreNotFound(err)
		}

		original := node.DeepCopy()

		if !setNodeCondition(&node, conditionType, message) {
			return nil
		}

		return r.Status().Patch(ctx, &node, client.MergeFromWithOptions(original, client.MergeFromWithOptimisticLock{}))
	})
}

func isNodeConditionTrue(node *corev1.Node, conditionType corev1.NodeConditionType) bool {
	for _, c := range node.Status.Conditions {
		if c.Type == conditionType && c.Status == corev1.ConditionTrue {
			return true
		}
	}

	return false
}

func setNodeCondition(node *corev1.Node, conditionType corev1.NodeConditionType, message string) bool {
	now := metav1.Now()

	for i := range node.Status.Conditions {
		c := &node.Status.Conditions[i]
		if c.Type == conditionType {
			changed := c.Status != corev1.ConditionTrue

			if changed {
				c.LastTransitionTime = now
			}

			c.Status = corev1.ConditionTrue
			c.Reason = newNodeValidationReason
			c.Message = message
			c.LastHeartbeatTime = now

			return changed
		}
	}

	node.Status.Conditions = append(node.Status.Conditions, corev1.NodeCondition{
		Type:               conditionType,
		Status:             corev1.ConditionTrue,
		Reason:             newNodeValidationReason,
		Message:            message,
		LastHeartbeatTime:  now,
		LastTransitionTime: now,
	})

	return true
}
