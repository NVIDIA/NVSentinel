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

// Package discovery enumerates InfiniBand/RoCE devices and their ports
// from sysfs. SR-IOV Virtual Functions are auto-detected (via the
// `device/physfn` symlink) and flagged in the returned device records so
// callers can skip them — unassigned VFs are expected to remain DOWN and
// reporting them would produce false positives.
package discovery

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/sysfs"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/topology"
)

// Vendor identifies the NIC vendor. Only Mellanox/NVIDIA is supported today.
type Vendor string

const (
	VendorMellanox Vendor = "mellanox"
	VendorUnknown  Vendor = "unknown"

	// mellanoxPCIVendorID is the PCI vendor ID reported in
	// /sys/class/infiniband/<dev>/device/vendor.
	mellanoxPCIVendorID = "0x15b3"
)

// IBPort represents the state of a single port on an IB/RoCE device.
type IBPort struct {
	Device        string `json:"device"`
	Port          int    `json:"port"`
	State         string `json:"state"`          // e.g., "ACTIVE", "DOWN"
	PhysicalState string `json:"physical_state"` // e.g., "LinkUp", "Disabled"
	LinkLayer     string `json:"link_layer"`     // "InfiniBand" or "Ethernet"
}

// IBDevice represents a discovered NIC device.
type IBDevice struct {
	Name               string   `json:"name"`   // e.g., "mlx5_0"
	Vendor             Vendor   `json:"vendor"` // detected from sysfs vendor ID
	HCAType            string   `json:"hca_type,omitempty"`
	FWVersion          string   `json:"fw_ver,omitempty"`
	Ports              []IBPort `json:"ports"`
	IsVF               bool     `json:"is_vf"` // true when `device/physfn` symlink exists
	NetDev             string   `json:"net_dev,omitempty"`
	IncludedByOverride bool     `json:"-"` // true when selected by the explicit inclusion override
}

// DiscoveryResult holds the output of device discovery, separating monitored
// devices from VFs skipped by the normal discovery flow.
type DiscoveryResult struct {
	Devices           []IBDevice
	SkippedVFs        int
	UnreadableDevices map[string]error
	Complete          bool
}

// DiscoverDevices enumerates IB/RoCE devices using the normal discovery flow.
// SR-IOV VFs are counted but excluded and exclusionRegex filters device names.
func DiscoverDevices(reader sysfs.Reader, exclusionRegex string) (*DiscoveryResult, error) {
	return DiscoverDevicesWithOverride(reader, exclusionRegex, "")
}

// DiscoverDevicesWithOverride enumerates all IB/RoCE devices from sysfs,
// parsing each device's metadata and ports.
// When inclusionRegexOverride contains at least one usable pattern, only
// matching names are returned and all automatic device filters, including
// exclusionRegex and the VF filter, are bypassed.
func DiscoverDevicesWithOverride(
	reader sysfs.Reader,
	exclusionRegex string,
	inclusionRegexOverride string,
) (*DiscoveryResult, error) {
	ibPath := reader.IBBasePath()

	entries, err := reader.ListDirs(ibPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// Absence of the top-level tree is normal on nodes without IB/RoCE,
			// but is an incomplete observation when a caller already has state.
			// Preserve that distinction instead of reporting a successful empty
			// enumeration that can fabricate mass-disappearance events.
			return &DiscoveryResult{
				Devices:           []IBDevice{},
				UnreadableDevices: map[string]error{},
				Complete:          false,
			}, nil
		}

		return nil, fmt.Errorf("failed to list IB devices at %s: %w", ibPath, err)
	}

	exclusions := compileRegexList(exclusionRegex)
	inclusions := compileRegexList(inclusionRegexOverride)
	// Enable the override only when at least one usable pattern was
	// compiled. Values such as "," or ",," contain no patterns and must
	// fall back to normal discovery instead of silently excluding every
	// device.
	inclusionOverrideEnabled := len(inclusions) > 0

	result := &DiscoveryResult{
		Devices:           make([]IBDevice, 0, len(entries)),
		UnreadableDevices: make(map[string]error),
		Complete:          true,
	}

	for _, devName := range entries {
		dev, skippedVF, readErr := discoverCandidate(
			reader, devName, exclusions, inclusions, inclusionOverrideEnabled,
		)
		if readErr != nil {
			result.UnreadableDevices[devName] = readErr
		}

		if skippedVF {
			result.SkippedVFs++
		}

		if dev != nil {
			result.Devices = append(result.Devices, *dev)
		}
	}

	return result, nil
}

