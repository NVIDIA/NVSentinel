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

package model

import (
	"errors"
	"fmt"
	"slices"
	"sort"
)

// PartialRemediationEntityTypes maps an entity type of health events, such as GPU_UUID, to the devices
// of that type and the actions that repair one device of that type (ADR-071). The Helm chart renders it
// from global.partialRemediationEntityTypes into the configuration of each component that needs it.
//
// The chart default holds the GPU_UUID entry. An empty map means that no entity type is configured, so no
// action gets a partial drain.
type PartialRemediationEntityTypes map[string]PartialRemediationEntityType

// PartialRemediationEntityType lists the devices of one entity type and the actions scoped to it.
type PartialRemediationEntityType struct {
	// Devices lists the ways a pod can get a device of the entity type.
	Devices []PartialRemediationDevice `toml:"devices"`
	// ScopedActions are action names, as GetEffectiveActionName returns them, that repair one device of
	// the entity type. node-drainer runs a partial drain for them.
	ScopedActions []string `toml:"scopedActions"`
}

// PartialRemediationDevice is one way a pod can get a device: a device-plugin entry sets ResourceNames,
// and a DRA entry sets Driver, DeviceClasses and EntityValueAttribute.
type PartialRemediationDevice struct {
	// ResourceNames are the exact resource names that pods request, such as nvidia.com/gpu.
	ResourceNames []string `toml:"resourceNames"`
	// Driver is the name that the kubelet reports for an allocated DRA device.
	Driver string `toml:"driver"`
	// DeviceClasses are the DeviceClass names that pods use in their claims.
	DeviceClasses []string `toml:"deviceClasses"`
	// EntityValueAttribute names the ResourceSlice attribute whose value is the entity value in events.
	EntityValueAttribute string `toml:"entityValueAttribute"`
}

// GPUUUIDEntityType is the entity type of health events that names a GPU.
const GPUUUIDEntityType = "GPU_UUID"

// Validate reports every entry that the components cannot use. The Helm chart runs the same checks.
func (t PartialRemediationEntityTypes) Validate() error {
	var errs []error

	actionOwner := map[string]string{}

	for _, name := range t.sortedNames() {
		if name == "" {
			errs = append(errs, errors.New("an entity type has an empty name"))
			continue
		}

		entityType := t[name]

		if len(entityType.Devices) == 0 {
			errs = append(errs, fmt.Errorf("entity type %s has no devices", name))
		}

		for i, device := range entityType.Devices {
			if err := device.validate(); err != nil {
				errs = append(errs, fmt.Errorf("entity type %s, devices[%d]: %w", name, i, err))
			}
		}

		errs = append(errs, entityType.validateDriverAttributes(name)...)

		for _, action := range entityType.ScopedActions {
			if action == "" {
				errs = append(errs, fmt.Errorf("entity type %s has an empty scoped action", name))
				continue
			}

			if owner, ok := actionOwner[action]; ok && owner != name {
				errs = append(errs, fmt.Errorf("action %s is scoped to both %s and %s; an action can be scoped "+
					"to only one entity type", action, owner, name))

				continue
			}

			actionOwner[action] = name
		}
	}

	errs = append(errs, t.validateUniqueDeviceNames()...)

	return errors.Join(errs...)
}

// validateUniqueDeviceNames reports a device name or DeviceClass that more than one entity type lists. The pod
// device annotation lists devices under resource names and driver names, without the entity type. A drain of a
// second entity type would compare its entity values with the devices of the first, evict no pod, and succeed.
func (t PartialRemediationEntityTypes) validateUniqueDeviceNames() []error {
	var errs []error

	nameOwner := map[string]string{}
	deviceClassOwner := map[string]string{}

	for _, name := range t.sortedNames() {
		entityType := t[name]
		errs = append(errs, claimNames(nameOwner, name, entityType.AnnotationKeys(), "device name")...)
		errs = append(errs, claimNames(deviceClassOwner, name, entityType.DeviceClasses(), "DeviceClass")...)
	}

	return errs
}

// validateDriverAttributes reports a driver that two entries list with different entityValueAttribute values. The
// pod device annotation lists the devices of a driver under the driver name only, so the pod device mapper can
// record one attribute for each driver.
func (e PartialRemediationEntityType) validateDriverAttributes(entityType string) []error {
	var errs []error

	attributeByDriver := map[string]string{}

	for _, device := range e.Devices {
		if device.Driver == "" || device.EntityValueAttribute == "" {
			continue
		}

		attribute, ok := attributeByDriver[device.Driver]
		if ok && attribute != device.EntityValueAttribute {
			errs = append(errs, fmt.Errorf("entity type %s lists driver %s with entityValueAttribute %s and %s; "+
				"a driver can have only one", entityType, device.Driver, attribute, device.EntityValueAttribute))

			continue
		}

		attributeByDriver[device.Driver] = device.EntityValueAttribute
	}

	return errs
}

