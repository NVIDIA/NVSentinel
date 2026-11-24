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

package nic

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// DefaultIBClassPath is the host InfiniBand sysfs class directory as
// seen by the metadata-collector container, which mounts the host /sys
// at /sys.
const DefaultIBClassPath = "/sys/class/infiniband"

// SnapshotPCIAddresses returns the PCI address of every IB device
// present under ibClassPath, keyed by device name. The address comes
// from the PCI_SLOT_NAME entry of the device's uevent file, the same
// source the NIC health monitor reads for its own lookups, so both
// sides compare identical strings. Devices whose uevent carries no
// PCI_SLOT_NAME are skipped: a partial map is still useful, and the
// caller degrades to name-based behaviour for the missing devices.
func SnapshotPCIAddresses(ibClassPath string) (map[string]string, error) {
	entries, err := os.ReadDir(ibClassPath)
	if err != nil {
		return nil, fmt.Errorf("list IB devices in %s: %w", ibClassPath, err)
	}

	addresses := make(map[string]string, len(entries))

	for _, entry := range entries {
		addr, err := PCIAddressOfDevice(ibClassPath, entry.Name())
		if err != nil {
			slog.Debug("Skipping IB device with no readable PCI address",
				"device", entry.Name(), "error", err)

			continue
		}

		addresses[entry.Name()] = addr
	}

	return addresses, nil
}

// PCIAddressOfDevice resolves the PCI address of one IB device from
// its sysfs uevent file.
func PCIAddressOfDevice(ibClassPath, device string) (string, error) {
	ueventPath := filepath.Join(ibClassPath, device, "device", "uevent")

	data, err := os.ReadFile(ueventPath)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", ueventPath, err)
	}

	for line := range strings.SplitSeq(string(data), "\n") {
		if after, ok := strings.CutPrefix(line, "PCI_SLOT_NAME="); ok {
			return NormalizePCIAddress(after), nil
		}
	}

	return "", fmt.Errorf("PCI_SLOT_NAME not found in uevent for %s", device)
}

// NormalizePCIAddress canonicalises a PCI address for comparison:
// PCI_SLOT_NAME is already lowercase hex, but consumers and producers
// must not depend on that.
func NormalizePCIAddress(addr string) string {
	return strings.ToLower(strings.TrimSpace(addr))
}

// AddressesForNames restricts a name → PCI address snapshot to the
// given device names. Names missing from the snapshot stay missing.
func AddressesForNames(snapshot map[string]string, names []string) map[string]string {
	out := make(map[string]string, len(names))
	for _, name := range names {
		if addr, ok := snapshot[name]; ok {
			out[name] = addr
		}
	}

	return out
}

// AddressesChangedForNames reports whether any of the given device
// names maps to a different PCI address in after than in before,
// including a device appearing in only one of the two snapshots. It
// detects an RDMA rename that landed between the two snapshots: the
// name set is unchanged by a rename, but the name → address pairing
// moves.
func AddressesChangedForNames(before, after map[string]string, names []string) bool {
	for _, name := range names {
		b, bok := before[name]
		a, aok := after[name]

		if bok != aok || b != a {
			return true
		}
	}

	return false
}
