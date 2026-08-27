// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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

package helpers

// Values shared by the e2e test helpers when building fixture workloads.
const (
	// busyboxImage is the image backing the throwaway pods these helpers create.
	busyboxImage = "busybox:latest"
	// shellBinary is the shell used to run fixture container commands.
	shellBinary = "/bin/sh"
	// sleepForever keeps a fixture container running until the test tears it down.
	sleepForever = "sleep 3600"
	// containerNameMain is the primary container name in fixture pod specs.
	containerNameMain = "main"

	// labelApp and labelTest are the pod labels fixture workloads are selected by.
	labelApp  = "app"
	labelTest = "test"

	// mongodContainerName is the mongod container in the MongoDB stateful set.
	mongodContainerName = "mongod"

	// metadataVersion is the schema version stamped on fixture GPU metadata.
	metadataVersion = "1.0"
	// deviceNameA100 is the GPU device name used by fixture metadata.
	deviceNameA100 = "NVIDIA A100"
	// nvSwitchPCIAddress is the NVSwitch PCI address used by fixture metadata.
	nvSwitchPCIAddress = "0000:c3:00.0"
)
