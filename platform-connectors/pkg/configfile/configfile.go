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

// Package configfile reads the platform connector's config.json, the file the
// platform-connector ConfigMap mounts. Both roles of the binary, the
// node-local DaemonSet and the deployment platform connector, take the
// connectors' settings and the pipeline from it.
package configfile

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
)

// Load reads and decodes the file at path. Numbers stay json.Number, so the
// chart's 20.00 and a hand-written 20 read back as the same value.
func Load(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config %s: %w", path, err)
	}

	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()

	result := map[string]any{}
	if err := dec.Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to unmarshal config %s: %w", path, err)
	}

	// The file is one JSON object; anything after it is a mistake that a
	// plain Unmarshal would have refused too.
	if err := dec.Decode(new(any)); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, fmt.Errorf("failed to unmarshal config %s: more than one JSON value", path)
		}

		return nil, fmt.Errorf("failed to unmarshal config %s: content after the JSON object: %w", path, err)
	}

	return result, nil
}

// Bool reads a toggle, accepting the chart's quoted "true" and a raw bool. A
// missing key or any other value is false.
func Bool(m map[string]any, key string) bool {
	switch v := m[key].(type) {
	case bool:
		return v
	case string:
		return v == "true"
	default:
		return false
	}
}

// String reads an optional string: an absent key or null is "", a present
// value must be a string.
func String(m map[string]any, key string) (string, error) {
	v, ok := m[key]
	if !ok || v == nil {
		return "", nil
	}

	str, isString := v.(string)
	if !isString {
		return "", fmt.Errorf("config key %q must be a string (got %T)", key, v)
	}

	return str, nil
}

// Int64 reads a required whole number.
func Int64(m map[string]any, key string) (int64, error) {
	n, err := number(m, key)
	if err != nil {
		return 0, err
	}

	v, err := n.Int64()
	if err != nil {
		return 0, fmt.Errorf("config key %q: %w", key, err)
	}

	return v, nil
}

// Float64 reads a required number.
func Float64(m map[string]any, key string) (float64, error) {
	n, err := number(m, key)
	if err != nil {
		return 0, err
	}

	v, err := n.Float64()
	if err != nil {
		return 0, fmt.Errorf("config key %q: %w", key, err)
	}

	return v, nil
}

func number(m map[string]any, key string) (json.Number, error) {
	n, ok := m[key].(json.Number)
	if !ok {
		return "", fmt.Errorf("config key %q missing or not a number (got %T)", key, m[key])
	}

	return n, nil
}
