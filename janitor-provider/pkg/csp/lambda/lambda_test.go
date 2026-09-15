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
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"

	commonslambda "github.com/nvidia/nvsentinel/commons/pkg/lambda"
)

// fakeAPI records the instance IDs each operation was called with.
type fakeAPI struct {
	instance  *commonslambda.Instance
	getErr    error
	powerErr  error
	terminErr error

	powerCycleCalls []string
	terminateCalls  []string
	getCalls        []string
}

// GetInstance returns the canned instance, recording the ID it was asked for so
// tests can assert which instance was polled.
func (f *fakeAPI) GetInstance(_ context.Context, instanceID string) (*commonslambda.Instance, error) {
	f.getCalls = append(f.getCalls, instanceID)

	if f.getErr != nil {
		return nil, f.getErr
	}

	return f.instance, nil
}

// PowerCycleInstance records the call so tests can assert a power cycle was, or
// crucially was not, issued.
func (f *fakeAPI) PowerCycleInstance(_ context.Context, instanceID string) error {
	f.powerCycleCalls = append(f.powerCycleCalls, instanceID)

	return f.powerErr
}

// TerminateInstance records the call so tests can assert a terminate was, or
// crucially was not, issued.
func (f *fakeAPI) TerminateInstance(_ context.Context, instanceID string) error {
	f.terminateCalls = append(f.terminateCalls, instanceID)

	return f.terminErr
}

// available/blocked build the API's action-availability entries.
func available() *commonslambda.InstanceAction {
	return &commonslambda.InstanceAction{Available: true}
}

// blocked builds an action the API reports as unavailable, with the reason
// fields an operator would see in the resulting error.
func blocked(code, desc string) *commonslambda.InstanceAction {
	return &commonslambda.InstanceAction{ReasonCode: code, ReasonDescription: desc}
}

// node builds the minimal Node the client reads: a provider ID and the bootID
// kubelet reports.
func node(providerID, bootID string) corev1.Node {
	return corev1.Node{
		Name:   "node-a",
		Spec:   corev1.NodeSpec{ProviderID: providerID},
		Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{BootID: bootID}},
	}
}

// ref builds a request ref for a power cycle that started ago in the past.
func ref(instanceID, bootID string, ago time.Duration) string {
	return strings.Join([]string{
		instanceID,
		bootID,
		time.Now().UTC().Add(-ago).Format(time.RFC3339),
	}, requestRefSeparator)
}

// pastFloor is comfortably beyond powerCycleStatusFloor.
const pastFloor = 2 * powerCycleStatusFloor