// claimNames records entityType as the owner of each name, and reports a name that another entity type owns.
func claimNames(owner map[string]string, entityType string, names []string, kind string) []error {
	var errs []error

	for _, name := range names {
		if name == "" {
			continue
		}

		if previous, ok := owner[name]; ok && previous != entityType {
			errs = append(errs, fmt.Errorf("%s %s is listed under both %s and %s; the pod device annotation does "+
				"not record the entity type, so a %s can belong to only one entity type", kind, name, previous,
				entityType, kind))

			continue
		}

		owner[name] = entityType
	}

	return errs
}

func (d PartialRemediationDevice) validate() error {
	isDevicePlugin := len(d.ResourceNames) > 0
	isDRA := d.Driver != "" || len(d.DeviceClasses) > 0 || d.EntityValueAttribute != ""

	switch {
	case isDevicePlugin && isDRA:
		return errors.New("an entry sets either resourceNames, or driver, deviceClasses and " +
			"entityValueAttribute, not both")
	case isDevicePlugin:
		return validateNames("resourceNames", d.ResourceNames)
	case isDRA:
		return d.validateDRA()
	default:
		return errors.New("an entry needs resourceNames, or driver, deviceClasses and entityValueAttribute")
	}
}

func (d PartialRemediationDevice) validateDRA() error {
	if d.Driver == "" || len(d.DeviceClasses) == 0 || d.EntityValueAttribute == "" {
		return errors.New("a DRA entry needs driver, deviceClasses and entityValueAttribute")
	}

	return validateNames("deviceClasses", d.DeviceClasses)
}

func validateNames(field string, names []string) error {
	if slices.Contains(names, "") {
		return fmt.Errorf("%s has an empty name", field)
	}

	return nil
}

// EntityTypeForAction returns the entity type whose scopedActions lists the action.
func (t PartialRemediationEntityTypes) EntityTypeForAction(action string) (string, bool) {
	for _, name := range t.sortedNames() {
		if slices.Contains(t[name].ScopedActions, action) {
			return name, true
		}
	}

	return "", false
}

// AllDevices returns one entity type that holds the devices of every entity type, for checks that apply
// to any configured device.
func (t PartialRemediationEntityTypes) AllDevices() PartialRemediationEntityType {
	var all PartialRemediationEntityType

	for _, name := range t.sortedNames() {
		all.Devices = append(all.Devices, t[name].Devices...)
	}

	return all
}

func (t PartialRemediationEntityTypes) sortedNames() []string {
	names := make([]string, 0, len(t))
	for name := range t {
		names = append(names, name)
	}

	sort.Strings(names)

	return names
}

// ResourceNames returns the device-plugin resource names of the entity type.
func (e PartialRemediationEntityType) ResourceNames() []string {
	var names []string
	for _, device := range e.Devices {
		names = append(names, device.ResourceNames...)
	}

	return names
}

// DeviceClasses returns the DRA DeviceClass names of the entity type.
func (e PartialRemediationEntityType) DeviceClasses() []string {
	var classes []string
	for _, device := range e.Devices {
		classes = append(classes, device.DeviceClasses...)
	}

	return classes
}

// Drivers returns the DRA driver names of the entity type.
func (e PartialRemediationEntityType) Drivers() []string {
	var drivers []string

	for _, device := range e.Devices {
		if device.Driver != "" {
			drivers = append(drivers, device.Driver)
		}
	}

	return drivers
}

// DriverForDeviceClass returns the driver of the entry that lists the DeviceClass.
func (e PartialRemediationEntityType) DriverForDeviceClass(deviceClass string) (string, bool) {
	for _, device := range e.Devices {
		if device.Driver != "" && slices.Contains(device.DeviceClasses, deviceClass) {
			return device.Driver, true
		}
	}

	return "", false
}

// AnnotationKeys returns the names under which the pod device annotation lists devices of the entity
// type: its resource names and its DRA driver names.
func (e PartialRemediationEntityType) AnnotationKeys() []string {
	return append(e.ResourceNames(), e.Drivers()...)
}
