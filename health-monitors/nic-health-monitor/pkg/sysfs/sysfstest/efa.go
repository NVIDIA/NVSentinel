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

// Package sysfstest builds fake sysfs trees on a real (temporary)
// filesystem so tests can drive the production sysfs.Reader instead of
// the function-table mock. It currently models AWS Elastic Fabric
// Adapters and a minimal Mellanox RoCE device.
package sysfstest

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

// Tree is a fake sysfs root with the class directories the NIC health
// monitor reads.
type Tree struct {
	Root    string
	IBBase  string
	NetBase string
}

// New creates an empty tree under t.TempDir().
func New(t *testing.T) *Tree {
	t.Helper()

	root := t.TempDir()
	tr := &Tree{
		Root:    root,
		IBBase:  filepath.Join(root, "class", "infiniband"),
		NetBase: filepath.Join(root, "class", "net"),
	}

	require.NoError(t, os.MkdirAll(tr.IBBase, 0o755))
	require.NoError(t, os.MkdirAll(tr.NetBase, 0o755))

	return tr
}

// WriteFile creates path (and parents) with the given content.
func WriteFile(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
}

// EFAOpts tunes an EFA device fixture.
type EFAOpts struct {
	// Driver is the basename of the device/driver symlink target
	// ("efa"). Empty leaves the symlink out entirely.
	Driver string
	// PCIAddress is written to device/uevent as PCI_SLOT_NAME
	// (default "0000:00:06.0").
	PCIAddress string
	// NUMANode is written to device/numa_node (default 0).
	NUMANode int
	// NetDev, when set, creates device/net/<NetDev> and the matching
	// /sys/class/net/<NetDev> entry with Operstate and Carrier.
	NetDev    string
	Operstate string
	// Carrier is the carrier attribute content ("0"/"1"); empty omits
	// the file, mirroring an admin-down interface where the read fails.
	Carrier string
	// LinkLayer is what the kernel writes for the port; the efa driver
	// reports an unspecified link layer, rendered as "Unknown".
	LinkLayer string
	// PortHWCounters / DeviceHWCounters seed
	// ports/1/hw_counters/<name> and hw_counters/<name>.
	PortHWCounters   map[string]uint64
	DeviceHWCounters map[string]uint64
}

// AddEFA materialises one AWS EFA adapter the way the efa kernel driver
// (plus rdma-core's predictable naming) lays it out on a p4d/p5
// instance: /sys/class/infiniband/rdmap<bus>s<slot> with a single port
// whose state/phys_state are hard-coded to ACTIVE/LinkUp, an Amazon PCI
// vendor ID, a driver symlink into /sys/bus/pci/drivers/efa, per-port
// hw_counters and device-level hw_counters.
func (tr *Tree) AddEFA(t *testing.T, name string, opts EFAOpts) {
	t.Helper()

	dev := filepath.Join(tr.IBBase, name)
	pciDev := filepath.Join(dev, "device")

	pci := opts.PCIAddress
	if pci == "" {
		pci = "0000:00:06.0"
	}

	WriteFile(t, filepath.Join(pciDev, "vendor"), "0x1d0f\n")
	WriteFile(t, filepath.Join(pciDev, "device"), "0xefa1\n")
	WriteFile(t, filepath.Join(pciDev, "numa_node"), strconv.Itoa(opts.NUMANode)+"\n")
	WriteFile(t, filepath.Join(pciDev, "uevent"), "DRIVER=efa\nPCI_SLOT_NAME="+pci+"\n")
	WriteFile(t, filepath.Join(dev, "node_type"), "1: CA\n")
	WriteFile(t, filepath.Join(dev, "fw_ver"), "0.0.0.0\n")

	linkLayer := opts.LinkLayer
	if linkLayer == "" {
		linkLayer = "Unknown"
	}

	port := filepath.Join(dev, "ports", "1")
	WriteFile(t, filepath.Join(port, "state"), "4: ACTIVE\n")
	WriteFile(t, filepath.Join(port, "phys_state"), "5: LinkUp\n")
	WriteFile(t, filepath.Join(port, "link_layer"), linkLayer+"\n")

	portCounters := map[string]uint64{"rx_drops": 0, "tx_pkts": 12345, "rx_pkts": 12345}
	for k, v := range opts.PortHWCounters {
		portCounters[k] = v
	}

	for k, v := range portCounters {
		WriteFile(t, filepath.Join(port, "hw_counters", k), strconv.FormatUint(v, 10)+"\n")
	}

	devCounters := map[string]uint64{"cmds_err": 0, "no_completion_cmds": 0, "keep_alive_rcvd": 100}
	for k, v := range opts.DeviceHWCounters {
		devCounters[k] = v
	}

	for k, v := range devCounters {
		WriteFile(t, filepath.Join(dev, "hw_counters", k), strconv.FormatUint(v, 10)+"\n")
	}

	if opts.Driver != "" {
		driverDir := filepath.Join(tr.Root, "bus", "pci", "drivers", opts.Driver)
		require.NoError(t, os.MkdirAll(driverDir, 0o755))
		require.NoError(t, os.Symlink(driverDir, filepath.Join(pciDev, "driver")))
	}

	if opts.NetDev != "" {
		require.NoError(t, os.MkdirAll(filepath.Join(pciDev, "net", opts.NetDev), 0o755))
		tr.SetNetDev(t, opts.NetDev, opts.Operstate, opts.Carrier)
	}
}

