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

// Package controller implements the periodic sweep of the NVCRE Certification
// Monitor. Each sweep derives the desired set of certification-failure tuples
// from terminal Certification CRs, compares it with the tuples recorded on Node
// annotations, and publishes unhealthy or healthy events to reconcile the two.
package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/noderesults"

	"github.com/nvidia/nvsentinel/health-monitors/nvcre-certification-monitor/pkg/config"
	"github.com/nvidia/nvsentinel/health-monitors/nvcre-certification-monitor/pkg/publisher"
	"github.com/nvidia/nvsentinel/health-monitors/nvcre-certification-monitor/pkg/state"
)

// Reconciler lists all Certification CRs on a fixed interval and
// reconciles the full set each cycle.
type Reconciler struct {
	client         client.Client
	publisher      *publisher.Publisher
	evaluator      *config.Evaluator
	annotator      *state.AnnotationManager
	certAnnotator  *state.CertAnnotationHelper
	resyncInterval time.Duration

	// Decoded result ConfigMaps from the previous sweep, keyed by ConfigMap.
	// Reused while the ConfigMap's resourceVersion is unchanged so the gunzip
	// and JSON decode run once per change rather than once per sweep. Only the
	// sweep goroutine touches this.
	results map[types.NamespacedName]decodedResult
}

type decodedResult struct {
	resourceVersion string
	failed          []nvcrev1alpha1.FailedNode
	succeeded       []string
}

// NewReconciler constructs a Reconciler.
func NewReconciler(
	c client.Client,
	pub *publisher.Publisher,
	evaluator *config.Evaluator,
	annotator *state.AnnotationManager,
	certAnnotator *state.CertAnnotationHelper,
	resyncInterval time.Duration,
) *Reconciler {
	return &Reconciler{
		client:         c,
		publisher:      pub,
		evaluator:      evaluator,
		annotator:      annotator,
		certAnnotator:  certAnnotator,
		resyncInterval: resyncInterval,
		results:        make(map[types.NamespacedName]decodedResult),
	}
}

// Start is called by the manager and runs the reconciliation loop until
// the context is cancelled.
func (r *Reconciler) Start(ctx context.Context) error {
	// Sweep once immediately so a restart does not leave a blind window of one
	// full interval before new certification failures are published.
	if err := r.reconcile(ctx); err != nil {
		slog.Error("Failed to reconcile", "error", err)
	}

	ticker := time.NewTicker(r.resyncInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			slog.Info("Context cancelled")

			return nil
		case <-ticker.C:
			if err := r.reconcile(ctx); err != nil {
				slog.Error("Failed to reconcile", "error", err)
			}
		}
	}
}

func (r *Reconciler) reconcile(ctx context.Context) error {
	certList := &nvcrev1alpha1.CertificationList{}

	if err := r.client.List(ctx, certList); err != nil {
		slog.Error("Failed to list Certification CRs", "error", err)

		return fmt.Errorf("failed to list Certification CRs: %w", err)
	}

	var completedCerts []nvcrev1alpha1.Certification

	for i := range certList.Items {
		if isCertificationTerminal(&certList.Items[i]) {
			completedCerts = append(completedCerts, certList.Items[i])
		}
	}

	sort.Slice(completedCerts, func(i, j int) bool {
		ti, errI := getCompletionTime(&completedCerts[i])
		tj, errJ := getCompletionTime(&completedCerts[j])

		if errI != nil {
			slog.Error("Failed to get completion time", "cert", completedCerts[i].Name, "error", errI)

			return false
		}

		if errJ != nil {
			slog.Error("Failed to get completion time", "cert", completedCerts[j].Name, "error", errJ)

			return true
		}

		return ti.Before(tj)
	})

	// Terminal transition time per cert, used as the cert-processed value so a
	// cert that CRE reopens and re-fails is seen as new again.
	certTimes := make(map[CertRef]time.Time, len(completedCerts))

	for i := range completedCerts {
		if t, err := getCompletionTime(&completedCerts[i]); err == nil {
			certTimes[CertRef{Name: completedCerts[i].Name, Namespace: completedCerts[i].Namespace}] = t
		}
	}

	if err := r.processCertificationCRs(ctx, completedCerts, certTimes); err != nil {
		slog.Error("Failed to process Certification CRs", "error", err)

		return fmt.Errorf("failed to process Certification CRs: %w", err)
	}

	return nil
}

