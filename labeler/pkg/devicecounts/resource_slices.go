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

package devicecounts

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/client-go/tools/cache"
)

// ResourceSliceNodeNameIndex names the informer index from spec.nodeName to its
// ResourceSlices. The index turns a node lookup from a scan of all N*K cached
// slices into a lookup over only that node's K slices.
const ResourceSliceNodeNameIndex = "nodeResourceSlice"

// ResourceSliceNodeNameIndexFunc indexes node-local ResourceSlices by spec.nodeName.
func ResourceSliceNodeNameIndexFunc(obj any) ([]string, error) {
	resourceSlice, ok := obj.(*resourcev1.ResourceSlice)
	if !ok {
		return nil, fmt.Errorf("object is not a ResourceSlice")
	}

	nodeName, ok := resourceSliceNodeName(resourceSlice)
	if !ok {
		return nil, nil
	}

	return []string{nodeName}, nil
}

// ResourceSlicesForNode returns node-local ResourceSlices through the node-name
// informer index. ByIndex visits only matching slices instead of scanning the
// complete ResourceSlice store for every target or peer node.
func ResourceSlicesForNode(indexer cache.Indexer, node *corev1.Node) []*resourcev1.ResourceSlice {
	if indexer == nil || node == nil {
		return nil
	}

	objects, err := indexer.ByIndex(ResourceSliceNodeNameIndex, node.Name)
	if err != nil {
		return nil
	}

	resourceSlices := make([]*resourcev1.ResourceSlice, 0, len(objects))
	for _, obj := range objects {
		resourceSlice, ok := obj.(*resourcev1.ResourceSlice)
		if !ok {
			continue
		}

		resourceSlices = append(resourceSlices, resourceSlice)
	}

	return resourceSlices
}

func resourceSliceNodeName(resourceSlice *resourcev1.ResourceSlice) (string, bool) {
	if resourceSlice == nil || resourceSlice.Spec.NodeName == nil || *resourceSlice.Spec.NodeName == "" {
		return "", false
	}

	return *resourceSlice.Spec.NodeName, true
}

// poolKey identifies one ResourcePool: a driver may publish several pools and
// pool names are only unique within a driver.
type poolKey struct {
	driver string
	pool   string
}

// completePoolSlices applies the ResourceSlice consumer contract from the
// Kubernetes DRA API: within each driver/pool only the slices carrying the
// highest spec.pool.generation are current, and a pool is only usable once
// exactly spec.pool.resourceSliceCount slices of that generation are visible.
// During a driver rollout both generations can coexist in the informer, and a
// multi-slice pool is published one object at a time, so counting the raw
// slices would double-count or under-count devices.
//
// Stale generations are dropped. If any remaining pool is incomplete the
// function reports complete=false and callers must skip the update instead of
// labelling a partial inventory.
func completePoolSlices(resourceSlices []*resourcev1.ResourceSlice) (
	current []*resourcev1.ResourceSlice, complete bool) {
	latestGeneration := make(map[poolKey]int64)

	for _, resourceSlice := range resourceSlices {
		if resourceSlice == nil {
			continue
		}

		key := poolKey{driver: resourceSlice.Spec.Driver, pool: resourceSlice.Spec.Pool.Name}
		if generation, ok := latestGeneration[key]; !ok || resourceSlice.Spec.Pool.Generation > generation {
			latestGeneration[key] = resourceSlice.Spec.Pool.Generation
		}
	}

	observed := make(map[poolKey]int64)
	expected := make(map[poolKey]int64)
	current = make([]*resourcev1.ResourceSlice, 0, len(resourceSlices))

	for _, resourceSlice := range resourceSlices {
		if resourceSlice == nil {
			continue
		}

		key := poolKey{driver: resourceSlice.Spec.Driver, pool: resourceSlice.Spec.Pool.Name}
		if resourceSlice.Spec.Pool.Generation != latestGeneration[key] {
			continue // stale generation superseded by a newer publication
		}

		observed[key]++
		expected[key] = resourceSlice.Spec.Pool.ResourceSliceCount
		current = append(current, resourceSlice)
	}

	for key, want := range expected {
		if observed[key] != want {
			return current, false
		}
	}

	return current, true
}