// SetNetDev (re)writes /sys/class/net/<iface>/operstate and carrier.
// An empty carrier removes the attribute.
func (tr *Tree) SetNetDev(t *testing.T, iface, operstate, carrier string) {
	t.Helper()

	WriteFile(t, filepath.Join(tr.NetBase, iface, "operstate"), operstate+"\n")

	carrierPath := filepath.Join(tr.NetBase, iface, "carrier")
	if carrier == "" {
		_ = os.Remove(carrierPath)
		return
	}

	WriteFile(t, carrierPath, carrier+"\n")
}

// SetPortHWCounter overwrites ports/1/hw_counters/<name> for a device.
func (tr *Tree) SetPortHWCounter(t *testing.T, device, name string, value uint64) {
	t.Helper()
	WriteFile(t, filepath.Join(tr.IBBase, device, "ports", "1", "hw_counters", name),
		strconv.FormatUint(value, 10)+"\n")
}

// SetDeviceHWCounter overwrites hw_counters/<name> for a device.
func (tr *Tree) SetDeviceHWCounter(t *testing.T, device, name string, value uint64) {
	t.Helper()
	WriteFile(t, filepath.Join(tr.IBBase, device, "hw_counters", name),
		strconv.FormatUint(value, 10)+"\n")
}

// RemoveDevice deletes /sys/class/infiniband/<name>, simulating an
// adapter that vanished after a failed reset.
func (tr *Tree) RemoveDevice(t *testing.T, name string) {
	t.Helper()
	require.NoError(t, os.RemoveAll(filepath.Join(tr.IBBase, name)))
}

// AddMlx5RoCE materialises a minimal Mellanox RoCE device so mixed
// fixtures can prove the two adapter families stay separate.
func (tr *Tree) AddMlx5RoCE(t *testing.T, name, pciAddress string) {
	t.Helper()

	dev := filepath.Join(tr.IBBase, name)
	pciDev := filepath.Join(dev, "device")

	WriteFile(t, filepath.Join(pciDev, "vendor"), "0x15b3\n")
	WriteFile(t, filepath.Join(pciDev, "numa_node"), "0\n")
	WriteFile(t, filepath.Join(pciDev, "uevent"), "DRIVER=mlx5_core\nPCI_SLOT_NAME="+pciAddress+"\n")
	WriteFile(t, filepath.Join(dev, "hca_type"), "MT4129\n")
	WriteFile(t, filepath.Join(dev, "ports", "1", "state"), "4: ACTIVE\n")
	WriteFile(t, filepath.Join(dev, "ports", "1", "phys_state"), "5: LinkUp\n")
	WriteFile(t, filepath.Join(dev, "ports", "1", "link_layer"), "Ethernet\n")

	driverDir := filepath.Join(tr.Root, "bus", "pci", "drivers", "mlx5_core")
	require.NoError(t, os.MkdirAll(driverDir, 0o755))
	require.NoError(t, os.Symlink(driverDir, filepath.Join(pciDev, "driver")))
}