func (r *Reconciler) processCertificationCRs(
	ctx context.Context, certs []nvcrev1alpha1.Certification, certTimes map[CertRef]time.Time,
) error {
	desired, err := r.buildDesired(ctx, certs)
	if err != nil {
		return fmt.Errorf("failed to build desired set: %w", err)
	}

	observed, err := r.getObserved(ctx)
	if err != nil {
		return fmt.Errorf("failed to get observed set: %w", err)
	}

	certsToMark, err := r.processDesiredAndObserved(ctx, desired, observed, certTimes)
	if err != nil {
		return fmt.Errorf("failed to reconcile: %w", err)
	}

	if err := r.markCertsProcessed(ctx, certsToMark); err != nil {
		return fmt.Errorf("failed to mark certs as processed: %w", err)
	}

	return nil
}

func (r *Reconciler) processDesiredAndObserved(
	ctx context.Context,
	desired map[TupleKey]NodeCertFailure,
	observed map[string]map[string]struct{},
	certTimes map[CertRef]time.Time,
) (map[CertRef]time.Time, error) {
	certsToMark := make(map[CertRef]time.Time)
	// Owners of a tuple that could not be written this sweep. Stamping them
	// would make the skipped tuple look like an operator clear once the node
	// annotation is repaired, so they are held back until every tuple lands.
	skipped := make(map[CertRef]struct{})

	for key, entry := range desired {
		annotationKey := key.ErrorCode()
		nodeObs := observed[key.Node]

		if _, inObs := nodeObs[annotationKey]; inObs {
			if err := r.markObservedOwners(ctx, entry, certTimes, certsToMark); err != nil {
				return nil, fmt.Errorf("failed to mark owners of observed tuple: %w", err)
			}

			continue
		}

		if err := r.handleDesiredNotObserved(ctx, key, entry, certTimes, certsToMark); err != nil {
			if errors.Is(err, state.ErrMalformedAnnotation) {
				slog.Warn("Skipping tuple, node cert-failures annotation is malformed",
					"node", key.Node, "errorCode", annotationKey, "error", err)

				for _, ref := range entry.CertRefs {
					skipped[ref] = struct{}{}
				}

				continue
			}

			return nil, fmt.Errorf("failed to handle desired not observed: %w", err)
		}
	}

	for ref := range skipped {
		delete(certsToMark, ref)
	}

	if err := r.healObservedNotDesired(ctx, desired, observed); err != nil {
		return nil, err
	}

	return certsToMark, nil
}

// healObservedNotDesired publishes healthy and clears the node annotation for
// every tuple that no terminal cert asserts any more.
func (r *Reconciler) healObservedNotDesired(
	ctx context.Context,
	desired map[TupleKey]NodeCertFailure,
	observed map[string]map[string]struct{},
) error {
	for nodeName, nodeKeys := range observed {
		for key := range nodeKeys {
			variant, reason, _ := strings.Cut(key, "/")

			tk := TupleKey{Node: nodeName, Variant: variant, Reason: reason}
			if _, inDesired := desired[tk]; inDesired {
				continue
			}

			if err := r.handleObservedNotDesired(ctx, nodeName, key); err != nil {
				if errors.Is(err, state.ErrMalformedAnnotation) {
					slog.Warn("Skipping tuple, node cert-failures annotation is malformed",
						"node", nodeName, "errorCode", key, "error", err)

					continue
				}

				return fmt.Errorf("failed to handle observed not desired: %w", err)
			}
		}
	}

	return nil
}

