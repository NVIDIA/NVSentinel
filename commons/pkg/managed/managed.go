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
//   - labeler reads it to gate its detection-label stamping.
//   - cluster-scope monitors (csp-health-monitor, kubernetes-object-monitor,
//     slurm-drain-monitor) read it via IsNodeOptedOut before emitting events.
//
// The literal string MUST NOT appear in NVSentinel Go code anywhere else.
package managed

import (
	"context"
	"fmt"

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
// Returns (true, nil) when the label is "false" and the node is in the cache.
// Returns (false, nil) when:
//   - the label is absent or set to any value other than "false";
//   - the Node is not in the cache (cache miss is benign — informer warmup
//     or post-deletion lookup);
//   - the lister or nodeName argument is empty.
//
// Returns (false, err) for any other lister error. Callers MUST decide their
// own failure policy: a reconciler should requeue (controller-runtime handles
// backoff), but an event publisher that cannot retry should fail closed (drop
// the event) — fail-open here would re-emit events for a node that may be
// under active external remediation, which is exactly the contract this label
// is meant to enforce.
func IsNodeOptedOut(ctx context.Context, nodeLister listersv1.NodeLister, nodeName string) (bool, error) {
	if nodeLister == nil || nodeName == "" {
		return false, nil
	}

	node, err := nodeLister.Get(nodeName)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// Stale or empty cache for this node — treat as not-opted-out. This
			// is expected during informer warmup and after node deletion.
			return false, nil
		}

		return false, fmt.Errorf("looking up managed label for node %q: %w", nodeName, err)
	}

	return node.Labels[ManagedLabelKey] == ManagedLabelValueFalse, nil
}
