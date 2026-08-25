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
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/nvidia/nvsentinel/commons/pkg/grpcauth"
	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
)

const (
	ownNode    = "gpu-node-01"
	otherNode  = "gpu-node-57"
	crossSA    = "system:serviceaccount:nvsentinel:csp-health-monitor"
	unlistedSA = "system:serviceaccount:other-ns:unlisted-monitor"
	testMethod = "/nvsentinel.PlatformConnector/HealthEventOccurredV1"
)

// stubValidator is a TokenValidator that maps tokens to identities.
type stubValidator struct {
	identities map[string]string // token -> username
	nodeClaims map[string]string // token -> attested node name (optional)
	// unbound lists tokens that carry no pod binding, as produced by
	// `kubectl create token <sa>` without --bound-object-ref. Everything else
	// is treated as a projected, pod-bound token.
	unbound map[string]bool
	// noNodeClaim lists tokens bound to a pod that has not been scheduled, so
	// they carry a pod UID but no node. Without this, every token models the
	// normal case: bound to a scheduled pod on the connector's own node.
	noNodeClaim map[string]bool
	err         error
	calls       int
}

func (s *stubValidator) Authenticate(_ context.Context, token string) (*grpcauth.Identity, error) {
	s.calls++

	if s.err != nil {
		return nil, s.err
	}

	username, ok := s.identities[token]
	if !ok {
		return nil, status.Error(codes.Unauthenticated, "token not authenticated")
	}

	identity := &grpcauth.Identity{Username: username}

	switch {
	case s.nodeClaims[token] != "":
		identity.NodeName = s.nodeClaims[token]
	case s.noNodeClaim[token]:
		identity.NodeName = ""
	default:
		// A projected token on a scheduled pod always carries its node.
		identity.NodeName = ownNode
	}

	if !s.unbound[token] {
		identity.PodName = "stub-pod"
		identity.PodUID = "stub-pod-uid"
	}

	return identity, nil
}

func events(nodeNames ...string) *pb.HealthEvents {
	e := make([]*pb.HealthEvent, 0, len(nodeNames))
	for _, n := range nodeNames {
		e = append(e, &pb.HealthEvent{NodeName: n, Agent: "gpu-health-monitor", CheckName: "xid"})
	}

	return &pb.HealthEvents{Version: 1, Events: e}
}

func nodeNames(e *pb.HealthEvents) []string {
	out := make([]string, 0, len(e.GetEvents()))
	for _, ev := range e.GetEvents() {
		out = append(out, ev.GetNodeName())
	}

	return out
}

func ctxWithAuth(header string) context.Context {
	return metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", header))
}

// run invokes the interceptor and reports whether the handler ran.
func run(t *testing.T, cfg Config, ctx context.Context, req any) (called bool, err error) {
	t.Helper()

	if cfg.Validator == nil {
		// A validator is mandatory, but the tokenless paths never reach it.
		cfg.Validator = &stubValidator{}
	}

	interceptor, err := NewNodeBindingInterceptor(cfg)
	require.NoError(t, err)

	handler := func(context.Context, any) (any, error) {
		called = true
		return nil, nil
	}

	_, err = interceptor(ctx, req, &grpc.UnaryServerInfo{FullMethod: testMethod}, handler)

	return called, err
}

func TestNodeBinding_Tokenless(t *testing.T) {
	tests := []struct {
		name      string
		in        *pb.HealthEvents
		wantNodes []string
		wantCode  codes.Code
	}{
		{
			name:      "own node is accepted unchanged",
			in:        events(ownNode),
			wantNodes: []string{ownNode},
			wantCode:  codes.OK,
		},
		{
			name:      "blank node name is stamped with the connector's node",
			in:        events(""),
			wantNodes: []string{ownNode},
			wantCode:  codes.OK,
		},
		{
			name:      "another node is rejected rather than rewritten",
			in:        events(otherNode),
			wantNodes: []string{otherNode},
			wantCode:  codes.PermissionDenied,
		},
		{
			name:      "one out-of-scope event rejects the whole batch",
			in:        events(ownNode, otherNode),
			wantNodes: []string{ownNode, otherNode},
			wantCode:  codes.PermissionDenied,
		},
		{
			name: "no event is stamped when a later one is rejected",
			// The blank name must survive untouched: a rejected batch may not
			// leave partially-mutated events behind.
			in:        events("", otherNode),
			wantNodes: []string{"", otherNode},
			wantCode:  codes.PermissionDenied,
		},
		{
			name:      "an empty batch is a no-op",
			in:        events(),
			wantNodes: []string{},
			wantCode:  codes.OK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			called, err := run(t, Config{NodeName: ownNode}, context.Background(), tt.in)

			assert.Equal(t, tt.wantCode, status.Code(err))
			assert.Equal(t, tt.wantCode == codes.OK, called,
				"handler should run only when the batch is authorized")
			assert.Equal(t, tt.wantNodes, nodeNames(tt.in))
		})
	}
}