// releasedForCurrentState reports whether the cert's error-recovered list
// releases the tuple for the terminal state the cert is in now. The list is
// written while the cert carries the matching cert-processed stamp; a stale
// stamp means CRE reopened and re-finished the cert since, so its rows are a
// new failure and the old release does not apply to them.
func (r *Reconciler) releasedForCurrentState(cert *nvcrev1alpha1.Certification, tupleKeyStr string) bool {
	if !r.certAnnotator.IsRecovered(cert, tupleKeyStr) {
		return false
	}

	terminalTime, err := getCompletionTime(cert)
	if err != nil {
		return false
	}

	return r.certAnnotator.IsProcessed(cert, terminalTime)
}

func (r *Reconciler) buildDesired(ctx context.Context,
	certs []nvcrev1alpha1.Certification) (map[TupleKey]NodeCertFailure, error) {
	desired := make(map[TupleKey]NodeCertFailure)
	seen := make(map[types.NamespacedName]struct{})

	for i := range certs {
		cert := &certs[i]
		certRef := CertRef{Name: cert.Name, Namespace: cert.Namespace}

		for _, cat := range cert.Status.CategoryStatuses {
			switch cat.Status {
			case nvcrev1alpha1.CertificationFailed:
				if err := r.handleCategoryFailure(ctx, cert, desired, cat, certRef, seen); err != nil {
					return nil, fmt.Errorf("failed to handle category failure: %w", err)
				}
			case nvcrev1alpha1.CertificationSucceeded:
				if err := r.handleCategorySuccess(ctx, desired, cat, cert.Namespace, seen); err != nil {
					return nil, fmt.Errorf("failed to handle category success: %w", err)
				}
			}
		}
	}

	// Drop memo entries for ConfigMaps no terminal cert references any more.
	for key := range r.results {
		if _, ok := seen[key]; !ok {
			delete(r.results, key)
		}
	}

	return desired, nil
}

// getResultConfigMap fetches a result ConfigMap directly from the API server.
// NotFound is reported as (nil, nil) with a warning; other errors propagate.
func (r *Reconciler) getResultConfigMap(ctx context.Context, key types.NamespacedName) (*corev1.ConfigMap, error) {
	cm := &corev1.ConfigMap{}
	if err := r.client.Get(ctx, key, cm); err != nil {
		if apierrors.IsNotFound(err) {
			slog.Warn("Result ConfigMap not found, treating category as having no entries", "configMap", key.String())

			return nil, nil
		}

		return nil, fmt.Errorf("failed to get result ConfigMap %s: %w", key.String(), err)
	}

	return cm, nil
}

// getFailedRows returns the decoded failed-nodes list for a result ConfigMap,
// reusing the previous sweep's decode when the ConfigMap has not changed.
// A missing ConfigMap yields (nil, nil) so the category contributes nothing.
// API errors and an undecodable ConfigMap are returned so the sweep aborts:
// NVCRE writes the ConfigMap in a single update, so unreadable content is
// corruption rather than a transient state, and treating it as empty would
// heal every hold the category asserts.
func (r *Reconciler) getFailedRows(
	ctx context.Context, key types.NamespacedName, seen map[types.NamespacedName]struct{},
) ([]nvcrev1alpha1.FailedNode, error) {
	cm, err := r.getResultConfigMap(ctx, key)
	if err != nil || cm == nil {
		return nil, err
	}

	seen[key] = struct{}{}

	if cached, ok := r.results[key]; ok && cached.resourceVersion == cm.ResourceVersion && cached.failed != nil {
		return cached.failed, nil
	}

	rows, err := noderesults.DecodeFailedNodesFromConfigMap(cm)
	if err != nil {
		return nil, fmt.Errorf("failed to decode failed-nodes ConfigMap %s: %w", key.String(), err)
	}

	r.results[key] = decodedResult{resourceVersion: cm.ResourceVersion, failed: rows}

	return rows, nil
}

