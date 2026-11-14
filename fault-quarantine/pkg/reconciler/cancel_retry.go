// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

package reconciler

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/util/workqueue"

	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/common"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/metrics"
)

// Fault-quarantine ends a quarantine session on a manual uncordon, a manual untaint or a
// dry-run discard. The handler cleans the node and, in the same node update, sets the
// quarantineCancellationPending annotation. A worker then marks the session Cancelled in the
// datastore and removes the annotation. A failed write is retried with a backoff, and the
// annotation lets a restarted fault-quarantine find the node again. Without this, one failed
// write left the session Quarantined after the node lost the annotations that show it, and
// node-drainer and fault-remediation went on with the drain and the reboot of the node.
const (
	// cancelAttemptTimeout bounds one cancellation with the retries of its store calls. It
	// covers a MongoDB primary election.
	cancelAttemptTimeout = 30 * time.Second
	cancelRetryBaseDelay = time.Second
	cancelRetryMaxDelay  = time.Minute
)

func newCancelQueue() workqueue.TypedRateLimitingInterface[string] {
	return workqueue.NewTypedRateLimitingQueue(
		workqueue.NewTypedItemExponentialFailureRateLimiter[string](cancelRetryBaseDelay, cancelRetryMaxDelay))
}

// cancellationPendingAnnotation is added in the cleanup of an ended session.
func cancellationPendingAnnotation() map[string]string {
	return map[string]string{
		common.QuarantineCancellationPendingAnnotationKey: common.QuarantineCancellationPendingAnnotationValue,
	}
}

// queueEndedSessionCancellation hands the cancellation of a cleaned node to the worker.
func (r *Reconciler) queueEndedSessionCancellation(ctx context.Context, nodeName, reason string) {
	slog.InfoContext(ctx, "Queued the cancellation of the ended quarantine session", "node", nodeName, "reason", reason)
	r.cancelQueue.Add(nodeName)
}

// queuePendingCancellations queues every node that carries the pending annotation, so a
// cancellation that had not been written before a restart runs again.
func (r *Reconciler) queuePendingCancellations(ctx context.Context) {
	nodes, err := r.k8sClient.NodeInformer.ListNodes()
	if err != nil {
		slog.WarnContext(ctx, "Failed to list nodes for pending cancellations", "error", err)
		return
	}

	queued := 0

	for _, node := range nodes {
		if _, pending := node.Annotations[common.QuarantineCancellationPendingAnnotationKey]; pending {
			r.cancelQueue.Add(node.Name)

			queued++
		}
	}

	slog.InfoContext(ctx, "Queued pending cancellations of ended quarantine sessions", "nodes", queued)
}

// runCancelQueue works the cancellation queue until ctx ends.
func (r *Reconciler) runCancelQueue(ctx context.Context) {
	go func() {
		<-ctx.Done()
		r.cancelQueue.ShutDown()
	}()

	for r.processNextCancellation(ctx) {
	}
}

func (r *Reconciler) processNextCancellation(ctx context.Context) bool {
	nodeName, shutdown := r.cancelQueue.Get()
	if shutdown {
		return false
	}

	defer r.cancelQueue.Done(nodeName)

	attemptCtx, cancel := context.WithTimeout(ctx, cancelAttemptTimeout)
	defer cancel()

	if err := r.cancelEndedSession(attemptCtx, nodeName); err != nil {
		slog.ErrorContext(ctx, "Failed to cancel an ended quarantine session, retrying", "node", nodeName,
			"attempt", r.cancelQueue.NumRequeues(nodeName)+1, "error", err)
		r.cancelQueue.AddRateLimited(nodeName)

		return true
	}

	r.cancelQueue.Forget(nodeName)

	return true
}

// cancelEndedSession cancels the ended session of a node that carries the pending annotation
// and then removes the annotation. It reads the node from the API server, because the cache
// can still miss the annotation that the cleanup just set.
func (r *Reconciler) cancelEndedSession(ctx context.Context, nodeName string) error {
	node, err := r.k8sClient.NodeInformer.GetNodeDirect(ctx, nodeName)
	if apierrors.IsNotFound(err) {
		return nil
	}

	if err != nil {
		metrics.ProcessingErrors.WithLabelValues("cancellation_pending_node_error").Inc()
		return fmt.Errorf("failed to read node %s: %w", nodeName, err)
	}

	if _, pending := node.Annotations[common.QuarantineCancellationPendingAnnotationKey]; !pending {
		return nil
	}

	// A node that is quarantined again has a newer session. The cancellation finds the latest
	// session, so it would end the newer one, and it is skipped: the earlier health events stay
	// active and the newer quarantine handles the node.
	if !quarantinedAgain(node) {
		if r.eventWatcher == nil {
			slog.WarnContext(ctx, "eventWatcher is NIL - cannot cancel quarantining events in database", "node", nodeName)
			return nil
		}

		if err := r.eventWatcher.CancelLatestQuarantiningEvents(
			ctx, nodeName, "Quarantine ended by an operator or a dry-run switch"); err != nil {
			metrics.ProcessingErrors.WithLabelValues("mongodb_cancel_quarantine_error").Inc()
			return fmt.Errorf("failed to cancel latest quarantining events for node %s: %w", nodeName, err)
		}

		slog.InfoContext(ctx, "Cancelled the ended quarantine session", "node", nodeName)
	} else {
		slog.WarnContext(ctx, "Node is quarantined again before its ended session was cancelled; "+
			"the earlier health events stay active", "node", nodeName)
	}

	if err := r.k8sClient.UpdateNode(ctx, nodeName, func(node *v1.Node) error {
		delete(node.Annotations, common.QuarantineCancellationPendingAnnotationKey)
		return nil
	}); err != nil {
		metrics.ProcessingErrors.WithLabelValues("cancellation_pending_node_error").Inc()

		return fmt.Errorf("failed to remove %s from node %s: %w",
			common.QuarantineCancellationPendingAnnotationKey, nodeName, err)
	}

	return nil
}

// quarantinedAgain reports whether a node carries a newer quarantine: a quarantine annotation
// together with an action that fault-quarantine applied or recorded in dry run. A quarantine
// annotation alone, as an event in the gap after the cleanup can leave it, is not a newer
// quarantine.
func quarantinedAgain(node *v1.Node) bool {
	if isEmptyHealthEventAnnotation(node.Annotations[common.QuarantineHealthEventAnnotationKey]) {
		return false
	}

	for _, key := range []string{
		common.QuarantineHealthEventIsCordonedAnnotationKey,
		common.QuarantineHealthEventAppliedTaintsAnnotationKey,
		common.QuarantineHealthEventAppliedLabelsAnnotationKey,
		common.QuarantineHealthEventDryRunAnnotationKey,
	} {
		if _, ok := node.Annotations[key]; ok {
			return true
		}
	}

	return false
}
