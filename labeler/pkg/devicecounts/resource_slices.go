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
	"maps"
	"slices"

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

// driverPoolSlices holds a driver's ResourceSlices for one node, limited to
// the newest generation of each pool.
type driverPoolSlices struct {
	slices []*resourcev1.ResourceSlice
	// complete is false while any pool's newest generation has fewer slices
	// than its resourceSliceCount, which happens while a driver republishes.
	complete bool
}

type poolGeneration struct {
	generation int64
	sliceCount int64
	slices     []*resourcev1.ResourceSlice
}

// currentPoolSlices returns the driver's current pool slices for the node. It
// reports false when the driver publishes no slice for the node.
func currentPoolSlices(resourceSlices []*resourcev1.ResourceSlice, driver string) (driverPoolSlices, bool) {
	pools := map[string]*poolGeneration{}

	for _, resourceSlice := range resourceSlices {
		if resourceSlice == nil || resourceSlice.Spec.Driver != driver {
			continue
		}

		pool := resourceSlice.Spec.Pool
		existing, ok := pools[pool.Name]

		switch {
		case !ok || pool.Generation > existing.generation:
			pools[pool.Name] = &poolGeneration{
				generation: pool.Generation,
				sliceCount: pool.ResourceSliceCount,
				slices:     []*resourcev1.ResourceSlice{resourceSlice},
			}
		case pool.Generation == existing.generation:
			existing.slices = append(existing.slices, resourceSlice)
		}
	}

	if len(pools) == 0 {
		return driverPoolSlices{}, false
	}

	current := driverPoolSlices{complete: true}

	for _, poolName := range slices.Sorted(maps.Keys(pools)) {
		pool := pools[poolName]
		if int64(len(pool.slices)) != pool.sliceCount {
			current.complete = false
		}

		current.slices = append(current.slices, pool.slices...)
	}

	return current, true
}

func resourceSliceNodeName(resourceSlice *resourcev1.ResourceSlice) (string, bool) {
	if resourceSlice == nil || resourceSlice.Spec.NodeName == nil || *resourceSlice.Spec.NodeName == "" {
		return "", false
	}

	return *resourceSlice.Spec.NodeName, true
}
