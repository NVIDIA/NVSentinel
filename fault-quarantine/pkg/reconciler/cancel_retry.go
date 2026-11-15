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
	"regexp"
	"slices"
	"strings"
	"time"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"k8s.io/client-go/util/workqueue"

	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/common"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/metrics"
)

// Fault-quarantine ends a quarantine session on a manual uncordon, a manual untaint or a
// dry-run discard. The handler cleans the node and, in the same node update, adds the session
// to the quarantineCancellationPending annotation. A worker then marks the session Cancelled in
// the datastore and removes it from the annotation. A failed write is retried with a backoff,
// and the annotation lets a restarted fault-quarantine find the node again. Without this, one
// failed write left the session Quarantined after the node lost the annotations that show it,
// and node-drainer and fault-remediation went on with the drain and the reboot of the node.
//
// The annotation names each ended session by the event IDs of its quarantine annotation, so the
// worker cancels that session and not a newer quarantine of the node. Sessions are separated by
// semicolons and the IDs of a session by commas. The IDs usually hold the first event of the
// session, which is cancelled last, so the same IDs work again after a write that failed part of
// the way. The worker leaves an ended session alone once a newer quarantine of the node started,
// and it works the newest pending session first, so an older one is not left alone only because
// a newer one waits to be cancelled.
const (
	// cancelAttemptTimeout bounds one cancellation with the retries of its store calls. It
	// covers a MongoDB primary election.
	cancelAttemptTimeout = 30 * time.Second
	cancelRetryBaseDelay = time.Second
	cancelRetryMaxDelay  = time.Minute

	cancelReason = "Quarantine ended by an operator or a dry-run switch"

	pendingSessionSeparator = ";"
	sessionIDSeparator      = ","
)

// recordIDPattern matches a MongoDB ObjectID and a PostgreSQL UUID.
var recordIDPattern = regexp.MustCompile(
	`^([0-9a-fA-F]{24}|[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})$`)

func newCancelQueue() workqueue.TypedRateLimitingInterface[string] {
	return workqueue.NewTypedRateLimitingQueue(
		workqueue.NewTypedItemExponentialFailureRateLimiter[string](cancelRetryBaseDelay, cancelRetryMaxDelay))
}

// pendingCancellationAnnotation is added in the cleanup of an ended session. It keeps the
// sessions that are already pending from the annotations the handler read, and adds the
// session that ends now. A node without a quarantine annotation has no session to add, as when
// one node update both uncordons and untaints and the second cleanup finds the first one done.
func (r *Reconciler) pendingCancellationAnnotation(
	ctx context.Context,
	nodeName string,
	annotations map[string]string,
) map[string]string {
	sessions := pendingSessions(annotations[common.QuarantineCancellationPendingAnnotationKey])

	if !isEmptyHealthEventAnnotation(annotations[common.QuarantineHealthEventAnnotationKey]) {
		session := sessionFromIDs(r.sourceDocIDsFromAnnotation(ctx, nodeName))
		if !slices.Contains(sessions, session) {
			sessions = append(sessions, session)
		}
	}

	if len(sessions) == 0 {
		return nil
	}

	return map[string]string{
		common.QuarantineCancellationPendingAnnotationKey: strings.Join(sessions, pendingSessionSeparator),
	}
}

// sessionFromIDs names a session by its record IDs, sorted, or as the latest session when none
// is a MongoDB or PostgreSQL record ID, as with an older fault-quarantine.
func sessionFromIDs(ids []string) string {
	ids = slices.DeleteFunc(slices.Clone(ids), func(id string) bool { return !isRecordID(id) })
	if len(ids) == 0 {
		return common.QuarantineCancellationLatestSession
	}

	slices.Sort(ids)

	return strings.Join(slices.Compact(ids), sessionIDSeparator)
}

// isRecordID reports whether id is a MongoDB ObjectID or a PostgreSQL UUID.
func isRecordID(id string) bool {
	return recordIDPattern.MatchString(id)
}

// pendingSessions splits the value of the pending annotation into its sessions.
func pendingSessions(value string) []string {
	var sessions []string

	for session := range strings.SplitSeq(value, pendingSessionSeparator) {
		if session = strings.TrimSpace(session); session != "" {
			sessions = append(sessions, session)
		}
	}

	return sessions
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

	if err := r.cancelEndedSessions(attemptCtx, nodeName); err != nil {
		slog.ErrorContext(ctx, "Failed to cancel an ended quarantine session, retrying", "node", nodeName,
			"attempt", r.cancelQueue.NumRequeues(nodeName)+1, "error", err)
		r.cancelQueue.AddRateLimited(nodeName)

		return true
	}

	r.cancelQueue.Forget(nodeName)

	return true
}

