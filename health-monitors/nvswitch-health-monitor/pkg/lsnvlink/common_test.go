// Copyright (c) 2024, NVIDIA CORPORATION.  All rights reserved.
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

package lsnvlink

import (
	"fmt"
	"reflect"
	"testing"
)

// mockCommandImpl implements command interface, returning fixed output or an error
type mockCommandImpl struct {
	output []byte
	err    error
}

func (m *mockCommandImpl) Output() ([]byte, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.output, nil
}

func mockCommandExec(output string, err error) func(name string, arg ...string) command {
	return func(name string, arg ...string) command {
		return &mockCommandImpl{
			output: []byte(output),
			err:    err,
		}
	}
}

func TestGetNVSwitchPCIAddresses_NoNVSwitch(t *testing.T) {
	originalCreator := commandExec
	defer func() { commandExec = originalCreator }()

	mockOutput := `
0000:0b:00.0 3D controller: NVIDIA Corporation GH100 [H100 SXM5 80GB] (rev a1)
        Subsystem: NVIDIA Corporation Device 16c1
        Kernel driver in use: nvidia
0000:8b:00.0 3D controller: NVIDIA Corporation GH100 [H100 SXM5 80GB] (rev a1)
        Subsystem: NVIDIA Corporation Device 16c1
        Kernel driver in use: nvidia
`
	commandExec = mockCommandExec(mockOutput, nil)

	got := GetNVSwitchPCIAddresses()
	if len(got) != 0 {
		t.Errorf("Expected no PCI addresses, got %v", got)
	}
}

func TestGetNVSwitchPCIAddresses_SingleNVSwitch(t *testing.T) {
	originalCreator := commandExec
	defer func() { commandExec = originalCreator }()

	mockOutput := `
0004:00:00.0 Bridge [0680]: NVIDIA Corporation Device [10de:22a3] (rev a1)
	Subsystem: NVIDIA Corporation Device [10de:1796]
	Kernel driver in use: nvidia-nvswitch
`
	commandExec = mockCommandExec(mockOutput, nil)

	got := GetNVSwitchPCIAddresses()
	want := []string{"0004:00:00.0"}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("Expected %v, got %v", want, got)
	}
}

func TestGetNVSwitchPCIAddresses_MultipleNVSwitch(t *testing.T) {
	originalCreator := commandExec
	defer func() { commandExec = originalCreator }()

	mockOutput := `
0000:04:00.0 3D controller: NVIDIA Corporation GH100 [H100 SXM5 80GB] (rev a1)
        Kernel driver in use: nvidia-nvswitch
0000:05:00.0 3D controller: NVIDIA Corporation GH100 [H100 SXM5 80GB] (rev a1)
        Kernel driver in use: nvidia
0004:00:00.0 Bridge [0680]: NVIDIA Corporation Device [10de:22a3] (rev a1)
	Subsystem: NVIDIA Corporation Device [10de:1796]
	Kernel driver in use: nvidia-nvswitch
`
	commandExec = mockCommandExec(mockOutput, nil)

	got := GetNVSwitchPCIAddresses()
	want := []string{"0000:04:00.0", "0004:00:00.0"}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("Expected %v, got %v", want, got)
	}
}

func TestGetNVSwitchPCIAddresses_ErrorFromCommand(t *testing.T) {
	originalCreator := commandExec
	defer func() { commandExec = originalCreator }()

	mockErr := fmt.Errorf("simulated command error")

	commandExec = mockCommandExec("", mockErr)

	got := GetNVSwitchPCIAddresses()
	if got != nil {
		t.Errorf("Expected nil on command error, got %v", got)
	}
}

func TestGetDGXType_NoNVIDIADevices(t *testing.T) {
	originalCreator := commandExec
	defer func() { commandExec = originalCreator }()

	mockOutput := `
00:03.0 Non-Volatile memory controller: Google, Inc. NVMe device (rev 01)
00:09.0 Non-Volatile memory controller: Google, Inc. NVMe device (rev 01)
00:0c.0 Ethernet controller: Google, Inc. Compute Engine Virtual Ethernet [gVNIC]
00:0d.0 Unclassified device [00ff]: Red Hat, Inc. Virtio RNG
01:00.0 PCI bridge: Google, Inc. Device a010
02:00.0 PCI bridge: PLX Technology, Inc. Device 8796
03:00.0 PCI bridge: PLX Technology, Inc. Device 8796
`
	commandExec = mockCommandExec(mockOutput, nil)

	got := GetDGXType()
	want := DGX_TYPE_UNKNOWN

	if got != want {
		t.Errorf("Expected DGX_TYPE_UNKNOWN, got %v", got)
	}
}

func TestGetDGXType_A100(t *testing.T) {
	originalCreator := commandExec
	defer func() { commandExec = originalCreator }()

	mockOutput := `
8a:03.0 PCI bridge: PLX Technology, Inc. Device 8796
8b:00.0 3D controller: NVIDIA Corporation GA100 [A100 SXM5 80GB] (rev a1)
8c:00.0 3D controller: NVIDIA Corporation GA100 [A100 SXM5 80GB] (rev a1)
8d:00.0 Ethernet controller: Google, Inc. Compute Engine Virtual Ethernet [gVNIC]
8d:00.1 Fabric controller: Google, Inc. Device 0084
`
	commandExec = mockCommandExec(mockOutput, nil)

	got := GetDGXType()
	want := DGX_TYPE_A100

	if got != want {
		t.Errorf("Expected DGX_TYPE_A100, got %v", got)
	}
}

func TestGetDGXType_H100(t *testing.T) {
	originalCreator := commandExec
	defer func() { commandExec = originalCreator }()

	mockOutput := `
8a:03.0 PCI bridge: PLX Technology, Inc. Device 8796
8b:00.0 3D controller: NVIDIA Corporation GH100 [H100 SXM5 80GB] (rev a1)
8c:00.0 3D controller: NVIDIA Corporation GH100 [H100 SXM5 80GB] (rev a1)
8d:00.0 Ethernet controller: Google, Inc. Compute Engine Virtual Ethernet [gVNIC]
8d:00.1 Fabric controller: Google, Inc. Device 0084
`
	commandExec = mockCommandExec(mockOutput, nil)

	got := GetDGXType()
	want := DGX_TYPE_H100

	if got != want {
		t.Errorf("Expected DGX_TYPE_H100, got %v", got)
	}
}

func TestGetDGXType_ErrorFromCommand(t *testing.T) {
	originalCreator := commandExec
	defer func() { commandExec = originalCreator }()

	mockErr := fmt.Errorf("simulated command error")

	commandExec = mockCommandExec("", mockErr)

	got := GetDGXType()
	want := DGX_TYPE_UNKNOWN

	if got != want {
		t.Errorf("Expected DGX_TYPE_UNKNOWN on command error, got %v", got)
	}
}
