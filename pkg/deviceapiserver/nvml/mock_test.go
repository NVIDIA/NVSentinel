// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

//go:build nvml

package nvml

import (
	"sync"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
)

// MockLibrary is a mock implementation of Library for testing.
type MockLibrary struct {
	// Init behavior
	InitReturn nvml.Return

	// Shutdown behavior
	ShutdownReturn nvml.Return

	// SystemGetDriverVersion behavior
	DriverVersion       string
	DriverVersionReturn nvml.Return

	// DeviceGetCount behavior
	DeviceCount       int
	DeviceCountReturn nvml.Return

	// Devices returns mock devices by index
	Devices map[int]*MockDevice

	// EventSetCreate behavior
	EventSet             *MockEventSet
	EventSetCreateReturn nvml.Return

	// Track calls for verification
	mu             sync.Mutex
	InitCalled     bool
	ShutdownCalled bool
}

// NewMockLibrary creates a new mock Library with defaults.
func NewMockLibrary() *MockLibrary {
	return &MockLibrary{
		InitReturn:           nvml.SUCCESS,
		ShutdownReturn:       nvml.SUCCESS,
		DriverVersion:        "535.104.05",
		DriverVersionReturn:  nvml.SUCCESS,
		DeviceCount:          0,
		DeviceCountReturn:    nvml.SUCCESS,
		Devices:              make(map[int]*MockDevice),
		EventSetCreateReturn: nvml.SUCCESS,
	}
}

// AddDevice adds a mock device at the specified index.
func (m *MockLibrary) AddDevice(index int, device *MockDevice) {
	m.Devices[index] = device
	m.DeviceCount = len(m.Devices)
}

// Init implements Library.
func (m *MockLibrary) Init() nvml.Return {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.InitCalled = true

	return m.InitReturn
}

// Shutdown implements Library.
func (m *MockLibrary) Shutdown() nvml.Return {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ShutdownCalled = true

	return m.ShutdownReturn
}

// SystemGetDriverVersion implements Library.
func (m *MockLibrary) SystemGetDriverVersion() (string, nvml.Return) {
	return m.DriverVersion, m.DriverVersionReturn
}

// DeviceGetCount implements Library.
func (m *MockLibrary) DeviceGetCount() (int, nvml.Return) {
	return m.DeviceCount, m.DeviceCountReturn
}

// DeviceGetHandleByIndex implements Library.
func (m *MockLibrary) DeviceGetHandleByIndex(index int) (Device, nvml.Return) {
	if device, ok := m.Devices[index]; ok {
		return device, nvml.SUCCESS
	}

	return nil, nvml.ERROR_NOT_FOUND
}

// EventSetCreate implements Library.
func (m *MockLibrary) EventSetCreate() (EventSet, nvml.Return) {
	if m.EventSet == nil {
		m.EventSet = NewMockEventSet()
	}

	return m.EventSet, m.EventSetCreateReturn
}

// MockDevice is a mock implementation of Device.
type MockDevice struct {
	UUID                  string
	UUIDReturn            nvml.Return
	Name                  string
	NameReturn            nvml.Return
	MemoryInfo            nvml.Memory
	MemoryInfoReturn      nvml.Return
	SupportedEvents       uint64
	SupportedEventsReturn nvml.Return
	RegisterEventsReturn  nvml.Return
}

// NewMockDevice creates a new mock device with sensible defaults.
func NewMockDevice(uuid, name string) *MockDevice {
	return &MockDevice{
		UUID:       uuid,
		UUIDReturn: nvml.SUCCESS,
		Name:       name,
		NameReturn: nvml.SUCCESS,
		MemoryInfo: nvml.Memory{
			Total: 16 * 1024 * 1024 * 1024, // 16 GB
			Free:  15 * 1024 * 1024 * 1024,
			Used:  1 * 1024 * 1024 * 1024,
		},
		MemoryInfoReturn:      nvml.SUCCESS,
		SupportedEvents:       uint64(nvml.EventTypeXidCriticalError | nvml.EventTypeDoubleBitEccError),
		SupportedEventsReturn: nvml.SUCCESS,
		RegisterEventsReturn:  nvml.SUCCESS,
	}
}

// GetUUID implements Device.
func (d *MockDevice) GetUUID() (string, nvml.Return) {
	return d.UUID, d.UUIDReturn
}

// GetName implements Device.
func (d *MockDevice) GetName() (string, nvml.Return) {
	return d.Name, d.NameReturn
}

// GetMemoryInfo implements Device.
func (d *MockDevice) GetMemoryInfo() (nvml.Memory, nvml.Return) {
	return d.MemoryInfo, d.MemoryInfoReturn
}

// GetSupportedEventTypes implements Device.
func (d *MockDevice) GetSupportedEventTypes() (uint64, nvml.Return) {
	return d.SupportedEvents, d.SupportedEventsReturn
}

// RegisterEvents implements Device.
func (d *MockDevice) RegisterEvents(_ uint64, _ nvml.EventSet) nvml.Return {
	return d.RegisterEventsReturn
}

// MockEventSet is a mock implementation of EventSet.
type MockEventSet struct {
	mu         sync.Mutex
	events     []nvml.EventData
	eventIdx   int
	WaitReturn nvml.Return
	FreeReturn nvml.Return
	Freed      bool
}

// NewMockEventSet creates a new mock event set.
func NewMockEventSet() *MockEventSet {
	return &MockEventSet{
		events:     make([]nvml.EventData, 0),
		WaitReturn: nvml.ERROR_TIMEOUT,
		FreeReturn: nvml.SUCCESS,
	}
}

// AddEvent adds an event to be returned by Wait.
func (e *MockEventSet) AddEvent(event nvml.EventData) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.events = append(e.events, event)
}

// Wait implements EventSet.
func (e *MockEventSet) Wait(_ uint32) (nvml.EventData, nvml.Return) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.eventIdx < len(e.events) {
		event := e.events[e.eventIdx]
		e.eventIdx++

		return event, nvml.SUCCESS
	}

	return nvml.EventData{}, e.WaitReturn
}

// Free implements EventSet.
func (e *MockEventSet) Free() nvml.Return {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.Freed = true

	return e.FreeReturn
}

// Raw implements EventSet - returns nil for mocks since we don't need real event set.
func (e *MockEventSet) Raw() nvml.EventSet {
	return nil
}

// Compile-time interface checks.
var (
	_ Library  = (*MockLibrary)(nil)
	_ Device   = (*MockDevice)(nil)
	_ EventSet = (*MockEventSet)(nil)
)
