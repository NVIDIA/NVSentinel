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

// MockReader is a function-table-based test double for Reader. Only the
// methods a given test needs are set; the rest return zero values.
type MockReader struct {
	IBBase         string
	NetBase        string
	IBPortPathFunc func(device string, port int) string
	NetIfacePathFn func(iface string) string
	ReadFileFunc   func(path string) (string, error)
	ListDirsFunc   func(path string) ([]string, error)

	ReadIBPortStateFunc     func(device string, port int) (string, error)
	ReadIBPortPhysStateFunc func(device string, port int) (string, error)
	ReadIBPortLinkLayerFunc func(device string, port int) (string, error)
	ReadIBDeviceFieldFunc   func(device, field string) (string, error)

	ReadNetOperStateFunc func(iface string) (string, error)

	ReadIBDeviceNUMAFunc  func(device string) (int, error)
	ReadPCIAddressFunc    func(device string) (string, error)
	IsVirtualFunctionFunc func(device string) bool
}

// Compile-time check.
var _ Reader = (*MockReader)(nil)

func (m *MockReader) IBBasePath() string {
	if m.IBBase == "" {
		return "/sys/class/infiniband"
	}

	return m.IBBase
}

func (m *MockReader) NetBasePath() string {
	if m.NetBase == "" {
		return "/sys/class/net"
	}

	return m.NetBase
}

func (m *MockReader) IBPortPath(device string, port int) string {
	if m.IBPortPathFunc != nil {
		return m.IBPortPathFunc(device, port)
	}

	return ""
}

func (m *MockReader) NetInterfacePath(iface string) string {
	if m.NetIfacePathFn != nil {
		return m.NetIfacePathFn(iface)
	}

	return ""
}

func (m *MockReader) ReadFile(path string) (string, error) {
	if m.ReadFileFunc != nil {
		return m.ReadFileFunc(path)
	}

	return "", nil
}

func (m *MockReader) ListDirs(path string) ([]string, error) {
	if m.ListDirsFunc != nil {
		return m.ListDirsFunc(path)
	}

	return nil, nil
}

func (m *MockReader) ReadIBPortState(device string, port int) (string, error) {
	if m.ReadIBPortStateFunc != nil {
		return m.ReadIBPortStateFunc(device, port)
	}

	return "", nil
}

func (m *MockReader) ReadIBPortPhysState(device string, port int) (string, error) {
	if m.ReadIBPortPhysStateFunc != nil {
		return m.ReadIBPortPhysStateFunc(device, port)
	}

	return "", nil
}

func (m *MockReader) ReadIBPortLinkLayer(device string, port int) (string, error) {
	if m.ReadIBPortLinkLayerFunc != nil {
		return m.ReadIBPortLinkLayerFunc(device, port)
	}

	return "", nil
}

func (m *MockReader) ReadIBDeviceField(device, field string) (string, error) {
	if m.ReadIBDeviceFieldFunc != nil {
		return m.ReadIBDeviceFieldFunc(device, field)
	}

	return "", nil
}

func (m *MockReader) ReadNetOperState(iface string) (string, error) {
	if m.ReadNetOperStateFunc != nil {
		return m.ReadNetOperStateFunc(iface)
	}

	return "", nil
}

func (m *MockReader) ReadIBDeviceNUMANode(device string) (int, error) {
	if m.ReadIBDeviceNUMAFunc != nil {
		return m.ReadIBDeviceNUMAFunc(device)
	}

	return 0, nil
}

func (m *MockReader) ReadPCIAddress(device string) (string, error) {
	if m.ReadPCIAddressFunc != nil {
		return m.ReadPCIAddressFunc(device)
	}

	return "", nil
}

func (m *MockReader) IsVirtualFunction(device string) bool {
	if m.IsVirtualFunctionFunc != nil {
		return m.IsVirtualFunctionFunc(device)
	}

	return false
}
