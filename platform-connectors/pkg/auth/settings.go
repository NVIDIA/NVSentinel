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

package auth

import (
	"fmt"

	"github.com/nvidia/nvsentinel/platform-connectors/pkg/configfile"
)

// Settings are the node-binding settings the platform connector's config.json
// carries. The chart writes them once, into the ConfigMap both roles mount;
// the node-local role and the deployment platform connector read them with
// SettingsFromConfig and differ only in what they do with them.
type Settings struct {
	// Enabled is enableNodeBindingAuth. When false the other fields are zero.
	Enabled bool
	// Audience is AuthAudience, the token audience TokenReview verifies.
	Audience string
	// CrossNodeServiceAccounts is AuthCrossNodeServiceAccounts: the
	// identities that may name nodes other than their own.
	CrossNodeServiceAccounts []string
	// Mode is AuthMode, ModeEnforce when absent.
	Mode Mode
	// FailOpenOnUnavailable is AuthFailOpenOnUnavailable, false when absent.
	FailOpenOnUnavailable bool
}

// SettingsFromConfig reads Settings from a config.json map loaded by
// configfile.Load.
//
// enableNodeBindingAuth must be present and exactly true or false. It is not
// defaulted in either direction: guessing "on" would silently enforce against
// a config that never asked for it, and guessing "off" would silently drop the
// check that keeps a publisher on one node from reporting faults about
// another. A ConfigMap that predates the flag is missing the audience and the
// cross-node list too, so it cannot work either way; saying so plainly is more
// useful than inferring an answer. When it is false nothing else is read.
func SettingsFromConfig(raw map[string]any) (Settings, error) {
	enabled, err := nodeBindingEnabled(raw)
	if err != nil {
		return Settings{}, err
	}

	if !enabled {
		return Settings{}, nil
	}

	settings := Settings{Enabled: true}

	if settings.CrossNodeServiceAccounts, err = stringSliceFromConfig(raw, "AuthCrossNodeServiceAccounts"); err != nil {
		return Settings{}, err
	}

	// Every monitor may present a token, not only the cross-node ones, so the
	// audience is required whenever node binding is on: without it no token
	// can be verified and the node claims this check rests on are unreadable.
	if settings.Audience, err = configfile.String(raw, "AuthAudience"); err != nil {
		return Settings{}, err
	}

	if settings.Audience == "" {
		return Settings{}, fmt.Errorf("AuthAudience must be set when node-binding auth is enabled")
	}

	if settings.Mode, err = authMode(raw); err != nil {
		return Settings{}, fmt.Errorf("parse AuthMode: %w", err)
	}

	if settings.FailOpenOnUnavailable, err = boolFromConfig(raw, "AuthFailOpenOnUnavailable"); err != nil {
		return Settings{}, fmt.Errorf("parse AuthFailOpenOnUnavailable: %w", err)
	}

	return settings, nil
}

// nodeBindingEnabled reads enableNodeBindingAuth: true / "true" enables,
// false / "false" disables, anything else (including absent) is an error.
// Values arrive as JSON, where the chart quotes them; an unquoted bool from a
// hand-edited ConfigMap is accepted too.
func nodeBindingEnabled(raw map[string]any) (bool, error) {
	const key = "enableNodeBindingAuth"

	value, present := raw[key]
	if !present {
		return false, fmt.Errorf(
			"%s is not set: it must be true or false. A ConfigMap without it "+
				"predates this platform-connector version and is missing AuthAudience and "+
				"AuthCrossNodeServiceAccounts as well; upgrade the chart rather than "+
				"relying on a default", key)
	}

	return parseBool(key, value)
}

// stringSliceFromConfig reads a JSON array of strings out of the ConfigMap.
//
// A missing key and an explicit null are errors, not empty lists. Silently
// reading either as an empty list would start the connector in a
// configuration where every cluster-scoped monitor is pinned to one node and
// its events rejected, a failure that surfaces far from its cause. Only an
// explicit [] says that on purpose.
func stringSliceFromConfig(raw map[string]any, key string) ([]string, error) {
	value, present := raw[key]
	if !present {
		return nil, fmt.Errorf("%s is not set: it must be a list of canonical "+
			"ServiceAccount usernames, or an explicit empty list", key)
	}

	if value == nil {
		return nil, fmt.Errorf("%s is null: use an explicit empty list to declare "+
			"that no identity is listed", key)
	}

	items, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("%s must be a list of strings, got %T", key, value)
	}

	result := make([]string, 0, len(items))

	for _, item := range items {
		str, ok := item.(string)
		if !ok {
			return nil, fmt.Errorf("%s must be a list of strings, found element of type %T", key, item)
		}

		result = append(result, str)
	}

	return result, nil
}

// authMode reads AuthMode. Absent means ModeEnforce, so that a ConfigMap that
// predates this setting keeps enforcing rather than silently switching to
// audit-only.
func authMode(raw map[string]any) (Mode, error) {
	const key = "AuthMode"

	value, present := raw[key]
	if !present {
		return ModeEnforce, nil
	}

	str, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("%s must be a string, got %#v", key, value)
	}

	switch Mode(str) {
	case ModeEnforce, ModeAudit:
		return Mode(str), nil
	default:
		return "", fmt.Errorf("%s must be %q or %q, got %q", key, ModeEnforce, ModeAudit, str)
	}
}

// boolFromConfig reads a strict boolean, false when absent. Values arrive as
// JSON, where the chart quotes them; an unquoted bool from a hand-edited
// ConfigMap is accepted too.
func boolFromConfig(raw map[string]any, key string) (bool, error) {
	value, present := raw[key]
	if !present {
		return false, nil
	}

	return parseBool(key, value)
}

// parseBool accepts a raw bool or exactly "true" / "false" for key.
func parseBool(key string, value any) (bool, error) {
	switch v := value.(type) {
	case bool:
		return v, nil
	case string:
		switch v {
		case "true":
			return true, nil
		case "false":
			return false, nil
		}
	}

	return false, fmt.Errorf("%s must be true or false, got %#v", key, value)
}
