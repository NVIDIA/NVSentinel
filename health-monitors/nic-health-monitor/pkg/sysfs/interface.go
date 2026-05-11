// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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

// Package sysfs abstracts the sysfs reads performed by the NIC Health
// Monitor. All production code goes through the Reader interface so unit
// tests can substitute MockReader.
package sysfs

// Reader is the contract between the monitor's domain logic (discovery,
// checks, topology) and the filesystem. The production implementation
// reads real sysfs paths; tests use the in-memory MockReader.
type Reader interface {
	// ListDirs returns the immediate child entries of a directory path.
	ListDirs(path string) ([]string, error)
	// ReadFile returns the trimmed contents of a text file under sysfs.
	ReadFile(path string) (string, error)

	// Paths -------------------------------------------------------------
	IBBasePath() string
	NetBasePath() string
	IBPortPath(device string, port int) string
	NetInterfacePath(iface string) string

	// InfiniBand port attributes ---------------------------------------
	ReadIBPortState(device string, port int) (string, error)
	ReadIBPortPhysState(device string, port int) (string, error)
	ReadIBPortLinkLayer(device string, port int) (string, error)
	ReadIBDeviceField(device, field string) (string, error)

	// Network (operstate) ----------------------------------------------
	ReadNetOperState(iface string) (string, error)

	// NUMA / PCI --------------------------------------------------------
	// ReadIBDeviceNUMANode reads /sys/class/infiniband/<dev>/device/numa_node.
	ReadIBDeviceNUMANode(device string) (int, error)
	// ReadPCIAddress resolves the PCI slot of an IB device from its
	// device/uevent file (e.g., "0000:47:00.1").
	ReadPCIAddress(device string) (string, error)
	// IsVirtualFunction reports whether the IB device exposes the
	// `device/physfn` symlink (present on VFs only).
	IsVirtualFunction(device string) bool
}
