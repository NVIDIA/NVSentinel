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

package mapper

import (
	"context"
	"fmt"
	"log/slog"

	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	resourceinformers "k8s.io/client-go/informers/resource/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

const (
	// draGPUDriverName is the DRA driver GPU Operator GPUCluster mode allocates GPUs through. It is also the
	// key the resolved UUIDs are written under in the devices annotation.
	draGPUDriverName = "gpu.nvidia.com"
	// draGPUUUIDAttribute is the ResourceSlice device attribute carrying the GPU UUID.
	draGPUUUIDAttribute = "uuid"
)

// draDeviceResolver maps a DRA allocation (pool name, device name such as gpu-0) to a GPU UUID.
// A nil resolver leaves DRA allocations unmapped.
type draDeviceResolver func(poolName, deviceName string) (string, bool)

// newDRADeviceResolver watches this node's gpu.nvidia.com ResourceSlices and resolves allocations against them.
// It returns a nil resolver when the cluster does not serve resource.k8s.io/v1, since then there is nothing to map.
func newDRADeviceResolver(ctx context.Context, client kubernetes.Interface,
	nodeName string) (draDeviceResolver, error) {
	groupVersion := resourcev1.SchemeGroupVersion.String()

	_, err := client.Discovery().ServerResourcesForGroupVersion(groupVersion)
	if errors.IsNotFound(err) {
		slog.Info("ResourceSlice API is not served, DRA GPU allocations will not be mapped",
			"groupVersion", groupVersion)

		return nil, nil
	}

	if err != nil {
		return nil, fmt.Errorf("discover %s: %w", groupVersion, err)
	}

	if nodeName == "" {
		return nil, fmt.Errorf("node name is required to select this node's ResourceSlices")
	}

	informer := resourceinformers.NewFilteredResourceSliceInformer(client, 0, cache.Indexers{},
		func(options *metav1.ListOptions) {
			options.FieldSelector = fields.SelectorFromSet(fields.Set{
				resourcev1.ResourceSliceSelectorNodeName: nodeName,
				resourcev1.ResourceSliceSelectorDriver:   draGPUDriverName,
			}).String()
		})

	go informer.Run(ctx.Done())

	if !cache.WaitForCacheSync(ctx.Done(), informer.HasSynced) {
		return nil, fmt.Errorf("ResourceSlice informer cache did not sync")
	}

	return storeResolver(informer.GetStore()), nil
}

// storeResolver scans the cached slices on each call.
// ponytail: a node carries a few slices with at most a few dozen devices, so one scan per 30s poll beats
// maintaining an index; add a (pool, device) index if slices ever grow past that.
func storeResolver(store cache.Store) draDeviceResolver {
	return func(poolName, deviceName string) (string, bool) {
		for _, obj := range store.List() {
			slice, ok := obj.(*resourcev1.ResourceSlice)
			if !ok || slice.Spec.Pool.Name != poolName {
				continue
			}

			for _, device := range slice.Spec.Devices {
				if device.Name != deviceName {
					continue
				}

				if uuid, ok := device.Attributes[draGPUUUIDAttribute]; ok && uuid.StringValue != nil {
					return *uuid.StringValue, true
				}
			}
		}

		return "", false
	}
}
