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

package patterns

import "regexp"

// XIDPattern matches standard NVIDIA XID error messages in the format:
// "NVRM: Xid (PCI:0000:b3:00.0): 79, pid=1234, name=process, Ch 00000001"
// This pattern is the canonical definition used across all handlers for
// detecting and parsing XID errors. If the XID format changes, this is the
// single source of truth that needs to be updated.
var XIDPattern = regexp.MustCompile(
	`NVRM: Xid \(PCI:([0-9a-fA-F:.]+)\): (\d+)(?:, pid=(\d+))?(?:, name=([^,]+))?(?:, Ch ([0-9a-fA-F]+))?`,
)
