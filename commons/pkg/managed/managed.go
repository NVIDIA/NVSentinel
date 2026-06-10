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

// Package managed centralises the nvsentinel.dgxc.nvidia.com/managed
// Node label that ADR-040 uses as the single opt-out signal for external
// remediation.
//
// The label has cluster-wide semantics:
//
//   - absent or any value other than "false" -> NVSentinel manages the node
//     normally (the common case).
//   - "false" -> an external system owns this node. NVSentinel must
//     stop reconciling against it: the ExtRR reconciler keeps off, node-labeler
//     strips its detection labels (causing DaemonSet monitors to evict via
//     their nodeSelectors), and cluster-scope monitors skip emission for
//     events targeting the node.
//
// Three places consume this constant:
//
//   - janitor's ExtRR reconciler writes the label as part of its apply path
//     and removes it during cleanup.
//   - labeler reads it to gate its detection-label stamping (JSC-89).
//   - cluster-scope monitors (csp-health-monitor, kubernetes-object-monitor,
//     slurm-drain-monitor) read it via IsNodeOptedOut before emitting events
//     (JSC-90).
//
// The literal string MUST NOT appear in NVSentinel Go code anywhere else.
package managed

import (
	"context"
	"log/slog"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	listersv1 "k8s.io/client-go/listers/core/v1"
)

const (
	// ManagedLabelKey is the Node label key.
	ManagedLabelKey = "nvsentinel.dgxc.nvidia.com/managed"

	// ManagedLabelValueFalse is the only value that indicates opt-out. Any
	// other value (including absent, "true", or a typo) means NVSentinel
	// manages the node normally.
	ManagedLabelValueFalse = "false"
)

// IsNodeOptedOut reports whether the named Node carries
// nvsentinel.dgxc.nvidia.com/managed="false". It reads from the supplied
// informer-backed NodeLister so it can be invoked on the emission hot path
// without round-tripping to the apiserver.
//
// Returns false when:
//   - the label is absent, or set to any value other than "false";
//   - the Node is not in the cache (defensive: a stale or empty cache must
//     never silence a monitor for a node we don't know about);
//   - the lookup fails for any other reason (logged at ERROR for visibility).
//
// The non-error fail-open is deliberate: ADR-040 calls out that the safe
// default is "keep observing the node." A lookup failure should produce a
// log line for operator attention but not change behaviour.
func IsNodeOptedOut(ctx context.Context, nodeLister listersv1.NodeLister, nodeName string) bool {
	if nodeLister == nil || nodeName == "" {
		return false
	}

	node, err := nodeLister.Get(nodeName)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// Stale or empty cache for this node — fail open. No log spam: this
			// is expected during informer warmup and after node deletion.
			return false
		}

		slog.ErrorContext(ctx, "managed-label lookup failed; falling back to managed (fail-open)",
			"node", nodeName, "error", err)

		return false
	}

	return node.Labels[ManagedLabelKey] == ManagedLabelValueFalse
}
