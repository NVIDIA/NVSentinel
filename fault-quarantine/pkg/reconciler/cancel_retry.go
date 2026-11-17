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
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/workqueue"

	"github.com/nvidia/nvsentinel/commons/pkg/tracing"
	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/coldstart"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/common"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/healthEventsAnnotation"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/informer"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/metrics"
)

// A manual uncordon, a manual untaint or switching dry run off ends a node's quarantine. FQ then
// writes Cancelled to the datastore first and cleans the node only after that. While the write
// keeps failing the annotations stay on the node, so the queue here can retry it and the
// stale-state check finds the node again after a restart.
//
// A new fault on such a node finishes the cancel first (endSessionBeforeEvent). If the write is
// still failing by then, the node is cleaned anyway so the fault can quarantine it, and the
// earlier events keep their status.
const (
	// cancelAttemptTimeout is the budget for one attempt by the worker, store retries included.
	// It's long enough to cover a MongoDB primary election.
	cancelAttemptTimeout = 30 * time.Second
	cancelRetryBaseDelay = time.Second
	cancelRetryMaxDelay  = time.Minute

	// eventCancelTimeout is how long an incoming event waits for the cancel. It's short because
	// the events of every other node wait behind it.
	eventCancelTimeout = 10 * time.Second

	// cleanedNodePollInterval is how often the event path checks the informer cache for the
	// cleaned node.
	cleanedNodePollInterval = 10 * time.Millisecond
)

func newCancelQueue() workqueue.TypedRateLimitingInterface[string] {
	return workqueue.NewTypedRateLimitingQueue(
		workqueue.NewTypedItemExponentialFailureRateLimiter[string](cancelRetryBaseDelay, cancelRetryMaxDelay))
}

// pendingSessionEnd is the state kept for a node while it waits in the queue.
type pendingSessionEnd struct {
	// seen is what the informer reported. If the operator cordons the node again before the
	// cancel is written, the node looks quarantined and this is the only record that it ended.
	seen informer.ManualChange
	// cancelled is the quarantine annotation whose events are already Cancelled, so a retry
	// after a failed node cleanup doesn't cancel them again.
	cancelled string
}

// pendingCancellations tracks the nodes whose cancel isn't finished yet. Its size is the
// fault_quarantine_nodes_pending_cancellation gauge.
type pendingCancellations struct {
	mu    sync.Mutex
	nodes map[string]pendingSessionEnd
}

func (p *pendingCancellations) add(nodeName string, seen informer.ManualChange) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.nodes == nil {
		p.nodes = map[string]pendingSessionEnd{}
	}

	pending := p.nodes[nodeName]
	pending.seen.Uncordoned = pending.seen.Uncordoned || seen.Uncordoned
	pending.seen.Untainted = pending.seen.Untainted || seen.Untainted
	p.nodes[nodeName] = pending

	metrics.NodesPendingCancellation.Set(float64(len(p.nodes)))
}

func (p *pendingCancellations) get(nodeName string) (pendingSessionEnd, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	pending, ok := p.nodes[nodeName]

	return pending, ok
}

// update changes a node's entry if the node is still pending.
func (p *pendingCancellations) update(nodeName string, change func(pending *pendingSessionEnd)) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if pending, ok := p.nodes[nodeName]; ok {
		change(&pending)
		p.nodes[nodeName] = pending
	}
}

func (p *pendingCancellations) remove(nodeName string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	delete(p.nodes, nodeName)
	metrics.NodesPendingCancellation.Set(float64(len(p.nodes)))
}

// queueManualUncordon is the informer callback for a manual uncordon, and for a stale cordon or
// dry-run record found at startup.
func (r *Reconciler) queueManualUncordon(nodeName string) error {
	r.queueSessionEnd(nodeName, informer.ManualChange{Uncordoned: true})

	return nil
}

// queueManualUntaint is the informer callback for a manual untaint, and for stale taints found
// at startup.
func (r *Reconciler) queueManualUntaint(nodeName string) error {
	r.queueSessionEnd(nodeName, informer.ManualChange{Untainted: true})

	return nil
}

// queueSessionEnd queues the node for the worker, which cancels its events in the datastore and
// then cleans the node.
func (r *Reconciler) queueSessionEnd(nodeName string, seen informer.ManualChange) {
	slog.Info("Queued the node for quarantine cancellation", "node", nodeName)

	r.pendingCancels.add(nodeName, seen)
	r.cancelQueue.Add(nodeName)
}

