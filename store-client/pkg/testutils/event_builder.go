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

package testutils

import (
	"crypto/rand"
	"encoding/hex"
)

// generateTestID creates a unique test ID without using database-specific types
func generateTestID() string {
	bytes := make([]byte, 12)
	_, _ = rand.Read(bytes) // ignore error for test utility

	return hex.EncodeToString(bytes)
}

// GenerateTestNodeName creates a unique test node name without database-specific types
func GenerateTestNodeName(prefix string) string {
	return prefix + "-" + generateTestID()[:8]
}
