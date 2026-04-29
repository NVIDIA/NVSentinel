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

// Package state implements the state-polling checks (InfiniBand and
// Ethernet/RoCE) for the NIC Health Monitor. Each check compares the
// ports it discovers on every poll against an in-memory (and
// optionally persisted) snapshot of the previous poll and emits a
// HealthEvent on every healthy↔unhealthy boundary crossing.
package state

import "fmt"

// portSnapshot captures the logical/physical state of a port at a point
// in time so the check can detect health-boundary crossings.
type portSnapshot struct {
	State         string
	PhysicalState string
	Device        string
	Port          int
}

// portKey returns a unique key for a (device, port) pair used in the
// in-memory state map.
func portKey(device string, port int) string {
	return fmt.Sprintf("%s_%d", device, port)
}