// cancelEndedSessions cancels the sessions in the pending annotation of a node, oldest first,
// and removes each one from the annotation once it is cancelled. It reads the node from the API
// server, because the cache can still miss the annotation that the cleanup just set.
func (r *Reconciler) cancelEndedSessions(ctx context.Context, nodeName string) error {
	node, err := r.k8sClient.NodeInformer.GetNodeDirect(ctx, nodeName)
	if apierrors.IsNotFound(err) {
		return nil
	}

	if err != nil {
		metrics.ProcessingErrors.WithLabelValues("cancellation_pending_node_error").Inc()
		return fmt.Errorf("failed to read node %s: %w", nodeName, err)
	}

	value, pending := node.Annotations[common.QuarantineCancellationPendingAnnotationKey]
	if !pending {
		return nil
	}

	if r.eventWatcher == nil {
		slog.WarnContext(ctx, "eventWatcher is NIL - cannot cancel quarantining events in database", "node", nodeName)
		return nil
	}

	sessions := pendingSessions(value)
	if len(sessions) == 0 {
		return r.removePendingSession(ctx, nodeName, "")
	}

	// Newest first, and the first failure stops the pass, so an older session is not left alone
	// because a newer pending one is still Quarantined.
	for _, sessionID := range slices.Backward(sessions) {
		if err := r.cancelEndedSession(ctx, node, sessionID); err != nil {
			return err
		}
	}

	return nil
}

// quarantinedNow reports whether a node carries a quarantine annotation with events.
func quarantinedNow(node *v1.Node) bool {
	return !isEmptyHealthEventAnnotation(node.Annotations[common.QuarantineHealthEventAnnotationKey])
}

// cancelEndedSession cancels one ended session and removes it from the pending annotation.
func (r *Reconciler) cancelEndedSession(ctx context.Context, node *v1.Node, sessionID string) error {
	nodeName := node.Name
	eventIDs := strings.Split(sessionFromIDs(strings.Split(sessionID, sessionIDSeparator)), sessionIDSeparator)

	if eventIDs[0] == common.QuarantineCancellationLatestSession && quarantinedNow(node) {
		slog.WarnContext(ctx, "Node is quarantined again and its ended session has no event ID; "+
			"the earlier health events stay active", "node", nodeName)

		return r.removePendingSession(ctx, nodeName, sessionID)
	}

	startID, quarantined, err := r.eventWatcher.FindQuarantineSession(ctx, nodeName, eventIDs)
	if err != nil {
		metrics.ProcessingErrors.WithLabelValues("mongodb_cancel_quarantine_error").Inc()
		return fmt.Errorf("failed to find the ended quarantine session %s of node %s: %w", sessionID, nodeName, err)
	}

	if !quarantined {
		slog.InfoContext(ctx, "Ended quarantine session has nothing left to cancel",
			"node", nodeName, "session", sessionID)

		return r.removePendingSession(ctx, nodeName, sessionID)
	}

	if err := r.eventWatcher.CancelQuarantineSession(ctx, nodeName, startID, cancelReason); err != nil {
		metrics.ProcessingErrors.WithLabelValues("mongodb_cancel_quarantine_error").Inc()
		return fmt.Errorf("failed to cancel the ended quarantine session %s of node %s: %w", startID, nodeName, err)
	}

	slog.InfoContext(ctx, "Finished the cancellation of the ended quarantine session",
		"node", nodeName, "session", sessionID, "firstEvent", startID)

	return r.removePendingSession(ctx, nodeName, sessionID)
}

// removePendingSession removes a session from the pending annotation, and the annotation once
// no session is left in it. It reads the node from the API server and updates it with that
// resourceVersion, so a session that a cleanup added in between is not lost.
func (r *Reconciler) removePendingSession(ctx context.Context, nodeName, sessionID string) error {
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		node, err := r.k8sClient.NodeInformer.GetNodeDirect(ctx, nodeName)
		if err != nil {
			return err
		}

		value, pending := node.Annotations[common.QuarantineCancellationPendingAnnotationKey]
		if !pending {
			return nil
		}

		sessions := slices.DeleteFunc(pendingSessions(value), func(session string) bool { return session == sessionID })
		if len(sessions) > 0 {
			node.Annotations[common.QuarantineCancellationPendingAnnotationKey] = strings.Join(sessions, ",")
		} else {
			delete(node.Annotations, common.QuarantineCancellationPendingAnnotationKey)
		}

		_, err = r.k8sClient.Clientset.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{})

		return err
	})
	if apierrors.IsNotFound(err) {
		return nil
	}

	if err != nil {
		metrics.ProcessingErrors.WithLabelValues("cancellation_pending_node_error").Inc()

		return fmt.Errorf("failed to update %s on node %s: %w",
			common.QuarantineCancellationPendingAnnotationKey, nodeName, err)
	}

	return nil
}
