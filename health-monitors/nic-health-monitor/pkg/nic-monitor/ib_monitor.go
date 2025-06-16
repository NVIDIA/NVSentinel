/*
 * Copyright (c) 2024, NVIDIA CORPORATION.  All rights reserved.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package nic_monitor

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"k8s.io/klog"
)

const (

	// port healthy message
	portIsHealthy = "Port is healthy"

	// NIC is detected
	nicIsDetected = "IB NIC is detected"

	// infiniband states
	stateActive    = "4: ACTIVE"
	phyStateLinkup = "5: LinkUp"
)

type InfiniBandPort struct {
	Name      string
	State     string
	PhysState string
}

type InfiniBandDevice struct {
	Name  string
	Ports map[string]InfiniBandPort
}

type InfinibandDeviceMonitor struct {
	Devices map[string]InfiniBandDevice
}

func GetInfinibandDevices(exclusionRegexList []string,
	sysClassInfinibandPath string) (map[string]InfiniBandDevice, error) {
	deviceList := map[string]InfiniBandDevice{}

	dirs, err := fileSystem.ReadDir(sysClassInfinibandPath)
	if err != nil {
		return nil, err
	}

	for _, device := range dirs {
		deviceName := device.Name()

		if isExcluded(deviceName, exclusionRegexList) {
			continue
		}

		ports, err := GetInfinibandPorts(deviceName, sysClassInfinibandPath)
		if err != nil {
			return nil, err
		}

		deviceList[deviceName] = InfiniBandDevice{deviceName, ports}
	}

	return deviceList, nil
}

func GetInfinibandPorts(devName string, sysClassInfinibandPath string) (map[string]InfiniBandPort, error) {
	portList := map[string]InfiniBandPort{}

	path := filepath.Join(sysClassInfinibandPath, devName, "ports")

	dirs, err := fileSystem.ReadDir(path)
	if err != nil {
		return nil, err
	}

	for _, port := range dirs {
		portName := port.Name()
		state := GetPortState(devName, portName, sysClassInfinibandPath)
		phystate := GetPortPhysState(devName, portName, sysClassInfinibandPath)
		portList[portName] = InfiniBandPort{portName, state, phystate}
	}

	return portList, nil
}

func GetPortState(devname, portName string, sysClassInfinibandPath string) string {
	path := filepath.Join(sysClassInfinibandPath, devname, "ports", portName, "state")

	state, err := fileSystem.ReadFile(path)
	if err != nil {
		return "-1: Unknown"
	}

	return strings.TrimSpace(string(state))
}

func GetPortPhysState(devname, portName string, sysClassInfinibandPath string) string {
	path := filepath.Join(sysClassInfinibandPath, devname, "ports", portName, "phys_state")

	state, err := fileSystem.ReadFile(path)
	if err != nil {
		return "-1: Unknown"
	}

	return strings.TrimSpace(string(state))
}

func getLinkLayer(config *NicMonitorConfig, ibDeviceName string, portName string) (string, error) {
	linkLayerPath := filepath.Join(config.SysClassInfinibandPath, ibDeviceName, "ports", portName, "link_layer")

	content, err := fileSystem.ReadFile(linkLayerPath)
	if err != nil {
		return "", fmt.Errorf("failed to read link_layer for %s port %s: %w", ibDeviceName, portName, err)
	}

	return strings.TrimSpace(string(content)), nil
}

// hasMatchingRoCEInterface checks if an IB device has any network interfaces
// matching the specified regex patterns in /sys/class/infiniband/<device>/device/net
func hasMatchingRoCEInterface(
	ibDeviceName string, roCEInterfaceRegexes []string, config *NicMonitorConfig) (bool, error) {
	if len(roCEInterfaceRegexes) == 0 {
		// If no regex specified, allow all devices
		return true, nil
	}

	netPath := filepath.Join(config.SysClassInfinibandPath, ibDeviceName, "device", "net")

	// Check if the net directory exists
	entries, err := fileSystem.ReadDir(netPath)
	if err != nil {
		if os.IsNotExist(err) {
			// No net directory means no network interfaces for this device
			return false, nil
		}

		return false, fmt.Errorf("failed to read net directory for %s: %w", ibDeviceName, err)
	}

	// Check each network interface against each regex
	for _, entry := range entries {
		interfaceName := entry.Name()

		for _, regexPattern := range roCEInterfaceRegexes {
			regex, err := regexp.Compile(regexPattern)
			if err != nil {
				return false, fmt.Errorf("invalid RoCE interface regex '%s': %w", regexPattern, err)
			}

			if regex.MatchString(interfaceName) {
				return true, nil
			}
		}
	}

	return false, nil
}

// nolint: gocognit, cyclop
func (m *InfinibandDeviceMonitor) Monitor(config *NicMonitorConfig) ([]NicHealthEvent, error) {
	deviceList, err := GetInfinibandDevices(config.ExclusionRegexes, config.SysClassInfinibandPath)
	if err != nil {
		return nil, err
	}

	events := []NicHealthEvent{}

	// Check if any nic device is disappeared
	for name := range m.Devices {
		device, ok := deviceList[name]
		if !ok {
			events = append(events, NicHealthEvent{
				NicType:        Infiniband,
				Name:           name,
				Message:        doesNotExistState,
				IsHealthyEvent: false,
				LinkLayer:      UNKNOWN_LINK_LAYER, // Device-level event, no specific port
			})

			continue
		}

		// Check if any port is disappeared
		for portName := range m.Devices[name].Ports {
			if _, ok := device.Ports[portName]; !ok {
				// Try to get link layer, but use "unknown" if we can't determine it
				linkLayer := UNKNOWN_LINK_LAYER
				if ll, err := getLinkLayer(config, name, portName); err == nil {
					linkLayer = ll
				}

				events = append(events, NicHealthEvent{
					NicType:        Infiniband,
					Name:           name + "_" + portName,
					Message:        doesNotExistState,
					IsHealthyEvent: false,
					LinkLayer:      linkLayer,
				})
			}
		}
	}

	for deviceName, device := range deviceList {
		oldDevice, oldDeviceExist := m.Devices[deviceName]

		if !oldDeviceExist {
			events = append(events, NicHealthEvent{
				NicType:        Infiniband,
				Name:           device.Name,
				Message:        nicIsDetected,
				IsHealthyEvent: true,
				LinkLayer:      UNKNOWN_LINK_LAYER, // Device-level event, no specific port
			})
		}

		// Check RoCE interface filter if MonitorNetworkType is RoCE
		if config.MonitorNetworkType == MonitorNetworkTypeRoCE {
			hasMatch, err := hasMatchingRoCEInterface(device.Name, config.RoCEInterfaceRegexes, config)
			if err != nil {
				klog.Warningf(
					"Could not check RoCE interfaces for device %s: %v, skipping device",
					device.Name,
					err,
				)

				continue
			}

			if !hasMatch {
				// Device doesn't have matching RoCE interfaces, skip it
				continue
			}
		}

		for portName, port := range device.Ports {
			// Get link layer for this port
			linkLayer, err := getLinkLayer(config, device.Name, portName)
			if err != nil {
				klog.Warningf(
					"Could not determine link_layer for IB port %s on device %s: %v, treating as unknown",
					portName,
					device.Name,
					err,
				)

				// Default to unknown if we can't read link layer
				linkLayer = UNKNOWN_LINK_LAYER
			}

			// Apply RoCE interface filtering for Ethernet/unknown link layers
			// For InfiniBand link layer, no additional filtering is needed
			if linkLayer == "Ethernet" || linkLayer == UNKNOWN_LINK_LAYER {
				// Check if any network interface for this device matches RoCE patterns
				hasMatch, err := hasMatchingRoCEInterface(
					device.Name,
					config.RoCEInterfaceRegexes,
					config,
				)
				if err != nil {
					klog.Warningf(
						"Could not check RoCE interfaces for device %s with %s link layer: %v, skipping port %s",
						device.Name,
						linkLayer,
						err,
						portName,
					)

					continue
				}

				if !hasMatch {
					// Device doesn't have matching RoCE interfaces for Ethernet/unknown link layer, skip this port
					continue
				}
			}

			// Filter based on MonitorNetworkType and link_layer
			if config.MonitorNetworkType == MonitorNetworkTypeRoCE ||
				config.MonitorNetworkType == MonitorNetworkTypeInfiniBand {
				if config.MonitorNetworkType == MonitorNetworkTypeRoCE && linkLayer != "Ethernet" {
					continue
				}

				if config.MonitorNetworkType == MonitorNetworkTypeInfiniBand && linkLayer != "InfiniBand" {
					continue
				}
			}

			var oldPort InfiniBandPort

			oldPortExist := false

			if oldDeviceExist {
				var exists bool
				oldPort, exists = oldDevice.Ports[portName]
				oldPortExist = exists
			}

			// port is new
			//nolint
			if !oldPortExist {
				// if port is new and healthy, then create a healthy event
				if port.State == stateActive && port.PhysState == phyStateLinkup {
					events = append(events, NicHealthEvent{
						NicType:        Infiniband,
						Name:           device.Name + "_" + port.Name,
						Message:        portIsHealthy,
						IsHealthyEvent: true,
						LinkLayer:      linkLayer,
					})
				} else {
					// Port is new and not healthy, create an unhealthy event
					var msgParts []string
					if port.State != stateActive {
						msgParts = append(msgParts, "state: "+port.State)
					}

					if port.PhysState != phyStateLinkup {
						msgParts = append(msgParts, "phys_state: "+port.PhysState)
					}

					msg := strings.Join(msgParts, ", ")

					events = append(events, NicHealthEvent{
						NicType:        Infiniband,
						Name:           device.Name + "_" + port.Name,
						Message:        msg,
						IsHealthyEvent: false,
						LinkLayer:      linkLayer,
					})
				}

				continue
			}

			// old port exists and the state or PhysState have changed
			if port.State != oldPort.State || port.PhysState != oldPort.PhysState {
				if port.State == stateActive && port.PhysState == phyStateLinkup {
					events = append(events, NicHealthEvent{
						NicType:        Infiniband,
						Name:           device.Name + "_" + port.Name,
						Message:        portIsHealthy,
						IsHealthyEvent: true,
						LinkLayer:      linkLayer,
					})

					continue
				}

				var msgParts []string

				if port.State != stateActive {
					msgParts = append(msgParts, "state: "+port.State)
				}

				if port.PhysState != phyStateLinkup {
					msgParts = append(msgParts, "phys_state: "+port.PhysState)
				}

				msg := strings.Join(msgParts, ", ")

				events = append(events, NicHealthEvent{
					NicType:        Infiniband,
					Name:           device.Name + "_" + port.Name,
					Message:        msg,
					IsHealthyEvent: false,
					LinkLayer:      linkLayer,
				})
			}
		}
	}

	m.Devices = deviceList

	return events, nil
}
