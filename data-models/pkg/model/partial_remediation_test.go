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
	"slices"
	"strings"
	"testing"
)

func accelEntityType(actions ...string) PartialRemediationEntityType {
	return PartialRemediationEntityType{
		Devices: []PartialRemediationDevice{{
			Driver:               "accel.example.com",
			DeviceClasses:        []string{"accel.example.com", "accel-large.example.com"},
			EntityValueAttribute: "serial",
		}},
		ScopedActions: actions,
	}
}

func TestPartialRemediationEntityTypesValidate(t *testing.T) {
	tests := []struct {
		name    string
		types   PartialRemediationEntityTypes
		wantErr []string
	}{
		{name: "default", types: DefaultPartialRemediationEntityTypes()},
		{name: "empty map", types: PartialRemediationEntityTypes{}},
		{
			name: "second entity type",
			types: PartialRemediationEntityTypes{
				GPUUUIDEntityType: DefaultPartialRemediationEntityTypes()[GPUUUIDEntityType],
				"ACCEL_SERIAL":    accelEntityType("accel-reset"),
			},
		},
		{
			name:  "entity type without scoped actions",
			types: PartialRemediationEntityTypes{"ACCEL_SERIAL": accelEntityType()},
		},
		{
			name:    "empty entity type name",
			types:   PartialRemediationEntityTypes{"": accelEntityType()},
			wantErr: []string{"an entity type has an empty name"},
		},
		{
			name:    "no devices",
			types:   PartialRemediationEntityTypes{"ACCEL_SERIAL": {ScopedActions: []string{"accel-reset"}}},
			wantErr: []string{"entity type ACCEL_SERIAL has no devices"},
		},
		{
			name: "entry mixes device plugin and DRA",
			types: PartialRemediationEntityTypes{"ACCEL_SERIAL": {Devices: []PartialRemediationDevice{{
				ResourceNames: []string{"example.com/accel"},
				Driver:        "accel.example.com",
			}}}},
			wantErr: []string{"entity type ACCEL_SERIAL, devices[0]: an entry sets either resourceNames"},
		},
		{
			name: "DRA entry without entityValueAttribute",
			types: PartialRemediationEntityTypes{"ACCEL_SERIAL": {Devices: []PartialRemediationDevice{{
				Driver:        "accel.example.com",
				DeviceClasses: []string{"accel.example.com"},
			}}}},
			wantErr: []string{"devices[0]: a DRA entry needs driver, deviceClasses and entityValueAttribute"},
		},
		{
			name:    "empty entry",
			types:   PartialRemediationEntityTypes{"ACCEL_SERIAL": {Devices: []PartialRemediationDevice{{}}}},
			wantErr: []string{"devices[0]: an entry needs resourceNames"},
		},
		{
			name: "empty resource name",
			types: PartialRemediationEntityTypes{"ACCEL_SERIAL": {Devices: []PartialRemediationDevice{{
				ResourceNames: []string{"example.com/accel", ""},
			}}}},
			wantErr: []string{"devices[0]: resourceNames has an empty name"},
		},
		{
			name: "empty scoped action",
			types: PartialRemediationEntityTypes{"ACCEL_SERIAL": accelEntityType("")},
			wantErr: []string{"entity type ACCEL_SERIAL has an empty scoped action"},
		},
		{
			name: "action scoped to two entity types",
			types: PartialRemediationEntityTypes{
				GPUUUIDEntityType: DefaultPartialRemediationEntityTypes()[GPUUUIDEntityType],
				"ACCEL_SERIAL":    accelEntityType("COMPONENT_RESET"),
			},
			wantErr: []string{"action COMPONENT_RESET is scoped to both ACCEL_SERIAL and GPU_UUID"},
		},
		{
			name: "reports every error",
			types: PartialRemediationEntityTypes{
				"ACCEL_SERIAL": {ScopedActions: []string{""}},
				"NIC_SERIAL":   {Devices: []PartialRemediationDevice{{}}},
			},
			wantErr: []string{
				"entity type ACCEL_SERIAL has no devices",
				"entity type ACCEL_SERIAL has an empty scoped action",
				"entity type NIC_SERIAL, devices[0]: an entry needs resourceNames",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.types.Validate()
			if len(tt.wantErr) == 0 {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}

				return
			}

			if err == nil {
				t.Fatalf("Validate() = nil, want errors containing %q", tt.wantErr)
			}

			for _, want := range tt.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("Validate() = %q, want it to contain %q", err, want)
				}
			}
		})
	}
}

func TestPartialRemediationEntityTypesEntityTypeForAction(t *testing.T) {
	types := PartialRemediationEntityTypes{
		GPUUUIDEntityType: DefaultPartialRemediationEntityTypes()[GPUUUIDEntityType],
		"ACCEL_SERIAL":    accelEntityType("accel-reset"),
	}

	tests := []struct {
		action   string
		want     string
		wantOkay bool
	}{
		{action: "COMPONENT_RESET", want: GPUUUIDEntityType, wantOkay: true},
		{action: "accel-reset", want: "ACCEL_SERIAL", wantOkay: true},
		{action: "RESTART_BM"},
		{action: ""},
	}

	for _, tt := range tests {
		got, ok := types.EntityTypeForAction(tt.action)
		if got != tt.want || ok != tt.wantOkay {
			t.Errorf("EntityTypeForAction(%q) = %q, %v, want %q, %v", tt.action, got, ok, tt.want, tt.wantOkay)
		}
	}

	if got, ok := PartialRemediationEntityTypes(nil).EntityTypeForAction("COMPONENT_RESET"); ok {
		t.Errorf("nil map EntityTypeForAction(COMPONENT_RESET) = %q, true, want no entity type", got)
	}
}

func TestPartialRemediationEntityTypeNames(t *testing.T) {
	gpu := DefaultPartialRemediationEntityTypes()[GPUUUIDEntityType]

	if got, want := gpu.AnnotationKeys(), []string{"nvidia.com/gpu", "nvidia.com/pgpu", "gpu.nvidia.com"}; !slices.Equal(got, want) {
		t.Errorf("AnnotationKeys() = %q, want %q", got, want)
	}

	if got, want := gpu.DeviceClasses(), []string{"gpu.nvidia.com"}; !slices.Equal(got, want) {
		t.Errorf("DeviceClasses() = %q, want %q", got, want)
	}

	accel := accelEntityType()
	if driver, ok := accel.DriverForDeviceClass("accel-large.example.com"); !ok || driver != "accel.example.com" {
		t.Errorf("DriverForDeviceClass(accel-large.example.com) = %q, %v, want accel.example.com, true", driver, ok)
	}

	if driver, ok := accel.DriverForDeviceClass("gpu.nvidia.com"); ok {
		t.Errorf("DriverForDeviceClass(gpu.nvidia.com) = %q, true, want no driver", driver)
	}

	all := PartialRemediationEntityTypes{GPUUUIDEntityType: gpu, "ACCEL_SERIAL": accel}.AllDevices()
	if got, want := all.DeviceClasses(), []string{"accel.example.com", "accel-large.example.com", "gpu.nvidia.com"}; !slices.Equal(got, want) {
		t.Errorf("AllDevices().DeviceClasses() = %q, want %q", got, want)
	}

	if got, want := all.ResourceNames(), []string{"nvidia.com/gpu", "nvidia.com/pgpu"}; !slices.Equal(got, want) {
		t.Errorf("AllDevices().ResourceNames() = %q, want %q", got, want)
	}
}
