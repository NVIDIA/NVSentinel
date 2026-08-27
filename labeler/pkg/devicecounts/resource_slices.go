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
