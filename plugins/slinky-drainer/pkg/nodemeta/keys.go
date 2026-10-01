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

// Package nodemeta holds the Node label and annotation keys that Slinky Drainer
// reads and writes. The reconciler and the cache configuration must agree on
// this set: the cache keeps only these keys, so a reconciler that reads a key
// absent from here reads an empty value.
//
// External automation keys off these names. Treat them as public API and add
// new keys alongside the existing ones rather than renaming them.
package nodemeta

const (
	// StateLabelKey marks a node that NVSentinel is remediating. Slinky Drainer
	// removes its cordon reason once this label is gone.
	StateLabelKey = "dgxc.nvidia.com/nvsentinel-state"

	// CordonReasonAnnotationKey carries the reason the Slinky operator shows for
	// a cordoned node. Slinky Drainer owns the value only when it starts with
	// the NVSentinel prefix.
	CordonReasonAnnotationKey = "nodeset.slinky.slurm.net/node-cordon-reason"
)
