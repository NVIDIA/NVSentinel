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

package sysfs

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Compile-time check that fsReader satisfies Reader.
var _ Reader = (*fsReader)(nil)

// fsReader reads from a real filesystem rooted at two sysfs base paths.
type fsReader struct {
	ibBase  string
	netBase string
}

// NewReader returns a Reader backed by real sysfs paths. The base paths
// are typically container mount points such as /nvsentinel/sys/class/...
// — the monitor never assumes the real /sys root.
func NewReader(ibBase, netBase string) Reader {
	return &fsReader{ibBase: ibBase, netBase: netBase}
}

func (r *fsReader) IBBasePath() string  { return r.ibBase }
func (r *fsReader) NetBasePath() string { return r.netBase }

func (r *fsReader) IBPortPath(device string, port int) string {
	return filepath.Join(r.ibBase, device, "ports", strconv.Itoa(port))
}

func (r *fsReader) NetInterfacePath(iface string) string {
	return filepath.Join(r.netBase, iface)
}

func (r *fsReader) ReadFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("failed to read %s: %w", path, err)
	}

	return strings.TrimSpace(string(data)), nil
}

func (r *fsReader) ListDirs(path string) ([]string, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, fmt.Errorf("failed to list %s: %w", path, err)
	}

	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name())
	}

	return out, nil
}

func (r *fsReader) ReadIBPortState(device string, port int) (string, error) {
	return r.ReadFile(filepath.Join(r.IBPortPath(device, port), "state"))
}

func (r *fsReader) ReadIBPortPhysState(device string, port int) (string, error) {
	return r.ReadFile(filepath.Join(r.IBPortPath(device, port), "phys_state"))
}

func (r *fsReader) ReadIBPortLinkLayer(device string, port int) (string, error) {
	return r.ReadFile(filepath.Join(r.IBPortPath(device, port), "link_layer"))
}

func (r *fsReader) ReadIBDeviceField(device, field string) (string, error) {
	return r.ReadFile(filepath.Join(r.ibBase, device, field))
}

func (r *fsReader) ReadNetOperState(iface string) (string, error) {
	return r.ReadFile(filepath.Join(r.netBase, iface, "operstate"))
}

func (r *fsReader) ReadIBDeviceNUMANode(device string) (int, error) {
	s, err := r.ReadFile(filepath.Join(r.ibBase, device, "device", "numa_node"))
	if err != nil {
		return 0, err
	}

	return strconv.Atoi(s)
}

func (r *fsReader) ReadPCIAddress(device string) (string, error) {
	uevent, err := r.ReadFile(filepath.Join(r.ibBase, device, "device", "uevent"))
	if err != nil {
		return "", err
	}

	for _, line := range strings.Split(uevent, "\n") {
		if strings.HasPrefix(line, "PCI_SLOT_NAME=") {
			return strings.TrimPrefix(line, "PCI_SLOT_NAME="), nil
		}
	}

	return "", fmt.Errorf("PCI_SLOT_NAME not found in uevent for %s", device)
}

// IsVirtualFunction reports whether the IB device's sysfs directory
// contains a `device/physfn` symlink, which the kernel creates on SR-IOV
// Virtual Functions only.
func (r *fsReader) IsVirtualFunction(device string) bool {
	path := filepath.Join(r.ibBase, device, "device", "physfn")
	_, err := os.Lstat(path)

	return err == nil
}

// ParsePortState extracts the trailing state name from a sysfs value such
// as "4: ACTIVE" → "ACTIVE" or "5: LinkUp" → "LinkUp". Inputs without a
// colon are returned unchanged (after whitespace trimming).
func ParsePortState(raw string) string {
	if idx := strings.Index(raw, ":"); idx >= 0 {
		return strings.TrimSpace(raw[idx+1:])
	}

	return strings.TrimSpace(raw)
}
