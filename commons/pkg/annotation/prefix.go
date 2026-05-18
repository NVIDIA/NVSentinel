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

package annotation

import "strings"

// HasPrefixMatch checks if any key in the annotations map starts with any of the provided prefixes.
// Returns the matching annotation key and value if found, or empty strings if no match.
func HasPrefixMatch(annotations map[string]string, prefixes []string) (string, string, bool) {
	for key, value := range annotations {
		for _, prefix := range prefixes {
			if strings.HasPrefix(key, prefix) {
				return key, value, true
			}
		}
	}

	return "", "", false
}

// FindAllPrefixMatches returns all annotation keys and values that match any of the provided prefixes.
// Returns a map of matching annotation keys to their values.
func FindAllPrefixMatches(annotations map[string]string, prefixes []string) map[string]string {
	matches := make(map[string]string)

	for key, value := range annotations {
		for _, prefix := range prefixes {
			if strings.HasPrefix(key, prefix) {
				matches[key] = value
				break
			}
		}
	}

	return matches
}

// IsEmptyValue reports whether an annotation value should be treated as absent.
// Kubernetes annotations are strings; some controllers serialize an empty list as "[]".
func IsEmptyValue(value string) bool {
	trimmed := strings.TrimSpace(value)
	return trimmed == "" || trimmed == "[]"
}
