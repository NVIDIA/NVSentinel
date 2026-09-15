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

package metadata

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/hashicorp/golang-lru/v2/expirable"
	"go.opentelemetry.io/otel/attribute"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/singleflight"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/nvidia/nvsentinel/commons/pkg/tracing"
	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
)

// NodeMetadata holds cached node information fetched from the Kubernetes API.
type NodeMetadata struct {
	ProviderID  string
	Labels      map[string]string
	SkipMatched bool
}

type Augmentor struct {
	config        *Config
	clientset     kubernetes.Interface
	cache         *expirable.LRU[string, *NodeMetadata]
	fetches       singleflight.Group
	lookupTimeout time.Duration
}

func New(ctx context.Context, config *Config, clientset kubernetes.Interface) (*Augmentor, error) {
	if err := config.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	cache := expirable.NewLRU[string, *NodeMetadata](
		config.CacheSize,
		nil,
		config.CacheTTL,
	)

	lookupTimeout := config.LookupTimeout
	if lookupTimeout == 0 {
		lookupTimeout = DefaultLookupTimeout
	}

	slog.InfoContext(ctx, "Metadata augmentor initialized",
		"cacheSize", config.CacheSize,
		"cacheTTL", config.CacheTTL,
		"lookupTimeout", lookupTimeout,
		"allowedLabels", config.AllowedLabels,
		"skipNodeLabel", config.SkipNodeLabel)

	return &Augmentor{
		config:        config,
		clientset:     clientset,
		cache:         cache,
		lookupTimeout: lookupTimeout,
	}, nil
}

func (a *Augmentor) Transform(ctx context.Context, event *pb.HealthEvent) error {
	if event.NodeName == "" {
		return fmt.Errorf("event has empty node name")
	}

	ctx, span := tracing.StartSpan(ctx, "platform_connector.transformer.metadata")
	defer span.End()

	metadata, err := a.getOrFetchMetadata(ctx, event.NodeName)
	if err != nil {
		// Fail-open: when the Kubernetes lookup fails the event proceeds with
		// its original ProcessingStrategy. This is intentional -- a transient
		// API error should not silently drop a legitimate health event. The
		// fail-open window lasts for the duration of the lookup failure, not
		// just the cache TTL. A Prometheus counter tracks these occurrences.
		metadataLookupFailureCounter.Inc()
		tracing.RecordError(span, err)
		span.SetAttributes(
			attribute.String("platform_connector.transformer.metadata.error.type", "failed_to_get_metadata"),
			attribute.String("platform_connector.transformer.metadata.error.message", err.Error()),
		)

		slog.WarnContext(ctx, "Metadata lookup failed, proceeding ungated (fail-open)",
			"node", event.NodeName,
			"error", err)

		return nil
	}

	if event.Metadata == nil {
		event.Metadata = make(map[string]string)
	}

	var labelsAdded int

	if metadata.ProviderID != "" {
		event.Metadata["providerID"] = metadata.ProviderID
	}

	for _, labelKey := range a.config.AllowedLabels {
		if labelValue, exists := metadata.Labels[labelKey]; exists {
			event.Metadata[labelKey] = labelValue
			labelsAdded++
		}
	}

	if metadata.SkipMatched {
		event.ProcessingStrategy = pb.ProcessingStrategy_STORE_ONLY

		skipLabelGatedCounter.Inc()
		span.SetAttributes(
			attribute.Bool("metadata.skip_label_matched", true),
		)
		slog.InfoContext(ctx, "Event gated to STORE_ONLY by managed-label check",
			"node", event.NodeName,
			"skipNodeLabel", a.config.SkipNodeLabel)
	}

	span.SetAttributes(
		attribute.Int("metadata.labels_added", labelsAdded),
	)

	// Per-event, so debug only: at info this is one log line per fleet event
	// now that the pipeline also runs centrally.
	slog.DebugContext(ctx, "Metadata augmented",
		"node", event.NodeName,
		"providerID", metadata.ProviderID,
		"labelsAdded", labelsAdded)

	return nil
}

func (a *Augmentor) Name() string {
	return "MetadataAugmentor"
}

// maxPrewarmReads bounds how many node reads one batch runs at once. The
// client's rate limiter and the lookup timeout already cap the API calls; the
// bound keeps a batch naming thousands of nodes from starting as many
// goroutines and connections at once.
const maxPrewarmReads = 64

