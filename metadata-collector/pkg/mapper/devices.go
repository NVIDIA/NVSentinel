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

package mapper

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	resourcev1 "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/kubernetes"

	"github.com/nvidia/nvsentinel/data-models/pkg/model"
)

// deviceFilter holds the configured devices that the mapper records: device-plugin resource names, and DRA
// drivers with the ResourceSlice attribute whose value is the entity value.
type deviceFilter struct {
	resourceNames     map[string]bool
	attributeByDriver map[string]string
}

// ErrNodeNameMissing means the configuration has a DRA entry but the node name is not set. The mapper needs the
// node name to read the ResourceSlices of the node.
var ErrNodeNameMissing = errors.New("NODE_NAME is not set; the pod device mapper needs it to read the " +
	"ResourceSlices of the node for the DRA entries of partialRemediationEntityTypes")

// CheckNodeName returns ErrNodeNameMissing when entityTypes has a DRA entry and nodeName is empty.
func CheckNodeName(entityTypes model.PartialRemediationEntityTypes, nodeName string) error {
	if nodeName == "" && len(entityTypes.AllDevices().Drivers()) > 0 {
		return ErrNodeNameMissing
	}

	return nil
}

// newDeviceFilter collects the devices of every entity type. PartialRemediationEntityTypes.Validate allows one
// attribute for each driver, because the annotation lists the devices of a driver under the driver name only.
func newDeviceFilter(entityTypes model.PartialRemediationEntityTypes) deviceFilter {
	filter := deviceFilter{resourceNames: map[string]bool{}, attributeByDriver: map[string]string{}}

	for _, device := range entityTypes.AllDevices().Devices {
		for _, name := range device.ResourceNames {
			filter.resourceNames[name] = true
		}

		if device.Driver != "" {
			filter.attributeByDriver[device.Driver] = device.EntityValueAttribute
		}
	}

	return filter
}

// draDevice identifies a device that the kubelet reports for a DRA allocation.
type draDevice struct {
	driver string
	pool   string
	name   string
}

// ErrDRALookupFailed means the mapper could not read the ResourceSlices for some DRA devices. Their pods get
// no annotation, and the poll counts as failed, so that a lasting failure reaches
// --pod-mapper-max-consecutive-failures instead of showing only in the logs.
var ErrDRALookupFailed = errors.New("failed to look up DRA devices in ResourceSlices")

// resourceSliceResolver reads the entity value of a DRA device from the ResourceSlices of the node. A device
// keeps its identity while the node runs, so a resolved value is kept until the process restarts, and the
// resolver lists ResourceSlices only for a device that it has not resolved yet.
type resourceSliceResolver struct {
	client            kubernetes.Interface
	nodeName          string
	attributeByDriver map[string]string
	values            map[draDevice]string
}

func newResourceSliceResolver(client kubernetes.Interface, nodeName string,
	attributeByDriver map[string]string) *resourceSliceResolver {
	return &resourceSliceResolver{
		client:            client,
		nodeName:          nodeName,
		attributeByDriver: attributeByDriver,
		values:            map[draDevice]string{},
	}
}

// resolve returns the entity value of each device that it finds. A device that is not in the result is unresolved.
// The error wraps ErrDRALookupFailed when a lookup failed; the result still holds the devices that resolved.
func (r *resourceSliceResolver) resolve(ctx context.Context, devices []draDevice) (map[draDevice]string, error) {
	driversToList := map[string]bool{}

	for _, device := range devices {
		if _, ok := r.values[device]; !ok {
			driversToList[device.driver] = true
		}
	}

	var errs []error

	for _, driver := range sortedKeys(driversToList) {
		if err := r.loadDriver(ctx, driver); err != nil {
			errs = append(errs, err)
		}
	}

	resolved := make(map[draDevice]string, len(devices))

	for _, device := range devices {
		if value, ok := r.values[device]; ok {
			resolved[device] = value
		}
	}

	return resolved, errors.Join(errs...)
}

// loadDriver lists the ResourceSlices of the driver on this node and keeps the entity value of each device. An
// error leaves the devices unresolved, so their pods get no annotation and a partial drain waits for them.
func (r *resourceSliceResolver) loadDriver(ctx context.Context, driver string) error {
	// resourceVersion 0 lets the API server answer from its cache instead of etcd.
	slices, err := r.client.ResourceV1().ResourceSlices().List(ctx, metav1.ListOptions{
		FieldSelector: fields.AndSelectors(
			fields.OneTermEqualSelector(resourcev1.ResourceSliceSelectorNodeName, r.nodeName),
			fields.OneTermEqualSelector(resourcev1.ResourceSliceSelectorDriver, driver),
		).String(),
		ResourceVersion: "0",
	})
	if err != nil {
		return fmt.Errorf("%w: failed to list the ResourceSlices of driver %s on node %s: %w",
			ErrDRALookupFailed, driver, r.nodeName, err)
	}

	attribute := r.attributeByDriver[driver]

	for _, slice := range currentPoolSlices(slices.Items) {
		for _, device := range slice.Spec.Devices {
			value, ok := attributeValue(device.Attributes, driver, attribute)
			if !ok {
				continue
			}

			r.values[draDevice{driver: driver, pool: slice.Spec.Pool.Name, name: device.Name}] = value
		}
	}

	return nil
}

// currentPoolSlices returns the slices of the newest generation of each pool. A driver that republishes a
// pool raises its generation, and the slices of older generations are stale.
func currentPoolSlices(slices []resourcev1.ResourceSlice) []resourcev1.ResourceSlice {
	generation := map[string]int64{}

	for _, slice := range slices {
		if current, ok := generation[slice.Spec.Pool.Name]; !ok || slice.Spec.Pool.Generation > current {
			generation[slice.Spec.Pool.Name] = slice.Spec.Pool.Generation
		}
	}

	var current []resourcev1.ResourceSlice

	for _, slice := range slices {
		if slice.Spec.Pool.Generation == generation[slice.Spec.Pool.Name] {
			current = append(current, slice)
		}
	}

	return current
}

// attributeValue returns the string value of the attribute. A driver can publish its own attributes without
// its domain, so uuid and gpu.nvidia.com/uuid name the same attribute of the gpu.nvidia.com driver.
func attributeValue(attributes map[resourcev1.QualifiedName]resourcev1.DeviceAttribute, driver,
	name string) (string, bool) {
	names := []string{name}

	if unqualified, found := strings.CutPrefix(name, driver+"/"); found {
		names = append(names, unqualified)
	} else if !strings.Contains(name, "/") {
		names = append(names, driver+"/"+name)
	}

	for _, candidate := range names {
		attribute, ok := attributes[resourcev1.QualifiedName(candidate)]
		if ok && attribute.StringValue != nil && *attribute.StringValue != "" {
			return *attribute.StringValue, true
		}
	}

	return "", false
}

func sortedKeys(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}

	sort.Strings(keys)

	return keys
}