// TestSendRebootSignal_Scenarios_PowerCyclesOrErrors covers the preconditions a
// reboot must satisfy before the API is touched, and the request ref handed
// back to the janitor on success.
func TestSendRebootSignal_Scenarios_PowerCyclesOrErrors(t *testing.T) {
	okInstance := &commonslambda.Instance{
		ID:     "i-123",
		Status: commonslambda.InstanceStatusActive,
		Actions: commonslambda.InstanceActions{
			ColdReboot: available(),
			Terminate:  available(),
		},
	}

	tests := []struct {
		name    string
		node    corev1.Node
		api     fakeAPI
		wantErr string
	}{
		{
			name: "power cycles the instance",
			node: node("lambda://i-123", "boot-1"),
			api:  fakeAPI{instance: okInstance},
		},
		{
			// The API reports power cycle under cold_reboot today, but the key
			// is expected to be renamed as the older operations are deprecated.
			name: "accepts the power_cycle action key",
			node: node("lambda://i-123", "boot-1"),
			api: fakeAPI{instance: &commonslambda.Instance{
				ID:      "i-123",
				Actions: commonslambda.InstanceActions{PowerCycle: available()},
			}},
		},
		{
			// An absent action block must not block the reboot, or a rename
			// upstream would silently stop every remediation.
			name: "proceeds when the API reports no actions",
			node: node("lambda://i-123", "boot-1"),
			api:  fakeAPI{instance: &commonslambda.Instance{ID: "i-123"}},
		},
		{
			name: "power cycle blocked by the API",
			node: node("lambda://i-123", "boot-1"),
			api: fakeAPI{instance: &commonslambda.Instance{
				ID: "i-123",
				Actions: commonslambda.InstanceActions{
					ColdReboot: blocked("vm-action-in-progress", "another action is running"),
				},
			}},
			wantErr: "power cycle is blocked for instance i-123: another action is running [reason_code=vm-action-in-progress]",
		},
		{
			name:    "instance lookup fails",
			node:    node("lambda://i-123", "boot-1"),
			api:     fakeAPI{getErr: errors.New("boom")},
			wantErr: "power cycle node node-a: boom",
		},
		{
			name:    "missing provider ID",
			node:    node("", "boot-1"),
			api:     fakeAPI{},
			wantErr: "no provider ID set",
		},
		{
			name:    "non-lambda provider ID",
			node:    node("aws:///us-west-2/i-123", "boot-1"),
			api:     fakeAPI{},
			wantErr: "is not lambda://",
		},
		{
			name:    "missing bootID",
			node:    node("lambda://i-123", ""),
			api:     fakeAPI{},
			wantErr: "has no bootID",
		},
		{
			name:    "API error",
			node:    node("lambda://i-123", "boot-1"),
			api:     fakeAPI{instance: okInstance, powerErr: errors.New("boom")},
			wantErr: "power cycle node node-a: boom",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := tt.api

			got, err := NewClient(&api).SendRebootSignal(context.Background(), tt.node, "")
			if tt.wantErr != "" {
				assert.ErrorContains(t, err, tt.wantErr)
				assert.Empty(t, got)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, []string{"i-123"}, api.powerCycleCalls)

			parsed, err := parseRequestRef(string(got))
			require.NoError(t, err)
			assert.Equal(t, "i-123", parsed.instanceID)
			assert.Equal(t, "boot-1", parsed.preRebootBootID)
			assert.WithinDuration(t, time.Now(), parsed.startedAt, time.Minute)
		})
	}
}

// A malformed provider ID must fail before any destructive call reaches the API.
func TestSendRebootSignal_BadProviderID_DoesNotCallAPI(t *testing.T) {
	api := fakeAPI{}

	_, err := NewClient(&api).SendRebootSignal(context.Background(), node("lambda://", "boot-1"), "")
	require.Error(t, err)
	assert.Empty(t, api.powerCycleCalls)
}

// TestSendRebootSignal_BlockedAction_DoesNotPowerCycle is the guard against
// power cycling a host the API has already marked ineligible. The endpoint does
// no in-flight validation, so this check is the only thing standing in the way.
func TestSendRebootSignal_BlockedAction_DoesNotPowerCycle(t *testing.T) {
	api := fakeAPI{instance: &commonslambda.Instance{
		ID:      "i-123",
		Actions: commonslambda.InstanceActions{ColdReboot: blocked("vm-is-too-old", "unsupported")},
	}}

	_, err := NewClient(&api).SendRebootSignal(context.Background(), node("lambda://i-123", "boot-1"), "")
	require.Error(t, err)
	assert.Empty(t, api.powerCycleCalls)
}

