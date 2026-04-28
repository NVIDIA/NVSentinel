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

package topology

// DefaultProcNetRoutePath is the host's /proc/net/route as seen inside
// the container (the DaemonSet bind-mounts /proc at /nvsentinel/proc).
const DefaultProcNetRoutePath = "/nvsentinel/proc/net/route"

// Role is one of Compute, Storage, or Management.
type Role int

const (
	RoleCompute Role = iota
	RoleStorage
	RoleManagement
)

// String returns a short label used in logs and event messages.
func (r Role) String() string {
	switch r {
	case RoleCompute:
		return "compute"
	case RoleStorage:
		return "storage"
	case RoleManagement:
		return "management"
	default:
		return "unknown"
	}
}

// Recognised topology level strings from `nvidia-smi topo -m`.
const (
	LevelSelf = "X"
	LevelPIX  = "PIX"
	LevelPXB  = "PXB"
	LevelPHB  = "PHB"
	LevelNODE = "NODE"
	LevelSYS  = "SYS"
)

// blueFieldHCATypes lists HCA type prefixes for BlueField DPUs.
// DPU NICs are managed by SmartNIC firmware and excluded from monitoring.
var blueFieldHCATypes = []string{
	"MT41682",
	"MT41686",
	"MT41692",
}

// Link-layer type constants shared across packages.
const (
	LinkLayerInfiniBand = "InfiniBand"
	LinkLayerEthernet   = "Ethernet"
)