// runCancelQueue runs the queue worker until ctx is done.
func (r *Reconciler) runCancelQueue(ctx context.Context) {
	go func() {
		<-ctx.Done()
		r.cancelQueue.ShutDown()
	}()

	for r.processNextSessionEnd(ctx) {
	}
}

func (r *Reconciler) processNextSessionEnd(ctx context.Context) bool {
	nodeName, shutdown := r.cancelQueue.Get()
	if shutdown {
		return false
	}

	defer r.cancelQueue.Done(nodeName)

	attemptCtx, cancel := context.WithTimeout(ctx, cancelAttemptTimeout)
	defer cancel()

	if _, err := r.endSession(attemptCtx, nodeName, sessionEndOptions{
		lockTimeout:   cancelAttemptTimeout,
		cancelTimeout: cancelAttemptTimeout,
	}); err != nil {
		slog.ErrorContext(ctx, "Failed to cancel the node's quarantine, will retry", "node", nodeName,
			"attempt", r.cancelQueue.NumRequeues(nodeName)+1, "error", err)
		r.cancelQueue.AddRateLimited(nodeName)

		return true
	}

	r.cancelQueue.Forget(nodeName)

	return true
}

type sessionEndOptions struct {
	// lockTimeout is how long to wait when the same node is already being handled.
	lockTimeout time.Duration
	// cancelTimeout is the budget for the datastore write.
	cancelTimeout time.Duration
	// cleanAnyway cleans the node even when the write fails. The events then keep their status.
	cleanAnyway bool
}

// lockSessionEnd lets only one caller handle a node at a time. The worker and the event path can
// both get to the same node, and the second one has to see what the first one did.
func (r *Reconciler) lockSessionEnd(ctx context.Context, nodeName string, timeout time.Duration) (func(), error) {
	lockValue, _ := r.sessionEndLocks.LoadOrStore(nodeName, make(chan struct{}, 1))
	lock, _ := lockValue.(chan struct{})

	lockCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	select {
	case lock <- struct{}{}:
		return func() { <-lock }, nil
	case <-lockCtx.Done():
		return nil, fmt.Errorf("waiting for another cancellation of node %s: %w", nodeName, lockCtx.Err())
	}
}

// sessionEndResult is what endSession found and did.
type sessionEndResult struct {
	// found is false when the node is gone.
	found bool
	// annotations are the node's quarantine annotations as endSession read them.
	annotations map[string]string
	// removed are the annotation keys endSession removed, nil when it had nothing to do.
	removed []string
}

// endSession cancels the node's quarantining events in the datastore and then cleans the node.
// It reads the node from the API server, so a node that was already cleaned is left alone. If
// the write fails the node keeps its annotations and the next call tries again.
func (r *Reconciler) endSession(
	ctx context.Context,
	nodeName string,
	opts sessionEndOptions,
) (sessionEndResult, error) {
	unlock, err := r.lockSessionEnd(ctx, nodeName, opts.lockTimeout)
	if err != nil {
		return sessionEndResult{}, err
	}
	defer unlock()

	node, err := r.k8sClient.NodeInformer.GetNodeDirect(ctx, nodeName)
	if apierrors.IsNotFound(err) {
		r.pendingCancels.remove(nodeName)
		return sessionEndResult{}, nil
	}

	if err != nil {
		return sessionEndResult{}, fmt.Errorf("failed to read node %s: %w", nodeName, err)
	}

	result := sessionEndResult{found: true, annotations: quarantineAnnotationsOf(node)}

	change := r.endedSession(node)
	if change == (informer.ManualChange{}) {
		r.pendingCancels.remove(nodeName)
		return result, nil
	}

	if err := r.cancelEndedSession(ctx, nodeName, result.annotations, change, opts.cancelTimeout); err != nil {
		if !opts.cleanAnyway {
			return result, err
		}

		slog.WarnContext(ctx, "Cancellation is still failing, cleaning the node anyway; "+
			"its earlier health events stay active", "node", nodeName, "error", err)
	}

	result.removed, err = r.cleanEndedSession(ctx, nodeName, result.annotations, change)
	if err != nil {
		return result, err
	}

	r.pendingCancels.remove(nodeName)

	return result, nil
}

