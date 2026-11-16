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

package ruleset

import (
	"errors"
	"fmt"

	"github.com/BurntSushi/toml"
)

// CheckUndecoded reports TOML keys under ruleSets that the config type did not
// decode. outcomeKey is the key the component accepts, for example "remediation".
//
// The TOML decoder skips a key that is not a field. A drain table in the
// fault-remediation config would otherwise disappear, and startup would not
// say why. The error names that key. The same check catches a misspelled
// field, such as priorty.
//
// Keys outside ruleSets are ignored. A check of every key would reject
// configuration this version does not know about, and old and new versions
// run at the same time during an upgrade.
func CheckUndecoded(md toml.MetaData, outcomeKey string) error {
	var unknown []toml.Key

	for _, key := range md.Undecoded() {
		if underRuleSets(key) {
			unknown = append(unknown, key)
		}
	}

	var errs []error

	seen := map[string]struct{}{}

	for _, key := range unknown {
		if hasUndecodedParent(key, unknown) {
			continue
		}

		text := key.String()
		if _, ok := seen[text]; ok {
			continue
		}

		seen[text] = struct{}{}
		errs = append(errs, fmt.Errorf("%s: %w (component outcome key is %q)", text, ErrUnknownKey, outcomeKey))
	}

	return errors.Join(errs...)
}

func underRuleSets(key toml.Key) bool {
	return len(key) >= 2 && key[0] == ruleSetsKey
}

func hasUndecodedParent(key toml.Key, unknown []toml.Key) bool {
	for _, other := range unknown {
		if len(other) < len(key) && keyHasPrefix(key, other) {
			return true
		}
	}

	return false
}

func keyHasPrefix(key, prefix toml.Key) bool {
	if len(prefix) > len(key) {
		return false
	}

	for i := range prefix {
		if key[i] != prefix[i] {
			return false
		}
	}

	return true
}
