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

import (
	"reflect"
	"testing"
)

func TestHasPrefixMatch(t *testing.T) {
	tests := []struct {
		name        string
		annotations map[string]string
		prefixes    []string
		wantKey     string
		wantValue   string
		wantFound   bool
	}{
		{
			name: "exact match",
			annotations: map[string]string{
				"io.katacontainers.config": "value1",
			},
			prefixes:  []string{"io.katacontainers.config"},
			wantKey:   "io.katacontainers.config",
			wantValue: "value1",
			wantFound: true,
		},
		{
			name: "prefix match with suffix",
			annotations: map[string]string{
				"io.katacontainers.config.foo": "value2",
			},
			prefixes:  []string{"io.katacontainers.config"},
			wantKey:   "io.katacontainers.config.foo",
			wantValue: "value2",
			wantFound: true,
		},
		{
			name: "prefix match with nested suffix",
			annotations: map[string]string{
				"io.katacontainers.config.foo.bar.baz": "nested",
			},
			prefixes:  []string{"io.katacontainers.config"},
			wantKey:   "io.katacontainers.config.foo.bar.baz",
			wantValue: "nested",
			wantFound: true,
		},
		{
			name: "multiple prefixes, second matches",
			annotations: map[string]string{
				"kata-runtime.io/enabled": "true",
			},
			prefixes:  []string{"io.katacontainers.config", "kata-runtime.io/"},
			wantKey:   "kata-runtime.io/enabled",
			wantValue: "true",
			wantFound: true,
		},
		{
			name: "no match",
			annotations: map[string]string{
				"other.annotation": "value",
			},
			prefixes:  []string{"io.katacontainers.config"},
			wantKey:   "",
			wantValue: "",
			wantFound: false,
		},
		{
			name:        "empty annotations",
			annotations: map[string]string{},
			prefixes:    []string{"io.katacontainers.config"},
			wantKey:     "",
			wantValue:   "",
			wantFound:   false,
		},
		{
			name:        "nil annotations",
			annotations: nil,
			prefixes:    []string{"io.katacontainers.config"},
			wantKey:     "",
			wantValue:   "",
			wantFound:   false,
		},
		{
			name: "empty prefixes",
			annotations: map[string]string{
				"io.katacontainers.config": "value",
			},
			prefixes:  []string{},
			wantKey:   "",
			wantValue: "",
			wantFound: false,
		},
		{
			name: "empty string prefix matches everything",
			annotations: map[string]string{
				"any.annotation": "value",
			},
			prefixes:  []string{""},
			wantKey:   "any.annotation",
			wantValue: "value",
			wantFound: true,
		},
		{
			name: "case sensitive match",
			annotations: map[string]string{
				"IO.KataContainers.Config": "value",
			},
			prefixes:  []string{"io.katacontainers.config"},
			wantKey:   "",
			wantValue: "",
			wantFound: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotKey, gotValue, gotFound := HasPrefixMatch(tt.annotations, tt.prefixes)
			if gotFound != tt.wantFound {
				t.Errorf("HasPrefixMatch() gotFound = %v, want %v", gotFound, tt.wantFound)
			}
			if gotKey != tt.wantKey {
				t.Errorf("HasPrefixMatch() gotKey = %v, want %v", gotKey, tt.wantKey)
			}
			if gotValue != tt.wantValue {
				t.Errorf("HasPrefixMatch() gotValue = %v, want %v", gotValue, tt.wantValue)
			}
		})
	}
}

func TestFindAllPrefixMatches(t *testing.T) {
	tests := []struct {
		name        string
		annotations map[string]string
		prefixes    []string
		want        map[string]string
	}{
		{
			name: "single exact match",
			annotations: map[string]string{
				"io.katacontainers.config": "value1",
			},
			prefixes: []string{"io.katacontainers.config"},
			want: map[string]string{
				"io.katacontainers.config": "value1",
			},
		},
		{
			name: "multiple matches same prefix",
			annotations: map[string]string{
				"io.katacontainers.config":     "value1",
				"io.katacontainers.config.foo": "value2",
				"io.katacontainers.config.bar": "value3",
				"other.annotation":             "ignored",
			},
			prefixes: []string{"io.katacontainers.config"},
			want: map[string]string{
				"io.katacontainers.config":     "value1",
				"io.katacontainers.config.foo": "value2",
				"io.katacontainers.config.bar": "value3",
			},
		},
		{
			name: "multiple prefixes",
			annotations: map[string]string{
				"io.katacontainers.config.foo": "kata1",
				"kata-runtime.io/enabled":      "true",
				"other.annotation":             "ignored",
			},
			prefixes: []string{"io.katacontainers.config", "kata-runtime.io/"},
			want: map[string]string{
				"io.katacontainers.config.foo": "kata1",
				"kata-runtime.io/enabled":      "true",
			},
		},
		{
			name: "no matches",
			annotations: map[string]string{
				"other.annotation": "value",
			},
			prefixes: []string{"io.katacontainers.config"},
			want:     map[string]string{},
		},
		{
			name:        "empty annotations",
			annotations: map[string]string{},
			prefixes:    []string{"io.katacontainers.config"},
			want:        map[string]string{},
		},
		{
			name:        "nil annotations",
			annotations: nil,
			prefixes:    []string{"io.katacontainers.config"},
			want:        map[string]string{},
		},
		{
			name: "empty prefixes",
			annotations: map[string]string{
				"io.katacontainers.config": "value",
			},
			prefixes: []string{},
			want:     map[string]string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FindAllPrefixMatches(tt.annotations, tt.prefixes)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("FindAllPrefixMatches() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIsEmptyValue(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  bool
	}{
		{name: "empty string", value: "", want: true},
		{name: "whitespace", value: " \n\t ", want: true},
		{name: "empty JSON array", value: "[]", want: true},
		{name: "empty JSON array with whitespace", value: "  []\n", want: true},
		{name: "non-empty JSON array", value: `[{"checkName":"GpuXidError"}]`, want: false},
		{name: "non-array value", value: "value", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsEmptyValue(tt.value); got != tt.want {
				t.Fatalf("IsEmptyValue(%q) = %v, want %v", tt.value, got, tt.want)
			}
		})
	}
}