// endedSession returns what ended the node's quarantine, or the zero value if nothing did. It
// also counts what the informer saw earlier, so a node that the operator cordoned or tainted
// again is still handled.
func (r *Reconciler) endedSession(node *corev1.Node) informer.ManualChange {
	change := r.k8sClient.NodeInformer.DetectManualChange(node)

	if change.DryRun {
		// In dry-run mode the record stays. Otherwise dry run was switched off and the record
		// goes, whatever the node's cordon and taints look like.
		if r.config.DryRun {
			return informer.ManualChange{}
		}

		return informer.ManualChange{DryRun: true}
	}

	if pending, ok := r.pendingCancels.get(node.Name); ok {
		_, cordonRecorded := node.Annotations[common.QuarantineHealthEventIsCordonedAnnotationKey]
		_, taintsRecorded := node.Annotations[common.QuarantineHealthEventAppliedTaintsAnnotationKey]

		change.Uncordoned = change.Uncordoned || (pending.seen.Uncordoned && cordonRecorded)
		change.Untainted = change.Untainted || (pending.seen.Untainted && taintsRecorded)
	}

	return change
}

// cancelEndedSession writes Cancelled for the node's quarantining events, including the ones
// named in its quarantine annotation.
func (r *Reconciler) cancelEndedSession(
	ctx context.Context,
	nodeName string,
	annotations map[string]string,
	change informer.ManualChange,
	timeout time.Duration,
) error {
	if r.eventWatcher == nil {
		slog.WarnContext(ctx, "eventWatcher is NIL - cannot cancel quarantining events in database", "node", nodeName)
		return nil
	}

	quarantineAnnotation := annotations[common.QuarantineHealthEventAnnotationKey]

	// Already cancelled on an earlier attempt where only the node cleanup failed.
	if pending, ok := r.pendingCancels.get(nodeName); ok && quarantineAnnotation != "" &&
		pending.cancelled == quarantineAnnotation {
		return nil
	}

	reason := "Manual untainting detected"

	switch {
	case change.DryRun:
		reason = "Dry-run quarantine discarded"
	case change.Uncordoned:
		reason = "Manual uncordoning detected"
	}

	cancelCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	err := r.eventWatcher.CancelLatestQuarantiningEvents(
		cancelCtx, nodeName, quarantineEventIDs(quarantineAnnotation), reason)
	if err != nil {
		slog.ErrorContext(ctx, "Failed to cancel latest quarantining events",
			"node", nodeName, "reason", reason, "error", err)
		metrics.ProcessingErrors.WithLabelValues("mongodb_cancel_quarantine_error").Inc()

		span := tracing.SpanFromContext(ctx)
		tracing.RecordError(span, err)
		span.SetAttributes(
			attribute.String("fault_quarantine.error.type", "mongodb_cancel_quarantine_error"),
			attribute.String("fault_quarantine.error.message", err.Error()),
		)

		return fmt.Errorf("failed to cancel the quarantining events of node %s: %w", nodeName, err)
	}

	slog.DebugContext(ctx, "Successfully cancelled latest quarantining events", "node", nodeName)

	r.pendingCancels.update(nodeName, func(pending *pendingSessionEnd) { pending.cancelled = quarantineAnnotation })

	return nil
}

// cleanEndedSession cleans the node and returns the annotation keys it removed. A node that was
// both uncordoned and untainted gets both cleanups.
func (r *Reconciler) cleanEndedSession(
	ctx context.Context,
	nodeName string,
	annotations map[string]string,
	change informer.ManualChange,
) ([]string, error) {
	if change.DryRun {
		return r.handleDryRunQuarantineOnNodeChange(ctx, nodeName, annotations)
	}

	removed := []string{}

	if change.Uncordoned {
		uncordonRemoved, err := r.handleManualUncordon(ctx, nodeName, annotations)
		if err != nil {
			return nil, err
		}

		removed = uncordonRemoved
	}

	if change.Untainted {
		untaintRemoved, err := r.handleManualUntaint(ctx, nodeName, annotations)
		if err != nil {
			return nil, err
		}

		removed = untaintRemoved
	}

	return removed, nil
}