// discoverCandidate applies the configured discovery scope to one device,
// parses devices that remain eligible, and reports normal-flow VFs separately
// so the caller can maintain its skipped count.
func discoverCandidate(
	reader sysfs.Reader,
	devName string,
	exclusions []*regexp.Regexp,
	inclusions []*regexp.Regexp,
	inclusionOverrideEnabled bool,
) (*IBDevice, bool, error) {
	includedByOverride := inclusionOverrideEnabled && matchesAny(devName, inclusions)
	if inclusionOverrideEnabled && !includedByOverride {
		return nil, false, nil
	}

	if !inclusionOverrideEnabled && matchesAny(devName, exclusions) {
		return nil, false, nil
	}

	dev, err := discoverDevice(reader, devName)
	if err != nil {
		slog.Debug("Skipping device", "device", devName, "error", err)

		return nil, false, err
	}

	dev.IncludedByOverride = includedByOverride
	if dev.IsVF && !includedByOverride {
		return nil, true, nil
	}

	return dev, false, nil
}

// discoverDevice gathers identity and port data for a single IB device.
func discoverDevice(reader sysfs.Reader, devName string) (*IBDevice, error) {
	vendor, err := detectVendor(reader, devName)
	if err != nil {
		// An unreadable vendor file must not silently demote the device
		// to "unsupported vendor" (which would drop it from monitoring
		// with no disappearance handling); treat the device as an
		// uncertain observation instead.
		return nil, fmt.Errorf("device %s: %w", devName, err)
	}

	dev := &IBDevice{
		Name:   devName,
		Vendor: vendor,
		IsVF:   reader.IsVirtualFunction(devName),
	}

	if hcaType, err := reader.ReadIBDeviceField(devName, "hca_type"); err == nil {
		dev.HCAType = hcaType
	}

	if fwVer, err := reader.ReadIBDeviceField(devName, "fw_ver"); err == nil {
		dev.FWVersion = fwVer
	}

	dev.NetDev = firstNetDevForIBDevice(reader, devName)

	portsDir := filepath.Join(reader.IBBasePath(), devName, "ports")

	portDirs, err := reader.ListDirs(portsDir)
	if err != nil {
		return nil, fmt.Errorf("failed to list ports for %s: %w", devName, err)
	}

	for _, entry := range portDirs {
		portNum, err := strconv.Atoi(entry)
		if err != nil {
			continue
		}

		port, err := readPort(reader, devName, portNum)
		if err != nil {
			// A port whose critical attributes cannot be read makes the
			// whole device an uncertain observation. Reporting it as
			// parsed would turn the read failure into fabricated health
			// data: an empty state reads as an unhealthy transition, and
			// an empty link_layer silently drops the port from its
			// check's layer — which can fire an undebounced false
			// port-disappearance FATAL. Unreadable devices instead flow
			// into the callers' hold-last-known-state machinery.
			return nil, fmt.Errorf("device %s: %w", devName, err)
		}

		dev.Ports = append(dev.Ports, port)
	}

	return dev, nil
}

