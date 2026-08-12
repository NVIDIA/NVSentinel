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

package labeler

import (
	"fmt"
	"log/slog"
	"maps"
	"reflect"
	"slices"

	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/client-go/tools/cache"
)

func (l *Labeler) newResourceSliceEventHandlers() cache.ResourceEventHandlerFuncs {
	return cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			resourceSlice, ok := resourceSliceFromEventObject(obj)
			if !ok {
				slog.Warn("Skipping ResourceSlice add event with unexpected object type",
					"type", fmt.Sprintf("%T", obj))

				return
			}

			l.handleResourceSliceEvent(resourceSlice)
		},
		UpdateFunc: func(oldObj, newObj any) {
			oldResourceSlice, oldOk := resourceSliceFromEventObject(oldObj)
			newResourceSlice, newOk := resourceSliceFromEventObject(newObj)

			if !oldOk || !newOk {
				slog.Warn("Skipping ResourceSlice update event with unexpected object type",
					"oldType", fmt.Sprintf("%T", oldObj), "newType", fmt.Sprintf("%T", newObj))

				return
			}

			// Device-count expressions only read ResourceSlice spec; ignore metadata-only churn.
			if reflect.DeepEqual(oldResourceSlice.Spec, newResourceSlice.Spec) {
				return
			}

			// Reconcile nodes matched by both old and new specs in case node selection changed.
			l.handleResourceSliceEvent(oldResourceSlice, newResourceSlice)
		},
		DeleteFunc: func(obj any) {
			resourceSlice, ok := resourceSliceFromEventObject(obj)
			if !ok {
				slog.Warn("Skipping ResourceSlice delete event with unexpected object type",
					"type", fmt.Sprintf("%T", obj))

				return
			}

			l.handleResourceSliceEvent(resourceSlice)
		},
	}
}

func (l *Labeler) handleResourceSliceEvent(resourceSlices ...*resourcev1.ResourceSlice) {
	if !l.allInformersSynced() {
		return
	}

	for _, nodeName := range nodeNamesForResourceSlices(resourceSlices...) {
		if err := l.updateNodeLabels(nodeName); err != nil {
			slog.Error("Failed to reconcile node labels after ResourceSlice event",
				"node", nodeName, "error", err)
		}
	}
}

func nodeNamesForResourceSlices(resourceSlices ...*resourcev1.ResourceSlice) []string {
	nodeNameSet := map[string]struct{}{}

	for _, resourceSlice := range resourceSlices {
		nodeName, ok := resourceSliceNodeName(resourceSlice)
		if !ok {
			continue
		}

		nodeNameSet[nodeName] = struct{}{}
	}

	return slices.Collect(maps.Keys(nodeNameSet))
}

func resourceSliceFromEventObject(obj any) (*resourcev1.ResourceSlice, bool) {
	resourceSlice, ok := obj.(*resourcev1.ResourceSlice)
	if ok {
		return resourceSlice, true
	}

	// Delete events can arrive as tombstones when the informer misses the final object state.
	tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
	if !ok {
		return nil, false
	}

	resourceSlice, ok = tombstone.Obj.(*resourcev1.ResourceSlice)

	return resourceSlice, ok
}

func resourceSliceNodeName(resourceSlice *resourcev1.ResourceSlice) (string, bool) {
	if resourceSlice == nil || resourceSlice.Spec.NodeName == nil || *resourceSlice.Spec.NodeName == "" {
		return "", false
	}

	return *resourceSlice.Spec.NodeName, true
}