// TestIsNodeReady_Scenarios_ReportsReadiness covers readiness reporting: which
// instance states are still transient, which mean the node is never coming
// back, and the bootID comparison that actually proves the reboot happened.
func TestIsNodeReady_Scenarios_ReportsReadiness(t *testing.T) {
	active := &commonslambda.Instance{ID: "i-123", Status: commonslambda.InstanceStatusActive}

	tests := []struct {
		name      string
		bootID    string
		requestID string
		api       fakeAPI
		want      bool
		wantErr   string
	}{
		{
			name:      "active instance with a new bootID is ready",
			bootID:    "boot-2",
			requestID: ref("i-123", "boot-1", pastFloor),
			api:       fakeAPI{instance: active},
			want:      true,
		},
		{
			name:      "active instance with the pre-reboot bootID has not rebooted yet",
			bootID:    "boot-1",
			requestID: ref("i-123", "boot-1", pastFloor),
			api:       fakeAPI{instance: active},
			want:      false,
		},
		{
			name:      "active instance with no bootID reported is not ready",
			bootID:    "",
			requestID: ref("i-123", "boot-1", pastFloor),
			api:       fakeAPI{instance: active},
			want:      false,
		},
		{
			name:      "within the startup floor nothing is ready",
			bootID:    "boot-2",
			requestID: ref("i-123", "boot-1", time.Second),
			api:       fakeAPI{instance: active},
			want:      false,
		},
		{
			name:      "booting instance is not ready",
			bootID:    "boot-2",
			requestID: ref("i-123", "boot-1", pastFloor),
			api:       fakeAPI{instance: &commonslambda.Instance{ID: "i-123", Status: "booting"}},
			want:      false,
		},
		{
			name:      "unhealthy instance is not ready",
			bootID:    "boot-2",
			requestID: ref("i-123", "boot-1", pastFloor),
			api:       fakeAPI{instance: &commonslambda.Instance{ID: "i-123", Status: "unhealthy"}},
			want:      false,
		},
		{
			name:      "terminated instance will never come back",
			bootID:    "boot-2",
			requestID: ref("i-123", "boot-1", pastFloor),
			api:       fakeAPI{instance: &commonslambda.Instance{ID: "i-123", Status: commonslambda.InstanceStatusTerminated}},
			wantErr:   "will not come back",
		},
		{
			name:      "terminating instance will never come back",
			bootID:    "boot-2",
			requestID: ref("i-123", "boot-1", pastFloor),
			api:       fakeAPI{instance: &commonslambda.Instance{ID: "i-123", Status: commonslambda.InstanceStatusTerminating}},
			wantErr:   "will not come back",
		},
		{
			name:      "preempted instance will never come back",
			bootID:    "boot-2",
			requestID: ref("i-123", "boot-1", pastFloor),
			api:       fakeAPI{instance: &commonslambda.Instance{ID: "i-123", Status: commonslambda.InstanceStatusPreempted}},
			wantErr:   "will not come back",
		},
		{
			// The janitor treats an error here as terminal for the CR.
			name:      "API error reports not ready instead of failing",
			bootID:    "boot-2",
			requestID: ref("i-123", "boot-1", pastFloor),
			api:       fakeAPI{getErr: errors.New("503")},
			want:      false,
		},
		{
			name:      "request ref missing the timestamp",
			bootID:    "boot-2",
			requestID: "i-123|boot-1",
			api:       fakeAPI{},
			wantErr:   "malformed request ref",
		},
		{
			name:      "request ref with an unparseable timestamp",
			bootID:    "boot-2",
			requestID: "i-123|boot-1|yesterday",
			api:       fakeAPI{},
			wantErr:   "malformed request ref",
		},
		{
			name:      "request ref with an empty instance ID",
			bootID:    "boot-2",
			requestID: ref("", "boot-1", pastFloor),
			api:       fakeAPI{},
			wantErr:   "malformed request ref",
		},
		{
			name:      "empty request ref",
			bootID:    "boot-2",
			requestID: "",
			api:       fakeAPI{},
			wantErr:   "malformed request ref",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := tt.api

			ready, err := NewClient(&api).IsNodeReady(
				context.Background(), node("lambda://i-123", tt.bootID), tt.requestID)
			if tt.wantErr != "" {
				assert.ErrorContains(t, err, tt.wantErr)
				assert.False(t, ready)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.want, ready)
		})
	}
}

// The polled instance must come from the ref, not the live Node: the ref is
// immutable for the CR's lifetime, the providerID is not. The providerID here
// deliberately names a different instance.
func TestIsNodeReady_PastFloor_PollsInstanceFromRequestRefNotProviderID(t *testing.T) {
	api := fakeAPI{instance: &commonslambda.Instance{ID: "i-123", Status: commonslambda.InstanceStatusActive}}

	ready, err := NewClient(&api).IsNodeReady(
		context.Background(), node("lambda://i-replacement", "boot-2"), ref("i-123", "boot-1", pastFloor))
	require.NoError(t, err)
	assert.True(t, ready)
	assert.Equal(t, []string{"i-123"}, api.getCalls)
}

