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
	current, err := p.currentNode(ctx, nodes, nodeName, cached)
	if err != nil {
		return false, err
	}

	changed := false

	err = retry.OnError(nodePatchBackoff(), isRetryableNodePatchError, func() error {
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

			current, err = nodes.Get(ctx, nodeName, metav1.GetOptions{})
			if err != nil {
				return fmt.Errorf("refresh node %q after patch conflict: %w", nodeName, err)
			}

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

	p.pendingVersions.CompareAndDelete(nodeName, writtenVersionValue)

	return current, nil
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

// NodeMergePatch builds an RFC 7386 JSON merge patch carrying the label and
// annotation differences between original and modified. It returns a nil patch when
// the two already agree, so callers can skip the write instead of spending an API
// call on a no-op.
//
// CreateTwoWayMergePatch compares metadata-only projections of the two Nodes.
// Excluding every other field from both inputs ensures an informer projection cannot
// patch its gaps back over the live object.
//
// Spec fields such as taints and unschedulable are deliberately out of scope: a merge
// patch replaces a list wholesale, so patching taints from a projected Node whose Spec
// had been cleared would silently drop every taint on the real object.
func NodeMergePatch(original, modified *v1.Node) ([]byte, error) {
	originalMetadata := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Labels:      original.Labels,
			Annotations: original.Annotations,
		},
	}
	modifiedMetadata := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Labels:      modified.Labels,
			Annotations: modified.Annotations,
		},
	}

	originalJSON, err := json.Marshal(originalMetadata)
	if err != nil {
		return nil, fmt.Errorf("marshal original metadata for node %q: %w", original.Name, err)
	}

	modifiedJSON, err := json.Marshal(modifiedMetadata)
	if err != nil {
		return nil, fmt.Errorf("marshal modified metadata for node %q: %w", original.Name, err)
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
