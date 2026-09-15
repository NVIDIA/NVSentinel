// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

package nicdriver

import "regexp"

// reBDF matches a PCI Bus:Device.Function address like "0000:3b:00.0".
// Case-insensitive hex to handle test fixtures and out-of-tree log sources.
var reBDF = regexp.MustCompile(`\b([0-9a-fA-F]{4}:[0-9a-fA-F]{2}:[0-9a-fA-F]{2}\.[0-9a-fA-F])\b`)

// extractBDF returns the first PCI BDF address found in the log line.
func extractBDF(line string) (string, bool) {
	m := reBDF.FindStringSubmatch(line)
	if len(m) < 2 {
		return "", false
	}

	return m[1], true
}

// reIBDevice matches the "infiniband <ibdev>:" prefix that ibdev_err()
// stamps on RDMA-device-scoped kernel messages (e.g. the efa driver's
// admin queue errors), yielding the sysfs device name.
var reIBDevice = regexp.MustCompile(`\binfiniband ([A-Za-z0-9_.-]+):`)

func extractIBDevice(line string) (string, bool) {
	m := reIBDevice.FindStringSubmatch(line)
	if len(m) < 2 {
		return "", false
	}

	return m[1], true
}
