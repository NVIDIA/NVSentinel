// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package discovery

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/sysfs"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/sysfs/sysfstest"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/topology"
)

func findDevice(t *testing.T, devices []IBDevice, name string) *IBDevice {
	t.Helper()

	for i := range devices {
		if devices[i].Name == name {
			return &devices[i]
		}
	}

	require.Failf(t, "device not discovered", "device %s missing from %v", name, devices)

	return nil
}

func TestDiscoverDevices_EFADeviceFromRealSysfsTree(t *testing.T) {
	tree := sysfstest.New(t)
	tree.AddEFA(t, "rdmap0s6", sysfstest.EFAOpts{
		Driver: "efa", NetDev: "ens6", Operstate: "up", Carrier: "1",
	})

	reader := sysfs.NewReader(tree.IBBase, tree.NetBase)

	result, err := DiscoverDevices(reader, "")
	require.NoError(t, err)
	require.True(t, result.Complete)
	require.Empty(t, result.UnreadableDevices)
	require.Len(t, result.Devices, 1)

	dev := result.Devices[0]
	assert.Equal(t, "rdmap0s6", dev.Name)
	assert.Equal(t, VendorAmazon, dev.Vendor)
	assert.Equal(t, "efa", dev.Driver)
	assert.Equal(t, "ens6", dev.NetDev)
	assert.False(t, dev.IsVF)
	assert.True(t, IsSupportedVendor(&dev), "EFA adapters are a supported vendor")
	assert.True(t, IsEFADevice(&dev))

	require.Len(t, dev.Ports, 1)
	port := dev.Ports[0]
	assert.Equal(t, 1, port.Port)
	assert.Equal(t, "ACTIVE", port.State)
	assert.Equal(t, "LinkUp", port.PhysicalState)
	assert.Equal(t, topology.LinkLayerEFA, port.LinkLayer,
		"the driver's unspecified link layer must be normalised to EFA")
	assert.True(t, IsEFAPort(&port))
	assert.False(t, IsEthernetPort(&port), "EFA ports must never be claimed by the RoCE check")
	assert.False(t, IsIBPort(&port), "EFA ports must never be claimed by the IB check")
}

func TestDiscoverDevices_EFADeviceWithoutNetdev(t *testing.T) {
	// EFA-only adapters (no ENA sibling) expose no device/net directory.
	tree := sysfstest.New(t)
	tree.AddEFA(t, "rdmap16s27", sysfstest.EFAOpts{Driver: "efa"})

	result, err := DiscoverDevices(sysfs.NewReader(tree.IBBase, tree.NetBase), "")
	require.NoError(t, err)
	require.Len(t, result.Devices, 1)

	dev := result.Devices[0]
	assert.Empty(t, dev.NetDev)
	assert.True(t, IsEFADevice(&dev))
	require.Len(t, dev.Ports, 1)
	assert.Equal(t, topology.LinkLayerEFA, dev.Ports[0].LinkLayer)
}

func TestDiscoverDevices_EFAFallsBackToVendorWhenDriverSymlinkMissing(t *testing.T) {
	tree := sysfstest.New(t)
	tree.AddEFA(t, "efa_0", sysfstest.EFAOpts{})

	result, err := DiscoverDevices(sysfs.NewReader(tree.IBBase, tree.NetBase), "")
	require.NoError(t, err)
	require.Len(t, result.Devices, 1)

	dev := result.Devices[0]
	assert.Empty(t, dev.Driver, "driver symlink absent")
	assert.Equal(t, VendorAmazon, dev.Vendor)
	assert.True(t, IsEFADevice(&dev), "Amazon PCI vendor ID identifies EFA when the driver is unknown")
	assert.Equal(t, topology.LinkLayerEFA, dev.Ports[0].LinkLayer)
}

func TestDiscoverDevices_MixedMlx5AndEFAKeepTheirLinkLayers(t *testing.T) {
	tree := sysfstest.New(t)
	tree.AddEFA(t, "rdmap0s6", sysfstest.EFAOpts{Driver: "efa"})
	tree.AddMlx5RoCE(t, "mlx5_0", "0000:18:00.0")

	result, err := DiscoverDevices(sysfs.NewReader(tree.IBBase, tree.NetBase), "")
	require.NoError(t, err)
	require.Len(t, result.Devices, 2)

	efa := findDevice(t, result.Devices, "rdmap0s6")
	mlx := findDevice(t, result.Devices, "mlx5_0")

	assert.Equal(t, "efa", efa.Driver)
	assert.Equal(t, "mlx5_core", mlx.Driver)
	assert.Equal(t, VendorMellanox, mlx.Vendor)
	assert.False(t, IsEFADevice(mlx))
	assert.True(t, IsEthernetPort(&mlx.Ports[0]))
	assert.False(t, IsEFAPort(&mlx.Ports[0]))
	assert.True(t, IsEFAPort(&efa.Ports[0]))
}

func TestDiscoverDevices_EFADriverBindingWinsOverVendor(t *testing.T) {
	// A device bound to efa is EFA even if its vendor read is unexpected;
	// conversely a non-efa driver on an Amazon device is not EFA.
	amazonMlx := &IBDevice{Name: "x", Vendor: VendorAmazon, Driver: "mlx5_core"}
	assert.False(t, IsEFADevice(amazonMlx))
	assert.False(t, IsSupportedVendor(amazonMlx))

	efaUnknownVendor := &IBDevice{Name: "y", Vendor: VendorUnknown, Driver: "efa"}
	assert.True(t, IsEFADevice(efaUnknownVendor))
	assert.True(t, IsSupportedVendor(efaUnknownVendor))
}

func TestFSReader_ReadIBDeviceDriverAndHWCounter(t *testing.T) {
	tree := sysfstest.New(t)
	tree.AddEFA(t, "rdmap0s6", sysfstest.EFAOpts{Driver: "efa"})

	reader := sysfs.NewReader(tree.IBBase, tree.NetBase)

	driver, err := reader.ReadIBDeviceDriver("rdmap0s6")
	require.NoError(t, err)
	assert.Equal(t, "efa", driver)

	keepAlive, err := reader.ReadIBDeviceHWCounter("rdmap0s6", "keep_alive_rcvd")
	require.NoError(t, err)
	assert.Equal(t, uint64(100), keepAlive)

	_, err = reader.ReadIBDeviceHWCounter("rdmap0s6", "does_not_exist")
	assert.Error(t, err)

	_, err = reader.ReadIBDeviceDriver("missing")
	assert.Error(t, err)
}
