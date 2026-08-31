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

package kubeclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sync"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	"k8s.io/apimachinery/pkg/util/wait"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/util/retry"
)

// NodePatcher applies Node mutations with cache-aware retries.
//
// The zero value is ready to use.
type NodePatcher struct {
	pendingVersions sync.Map // map[string]string
}

// Patch applies mutate to a copy of cached and writes only the resulting diff.
// cached may be nil when an informer is unavailable; in that case Patch reads the
// current Node from the API server. A write not yet observed at cached's
// ResourceVersion is also refreshed before the next mutation.
func (p *NodePatcher) Patch(
	ctx context.Context,
	nodes typedcorev1.NodeInterface,
	nodeName string,
	cached *v1.Node,
	mutate func(*v1.Node) error,
) (bool, error) {
	var current *v1.Node

	err := retryNodePatch(func() error {
		var err error

		current, err = p.currentNode(ctx, nodes, nodeName, cached)

		return err
	})
	if err != nil {
		return false, err
	}

	changed := false

	err = retryNodePatch(func() error {
		desired := current.DeepCopy()

		if err := mutate(desired); err != nil {
			return fmt.Errorf("mutate node %q: %w", nodeName, err)
		}

		patch, err := NodeMergePatch(current, desired)
		if err != nil {
			return fmt.Errorf("build merge patch for node %q: %w", nodeName, err)
		}

		if patch == nil {
			return nil
		}

		updated, err := nodes.Patch(ctx, nodeName, types.MergePatchType, patch, metav1.PatchOptions{})
		if err == nil {
			p.pendingVersions.Store(nodeName, updated.ResourceVersion)

			changed = true

			return nil
		}

		if errors.IsConflict(err) {
			patchErr := err

			refreshed, err := nodes.Get(ctx, nodeName, metav1.GetOptions{})
			if err != nil {
				return fmt.Errorf("refresh node %q after patch conflict: %w", nodeName, err)
			}

			current = refreshed

			return fmt.Errorf("patch node %q: %w", nodeName, patchErr)
		}

		return fmt.Errorf("patch node %q: %w", nodeName, err)
	})
	if err != nil {
		return false, err
	}

	return changed, nil
}

func (p *NodePatcher) currentNode(
	ctx context.Context,
	nodes typedcorev1.NodeInterface,
	nodeName string,
	cached *v1.Node,
) (*v1.Node, error) {
	writtenVersionValue, hasPendingWrite := p.pendingVersions.Load(nodeName)
	if !hasPendingWrite {
		if cached != nil {
			return cached, nil
		}

		current, err := nodes.Get(ctx, nodeName, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("get node %q from API server: %w", nodeName, err)
		}

		return current, nil
	}

	writtenVersion, _ := writtenVersionValue.(string)
	if cached != nil && writtenVersion != "" && cached.ResourceVersion == writtenVersion {
		p.pendingVersions.CompareAndDelete(nodeName, writtenVersionValue)

		return cached, nil
	}

	current, err := nodes.Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("refresh node %q while pending write is not in cache: %w", nodeName, err)
	}

	return current, nil
}

// retryNodePatch retries fn until it succeeds, exhausts the backoff, or fails
// for a non-retryable reason.
//
// retry.OnError replaces a context cancellation or timeout with the last
// retryable error, which is nil when the attempt failed for a non-retryable
// reason. Reporting fn's own error keeps a cancelled attempt from looking like
// a completed one, so callers never continue with an unwritten result.
func retryNodePatch(fn func() error) error {
	var lastErr error

	err := retry.OnError(nodePatchBackoff(), isRetryableNodePatchError, func() error {
		lastErr = fn()

		return lastErr
	})

	if err == nil && lastErr != nil {
		return lastErr
	}

	return err
}

func nodePatchBackoff() wait.Backoff {
	return wait.Backoff{
		Steps:    10,
		Duration: 20 * time.Millisecond,
		Factor:   2,
		Jitter:   0.1,
	}
}

func isRetryableNodePatchError(err error) bool {
	return errors.IsConflict(err) ||
		errors.IsServerTimeout(err) ||
		errors.IsTooManyRequests(err) ||
		errors.IsTimeout(err) ||
		errors.IsServiceUnavailable(err)
}

// NodeMergePatch builds an RFC 7386 JSON merge patch carrying differences in labels,
// annotations, taints, and unschedulable state. It returns a nil patch when the two
// nodes already agree, so callers can skip a no-op write.
//
// CreateTwoWayMergePatch compares projections containing only the fields this helper
// supports. Excluding every other field from both inputs ensures an informer
// projection cannot patch its gaps back over the live object.
//
// Taints are emitted only when the caller changed them. A projected Node whose Spec
// is empty on both sides therefore cannot erase taints from the real object.
func NodeMergePatch(original, modified *v1.Node) ([]byte, error) {
	originalProjection := projectNodePatchableFields(original)
	modifiedProjection := projectNodePatchableFields(modified)

	specChanged := !reflect.DeepEqual(original.Spec.Taints, modified.Spec.Taints) ||
		original.Spec.Unschedulable != modified.Spec.Unschedulable
	if specChanged {
		// Lists in spec, such as taints, are replaced wholesale. ResourceVersion
		// prevents a stale list from overwriting a concurrent update.
		modifiedProjection.ResourceVersion = original.ResourceVersion
	}

	originalJSON, err := json.Marshal(originalProjection)
	if err != nil {
		return nil, fmt.Errorf("marshal original patch projection for node %q: %w", original.Name, err)
	}

	modifiedJSON, err := json.Marshal(modifiedProjection)
	if err != nil {
		return nil, fmt.Errorf("marshal modified patch projection for node %q: %w", original.Name, err)
	}

	patch, err := strategicpatch.CreateTwoWayMergePatch(originalJSON, modifiedJSON, v1.Node{})
	if err != nil {
		return nil, fmt.Errorf("build strategic merge patch for node %q: %w", original.Name, err)
	}

	if bytes.Equal(patch, []byte("{}")) {
		return nil, nil
	}

	return patch, nil
}

// projectNodePatchableFields restricts patch generation to the Node fields this
// helper intentionally supports, preventing callbacks from patching unrelated fields.
func projectNodePatchableFields(node *v1.Node) *v1.Node {
	return &v1.Node{
		Labels:      node.Labels,
		Annotations: node.Annotations,
		Spec: v1.NodeSpec{
			Taints:        node.Spec.Taints,
			Unschedulable: node.Spec.Unschedulable,
		},
	}
}