// getSucceededNodes is the succeeded-nodes counterpart of getFailedRows.
// Unlike getFailedRows, an undecodable ConfigMap contributes nothing instead
// of aborting the sweep: a pass that cannot be read recovers nothing, which
// errs toward keeping holds in place.
func (r *Reconciler) getSucceededNodes(
	ctx context.Context, key types.NamespacedName, seen map[types.NamespacedName]struct{},
) ([]string, error) {
	cm, err := r.getResultConfigMap(ctx, key)
	if err != nil || cm == nil {
		return nil, err
	}

	seen[key] = struct{}{}

	if cached, ok := r.results[key]; ok && cached.resourceVersion == cm.ResourceVersion && cached.succeeded != nil {
		return cached.succeeded, nil
	}

	nodes, err := decodeSucceededNodesFromConfigMap(cm)
	if err != nil {
		slog.Warn("Unreadable succeeded-nodes ConfigMap, treating category as having no entries",
			"configMap", key.String(), "error", err)

		return nil, nil
	}

	r.results[key] = decodedResult{resourceVersion: cm.ResourceVersion, succeeded: nodes}

	return nodes, nil
}

func (r *Reconciler) getObserved(ctx context.Context) (map[string]map[string]struct{}, error) {
	nodeList := &corev1.NodeList{}
	if err := r.client.List(ctx, nodeList, client.HasLabels{state.LabelKey}); err != nil {
		return nil, fmt.Errorf("failed to list annotated nodes: %w", err)
	}

	observed := make(map[string]map[string]struct{})

	for i := range nodeList.Items {
		node := &nodeList.Items[i]

		keys, err := r.annotator.ParseAnnotation(node)
		if err != nil {
			// The annotation is present but is not a JSON string array. The
			// node is read as holding nothing and the value is left for the
			// operator to repair; a failure that persists is re-asserted by
			// the next certification run.
			slog.Warn("Skipping node, cert-failures annotation is malformed",
				"node", node.Name, "error", err)

			continue
		}

		if len(keys) > 0 {
			observed[node.Name] = keys
		}
	}

	return observed, nil
}

func (r *Reconciler) handleCategorySuccess(
	ctx context.Context,
	desired map[TupleKey]NodeCertFailure,
	cat nvcrev1alpha1.CertificationCategoryStatus,
	certNamespace string,
	seen map[types.NamespacedName]struct{},
) error {
	cmName := getSucceededNodesRef(cat)
	if cmName == "" {
		return nil
	}

	nodes, err := r.getSucceededNodes(ctx, types.NamespacedName{Namespace: certNamespace, Name: cmName}, seen)
	if err != nil {
		return err
	}

	for _, nodeName := range nodes {
		for key := range desired {
			if key.Node == nodeName && key.Variant == cat.Variant {
				delete(desired, key)
			}
		}
	}

	return nil
}

func (r *Reconciler) handleCategoryFailure(
	ctx context.Context,
	cert *nvcrev1alpha1.Certification,
	desired map[TupleKey]NodeCertFailure,
	cat nvcrev1alpha1.CertificationCategoryStatus,
	certRef CertRef,
	seen map[types.NamespacedName]struct{},
) error {
	cmName := getFailedNodesRef(cat)
	if cmName == "" {
		return nil
	}

	rows, err := r.getFailedRows(ctx, types.NamespacedName{Namespace: certRef.Namespace, Name: cmName}, seen)
	if err != nil {
		return err
	}

	for _, row := range rows {
		if ok, _ := r.evaluator.Matches(config.EvalContext{
			FailedNode: map[string]string{
				"name":    row.Name,
				"reason":  string(row.Reason),
				"message": row.Message,
			},
			Category: map[string]string{
				"domain":  cat.Domain,
				"variant": cat.Variant,
			},
		}); !ok {
			continue
		}

		key := TupleKey{Node: row.Name, Variant: cat.Variant, Reason: string(row.Reason)}
		tupleKeyStr := key.Node + "#" + key.ErrorCode()

		if existing, exists := desired[key]; exists {
			existing.CertRefs = append(existing.CertRefs, certRef)
			desired[key] = existing

			continue
		}

		if r.releasedForCurrentState(cert, tupleKeyStr) {
			continue
		}

		desired[key] = NodeCertFailure{
			Message:  row.Message,
			CertRefs: []CertRef{certRef},
		}
	}

	return nil
}

