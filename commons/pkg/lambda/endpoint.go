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

package lambda

import (
	"fmt"
	"net/url"
	"strings"
)

const (
	// DefaultAPIHost is the host of the production Lambda Cloud API.
	DefaultAPIHost = "cloud.lambda.ai"

	// DefaultAPIEndpoint is the production Lambda Cloud API.
	DefaultAPIEndpoint = "https://" + DefaultAPIHost
)

// allowedAPIHosts is every host an endpoint may point at. Anything else fails
// at startup, bounding where the credential can be sent.
var allowedAPIHosts = []string{
	DefaultAPIHost,
	"cloud.lambdastaging.com",
}

// NormalizeEndpoint validates raw as a Lambda API base URL and returns it with
// any trailing slash trimmed. An empty raw yields DefaultAPIEndpoint. setting
// names the configuration key in every error, so the operator is told which
// value to fix rather than which package rejected it.
//
// Callers must run this before constructing a Client: both credentials are
// bearer tokens sent on every request, including the workload identity token
// exchange, so an unvalidated endpoint is an endpoint that can be handed a
// credential.
//
// http is refused outright rather than warned about or gated behind an opt-in:
// there is no configuration under which sending a bearer token in cleartext is
// acceptable.
//
// The host must be one of allowedAPIHosts, so a mistyped or altered endpoint
// cannot send the credential somewhere unintended.
func NormalizeEndpoint(setting, raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return DefaultAPIEndpoint, nil
	}

	parsed, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("parse %s: %w", setting, err)
	}

	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("%s must not include userinfo, query parameters, or fragments", setting)
	}

	if parsed.Scheme == "http" {
		return "", fmt.Errorf("%s uses http, which would send the credential in cleartext: use https", setting)
	}

	if parsed.Scheme != "https" {
		return "", fmt.Errorf("%s %q must be an absolute https URL", setting, raw)
	}

	if parsed.Host == "" {
		return "", fmt.Errorf("%s %q has no host", setting, raw)
	}

	if !allowedHost(parsed.Hostname()) {
		return "", fmt.Errorf("%s host %q is not an approved Lambda API host", setting, parsed.Hostname())
	}

	return strings.TrimRight(raw, "/"), nil
}

// allowedHost reports whether a bearer token may be sent to host. The list is
// deliberately fixed: making it configurable would let whoever sets the
// endpoint widen the bound too, which defeats the point of having one.
func allowedHost(host string) bool {
	for _, allowed := range allowedAPIHosts {
		if strings.EqualFold(allowed, host) {
			return true
		}
	}

	return false
}
