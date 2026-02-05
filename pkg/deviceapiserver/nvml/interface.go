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
	"github.com/NVIDIA/go-nvml/pkg/nvml"
)

// Library is the interface for NVML library operations.
// This interface contains only the methods used by the Provider,
// making it easier to mock for testing.
type Library interface {
	Init() nvml.Return
	Shutdown() nvml.Return
	SystemGetDriverVersion() (string, nvml.Return)
	DeviceGetCount() (int, nvml.Return)
	DeviceGetHandleByIndex(index int) (Device, nvml.Return)
	EventSetCreate() (EventSet, nvml.Return)
}

// Device is the interface for NVML device operations.
type Device interface {
	GetUUID() (string, nvml.Return)
	GetName() (string, nvml.Return)
	GetMemoryInfo() (nvml.Memory, nvml.Return)
	GetSupportedEventTypes() (uint64, nvml.Return)
	RegisterEvents(eventTypes uint64, set nvml.EventSet) nvml.Return
}

// EventSet is the interface for NVML event set operations.
type EventSet interface {
	Wait(timeout uint32) (nvml.EventData, nvml.Return)
	Free() nvml.Return
	// Raw returns the underlying nvml.EventSet for use with RegisterEvents.
	Raw() nvml.EventSet
}

// nvmlLibraryWrapper wraps the real nvml.Interface to implement Library.
type nvmlLibraryWrapper struct {
	lib nvml.Interface
}

// NewLibraryWrapper creates a Library wrapper around an nvml.Interface.
func NewLibraryWrapper(lib nvml.Interface) Library {
	return &nvmlLibraryWrapper{lib: lib}
}

func (w *nvmlLibraryWrapper) Init() nvml.Return {
	return w.lib.Init()
}

func (w *nvmlLibraryWrapper) Shutdown() nvml.Return {
	return w.lib.Shutdown()
}

func (w *nvmlLibraryWrapper) SystemGetDriverVersion() (string, nvml.Return) {
	return w.lib.SystemGetDriverVersion()
}

func (w *nvmlLibraryWrapper) DeviceGetCount() (int, nvml.Return) {
	return w.lib.DeviceGetCount()
}

func (w *nvmlLibraryWrapper) DeviceGetHandleByIndex(index int) (Device, nvml.Return) {
	device, ret := w.lib.DeviceGetHandleByIndex(index)
	if ret != nvml.SUCCESS {
		return nil, ret
	}

	return &nvmlDeviceWrapper{device: device}, ret
}

func (w *nvmlLibraryWrapper) EventSetCreate() (EventSet, nvml.Return) {
	es, ret := w.lib.EventSetCreate()
	if ret != nvml.SUCCESS {
		return nil, ret
	}

	return &nvmlEventSetWrapper{es: es}, ret
}

// nvmlDeviceWrapper wraps nvml.Device to implement Device.
type nvmlDeviceWrapper struct {
	device nvml.Device
}

func (w *nvmlDeviceWrapper) GetUUID() (string, nvml.Return) {
	return w.device.GetUUID()
}

func (w *nvmlDeviceWrapper) GetName() (string, nvml.Return) {
	return w.device.GetName()
}

func (w *nvmlDeviceWrapper) GetMemoryInfo() (nvml.Memory, nvml.Return) {
	return w.device.GetMemoryInfo()
}

func (w *nvmlDeviceWrapper) GetSupportedEventTypes() (uint64, nvml.Return) {
	return w.device.GetSupportedEventTypes()
}

func (w *nvmlDeviceWrapper) RegisterEvents(eventTypes uint64, set nvml.EventSet) nvml.Return {
	return w.device.RegisterEvents(eventTypes, set)
}

// nvmlEventSetWrapper wraps nvml.EventSet to implement EventSet.
type nvmlEventSetWrapper struct {
	es nvml.EventSet
}

func (w *nvmlEventSetWrapper) Wait(timeout uint32) (nvml.EventData, nvml.Return) {
	return w.es.Wait(timeout)
}

func (w *nvmlEventSetWrapper) Free() nvml.Return {
	return w.es.Free()
}

// Raw returns the underlying nvml.EventSet for use with device.RegisterEvents.
// This is needed because RegisterEvents expects the concrete nvml.EventSet type.
func (w *nvmlEventSetWrapper) Raw() nvml.EventSet {
	return w.es
}