func TestNodeBinding_CrossNodeToken(t *testing.T) {
	validator := &stubValidator{identities: map[string]string{"good": crossSA}}
	cfg := Config{
		NodeName:                 ownNode,
		Validator:                validator,
		CrossNodeServiceAccounts: []string{crossSA},
	}

	t.Run("allowlisted identity may name any node", func(t *testing.T) {
		in := events(otherNode, "gpu-node-99")

		called, err := run(t, cfg, ctxWithAuth("Bearer good"), in)

		require.NoError(t, err)
		assert.True(t, called)
		assert.Equal(t, []string{otherNode, "gpu-node-99"}, nodeNames(in))
	})

	t.Run("blank node name from a cross-node caller is rejected, not stamped", func(t *testing.T) {
		in := events("")

		called, err := run(t, cfg, ctxWithAuth("Bearer good"), in)

		assert.Equal(t, codes.InvalidArgument, status.Code(err))
		assert.False(t, called)
		assert.Equal(t, []string{""}, nodeNames(in),
			"stamping our own node here would misattribute another node's fault")
	})
}

func TestNodeBinding_TokenNotEntitledToCrossNode(t *testing.T) {
	// A pod may request a token for any audience through a projected volume in
	// its own spec, so authenticating alone does not grant cross-node reach.
	validator := &stubValidator{identities: map[string]string{"minted": unlistedSA}}
	cfg := Config{
		NodeName:                 ownNode,
		Validator:                validator,
		CrossNodeServiceAccounts: []string{crossSA},
	}

	t.Run("is pinned to the local node", func(t *testing.T) {
		in := events(otherNode)

		called, err := run(t, cfg, ctxWithAuth("Bearer minted"), in)

		assert.Equal(t, codes.PermissionDenied, status.Code(err))
		assert.False(t, called)
	})

	t.Run("can still report about the local node", func(t *testing.T) {
		in := events(ownNode)

		called, err := run(t, cfg, ctxWithAuth("Bearer minted"), in)

		require.NoError(t, err)
		assert.True(t, called)
	})
}

func TestNodeBinding_FailsClosedOnTokenError(t *testing.T) {
	tests := []struct {
		name       string
		validator  TokenValidator
		authHeader string
		wantCode   codes.Code
	}{
		{
			name:       "unrecognised token",
			validator:  &stubValidator{identities: map[string]string{}},
			authHeader: "Bearer not-a-known-token",
			wantCode:   codes.Unauthenticated,
		},
		{
			// Surfaced as Unavailable so the publisher backs off and retries
			// rather than dropping the event; see commons/pkg/grpcauth.
			name:       "TokenReview unavailable",
			validator:  &stubValidator{err: status.Error(codes.Unavailable, "api server down")},
			authHeader: "Bearer good",
			wantCode:   codes.Unavailable,
		},
		{
			name:       "non-bearer scheme",
			validator:  &stubValidator{identities: map[string]string{"good": crossSA}},
			authHeader: "Basic Zm9v",
			wantCode:   codes.Unauthenticated,
		},
		{
			name:       "empty bearer token",
			validator:  &stubValidator{identities: map[string]string{"good": crossSA}},
			authHeader: "Bearer ",
			wantCode:   codes.Unauthenticated,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Config{
				NodeName:                 ownNode,
				Validator:                tt.validator,
				CrossNodeServiceAccounts: []string{crossSA},
			}

			// Even an event about the local node is refused: a caller that
			// presented a credential we could not verify never falls through to
			// the credential-less path.
			called, err := run(t, cfg, ctxWithAuth(tt.authHeader), events(ownNode))

			assert.Equal(t, tt.wantCode, status.Code(err))
			assert.False(t, called)
		})
	}
}

func TestNodeBinding_UnverifiableTokenIsRejected(t *testing.T) {
	// A caller that presents a credential we cannot verify is refused, not
	// quietly downgraded to the anonymous path: silently ignoring a token would
	// hide a broken publisher behind a working-looking one.
	in := events(otherNode)

	called, err := run(t, Config{NodeName: ownNode}, ctxWithAuth("Bearer whatever"), in)

	assert.Equal(t, codes.Unauthenticated, status.Code(err))
	assert.False(t, called)
}

func TestNodeBinding_OtherMessageTypesPassThrough(t *testing.T) {
	called, err := run(t, Config{NodeName: ownNode}, context.Background(), "not-a-health-event")

	require.NoError(t, err)
	assert.True(t, called)
}

func TestNodeBinding_SkipsNilEvents(t *testing.T) {
	in := &pb.HealthEvents{Version: 1, Events: []*pb.HealthEvent{nil, {NodeName: ownNode}}}

	called, err := run(t, Config{NodeName: ownNode}, context.Background(), in)

	require.NoError(t, err)
	assert.True(t, called)
}