// Inside the floor the provider must not spend an API call.
func TestIsNodeReady_WithinFloor_DoesNotCallAPI(t *testing.T) {
	api := fakeAPI{instance: &commonslambda.Instance{ID: "i-123", Status: commonslambda.InstanceStatusActive}}

	ready, err := NewClient(&api).IsNodeReady(
		context.Background(), node("lambda://i-123", "boot-2"), ref("i-123", "boot-1", time.Second))
	require.NoError(t, err)
	assert.False(t, ready)
	assert.Empty(t, api.getCalls)
}

// TestSendTerminateSignal_Scenarios_TerminatesOrErrors mirrors the reboot
// scenarios for termination, including the availability check that stops a
// terminate the API would refuse.
func TestSendTerminateSignal_Scenarios_TerminatesOrErrors(t *testing.T) {
	okInstance := &commonslambda.Instance{
		ID:      "i-123",
		Actions: commonslambda.InstanceActions{Terminate: available()},
	}

	tests := []struct {
		name    string
		node    corev1.Node
		api     fakeAPI
		wantRef string
		wantErr string
	}{
		{
			name:    "terminates the instance",
			node:    node("lambda://i-123", "boot-1"),
			api:     fakeAPI{instance: okInstance},
			wantRef: "i-123",
		},
		{
			// The controller waits for the Node to go NotReady instead.
			name:    "does not require a bootID",
			node:    node("lambda://i-123", ""),
			api:     fakeAPI{instance: okInstance},
			wantRef: "i-123",
		},
		{
			name:    "missing provider ID",
			node:    node("", "boot-1"),
			api:     fakeAPI{},
			wantErr: "no provider ID set",
		},
		{
			name:    "API error",
			node:    node("lambda://i-123", "boot-1"),
			api:     fakeAPI{instance: okInstance, terminErr: errors.New("boom")},
			wantErr: "terminate node node-a: boom",
		},
		{
			name: "terminate blocked by the API",
			node: node("lambda://i-123", "boot-1"),
			api: fakeAPI{instance: &commonslambda.Instance{
				ID: "i-123",
				Actions: commonslambda.InstanceActions{
					Terminate: blocked("vm-is-terminating", "already terminating"),
				},
			}},
			wantErr: "terminate is blocked for instance i-123: already terminating [reason_code=vm-is-terminating]",
		},
		{
			name:    "instance lookup fails",
			node:    node("lambda://i-123", "boot-1"),
			api:     fakeAPI{getErr: errors.New("boom")},
			wantErr: "terminate node node-a: boom",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := tt.api

			got, err := NewClient(&api).SendTerminateSignal(context.Background(), tt.node)
			if tt.wantErr != "" {
				assert.ErrorContains(t, err, tt.wantErr)
				assert.Empty(t, got)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.wantRef, string(got))
			assert.Equal(t, []string{"i-123"}, api.terminateCalls)
		})
	}
}

