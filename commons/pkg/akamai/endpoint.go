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

package akamai

import (
	"fmt"
	"net/url"
	"strings"
)

const (
	// DefaultAPIHost is the host of the production Akamai Cloud API.
	DefaultAPIHost = "api.linode.com"

	// apiVersionPath is the path of the API version this client speaks.
	apiVersionPath = "/v4"

	// DefaultAPIEndpoint is the production Akamai Cloud API, version included.
	DefaultAPIEndpoint = "https://" + DefaultAPIHost + apiVersionPath
)

// NormalizeEndpoint validates raw as an Akamai Cloud API base URL and returns
// it with any trailing slash trimmed. An empty raw yields DefaultAPIEndpoint.
// setting names the configuration key in every error, so the operator is told
// which value to fix rather than which package rejected it.
//
// Callers must run this before constructing a Client: the API token is a bearer
// token sent on every request, so an unvalidated endpoint is an endpoint that
// can be handed the credential.
//
// http is refused outright, with no warning and no opt-in: there is no
// configuration under which sending a bearer token in cleartext is acceptable.
// The host is pinned to DefaultAPIHost for the same reason, so a mistyped or
// altered endpoint cannot send the token somewhere unintended.
func NormalizeEndpoint(setting, raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return DefaultAPIEndpoint, nil
	}

	parsed, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("parse %s: %w", setting, err)
	}

	// The raw string is checked for ? and # because url.Parse reports a bare
	// "?" only through ForceQuery and a bare "#" not at all. Either would turn
	// the path the client appends into a query or a fragment.
	if parsed.User != nil || strings.ContainsAny(raw, "?#") {
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

	if !strings.EqualFold(parsed.Hostname(), DefaultAPIHost) {
		return "", fmt.Errorf("%s host %q is not an approved Akamai Cloud API host", setting, parsed.Hostname())
	}

	if err := checkAPIPath(setting, raw, parsed); err != nil {
		return "", err
	}

	return strings.TrimRight(raw, "/"), nil
}

// checkAPIPath refuses an endpoint whose path is not the API version path. The
// client appends each request path to the endpoint, so a missing or different
// version path sends every request to a URL the API does not serve.
func checkAPIPath(setting, raw string, parsed *url.URL) error {
	if strings.TrimRight(parsed.Path, "/") != apiVersionPath {
		return fmt.Errorf("%s %q must end in the %s API path", setting, raw, apiVersionPath)
	}

	return nil
}