// ErrBudgetExhausted reports that a batch's budget ended before the metadata
// of every node it names was read, so the managed-label gate could not be
// evaluated for the rest. The caller defers the batch; the reads that were
// started finish detached and warm the cache for the resend.
var ErrBudgetExhausted = errors.New("node metadata not read within the batch budget")

// Prewarm reads the metadata of every distinct uncached node in the batch
// concurrently, up to maxPrewarmReads at a time, so the per-event pass finds
// it cached. The events of a batch are processed one after another inside one
// budget; without this, a cross-node batch naming many uncached nodes would
// spend the budget on the first few. A read that fails inside the budget is
// left to the per-event pass, which reports it and fails open; the nodes the
// budget ended before reading are reported as ErrBudgetExhausted instead, so
// the batch is deferred rather than passing the skip-label gate unread.
func (a *Augmentor) Prewarm(ctx context.Context, events []*pb.HealthEvent) error {
	var (
		group  errgroup.Group
		unread atomic.Int32
	)

	group.SetLimit(maxPrewarmReads)

	started := map[string]bool{}

	for _, event := range events {
		name := event.GetNodeName()
		if name == "" || started[name] {
			continue
		}

		started[name] = true

		if _, found := a.cache.Get(name); found {
			continue
		}

		group.Go(func() error {
			// A read that ended without metadata once the budget was spent
			// was cut short by it, whether it never started or was running.
			if _, err := a.getOrFetchMetadata(ctx, name); err != nil && ctx.Err() != nil {
				unread.Add(1)
			}

			return nil
		})
	}

	_ = group.Wait()

	if n := unread.Load(); n > 0 {
		return fmt.Errorf("%w: %d of %d node(s)", ErrBudgetExhausted, n, len(started))
	}

	return nil
}

// getOrFetchMetadata serves a node's metadata from the cache and reads it
// from the API on a miss. Concurrent misses for the same node share one read;
// misses for different nodes proceed independently (across requests, and
// within a batch through Prewarm). The shared read does not die with the
// caller that happened to start it: it runs detached from that caller's
// cancellation, bounded by the lookup timeout, and each waiter leaves on its
// own context instead.
func (a *Augmentor) getOrFetchMetadata(ctx context.Context, nodeName string) (*NodeMetadata, error) {
	if metadata, found := a.cache.Get(nodeName); found {
		return metadata, nil
	}

	// A caller whose budget is already spent must not start a read it will
	// not wait for: a large batch past its budget would otherwise fan out one
	// detached read per remaining node.
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	results := a.fetches.DoChan(nodeName, func() (any, error) {
		if metadata, found := a.cache.Get(nodeName); found {
			return metadata, nil
		}

		metadata, err := a.fetchNodeMetadata(context.WithoutCancel(ctx), nodeName)
		if err != nil {
			return nil, err
		}

		a.cache.Add(nodeName, metadata)

		return metadata, nil
	})

	select {
	case result := <-results:
		if result.Err != nil {
			return nil, result.Err
		}

		metadata, ok := result.Val.(*NodeMetadata)
		if !ok {
			return nil, fmt.Errorf("unexpected metadata type %T", result.Val)
		}

		return metadata, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// fetchNodeMetadata reads one node, bounded by the lookup timeout so a
// stalled API server cannot hold the event (and its acknowledgement) longer
// than that; the caller fails open on the timeout.
func (a *Augmentor) fetchNodeMetadata(ctx context.Context, nodeName string) (*NodeMetadata, error) {
	ctx, cancel := context.WithTimeout(ctx, a.lookupTimeout)
	defer cancel()

	node, err := a.clientset.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get node from API: %w", err)
	}

	metadata := &NodeMetadata{
		ProviderID: node.Spec.ProviderID,
		Labels:     make(map[string]string),
	}

	for _, labelKey := range a.config.AllowedLabels {
		if labelValue, exists := node.Labels[labelKey]; exists {
			metadata.Labels[labelKey] = labelValue
		}
	}

	if a.config.skipLabelKey != "" {
		if val, ok := node.Labels[a.config.skipLabelKey]; ok && val == a.config.skipLabelValue {
			metadata.SkipMatched = true
		}
	}

	return metadata, nil
}