// markObservedOwners stamps every cert that asserts a tuple already present on
// the node but that has not been stamped itself. Such a cert completed while
// the tuple was already annotated (a rerun over a still-failing node), so it
// never published anything and would otherwise stay unstamped for good. Left
// unstamped, it re-publishes the failure the sweep after an operator clears the
// annotation, because handleDesiredNotObserved reads "unstamped" as "new".
func (r *Reconciler) markObservedOwners(
	ctx context.Context,
	entry NodeCertFailure,
	certTimes map[CertRef]time.Time,
	certsToMark map[CertRef]time.Time,
) error {
	for _, ref := range entry.CertRefs {
		terminalTime, ok := certTimes[ref]
		if !ok {
			continue
		}

		if _, queued := certsToMark[ref]; queued {
			continue
		}

		cert := &nvcrev1alpha1.Certification{}
		if err := r.client.Get(ctx, client.ObjectKey{Name: ref.Name, Namespace: ref.Namespace}, cert); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}

			return fmt.Errorf("failed to get cert %s/%s: %w", ref.Namespace, ref.Name, err)
		}

		if !r.certAnnotator.IsProcessed(cert, terminalTime) {
			slog.Info("Stamping cert whose failure is already annotated",
				"cert", ref.Name, "namespace", ref.Namespace)

			certsToMark[ref] = terminalTime
		}
	}

	return nil
}

func (r *Reconciler) handleDesiredNotObserved(
	ctx context.Context,
	key TupleKey,
	entry NodeCertFailure,
	certTimes map[CertRef]time.Time,
	certsToMark map[CertRef]time.Time,
) error {
	owner := entry.CertRefs[0]

	terminalTime, ok := certTimes[owner]
	if !ok {
		return fmt.Errorf("cert %s/%s has no usable terminal time", owner.Namespace, owner.Name)
	}

	cert := &nvcrev1alpha1.Certification{}
	if err := r.client.Get(ctx, client.ObjectKey{Name: owner.Name, Namespace: owner.Namespace}, cert); err != nil {
		return fmt.Errorf("failed to get cert %s/%s: %w", owner.Namespace, owner.Name, err)
	}

	if !r.certAnnotator.IsProcessed(cert, terminalTime) {
		return r.handleNewFailure(ctx, key, entry, certTimes, certsToMark)
	}

	return r.handleOperatorRecovery(ctx, key, entry)
}

// handleNewFailure publishes unhealthy for a tuple whose owner cert has not
// been published for yet and queues every owner for the cert-processed stamp.
func (r *Reconciler) handleNewFailure(
	ctx context.Context,
	key TupleKey,
	entry NodeCertFailure,
	certTimes map[CertRef]time.Time,
	certsToMark map[CertRef]time.Time,
) error {
	owner := entry.CertRefs[0]

	// A result row can name a node that no longer exists (replaced before
	// the first sweep, or a cert kept as a record long after the node left).
	// Publishing for it would only produce failed condition updates
	// downstream, so leave the tuple unpublished and re-check next sweep.
	node := &corev1.Node{}
	if err := r.client.Get(ctx, client.ObjectKey{Name: key.Node}, node); err != nil {
		if apierrors.IsNotFound(err) {
			slog.Warn("Skipping certification failure for a node that does not exist",
				"node", key.Node, "variant", key.Variant, "reason", key.Reason,
				"ownerCert", owner.Name)

			return nil
		}

		return fmt.Errorf("failed to get node %s: %w", key.Node, err)
	}

	slog.Info("New failure, publishing unhealthy",
		"node", key.Node, "variant", key.Variant, "reason", key.Reason,
		"ownerCert", owner.Name)

	if err := r.publishNewFailure(ctx, key, entry); err != nil {
		if !errors.Is(err, state.ErrMalformedAnnotation) {
			slog.Error("Failed to publish new failure", "error", err)
		}

		return fmt.Errorf("failed to publish new failure: %w", err)
	}

	for _, ref := range entry.CertRefs {
		if t, ok := certTimes[ref]; ok {
			certsToMark[ref] = t
		}
	}

	return nil
}

