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
	"net"
	"net/url"
	"strings"
	"sync/atomic"
)

const (
	// DefaultAPIHost is the host of the production Akamai Cloud API.
	DefaultAPIHost = "api.linode.com"

	// DefaultAPIEndpoint is the production Akamai Cloud API, version included.
	DefaultAPIEndpoint = "https://" + DefaultAPIHost + "/v4"
)

// endpointPolicy says where the API token may be sent.
type endpointPolicy struct {
	// host is the only host NormalizeEndpoint accepts.
	host string

	// loopbackHTTP lets the endpoint use http. It is true only while a test has
	// moved host to a loopback address, because an httptest server speaks http.
	loopbackHTTP bool
}

// productionPolicy is the pin every binary runs with.
var productionPolicy = &endpointPolicy{host: DefaultAPIHost}

// policy holds the pin in force. It is a variable only so a test can move it
// to a local server with AllowLoopbackHostForTest. A nil value means the
// production pin.
var policy atomic.Pointer[endpointPolicy]

func currentPolicy() *endpointPolicy {
	if p := policy.Load(); p != nil {
		return p
	}

	return productionPolicy
}

// testingT is the part of testing.TB that AllowLoopbackHostForTest needs.
// Naming it here keeps the testing package out of production builds.
type testingT interface {
	Helper()
	Cleanup(func())
	Fatalf(format string, args ...any)
}

// AllowLoopbackHostForTest moves the pinned API host to host until the test
// ends, so the client and the code that validates the endpoint can run against
// an httptest server instead of the real API. While it is in force, an http
// endpoint on that host is accepted too, because an httptest server speaks http.
//
// It accepts only a loopback host (localhost or a loopback IP address). A stray
// call outside a test therefore still cannot send the token off the machine.
// It restores the previous pin when the test ends.
//
// The pin is process-wide, so tests that call this must not run in parallel
// with each other or with tests that depend on the production pin.
func AllowLoopbackHostForTest(t testingT, host string) {
	t.Helper()

	if !isLoopbackHost(host) {
		t.Fatalf("AllowLoopbackHostForTest: %q is not a loopback host", host)

		return
	}

	previous := policy.Load()

	policy.Store(&endpointPolicy{host: host, loopbackHTTP: true})
	t.Cleanup(func() { policy.Store(previous) })
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}

	ip := net.ParseIP(host)

	return ip != nil && ip.IsLoopback()
}

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
// altered endpoint cannot send the token somewhere unintended. The one
// exception is a test that has moved the pin to a loopback server with
// AllowLoopbackHostForTest.
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

	pin := currentPolicy()

	if err := checkScheme(setting, raw, parsed.Scheme, pin); err != nil {
		return "", err
	}

	if parsed.Host == "" {
		return "", fmt.Errorf("%s %q has no host", setting, raw)
	}

	if !strings.EqualFold(parsed.Hostname(), pin.host) {
		return "", fmt.Errorf("%s host %q is not an approved Akamai Cloud API host", setting, parsed.Hostname())
	}

	return strings.TrimRight(raw, "/"), nil
}

// checkScheme refuses every scheme but https. It also lets http through while a
// test has moved the pin to a loopback server, and at no other time.
func checkScheme(setting, raw, scheme string, pin *endpointPolicy) error {
	if scheme == "http" && !pin.loopbackHTTP {
		return fmt.Errorf("%s uses http, which would send the credential in cleartext: use https", setting)
	}

	if scheme != "https" && scheme != "http" {
		return fmt.Errorf("%s %q must be an absolute https URL", setting, raw)
	}

	return nil
}
