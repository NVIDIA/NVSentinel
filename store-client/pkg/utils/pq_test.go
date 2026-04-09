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

package utils

import "testing"

func TestQuotePQValue(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"simple", "secret", "secret"},
		{"empty", "", ""},
		{"space", "my secret", "'my secret'"},
		{"tab", "abc\tdef", "'abc\tdef'"},
		{"newline", "abc\ndef", "'abc\ndef'"},
		{"backslash", `ab\cd`, `'ab\\cd'`},
		{"single_quote", "it's", `'it\'s'`},
		{"mixed", "a b\\c'd\te", `'a b\\c\'d` + "\t" + `e'`},
		{"only_backslash", `\`, `'\\'`},
		{"only_quote", "'", `'\''`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := QuotePQValue(tt.in)
			if got != tt.want {
				t.Errorf("QuotePQValue(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
