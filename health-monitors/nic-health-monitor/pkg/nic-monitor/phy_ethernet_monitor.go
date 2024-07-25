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
	"os"
	"path/filepath"
	"strings"

	"k8s.io/klog"
)

const (
	SYS_CLASS_NET_PATH = "/sys/class/net"
)

type EthernetDevice struct {
	Name      string
	Operstate string
}

type EthernetDeviceMonitor struct {
	Devices map[string]EthernetDevice
}

// if this function return err (err != nil), then ignore the bool value
func IsPhyEthernet(dev string) (bool, error) {
	// This is for creating a dummy interface for testing purpose
	// Creating a dummy interface
	//   $ sudo modprobe dummy
	//   $ sudo ip link add dummy1 type dummy
	// Test by up down the interface
	//   $ sudo ip link set dummy1 [ up | down ]
	// Remove dummy interface
	//   $ sudo ip link delete dummy1 type dummy
	//   $ sudo rmmod dummy
	if strings.Contains(dev, "dummy") {
		return true, nil
	}

	// physical device must be exist
	path := filepath.Join(SYS_CLASS_NET_PATH, dev, "device")
	if _, err := fileSystem.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		} else {
			return false, err
		}
	}

	// type must be an ethernet
	path = filepath.Join(SYS_CLASS_NET_PATH, dev, "type")
	if _, err := fileSystem.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		} else {
			return false, err
		}
	}

	devtype, err := fileSystem.ReadFile(path)
	if err != nil {
		return false, err
	}

	if strings.TrimSpace(string(devtype)) != "1" {
		return false, nil
	}

	// must be a master interface
	path = filepath.Join(SYS_CLASS_NET_PATH, dev, "master")
	if _, err := fileSystem.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return true, nil
		} else {
			return false, err
		}
	}

	return false, nil
}

func GetEthernetOperstate(devname string) string {
	path := filepath.Join(SYS_CLASS_NET_PATH, devname, "operstate")

	state, err := fileSystem.ReadFile(path)
	if err != nil {
		return "unknown"
	}

	return strings.TrimSpace(string(state))
}

func GetPhyEthernetDevices() (map[string]EthernetDevice, error) {
	deviceList := map[string]EthernetDevice{}

	dirs, err := fileSystem.ReadDir(SYS_CLASS_NET_PATH)
	if err != nil {
		return nil, err
	}

	for _, device := range dirs {
		deviceName := device.Name()

		isPhy, err := IsPhyEthernet(deviceName)
		if err != nil {
			klog.Errorf("error on IsPhyEthernet(%s): %v", deviceName, err)
		} else if !isPhy {
			continue
		}

		operstate := GetEthernetOperstate(deviceName)

		deviceList[deviceName] = EthernetDevice{deviceName, operstate}
	}

	return deviceList, nil
}

func (m *EthernetDeviceMonitor) Monitor() ([]NicErrorEvent, error) {
	deviceList, err := GetPhyEthernetDevices()
	if err != nil {
		return nil, err
	}

	events := []NicErrorEvent{}

	// Check if any nic device is disappear
	for name := range m.Devices {
		if _, ok := deviceList[name]; !ok {
			events = append(events, NicErrorEvent{
				Ethernet,
				name,
				"state: Not Exist",
			})
		}
	}

	for name, device := range deviceList {
		if old, ok := m.Devices[name]; ok && device.Operstate == old.Operstate {
			continue
		}

		if device.Operstate != "up" {
			events = append(events, NicErrorEvent{
				Ethernet,
				device.Name,
				"state: " + device.Operstate,
			})
		}
	}

	m.Devices = deviceList

	return events, nil
}
