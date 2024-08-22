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
	"path/filepath"
	"strings"
)

const (
	SYS_CLASS_INFINIBAND_PATH = "/sys/class/infiniband"
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

func GetInfinibandDevices(exclusionRegexList []string) (map[string]InfiniBandDevice, error) {
	deviceList := map[string]InfiniBandDevice{}

	dirs, err := fileSystem.ReadDir(SYS_CLASS_INFINIBAND_PATH)
	if err != nil {
		return nil, err
	}

	for _, device := range dirs {
		deviceName := device.Name()

		if isExcluded(deviceName, exclusionRegexList) {
			continue
		}

		ports, err := GetInfinibandPorts(deviceName)
		if err != nil {
			return nil, err
		}

		deviceList[deviceName] = InfiniBandDevice{deviceName, ports}
	}

	return deviceList, nil
}

func GetInfinibandPorts(devName string) (map[string]InfiniBandPort, error) {
	portList := map[string]InfiniBandPort{}

	path := filepath.Join(SYS_CLASS_INFINIBAND_PATH, devName, "ports")

	dirs, err := fileSystem.ReadDir(path)
	if err != nil {
		return nil, err
	}

	for _, port := range dirs {
		portName := port.Name()
		state := GetPortState(devName, portName)
		phystate := GetPortPhysState(devName, portName)
		portList[portName] = InfiniBandPort{portName, state, phystate}
	}

	return portList, nil
}

func GetPortState(devname, portName string) string {
	path := filepath.Join(SYS_CLASS_INFINIBAND_PATH, devname, "ports", portName, "state")

	state, err := fileSystem.ReadFile(path)
	if err != nil {
		return "-1: Unknown"
	}

	return strings.TrimSpace(string(state))
}

func GetPortPhysState(devname, portName string) string {
	path := filepath.Join(SYS_CLASS_INFINIBAND_PATH, devname, "ports", portName, "phys_state")

	state, err := fileSystem.ReadFile(path)
	if err != nil {
		return "-1: Unknown"
	}

	return strings.TrimSpace(string(state))
}

// nolint: gocognit, cyclop
func (m *InfinibandDeviceMonitor) Monitor(config *NicMonitorConfig) ([]NicErrorEvent, error) {
	deviceList, err := GetInfinibandDevices(config.ExclusionRegexes)
	if err != nil {
		return nil, err
	}

	events := []NicErrorEvent{}

	// Check if any nic device is disappeared
	for name := range m.Devices {
		device, ok := deviceList[name]
		if !ok {
			events = append(events, NicErrorEvent{
				Ethernet,
				name,
				"state: Not Exist",
			})

			continue
		}

		// Check if any port is disappeared
		for portName := range m.Devices[name].Ports {
			if _, ok := device.Ports[portName]; !ok {
				events = append(events, NicErrorEvent{
					Ethernet,
					name + "_" + portName,
					"state: Not Exist",
				})
			}
		}
	}

	for deviceName, device := range deviceList {
		oldDevice, oldDeviceExist := m.Devices[deviceName]

		for portName, port := range device.Ports {
			var oldPort InfiniBandPort

			oldPortExist := oldDeviceExist

			if oldDeviceExist {
				oldPort, oldPortExist = oldDevice.Ports[portName]
			}

			if !oldPortExist || port.State != oldPort.State {
				if port.State != "4: ACTIVE" {
					events = append(events, NicErrorEvent{
						Infiniband,
						device.Name + "_" + port.Name,
						"state: " + port.State,
					})
				}
			}

			if !oldPortExist || port.PhysState != oldPort.PhysState {
				if port.PhysState != "5: LinkUp" {
					events = append(events, NicErrorEvent{
						Infiniband,
						device.Name + "_" + port.Name,
						"phys_state: " + port.PhysState,
					})
				}
			}
		}
	}

	m.Devices = deviceList

	return events, nil
}
