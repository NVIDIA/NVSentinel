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

package cancellation

// Resolver maps a source error code to the error codes it cancels.
// The nil map is a valid empty resolver (lookups return nil).
type Resolver map[string][]string

// NewResolver returns nil for a nil or disabled check.
func NewResolver(check *CheckCancellations) Resolver {
	if check == nil || !check.Enabled {
		return nil
	}

	rules := make(Resolver, len(check.Rules))
	for _, rule := range check.Rules {
		rules[rule.OnErrorCode] = rule.CancelErrorCodes
	}

	return rules
}