// TestParseProviderID_Formats_ExtractsInstanceID pins the lambda:// shape the
// cloud controller manager writes, and rejects anything else so another CSP's
// provider ID cannot be read as a Lambda instance.
func TestParseProviderID_Formats_ExtractsInstanceID(t *testing.T) {
	tests := []struct {
		name       string
		providerID string
		want       string
		wantErr    bool
	}{
		{name: "lambda provider ID", providerID: "lambda://i-123", want: "i-123"},
		{name: "opaque hex id", providerID: "lambda://0920582c7ff041399e34823a0be62549", want: "0920582c7ff041399e34823a0be62549"},
		{name: "empty", providerID: "", wantErr: true},
		{name: "prefix only", providerID: "lambda://", wantErr: true},
		{name: "wrong scheme", providerID: "aws:///us-west-2/i-123", wantErr: true},
		{name: "no scheme", providerID: "i-123", wantErr: true},
		{name: "triple slash", providerID: "lambda:///i-123", wantErr: true},
		{name: "extra path segment", providerID: "lambda://region/i-123", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseProviderID(tt.providerID)
			if tt.wantErr {
				assert.Error(t, err)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

// TestParseRequestRef_RoundTrip_PreservesFields checks the ref SendRebootSignal
// emits survives the trip through the janitor and parses back to the same
// fields, since IsNodeReady has nothing else to work from.
func TestParseRequestRef_RoundTrip_PreservesFields(t *testing.T) {
	started := time.Now().UTC().Truncate(time.Second)

	got, err := parseRequestRef(strings.Join(
		[]string{"i-123", "boot-1", started.Format(time.RFC3339)}, requestRefSeparator))
	require.NoError(t, err)
	assert.Equal(t, "i-123", got.instanceID)
	assert.Equal(t, "boot-1", got.preRebootBootID)
	assert.True(t, started.Equal(got.startedAt), "want %s, got %s", started, got.startedAt)
}

// TestEndpointFromEnv_ReadsEnvAndAppliesSharedPolicy checks the env var is
// what feeds the shared policy, and that an unset one still defaults. The
// policy's own cases live in commons/pkg/lambda.
func TestEndpointFromEnv_ReadsEnvAndAppliesSharedPolicy(t *testing.T) {
	t.Setenv(APIEndpointEnvVar, "")
	got, err := endpointFromEnv()
	require.NoError(t, err)
	assert.Equal(t, commonslambda.DefaultAPIEndpoint, got)

	t.Setenv(APIEndpointEnvVar, "http://cloud.lambda.ai")

	_, err = endpointFromEnv()
	assert.ErrorContains(t, err, "cleartext")
	assert.ErrorContains(t, err, APIEndpointEnvVar)
}

// TestNewClientFromEnv_MissingAPIKey_ReturnsError checks a missing key fails at
// construction, so the pod crash-loops instead of waiting for a remediation to
// discover it has no credential.
func TestNewClientFromEnv_MissingAPIKey_ReturnsError(t *testing.T) {
	t.Setenv(commonslambda.APIKeyEnvVar, "")
	t.Setenv(APIEndpointEnvVar, "")

	_, err := NewClientFromEnv(context.Background())
	assert.ErrorContains(t, err, commonslambda.APIKeyEnvVar)
}

// TestNewClientFromEnv_BadEndpoint_ReturnsError checks a malformed endpoint
// fails at construction and that the error names the env var to fix.
func TestNewClientFromEnv_BadEndpoint_ReturnsError(t *testing.T) {
	t.Setenv(commonslambda.APIKeyEnvVar, "test-key")
	t.Setenv(APIEndpointEnvVar, "not-a-url")

	_, err := NewClientFromEnv(context.Background())
	assert.ErrorContains(t, err, APIEndpointEnvVar)
}

// TestNewClientFromEnv_ValidEnv_ReturnsClient checks the happy path wires up a
// usable client from environment alone, with no Kubernetes or CSP SDK involved.
func TestNewClientFromEnv_ValidEnv_ReturnsClient(t *testing.T) {
	t.Setenv(commonslambda.APIKeyEnvVar, "test-key")
	t.Setenv(APIEndpointEnvVar, commonslambda.DefaultAPIEndpoint)

	c, err := NewClientFromEnv(context.Background())
	require.NoError(t, err)
	assert.NotNil(t, c)
}

// TestNewClientFromEnv_UnapprovedHost_ReturnsError checks the allowlist is
// enforced before an authenticated client exists, so the bearer token is never
// wired up against a host outside the shared allowlist.
func TestNewClientFromEnv_UnapprovedHost_ReturnsError(t *testing.T) {
	t.Setenv(commonslambda.APIKeyEnvVar, "test-key")
	t.Setenv(APIEndpointEnvVar, "https://attacker.example.com")

	_, err := NewClientFromEnv(context.Background())
	assert.ErrorContains(t, err, "is not an approved")
}