// quarantineEventIDs returns the event IDs of a quarantine annotation.
func quarantineEventIDs(value string) []string {
	if isEmptyHealthEventAnnotation(value) {
		return nil
	}

	var healthEventsMap healthEventsAnnotation.HealthEventsAnnotationMap

	if err := json.Unmarshal([]byte(value), &healthEventsMap); err != nil {
		var event protos.HealthEvent
		if json.Unmarshal([]byte(value), &event) != nil {
			return nil
		}

		healthEventsMap = *healthEventsAnnotation.NewHealthEventsAnnotationMap()
		healthEventsMap.AddOrUpdateEvent(&event)
	}

	ids := make([]string, 0, len(healthEventsMap.Events))

	for _, event := range healthEventsMap.Events {
		if event.GetId() != "" {
			ids = append(ids, event.GetId())
		}
	}

	return ids
}

// sessionEnded reports whether the node lost the cordon or a taint that its annotations record.
// A dry-run record never counts in dry-run mode. Outside it, a record whose cordon and taints
// are on the node is a real quarantine with a leftover marker, and that one isn't ended.
func (r *Reconciler) sessionEnded(node *corev1.Node) bool {
	change := r.k8sClient.NodeInformer.DetectManualChange(node)
	if change.DryRun && r.config.DryRun {
		return false
	}

	return change.Uncordoned || change.Untainted
}

// endSessionBeforeEvent finishes a cancel that's still owed on the event's node before the event
// is handled. Otherwise a new fault would join the old quarantine and be cancelled with it.
//
// The cancel only gets eventCancelTimeout here. If it still fails, a fault cleans the node
// anyway and quarantines it, and a healthy event is handled against the quarantine as it is.
func (r *Reconciler) endSessionBeforeEvent(ctx context.Context, event *protos.HealthEvent) error {
	nodeName := event.GetNodeName()

	node, err := r.getNode(ctx, nodeName)
	if err != nil {
		return fmt.Errorf("failed to get node %s: %w", nodeName, err)
	}

	if _, pending := r.pendingCancels.get(nodeName); !pending && !r.sessionEnded(node) {
		return nil
	}

	slog.InfoContext(ctx, "Finishing the pending cancellation before handling the new event", "node", nodeName)

	result, err := r.endSession(ctx, nodeName, sessionEndOptions{
		lockTimeout:   cancelAttemptTimeout + eventCancelTimeout,
		cancelTimeout: eventCancelTimeout,
		cleanAnyway:   !event.GetIsHealthy(),
	})
	if err != nil {
		if !event.GetIsHealthy() {
			return err
		}

		// The node stays queued with what the informer saw. If the healthy event doesn't end the
		// quarantine, the worker still has to cancel it.
		slog.WarnContext(ctx, "Pending cancellation is still failing; "+
			"handling the healthy event anyway", "node", nodeName, "error", err)

		return nil
	}

	// The event reads the node again after this, and it has to see the cleaned one.
	if coldstart.IsRecoveryContext(ctx) {
		coldstart.ForgetRecoveryNode(ctx, nodeName)
		return nil
	}

	if !result.found {
		return nil
	}

	return r.waitForNodeInCache(ctx, nodeName, result)
}

// waitForNodeInCache waits for the informer cache to catch up with endSession. After a cleanup
// that means the removed annotations are gone. If endSession had nothing to do, for example
// because the worker got there first, it means the cache shows the annotations endSession read.
func (r *Reconciler) waitForNodeInCache(ctx context.Context, nodeName string, result sessionEndResult) error {
	waitCtx, cancel := context.WithTimeout(ctx, cancelAttemptTimeout)
	defer cancel()

	err := wait.PollUntilContextCancel(waitCtx, cleanedNodePollInterval, true, func(context.Context) (bool, error) {
		node, err := r.k8sClient.NodeInformer.GetNode(nodeName)
		if apierrors.IsNotFound(err) {
			return true, nil
		}

		if err != nil {
			return false, err
		}

		if result.removed == nil {
			return maps.Equal(quarantineAnnotationsOf(node), result.annotations), nil
		}

		for _, key := range result.removed {
			if _, present := node.Annotations[key]; present {
				return false, nil
			}
		}

		return true, nil
	})
	if err != nil {
		return fmt.Errorf("node %s is not up to date in the informer cache: %w", nodeName, err)
	}

	return nil
}