// readPort reads the per-port state, phys_state, and link_layer. A read
// error on any of these critical attributes fails the port: "" must
// never double as both "unreadable" and "observed value".
func readPort(reader sysfs.Reader, device string, port int) (IBPort, error) {
	p := IBPort{Device: device, Port: port}

	s, err := reader.ReadIBPortState(device, port)
	if err != nil {
		return p, fmt.Errorf("read port %d state: %w", port, err)
	}

	p.State = sysfs.ParsePortState(s)

	s, err = reader.ReadIBPortPhysState(device, port)
	if err != nil {
		return p, fmt.Errorf("read port %d phys_state: %w", port, err)
	}

	p.PhysicalState = sysfs.ParsePortState(s)

	s, err = reader.ReadIBPortLinkLayer(device, port)
	if err != nil {
		return p, fmt.Errorf("read port %d link_layer: %w", port, err)
	}

	p.LinkLayer = strings.TrimSpace(s)

	return p, nil
}

// detectVendor classifies the IB device's PCI vendor ID. We match only
// Mellanox (0x15b3) today; everything else is reported as Unknown so
// the caller can skip it. A read error is returned as an error — it is
// an observation failure, not evidence of an unsupported vendor.
func detectVendor(reader sysfs.Reader, device string) (Vendor, error) {
	vendorID, err := reader.ReadIBDeviceField(device, "device/vendor")
	if err != nil {
		return VendorUnknown, fmt.Errorf("read vendor: %w", err)
	}

	if strings.TrimSpace(vendorID) == mellanoxPCIVendorID {
		return VendorMellanox, nil
	}

	return VendorUnknown, nil
}

// firstNetDevForIBDevice returns the first entry in
// /sys/class/infiniband/<dev>/device/net/ (e.g., "rdma4", "eth0"), which is
// the associated network interface used for RoCE. Returns "" if the
// directory is missing or empty.
func firstNetDevForIBDevice(reader sysfs.Reader, device string) string {
	netPath := filepath.Join(reader.IBBasePath(), device, "device", "net")

	entries, err := reader.ListDirs(netPath)
	if err != nil || len(entries) == 0 {
		return ""
	}

	return entries[0]
}

// IsSupportedVendor reports whether the device is from a vendor we monitor.
func IsSupportedVendor(dev *IBDevice) bool {
	return dev.Vendor == VendorMellanox
}

// IsIBPort reports whether the port uses the InfiniBand link layer.
func IsIBPort(port *IBPort) bool {
	return strings.EqualFold(port.LinkLayer, topology.LinkLayerInfiniBand)
}

// IsEthernetPort reports whether the port uses the Ethernet (RoCE) link layer.
func IsEthernetPort(port *IBPort) bool {
	return strings.EqualFold(port.LinkLayer, topology.LinkLayerEthernet)
}

// PortEntityValue returns the string representation of a port number used
// in health event entity references.
func PortEntityValue(port int) string {
	return strconv.Itoa(port)
}

// compileRegexList compiles a comma-separated regex list, tolerating
// malformed entries (logged and skipped).
func compileRegexList(commaSeparated string) []*regexp.Regexp {
	if commaSeparated == "" {
		return nil
	}

	var out []*regexp.Regexp

	for _, pat := range strings.Split(commaSeparated, ",") {
		pat = strings.TrimSpace(pat)
		if pat == "" {
			continue
		}

		re, err := regexp.Compile(pat)
		if err != nil {
			slog.Warn("Invalid regex, skipping", "pattern", pat, "error", err)
			continue
		}

		out = append(out, re)
	}

	return out
}

// matchesAny reports whether a name matches any of the supplied regexes.
func matchesAny(name string, patterns []*regexp.Regexp) bool {
	for _, re := range patterns {
		if re.MatchString(name) {
			return true
		}
	}

	return false
}

// MatchesAny is the exported form of matchesAny for callers that want to
// reuse the helper (e.g., the inclusion-override path in main).
func MatchesAny(name, commaSeparated string) bool {
	return matchesAny(name, compileRegexList(commaSeparated))
}