func TestNodeBinding_TokenlessCallerSkipsTokenReview(t *testing.T) {
	// Node-local fault reporting is the safety-critical, high-volume path and
	// must keep working while the API server is degraded.
	validator := &stubValidator{err: errors.New("api server must not be called")}
	cfg := Config{
		NodeName:                 ownNode,
		Validator:                validator,
		CrossNodeServiceAccounts: []string{crossSA},
	}

	called, err := run(t, cfg, context.Background(), events(ownNode))

	require.NoError(t, err)
	assert.True(t, called)
	assert.Zero(t, validator.calls)
}

func TestNewNodeBindingInterceptor_Validation(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantErr string
	}{
		{
			name:    "node name is required",
			cfg:     Config{},
			wantErr: "node name is required",
		},
		{
			name:    "validator is required",
			cfg:     Config{NodeName: ownNode},
			wantErr: "token validator is required",
		},
		{
			// The chart used to build these from a bare name and the release
			// namespace. Now they arrive whole, so a bare name is a typo that
			// would otherwise match nothing and silently pin a cluster-scoped
			// publisher to one node.
			name: "bare service account name is rejected",
			cfg: Config{
				NodeName:                 ownNode,
				Validator:                &stubValidator{},
				CrossNodeServiceAccounts: []string{"csp-health-monitor"},
			},
			wantErr: "not a canonical Kubernetes username",
		},
		{
			name: "padded entry is rejected rather than trimmed",
			cfg: Config{
				NodeName:                 ownNode,
				Validator:                &stubValidator{},
				CrossNodeServiceAccounts: []string{"  " + crossSA + "  "},
			},
			wantErr: "not a canonical Kubernetes username",
		},
		{
			name: "blank entry is rejected",
			cfg: Config{
				NodeName:                 ownNode,
				Validator:                &stubValidator{},
				CrossNodeServiceAccounts: []string{""},
			},
			wantErr: "not a canonical Kubernetes username",
		},
		{
			name: "wrong prefix is rejected",
			cfg: Config{
				NodeName:                 ownNode,
				Validator:                &stubValidator{},
				CrossNodeServiceAccounts: []string{"system:node:gpu-node-01"},
			},
			wantErr: "not a canonical Kubernetes username",
		},
		{
			name: "missing name segment is rejected",
			cfg: Config{
				NodeName:                 ownNode,
				Validator:                &stubValidator{},
				CrossNodeServiceAccounts: []string{"system:serviceaccount:nvsentinel:"},
			},
			wantErr: "not a canonical Kubernetes username",
		},
		{
			name: "unknown mode is rejected",
			cfg: Config{
				NodeName:  ownNode,
				Validator: &stubValidator{},
				Mode:      "warn",
			},
			wantErr: `mode must be "enforce" or "audit"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewNodeBindingInterceptor(tt.cfg)

			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestNewNodeBindingInterceptor_AlwaysEnforces(t *testing.T) {
	// An interceptor built with no Mode set defaults to ModeEnforce and
	// rejects. ModeAudit is opt-in; see TestNodeBinding_AuditMode.
	in := events(otherNode)

	_, err := run(t, Config{NodeName: ownNode}, context.Background(), in)

	assert.Equal(t, codes.PermissionDenied, status.Code(err))
}

func TestNodeBinding_AuditMode(t *testing.T) {
	// ModeAudit records the same violation it would have rejected under
	// ModeEnforce, but lets the request through.
	in := events(otherNode)
	cfg := Config{NodeName: ownNode, Mode: ModeAudit}

	counter := authViolations.WithLabelValues(reasonNodeMismatch)
	before := testutil.ToFloat64(counter)

	called, err := run(t, cfg, context.Background(), in)

	require.NoError(t, err, "audit mode must not reject")
	assert.True(t, called, "audit mode must still call the handler")
	assert.Equal(t, before+1, testutil.ToFloat64(counter), "the violation must still be counted")
	assert.Equal(t, []string{otherNode}, nodeNames(in), "audit mode must not rewrite the event")
}

func TestNodeBinding_AuditMode_RejectedTokenStillCountedAndLetThrough(t *testing.T) {
	validator := &stubValidator{err: status.Error(codes.Unauthenticated, "token not authenticated")}
	cfg := Config{NodeName: ownNode, Mode: ModeAudit, Validator: validator}

	counter := authViolations.WithLabelValues(reasonTokenInvalid)
	before := testutil.ToFloat64(counter)

	called, err := run(t, cfg, ctxWithAuth("Bearer forged"), events(ownNode))

	require.NoError(t, err, "audit mode must not reject an invalid credential either")
	assert.True(t, called)
	assert.Equal(t, before+1, testutil.ToFloat64(counter))
}

func TestNodeBinding_FailOpenOnUnavailable(t *testing.T) {
	validator := &stubValidator{err: status.Error(codes.Unavailable, "api server unreachable")}
	cfg := Config{NodeName: ownNode, Validator: validator, FailOpenOnUnavailable: true}

	counter := authViolations.WithLabelValues(reasonValidatorUnavailable)
	before := testutil.ToFloat64(counter)

	called, err := run(t, cfg, ctxWithAuth("Bearer whatever"), events(ownNode))

	require.NoError(t, err, "an unreachable validator must fail open to node-local scope, not reject")
	assert.True(t, called)
	assert.Equal(t, before+1, testutil.ToFloat64(counter), "the outage must still be counted")
}

func TestNodeBinding_FailOpenOnUnavailable_ForeignNodeNameIsRetryableNotPermissionDenied(t *testing.T) {
	// A degraded scope is a guess, not a verified identity: the caller might
	// really be an allowlisted cross-node publisher the outage prevented from
	// being verified. Rejecting it as node_mismatch/PermissionDenied would be
	// both wrong (every publisher treats PermissionDenied as non-retryable, so
	// the batch would be dropped for good) and misleading (node_mismatch is
	// the reason METRICS.md tells operators to alert on as suspected
	// credential abuse).
	validator := &stubValidator{err: status.Error(codes.Unavailable, "api server unreachable")}
	cfg := Config{NodeName: ownNode, Validator: validator, FailOpenOnUnavailable: true}

	mismatchCounter := authViolations.WithLabelValues(reasonNodeMismatch)
	beforeMismatch := testutil.ToFloat64(mismatchCounter)

	called, err := run(t, cfg, ctxWithAuth("Bearer whatever"), events(otherNode))

	assert.Equal(t, codes.Unavailable, status.Code(err),
		"an event naming another node during an outage must stay retryable, not become PermissionDenied")
	assert.False(t, called)
	assert.Equal(t, beforeMismatch, testutil.ToFloat64(mismatchCounter),
		"a degraded guess must not be counted as node_mismatch")
}

func TestNodeBinding_FailOpenOnUnavailable_BlankNodeNameIsStillStamped(t *testing.T) {
	// The common case this fallback exists for: an ordinary node-local
	// publisher whose blank node name gets filled in exactly as it would
	// under a verified node-local scope.
	validator := &stubValidator{err: status.Error(codes.Unavailable, "api server unreachable")}
	cfg := Config{NodeName: ownNode, Validator: validator, FailOpenOnUnavailable: true}

	in := events("")

	called, err := run(t, cfg, ctxWithAuth("Bearer whatever"), in)

	require.NoError(t, err)
	assert.True(t, called)
	assert.Equal(t, []string{ownNode}, nodeNames(in))
}

func TestNodeBinding_AuditMode_CrossNodeCallerBlankNodeNameStillRejected(t *testing.T) {
	// A blank node name from a cross-node caller produces an event nothing
	// downstream can handle. ModeEnforce could never accept it intact, so
	// ModeAudit must not forward it either — this is the one rejection that
	// stays enforced regardless of Mode.
	validator := &stubValidator{identities: map[string]string{"good": crossSA}}
	cfg := Config{
		NodeName:                 ownNode,
		Validator:                validator,
		CrossNodeServiceAccounts: []string{crossSA},
		Mode:                     ModeAudit,
	}

	counter := authViolations.WithLabelValues(reasonMissingNodeName)
	before := testutil.ToFloat64(counter)

	called, err := run(t, cfg, ctxWithAuth("Bearer good"), events(""))

	assert.Equal(t, codes.InvalidArgument, status.Code(err),
		"a structurally-broken event must be rejected even in audit mode")
	assert.False(t, called)
	assert.Equal(t, before+1, testutil.ToFloat64(counter))
}

func TestNodeBinding_FailOpenOnUnavailable_DoesNotWeakenInvalidCredentialCheck(t *testing.T) {
	// A rejected credential says something about the caller and must still be
	// rejected in enforce mode, even with FailOpenOnUnavailable set.
	validator := &stubValidator{err: status.Error(codes.Unauthenticated, "token not authenticated")}
	cfg := Config{NodeName: ownNode, Validator: validator, FailOpenOnUnavailable: true}

	called, err := run(t, cfg, ctxWithAuth("Bearer forged"), events(ownNode))

	assert.Equal(t, codes.Unauthenticated, status.Code(err))
	assert.False(t, called)
}

func TestNodeBinding_FailOpenOnUnavailable_DefaultsToFailClosed(t *testing.T) {
	validator := &stubValidator{err: status.Error(codes.Unavailable, "api server unreachable")}
	cfg := Config{NodeName: ownNode, Validator: validator}

	called, err := run(t, cfg, ctxWithAuth("Bearer whatever"), events(ownNode))

	assert.Equal(t, codes.Unavailable, status.Code(err), "unavailable must still reject unless opted in")
	assert.False(t, called)
}

func TestNewNodeBindingInterceptor_DuplicateAllowlistEntries(t *testing.T) {
	// Canonical entries are matched verbatim; repeating one is harmless.
	validator := &stubValidator{identities: map[string]string{"good": crossSA}}
	cfg := Config{
		NodeName:                 ownNode,
		Validator:                validator,
		CrossNodeServiceAccounts: []string{crossSA, crossSA},
	}

	called, err := run(t, cfg, ctxWithAuth("Bearer good"), events(otherNode))

	require.NoError(t, err, "a duplicated entry must still match")
	assert.True(t, called)
}

func TestNodeBinding_NilHealthEventsPayload(t *testing.T) {
	// A typed-nil *pb.HealthEvents still satisfies the type assertion, so it
	// reaches the batch walk. It must not panic.
	var in *pb.HealthEvents

	called, err := run(t, Config{NodeName: ownNode}, context.Background(), in)

	require.NoError(t, err)
	assert.True(t, called)
}

func TestNodeBinding_RecordsViolationMetrics(t *testing.T) {
	// This counter is what an operator alerts on, so a violation that is logged
	// but not counted would read as a quiet socket when it is not.
	tests := []struct {
		name   string
		cfg    Config
		ctx    context.Context
		in     *pb.HealthEvents
		reason string
	}{
		{
			name:   "foreign node name",
			cfg:    Config{NodeName: ownNode},
			ctx:    context.Background(),
			in:     events(otherNode),
			reason: reasonNodeMismatch,
		},
		{
			name: "blank node name from a cross-node caller",
			cfg: Config{
				NodeName:                 ownNode,
				Validator:                &stubValidator{identities: map[string]string{"good": crossSA}},
				CrossNodeServiceAccounts: []string{crossSA},
			},
			ctx:    ctxWithAuth("Bearer good"),
			in:     events(""),
			reason: reasonMissingNodeName,
		},
		{
			name: "rejected token",
			cfg: Config{
				NodeName:                 ownNode,
				Validator:                &stubValidator{identities: map[string]string{}},
				CrossNodeServiceAccounts: []string{crossSA},
			},
			ctx:    ctxWithAuth("Bearer nope"),
			in:     events(ownNode),
			reason: reasonTokenInvalid,
		},
		{
			name:   "malformed credentials",
			cfg:    Config{NodeName: ownNode},
			ctx:    ctxWithAuth("Basic Zm9v"),
			in:     events(ownNode),
			reason: reasonMalformedCreds,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			counter := authViolations.WithLabelValues(tt.reason)
			before := testutil.ToFloat64(counter)

			_, _ = run(t, tt.cfg, tt.ctx, tt.in)

			assert.Equal(t, before+1, testutil.ToFloat64(counter))
		})
	}
}

func TestNodeBinding_RecordsScopeDecision(t *testing.T) {
	cfg := Config{
		NodeName:                 ownNode,
		Validator:                &stubValidator{identities: map[string]string{"good": crossSA}},
		CrossNodeServiceAccounts: []string{crossSA},
	}

	local := authDecisions.WithLabelValues(scopeNodeLocal.String())
	cross := authDecisions.WithLabelValues(scopeCrossNode.String())
	beforeLocal, beforeCross := testutil.ToFloat64(local), testutil.ToFloat64(cross)

	_, err := run(t, cfg, context.Background(), events(ownNode))
	require.NoError(t, err)

	_, err = run(t, cfg, ctxWithAuth("Bearer good"), events(otherNode))
	require.NoError(t, err)

	assert.Equal(t, beforeLocal+1, testutil.ToFloat64(local))
	assert.Equal(t, beforeCross+1, testutil.ToFloat64(cross))
}

func TestNodeBinding_NodeClaim(t *testing.T) {
	// A non-allowlisted caller's token carries the node it was issued on,
	// written by the API server. The claim can only legitimately equal this
	// connector's node, because no other node's socket is reachable.
	validator := &stubValidator{
		identities: map[string]string{"local": unlistedSA, "foreign": unlistedSA},
		nodeClaims: map[string]string{"local": ownNode, "foreign": otherNode},
	}
	cfg := Config{
		NodeName:                 ownNode,
		Validator:                validator,
		CrossNodeServiceAccounts: []string{crossSA},
	}

	t.Run("matching claim: own-node events accepted", func(t *testing.T) {
		in := events(ownNode)

		called, err := run(t, cfg, ctxWithAuth("Bearer local"), in)

		require.NoError(t, err)
		assert.True(t, called)
	})

	t.Run("matching claim: blank names stamped", func(t *testing.T) {
		in := events("")

		called, err := run(t, cfg, ctxWithAuth("Bearer local"), in)

		require.NoError(t, err)
		assert.True(t, called)
		assert.Equal(t, []string{ownNode}, nodeNames(in))
	})

	t.Run("matching claim: foreign event names still rejected", func(t *testing.T) {
		in := events(otherNode)

		called, err := run(t, cfg, ctxWithAuth("Bearer local"), in)

		assert.Equal(t, codes.PermissionDenied, status.Code(err))
		assert.False(t, called)
	})

	t.Run("mismatched claim: rejected outright, even for own-node events", func(t *testing.T) {
		// The token was issued on another node; presenting it here means it was
		// carried off that node. Nothing it says is trusted.
		counter := authViolations.WithLabelValues(reasonNodeClaimMismatch)
		before := testutil.ToFloat64(counter)

		called, err := run(t, cfg, ctxWithAuth("Bearer foreign"), events(ownNode))

		assert.Equal(t, codes.PermissionDenied, status.Code(err))
		assert.False(t, called)
		assert.Equal(t, before+1, testutil.ToFloat64(counter))
	})

	t.Run("no claim on the token: pinned like an anonymous caller", func(t *testing.T) {
		noClaims := &stubValidator{identities: map[string]string{"bare": unlistedSA}}
		bareCfg := cfg
		bareCfg.Validator = noClaims

		called, err := run(t, bareCfg, ctxWithAuth("Bearer bare"), events(ownNode))

		require.NoError(t, err)
		assert.True(t, called)
	})
}

// nilIdentityValidator returns neither an identity nor an error, the one shape
// that would dereference nil in the interceptor.
type nilIdentityValidator struct{}

func (nilIdentityValidator) Authenticate(context.Context, string) (*grpcauth.Identity, error) {
	return nil, nil //nolint:nilnil // deliberately the pathological case under test
}

func TestNodeBinding_NilIdentityDoesNotPanic(t *testing.T) {
	// The interface cannot enforce "non-nil identity when err is nil", so a
	// misbehaving implementation must produce an authentication error rather
	// than panicking the gRPC server it runs inside.
	cfg := Config{
		NodeName:                 ownNode,
		Validator:                nilIdentityValidator{},
		CrossNodeServiceAccounts: []string{crossSA},
	}

	called, err := run(t, cfg, ctxWithAuth("Bearer anything"), events(ownNode))

	assert.Equal(t, codes.Internal, status.Code(err))
	assert.False(t, called)
}

func TestNodeBinding_MissingClaimFallsBackToPinning(t *testing.T) {
	// A node-local caller whose token carries no node claim keeps working,
	// scoped to its own node, exactly like a tokenless caller. Its scope is
	// what reaching the socket already grants, so there is nothing to gain by
	// refusing it. Cross-node callers are refused an absent claim instead.
	validator := &stubValidator{
		identities:  map[string]string{"noclaim": unlistedSA},
		noNodeClaim: map[string]bool{"noclaim": true},
	}
	cfg := Config{
		NodeName:                 ownNode,
		Validator:                validator,
		CrossNodeServiceAccounts: []string{crossSA},
	}

	absent := authNodeClaim.WithLabelValues(nodeClaimAbsent)
	before := testutil.ToFloat64(absent)

	t.Run("own-node event accepted", func(t *testing.T) {
		called, err := run(t, cfg, ctxWithAuth("Bearer noclaim"), events(ownNode))

		require.NoError(t, err)
		assert.True(t, called)
	})

	t.Run("foreign-node event still rejected", func(t *testing.T) {
		called, err := run(t, cfg, ctxWithAuth("Bearer noclaim"), events(otherNode))

		assert.Equal(t, codes.PermissionDenied, status.Code(err))
		assert.False(t, called)
	})

	assert.GreaterOrEqual(t, testutil.ToFloat64(absent), before+1,
		"the claimless fallback must be counted so operators can measure coverage")
}

func TestNodeBinding_UnboundTokenIsRefusedCrossNodeScope(t *testing.T) {
	// `kubectl create token <sa>` without --bound-object-ref mints a credential
	// that authenticates as an allowlisted ServiceAccount with the right
	// audience, but is tied to no pod — and so carries no node claim either.
	// Without this check it would skip verifyNodeClaim and be replayable from
	// anywhere in the cluster with authority to name any node, making an
	// unbound token strictly more powerful than a correctly bound one.
	validator := &stubValidator{
		identities: map[string]string{"handmade": crossSA},
		unbound:    map[string]bool{"handmade": true},
	}
	cfg := Config{
		NodeName:                 ownNode,
		Validator:                validator,
		CrossNodeServiceAccounts: []string{crossSA},
	}
	counter := authViolations.WithLabelValues(reasonUnboundCrossNodeToken)
	before := testutil.ToFloat64(counter)

	called, err := run(t, cfg, ctxWithAuth("Bearer handmade"), events(otherNode))

	assert.Equal(t, codes.PermissionDenied, status.Code(err))
	assert.False(t, called)
	assert.Equal(t, before+1, testutil.ToFloat64(counter))
}

func TestNodeBinding_PodBoundTokenWithoutNodeClaimIsRefusedCrossNodeScope(t *testing.T) {
	// A token bound to a pod that was never scheduled carries a pod UID but no
	// node, so the provenance check has nothing to compare. Anyone able to
	// create a pod that never schedules — an unsatisfiable nodeSelector is
	// enough — could otherwise mint a credential with cluster-wide authority
	// and no node binding at all.
	validator := &stubValidator{
		identities:  map[string]string{"pending": crossSA},
		noNodeClaim: map[string]bool{"pending": true},
	}
	cfg := Config{
		NodeName:                 ownNode,
		Validator:                validator,
		CrossNodeServiceAccounts: []string{crossSA},
	}
	counter := authViolations.WithLabelValues(reasonCrossNodeClaimAbsent)
	before := testutil.ToFloat64(counter)

	called, err := run(t, cfg, ctxWithAuth("Bearer pending"), events(otherNode))

	assert.Equal(t, codes.PermissionDenied, status.Code(err))
	assert.False(t, called)
	assert.Equal(t, before+1, testutil.ToFloat64(counter))
}

func TestNodeBinding_ClaimlessTokenStillPinsNodeLocalCallers(t *testing.T) {
	// Node-local callers keep the permissive treatment: their scope is the
	// connector's own node, exactly what reaching the socket already grants.
	validator := &stubValidator{
		identities:  map[string]string{"pending": unlistedSA},
		noNodeClaim: map[string]bool{"pending": true},
	}
	cfg := Config{
		NodeName:                 ownNode,
		Validator:                validator,
		CrossNodeServiceAccounts: []string{crossSA},
	}

	called, err := run(t, cfg, ctxWithAuth("Bearer pending"), events(ownNode))

	require.NoError(t, err)
	assert.True(t, called)
}

func TestNodeBinding_UnboundTokenStillWorksForNodeLocalCallers(t *testing.T) {
	// Node-local scope equals what reaching the socket already grants, so an
	// unbound token gains such a caller nothing and is not worth refusing.
	validator := &stubValidator{
		identities: map[string]string{"handmade": unlistedSA},
		unbound:    map[string]bool{"handmade": true},
	}
	cfg := Config{
		NodeName:                 ownNode,
		Validator:                validator,
		CrossNodeServiceAccounts: []string{crossSA},
	}

	called, err := run(t, cfg, ctxWithAuth("Bearer handmade"), events(ownNode))

	require.NoError(t, err)
	assert.True(t, called)
}

func TestNodeBinding_AllowlistedClaimMismatchIsRejected(t *testing.T) {
	// Cross-node reach is permission to name other nodes, not permission to
	// present the credential from other nodes. A token bound elsewhere has been
	// carried off its node, and the allowlist does not excuse that: refusing it
	// confines a copied token to the node where its pod actually runs.
	validator := &stubValidator{
		identities: map[string]string{"fwd": crossSA},
		nodeClaims: map[string]string{"fwd": otherNode},
	}
	cfg := Config{
		NodeName:                 ownNode,
		Validator:                validator,
		CrossNodeServiceAccounts: []string{crossSA},
	}
	counter := authViolations.WithLabelValues(reasonNodeClaimMismatch)
	before := testutil.ToFloat64(counter)

	called, err := run(t, cfg, ctxWithAuth("Bearer fwd"), events(otherNode, "gpu-node-99"))

	assert.Equal(t, codes.PermissionDenied, status.Code(err))
	assert.False(t, called)
	assert.Equal(t, before+1, testutil.ToFloat64(counter))
}

func TestNodeBinding_AllowlistedCallerWithMatchingClaimKeepsCrossNodeScope(t *testing.T) {
	// The other side of the same rule: provenance verified, so the allowlist
	// decides scope and the caller may name any node.
	validator := &stubValidator{
		identities: map[string]string{"cross": crossSA},
		nodeClaims: map[string]string{"cross": ownNode},
	}
	cfg := Config{
		NodeName:                 ownNode,
		Validator:                validator,
		CrossNodeServiceAccounts: []string{crossSA},
	}

	in := events(otherNode, "gpu-node-99")

	called, err := run(t, cfg, ctxWithAuth("Bearer cross"), in)

	require.NoError(t, err)
	assert.True(t, called)
	assert.Equal(t, []string{otherNode, "gpu-node-99"}, nodeNames(in))
}

// concurrentValidator is race-free, unlike stubValidator's call counter.
type concurrentValidator struct{ username string }

func (c concurrentValidator) Authenticate(context.Context, string) (*grpcauth.Identity, error) {
	return &grpcauth.Identity{Username: c.username, PodName: "stub-pod", PodUID: "stub-pod-uid", NodeName: ownNode}, nil
}

func TestNodeBinding_ConcurrentRequests(t *testing.T) {
	// gRPC shares one interceptor across every in-flight call, so the binder is
	// read by many goroutines at once. Run under -race.
	interceptor, err := NewNodeBindingInterceptor(Config{
		NodeName:                 ownNode,
		Validator:                concurrentValidator{username: crossSA},
		CrossNodeServiceAccounts: []string{crossSA},
	})
	require.NoError(t, err)

	handler := func(context.Context, any) (any, error) { return nil, nil }
	info := &grpc.UnaryServerInfo{FullMethod: testMethod}

	const goroutines = 64

	var wg sync.WaitGroup

	wg.Add(goroutines)

	for i := range goroutines {
		go func() {
			defer wg.Done()

			// Each goroutine gets its own batch: gRPC unmarshals a fresh message
			// per call, so sharing one here would test the test, not the code.
			switch i % 4 {
			case 0: // tokenless, own node
				_, err := interceptor(context.Background(), events(ownNode), info, handler)
				assert.NoError(t, err)
			case 1: // tokenless, blank name gets stamped
				in := events("")
				_, err := interceptor(context.Background(), in, info, handler)
				assert.NoError(t, err)
				assert.Equal(t, []string{ownNode}, nodeNames(in))
			case 2: // tokenless, foreign node is rejected
				_, err := interceptor(context.Background(), events(otherNode), info, handler)
				assert.Equal(t, codes.PermissionDenied, status.Code(err))
			case 3: // allowlisted token, foreign node is allowed
				_, err := interceptor(ctxWithAuth("Bearer good"), events(otherNode), info, handler)
				assert.NoError(t, err)
			}
		}()
	}

	wg.Wait()
}

func TestViolationReasonFor_SeparatesOutagesFromBadCredentials(t *testing.T) {
	// The point of this mapping is that an unreachable API server must not be
	// counted as a rejected credential: during a control-plane blip every
	// in-flight request fails, and if those land on token_invalid the graph is
	// indistinguishable from a node being attacked with forged tokens.
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"rejected token", status.Error(codes.Unauthenticated, "token not authenticated"), reasonTokenInvalid},
		{"wrong audience", status.Error(codes.Unauthenticated, "audience mismatch"), reasonTokenInvalid},
		// The validator answers "who is this?" only, so it has no reason to
		// return PermissionDenied. If it ever does, that is our bug, not the
		// caller's credential, and must not land on token_invalid.
		{"unexpected permission denied", status.Error(codes.PermissionDenied, "denied"), reasonValidatorError},
		{"api server outage", status.Error(codes.Unavailable, "unavailable"), reasonValidatorUnavailable},
		{"caller deadline", status.Error(codes.DeadlineExceeded, "deadline"), reasonValidatorTimeout},
		{"caller canceled", status.Error(codes.Canceled, "canceled"), reasonValidatorTimeout},
		{"our own RBAC broken", status.Error(codes.Internal, "not permitted"), reasonValidatorError},
		{"plain non-status error", errors.New("boom"), reasonValidatorError},
	}

	seen := map[string]struct{}{}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := violationReasonFor(tt.err)
			assert.Equal(t, tt.want, got)
			seen[got] = struct{}{}
		})
	}

	// Cardinality is the other half of the contract: this label feeds a
	// Prometheus counter, so the value set must stay closed.
	assert.LessOrEqual(t, len(seen), 5, "violation reasons must remain a small closed set")
}

func TestNodeBinding_CrossNamespaceAllowlistedIdentity(t *testing.T) {
	// The allowlist is a flat list of canonical usernames, so an entry from
	// outside the release namespace is just another entry. This pins down that
	// matching is on the whole username, with no assumption anywhere that the
	// namespace segment equals the release namespace.
	const foreignSA = "system:serviceaccount:other-namespace:my-monitor"

	validator := &stubValidator{identities: map[string]string{
		"foreign":  foreignSA,
		"unlisted": "system:serviceaccount:other-namespace:not-listed",
	}}
	cfg := Config{
		NodeName:  ownNode,
		Validator: validator,
		// Mixed list: one in-release name and one from another namespace.
		CrossNodeServiceAccounts: []string{crossSA, foreignSA},
	}

	t.Run("allowlisted identity from another namespace may name any node", func(t *testing.T) {
		in := events(otherNode, "gpu-node-99")

		called, err := run(t, cfg, ctxWithAuth("Bearer foreign"), in)

		require.NoError(t, err)
		assert.True(t, called)
		assert.Equal(t, []string{otherNode, "gpu-node-99"}, nodeNames(in),
			"a cross-namespace allowlist entry must grant the same reach as an in-namespace one")
	})

	t.Run("a different SA in that same namespace is still pinned", func(t *testing.T) {
		// Guards against matching on namespace rather than the whole username.
		in := events(otherNode)

		called, err := run(t, cfg, ctxWithAuth("Bearer unlisted"), in)

		assert.Equal(t, codes.PermissionDenied, status.Code(err))
		assert.False(t, called)
	})
}