// handleOperatorRecovery publishes healthy for a tuple the monitor published
// earlier and that an operator has since cleared from the node, and records
// the tuple on every owner cert so it is not asserted again.
func (r *Reconciler) handleOperatorRecovery(ctx context.Context, key TupleKey, entry NodeCertFailure) error {
	owner := entry.CertRefs[0]

	slog.Info("Operator removed annotation, publishing healthy",
		"node", key.Node, "variant", key.Variant, "reason", key.Reason,
		"ownerCert", owner.Name)

	errorCode := key.ErrorCode()
	if err := r.publisher.PublishHealthEvent(ctx, key.Node, true, "", errorCode); err != nil {
		slog.Error("Failed to publish healthy event", "error", err, "node", key.Node, "errorCode", errorCode)

		return fmt.Errorf("failed to publish healthy event for node %s: %w", key.Node, err)
	}

	tupleKeyStr := key.Node + "#" + errorCode
	for _, ref := range entry.CertRefs {
		if err := r.certAnnotator.AddRecovered(ctx, ref.Name, ref.Namespace, tupleKeyStr); err != nil {
			slog.Error("Failed to write error-recovered on cert", "error", err, "cert", ref.Name, "namespace", ref.Namespace)
		}
	}

	return nil
}

func (r *Reconciler) handleObservedNotDesired(ctx context.Context, nodeName, key string) error {
	slog.Info("Publishing healthy recovery event",
		"node", nodeName, "errorCode", key)

	if err := r.publisher.PublishHealthEvent(ctx, nodeName, true, "", key); err != nil {
		slog.Error("Failed to publish healthy event", "error", err, "node", nodeName, "errorCode", key)

		return fmt.Errorf("failed to publish healthy event: %w", err)
	}

	if err := r.annotator.RemoveTuple(ctx, nodeName, key); err != nil {
		return fmt.Errorf("failed to remove annotation tuple for node %s: %w", nodeName, err)
	}

	return nil
}

func (r *Reconciler) publishNewFailure(ctx context.Context, key TupleKey, entry NodeCertFailure) error {
	errorCode := key.ErrorCode()

	slog.Info("Publishing unhealthy certification event",
		"node", key.Node, "variant", key.Variant, "reason", key.Reason, "errorCode", errorCode)

	if err := r.publisher.PublishHealthEvent(
		ctx, key.Node, false, entry.Message, errorCode,
	); err != nil {
		slog.Error("Failed to publish unhealthy event", "error", err, "node", key.Node, "errorCode", errorCode)
		return fmt.Errorf("failed to publish unhealthy event: %w", err)
	}

	if err := r.annotator.AddTuple(ctx, key.Node, errorCode); err != nil {
		return fmt.Errorf("failed to add annotation tuple for node %s: %w", key.Node, err)
	}

	return nil
}

func (r *Reconciler) markCertsProcessed(ctx context.Context, certsToMark map[CertRef]time.Time) error {
	for ref, terminalTime := range certsToMark {
		if err := r.certAnnotator.SetProcessed(ctx, ref.Name, ref.Namespace, terminalTime); err != nil {
			slog.Error("Failed to mark cert as processed", "error", err, "cert", ref.Name, "namespace", ref.Namespace)
		}
	}

	return nil
}
