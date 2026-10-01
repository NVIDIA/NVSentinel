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

// Package auth binds an incoming health event to the identity of its publisher.
//
// platform-connector listens on a host-mounted Unix socket that is shared by
// two classes of publisher:
//
//   - Node-local publishers (syslog, nic, gpu, preflight checks, custom
//     monitors) only ever report the node they run on. They present a
//     projected ServiceAccount token whose node claim attests where they run;
//     tokenless callers are also still accepted, pinned to this node.
//   - Cluster-scoped publishers (csp-health-monitor, kubernetes-object-monitor,
//     nvcre-certification-monitor, slurm-drain-monitor, health-events-analyzer)
//     run centrally and must be
//     able to name any node in the cluster. They present a projected
//     ServiceAccount token minted for a dedicated audience, and their
//     ServiceAccounts are explicitly allowlisted.
//
// The node name arrives as a plain field on the event and is not re-derived
// anywhere downstream: fault-quarantine cordons it verbatim and
// fault-remediation stamps it onto the RebootNode CR. A publisher that names
// the wrong node therefore has its mistake carried all the way through to a
// cordon, drain or reboot of that node. This interceptor is the one place the
// claim can be checked against who is making it.
//
// A caller presenting a token is asked two independent questions.
//
// First, is the token being presented where it was issued? The API server
// writes the bound pod's node into the token at issuance, so a claim that names
// a different node means the token has been carried off its node and replayed,
// and the request is rejected. This applies to every token-presenting caller,
// allowlisted or not: it is about the credential's provenance, not about what
// its holder is entitled to say. Tokens from clusters that do not embed node
// info carry no claim and skip this question.
//
// Second, may this identity name nodes other than this one? Only the
// explicitly allowlisted ServiceAccounts may. Everyone else — tokenless
// callers, and authenticated callers that are not on the list — is scoped to
// this connector's own node, where a blank node name is filled in and a
// different one is rejected.
//
// Rejection, rather than silently rewriting a foreign node name to the local
// one, is deliberate: a rewrite would turn a misdirected event about node B
// into a real event against node A, and would hide the misconfiguration that
// produced it.
//
// The whole batch is validated before any of it is mutated or forwarded, so a
// batch is either accepted in full or rejected in full.
//
// The deployment platform connector serves the whole fleet over the network
// and uses the same interceptor with no local node (Config.NodeName empty).
// Everything above holds, with the node the caller's token claims in place of
// the connector's own: there is no socket to vouch for where a caller runs,
// so every caller must present a token bound to a scheduled pod and its
// events are pinned to that pod's node.
package auth

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/nvidia/nvsentinel/commons/pkg/grpcauth"
	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
)

// scope is the set of node names a caller may write events about.
type scope int

const (
	// scopeNodeLocal restricts the caller to this connector's own node.
	scopeNodeLocal scope = iota
	// scopeCrossNode lets the caller name any node.
	scopeCrossNode
)

func (s scope) String() string {
	if s == scopeCrossNode {
		return "cross_node"
	}

	return "node_local"
}

// Rejection reasons, used as a bounded-cardinality metric label.
//
//nolint:gosec // G101: these are metric label values, not credentials.
const (
	reasonNodeMismatch      = "node_mismatch"
	reasonMissingNodeName   = "missing_node_name"
	reasonTokenInvalid      = "token_invalid"
	reasonMalformedCreds    = "malformed_credentials"
	reasonNodeClaimMismatch = "node_claim_mismatch"
	// reasonUnboundCrossNodeToken is an allowlisted identity presenting a
	// credential the API server never tied to a running pod — see
	// requirePodBinding.
	reasonUnboundCrossNodeToken = "unbound_cross_node_token"
	// reasonCrossNodeClaimAbsent is an allowlisted identity whose token carries
	// no node claim at all — see requireAttestedOrigin.
	reasonCrossNodeClaimAbsent = "cross_node_claim_absent"
	// The reasons below only occur without a local node (the deployment
	// platform connector), where the token is the only evidence of where a
	// caller runs: no token at all, a token bound to no pod, and a token
	// bound to a pod that never scheduled.
	reasonTokenMissing    = "token_missing"
	reasonUnboundToken    = "unbound_token"
	reasonNodeClaimAbsent = "node_claim_absent"
	// The reasons below distinguish "we could not reach a verdict" from
	// "the caller's credential was rejected". Both fail the request, but only
	// the latter says anything about the caller: an API server outage would
	// otherwise increment the same counter as a forged token and make a routine
	// control-plane blip indistinguishable from an attack on a dashboard.
	reasonValidatorUnavailable = "validator_unavailable"
	reasonValidatorTimeout     = "validator_timeout"
	reasonValidatorError       = "validator_error"
)

// violationReasonsByCode maps the codes the validator returns to metric labels.
// Anything absent is deliberately collapsed by violationReasonFor, so the label
// set stays closed no matter what the validator grows to return.
var violationReasonsByCode = map[codes.Code]string{
	// The only code that is genuinely about the caller's credential.
	codes.Unauthenticated: reasonTokenInvalid,
	// No verdict reached: the API server was unreachable, or the caller gave up.
	codes.Unavailable:      reasonValidatorUnavailable,
	codes.DeadlineExceeded: reasonValidatorTimeout,
	codes.Canceled:         reasonValidatorTimeout,
}

// violationReasonFor maps a validator error to a fixed, bounded-cardinality
// metric label.
func violationReasonFor(err error) string {
	if reason, ok := violationReasonsByCode[status.Code(err)]; ok {
		return reason
	}

	return reasonValidatorError
}

var (
	authDecisions = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "platform_connector_auth_decisions_total",
		Help: "Health event batches by the node scope granted to the caller.",
	}, []string{"decision"})

	authViolations = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "platform_connector_auth_violations_total",
		Help: "Health event batches that violated the node-binding rule. Rejected under mode: enforce; " +
			"recorded but allowed through under mode: audit, except missing_node_name from a caller " +
			"resolved to verified cross-node scope, which is always rejected.",
	}, []string{"reason"})

	// authNodeClaim tracks whether authenticated callers' tokens carried a node
	// claim, so operators can see how much of the fleet issues them.
	// "verified": claim present and matched this node, or, without a local
	// node, taken as the caller's node. "absent": no node claim on the token,
	// so the check was skipped (the older-cluster fallback on the node-local
	// connector; a rejection without a local node).
	// A claim naming a different node is a rejection, counted in authViolations.
	authNodeClaim = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "platform_connector_auth_node_claim_total",
		Help: "Node-claim outcomes for authenticated callers.",
	}, []string{"result"})
)

const (
	nodeClaimVerified = "verified"
	nodeClaimAbsent   = "absent"
)

// TokenValidator authenticates a bearer token and reports who presented it.
// Satisfied by commons/pkg/grpcauth.Validator, which is also where the
// contract is enforced: a nil error must come with a non-nil Identity.
type TokenValidator interface {
	Authenticate(ctx context.Context, token string) (*grpcauth.Identity, error)
}

// Mode selects what the interceptor does with a violation it detects.
type Mode string

const (
	// ModeEnforce rejects a request that violates the node-binding rule. The
	// default: an empty Mode is treated as ModeEnforce.
	ModeEnforce Mode = "enforce"
	// ModeAudit records a violation exactly as ModeEnforce does — the same
	// counters, by the same reasons — but lets the request through instead of
	// rejecting it. It exists so operators can run node-binding against real
	// traffic, confirm the violation counters stay at zero, and switch to
	// ModeEnforce with evidence rather than finding out what it breaks in
	// production.
	ModeAudit Mode = "audit"
)

// Config configures the node-binding interceptor.
type Config struct {
	// NodeName is the node this platform-connector runs on, from the downward
	// API (NODE_NAME). Empty for the deployment platform connector, which has
	// no local node: callers are then required to present a pod-bound token,
	// and the node its claim names takes the place of NodeName everywhere.
	NodeName string

	// Validator authenticates callers presenting a token. Required: an
	// interceptor that cannot authenticate cannot enforce anything.
	Validator TokenValidator

	// CrossNodeServiceAccounts holds the canonical usernames
	// ("system:serviceaccount:<namespace>:<name>") permitted to name other
	// nodes. An authenticated identity outside this set is pinned to NodeName,
	// which is the same treatment an anonymous caller gets.
	CrossNodeServiceAccounts []string

	// Mode selects whether a violation rejects the request (ModeEnforce) or
	// only records it (ModeAudit). Defaults to ModeEnforce when empty.
	Mode Mode

	// FailOpenOnUnavailable controls the response to a validator that never
	// reached a verdict — the API server was unreachable, or the call timed
	// out — as opposed to one that reached a verdict and rejected the
	// credential. An outage says nothing about the caller: it is not evidence
	// of a forged token, only of the validator's own availability. When true,
	// that case falls back to node-local scope, the same treatment an
	// anonymous caller gets, instead of rejecting the request. A rejected
	// credential (an invalid or malformed token) is unaffected by this flag
	// and is always treated as ModeEnforce would treat it. Defaults to false:
	// fail closed.
	FailOpenOnUnavailable bool
}

type nodeBinder struct {
	nodeName              string
	validator             TokenValidator
	crossNode             map[string]struct{}
	mode                  Mode
	failOpenOnUnavailable bool
}

// noLocalNode reports whether this interceptor serves the deployment platform
// connector, where the node a caller may report on comes from its token.
func (b *nodeBinder) noLocalNode() bool {
	return b.nodeName == ""
}

// callerKey keys the authenticated caller in the request context.
type callerKey struct{}

// ContextWithCaller returns ctx carrying the authenticated caller.
func ContextWithCaller(ctx context.Context, identity *grpcauth.Identity) context.Context {
	return context.WithValue(ctx, callerKey{}, identity)
}

// CallerFromContext returns the authenticated caller the interceptor stored
// in the request context, or nil when the caller presented no token.
func CallerFromContext(ctx context.Context) *grpcauth.Identity {
	identity, _ := ctx.Value(callerKey{}).(*grpcauth.Identity)

	return identity
}

// NewNodeBindingInterceptor returns a gRPC unary server interceptor enforcing
// the package's node-binding rule on HealthEvents payloads. Requests carrying
// any other message type pass through untouched.
func NewNodeBindingInterceptor(cfg Config) (grpc.UnaryServerInterceptor, error) {
	crossNode, mode, err := validateConfig(cfg)
	if err != nil {
		return nil, err
	}

	b := &nodeBinder{
		nodeName:              cfg.NodeName,
		validator:             cfg.Validator,
		crossNode:             crossNode,
		mode:                  mode,
		failOpenOnUnavailable: cfg.FailOpenOnUnavailable,
	}

	slog.Info("platform-connector node binding enabled",
		"nodeName", b.nodeName, "tokenRequired", b.noLocalNode(), "crossNodeServiceAccounts", len(crossNode),
		"mode", b.mode, "failOpenOnUnavailable", b.failOpenOnUnavailable)

	return b.intercept, nil
}

// validateConfig checks cfg and returns the cross-node username set.
//
// Entries must already be canonical usernames. The namespace is not
// filled in here on the caller's behalf: an entry that silently became
// "system:serviceaccount:default:x" because a namespace was assumed would grant
// cross-node reach to an account nobody meant to name, so a malformed entry
// stops the process instead.
func validateConfig(cfg Config) (crossNode map[string]struct{}, mode Mode, err error) {
	if cfg.Validator == nil {
		return nil, "", fmt.Errorf("a token validator is required for node-binding enforcement")
	}

	mode = cfg.Mode
	if mode == "" {
		mode = ModeEnforce
	}

	if mode != ModeEnforce && mode != ModeAudit {
		return nil, "", fmt.Errorf("node-binding mode must be %q or %q, got %q", ModeEnforce, ModeAudit, cfg.Mode)
	}

	if err = validateNoLocalNode(cfg, mode); err != nil {
		return nil, "", err
	}

	if crossNode, err = crossNodeSet(cfg.CrossNodeServiceAccounts); err != nil {
		return nil, "", err
	}

	return crossNode, mode, nil
}

// validateNoLocalNode refuses the settings that only make sense with a local
// node to fall back to: without one every caller must be verified, and a
// caller whose rejection is only recorded would have no node its events
// could be pinned to.
func validateNoLocalNode(cfg Config, mode Mode) error {
	if cfg.NodeName != "" {
		return nil
	}

	if cfg.FailOpenOnUnavailable {
		return fmt.Errorf("fail-open on an unavailable validator needs a local node to " +
			"fall back to; without one every caller must be verified")
	}

	if mode == ModeAudit {
		return fmt.Errorf("audit mode needs a local node: without one a caller whose " +
			"rejection is only recorded has no node its events could be pinned to")
	}

	return nil
}

// crossNodeSet builds the cross-node set.
func crossNodeSet(usernames []string) (map[string]struct{}, error) {
	crossNode := make(map[string]struct{}, len(usernames))

	for _, sa := range usernames {
		if err := validateServiceAccountUsername(sa); err != nil {
			return nil, err
		}

		crossNode[sa] = struct{}{}
	}

	return crossNode, nil
}

// validateServiceAccountUsername checks that sa is the exact form TokenReview
// reports in status.user.username, which is the form the allowlist is matched
// against.
//
// Delegates to the shared validator so both resource servers agree on what
// "canonical" means. The colon-shape check this replaced accepted uppercase,
// underscores and over-long segments — identities Kubernetes cannot issue, so
// the entry silently matched nothing.
func validateServiceAccountUsername(sa string) error {
	if err := grpcauth.ValidateServiceAccountUsername(sa); err != nil {
		return fmt.Errorf("cross-node service account %w", err)
	}

	return nil
}

// intercept applies the node binding to a HealthEvents batch. The
// authenticated caller, when there is one, is left in the context for the
// handler (see CallerFromContext).
func (b *nodeBinder) intercept(
	ctx context.Context,
	req any,
	_ *grpc.UnaryServerInfo,
	handler grpc.UnaryHandler,
) (any, error) {
	events, isBatch := req.(*pb.HealthEvents)
	if !isBatch && !b.noLocalNode() {
		// On the node-local socket only health events are bound to a node;
		// anything else passes through untouched.
		return handler(ctx, req)
	}

	callerScope, identity, degraded, err := b.resolveScope(ctx)
	if err := b.auditOrReject(ctx, err); err != nil {
		return nil, err
	}

	if identity != nil {
		ctx = ContextWithCaller(ctx, identity)
	}

	if !isBatch {
		// Without a local node every request is authenticated; a request
		// that is not a batch has nothing left to bind.
		return handler(ctx, req)
	}

	authDecisions.WithLabelValues(callerScope.String()).Inc()

	// A blank node name from a cross-node caller produces an event nothing
	// downstream can handle, in every Mode: not something audit mode can
	// usefully let through, so it is enforced unconditionally rather than
	// through auditOrReject. See requireNodeNames.
	if err := b.requireNodeNames(ctx, events, callerScope); err != nil {
		return nil, err
	}

	// Validate the entire batch before mutating any of it, so a rejected batch
	// leaves no partially-stamped events behind. A degraded (fail-open) scope
	// is a guess, not a verified identity, so it gets the more conservative,
	// retryable check — see validateDegradedBatch — instead of validateBatch,
	// which would otherwise record a node_mismatch violation as a side effect
	// even though its result is about to be replaced.
	scopeNode := b.scopeNode(identity)

	var validateErr error
	if degraded {
		validateErr = b.validateDegradedBatch(ctx, events)
	} else {
		validateErr = b.validateBatch(ctx, events, callerScope, scopeNode)
	}

	if err := b.auditOrReject(ctx, validateErr); err != nil {
		return nil, err
	}

	if callerScope == scopeNodeLocal {
		b.stampMissingNodeNames(ctx, events, scopeNode)
	}

	return handler(ctx, req)
}

// scopeNode is the one node a node-local caller may report on: this
// connector's own node, or, without one, the node the caller's token claims.
func (b *nodeBinder) scopeNode(identity *grpcauth.Identity) string {
	if !b.noLocalNode() || identity == nil {
		return b.nodeName
	}

	return identity.NodeName
}

// auditOrReject implements the Mode toggle. err is the violation the caller
// just detected (nil if there was none). In ModeEnforce it is returned
// unchanged, rejecting the request. In ModeAudit the violation has already
// been counted by the caller that produced err, so it is logged and swallowed,
// letting the request through exactly as if no violation had occurred.
func (b *nodeBinder) auditOrReject(ctx context.Context, err error) error {
	if err == nil || b.mode != ModeAudit {
		return err
	}

	slog.WarnContext(ctx, "Audit mode: request would be rejected under enforce mode; allowing it through",
		"error", err)

	return nil
}

// isValidatorUnavailable reports whether reason means the validator never
// reached a verdict, as opposed to reaching one and rejecting the credential.
func isValidatorUnavailable(reason string) bool {
	return reason == reasonValidatorUnavailable || reason == reasonValidatorTimeout
}

// resolveScope authenticates the caller and returns the node scope it is
// entitled to, its identity (nil for a tokenless caller), and whether that
// scope is degraded: a fallback guess made without a verdict from the
// validator, rather than a verified identity. It fails closed: on any
// authentication error the returned scope is node-local and the error is
// non-nil, so the caller is rejected rather than having a cross-node claim
// silently downgraded to an unverified one. The one exception is a validator
// that never reached a verdict: when FailOpenOnUnavailable is set, that case
// falls back to node-local scope with no error but degraded set, because an
// outage says nothing about the caller's credential. Callers that get a
// degraded scope back must treat it as unverified — see validateDegradedBatch.
func (b *nodeBinder) resolveScope(ctx context.Context) (scope, *grpcauth.Identity, bool, error) {
	token, present, err := grpcauth.BearerTokenFromContext(ctx)
	if err != nil {
		b.recordViolation(reasonMalformedCreds)

		return scopeNodeLocal, nil, false, err
	}

	if !present {
		if b.noLocalNode() {
			// Nothing vouches for where a tokenless caller runs.
			b.recordViolation(reasonTokenMissing)

			return scopeNodeLocal, nil, false, status.Error(codes.Unauthenticated, "caller token required")
		}

		return scopeNodeLocal, nil, false, nil
	}

	identity, err := b.validator.Authenticate(ctx, token)
	if err != nil {
		reason := violationReasonFor(err)
		b.recordViolation(reason)

		if b.failOpenOnUnavailable && isValidatorUnavailable(reason) {
			slog.WarnContext(ctx, "Validator unavailable; failing open to a degraded node-local scope "+
				"rather than rejecting the caller", "reason", reason, "error", err)

			return scopeNodeLocal, nil, true, nil
		}

		return scopeNodeLocal, nil, false, err
	}

	// TokenValidator is an interface, so the non-nil-on-success contract cannot
	// be enforced at compile time however clearly it is documented. This runs
	// inside a gRPC server with no panic recovery, so an implementation
	// returning (nil, nil) would take down health event ingestion for the whole
	// node rather than failing one request. Treat it as a failed authentication.
	if identity == nil {
		b.recordViolation(reasonValidatorError)

		return scopeNodeLocal, nil, false, status.Error(codes.Internal, "token validation returned no identity")
	}

	callerScope, err := b.scopeForIdentity(ctx, identity)
	if err != nil {
		return scopeNodeLocal, nil, false, err
	}

	return callerScope, identity, false, nil
}

// scopeForIdentity decides what an authenticated caller may name: the
// token's provenance, then cross-node or node-local scope.
func (b *nodeBinder) scopeForIdentity(ctx context.Context, identity *grpcauth.Identity) (scope, error) {
	// Provenance first: a replayed token is refused whatever its holder is
	// entitled to say.
	if err := b.verifyNodeClaim(ctx, identity); err != nil {
		return scopeNodeLocal, err
	}

	_, crossNode := b.crossNode[identity.Username]

	// Cross-node reach always needs a fully attested credential; without a
	// local node so does every caller, because the token is the only evidence
	// of where it runs.
	if crossNode || b.noLocalNode() {
		if err := b.requireAttestedOrigin(ctx, identity, crossNode); err != nil {
			return scopeNodeLocal, err
		}
	}

	if b.noLocalNode() {
		// The claim is the caller's node; the API server attested it.
		authNodeClaim.WithLabelValues(nodeClaimVerified).Inc()
	}

	if crossNode {
		slog.DebugContext(ctx, "Caller granted cross-node scope",
			"user", identity.Username, "pod", identity.PodName, "tokenNode", identity.NodeName)

		return scopeCrossNode, nil
	}

	slog.DebugContext(ctx, "Caller scoped to one node",
		"user", identity.Username, "pod", identity.PodName, "nodeName", b.scopeNode(identity))

	return scopeNodeLocal, nil
}

// verifyNodeClaim answers the provenance question: was this token presented on
// the node it was issued on?
//
// The claim is written into the token by the API server at issuance, so it is
// an attested statement the holder cannot alter. The connector's socket is
// reachable only from its own node, so a claim naming any other node means the
// token has been carried off that node and replayed.
//
// This is deliberately independent of the allowlist. A cross-node identity is
// entitled to name other nodes, not to present its credential from other
// nodes — conflating the two would let a token copied to another node be
// used there, when refusing it confines the token to the node where its own
// pod runs.
func (b *nodeBinder) verifyNodeClaim(ctx context.Context, identity *grpcauth.Identity) error {
	// No claim to compare against. Counted for visibility, but not an error
	// here: on a connector with a local node the caller is pinned to that node
	// exactly as a tokenless one would be, so it gains nothing that reaching
	// the socket did not already grant. A cross-node caller, and every caller
	// of a connector without a local node, is refused right after this by
	// requireAttestedOrigin.
	if identity.NodeName == "" {
		authNodeClaim.WithLabelValues(nodeClaimAbsent).Inc()
		slog.DebugContext(ctx, "Token carries no node claim; provenance not checked",
			"user", identity.Username, "pod", identity.PodName, "nodeName", b.nodeName)

		return nil
	}

	// Without a local node there is nothing to compare the claim against: it
	// becomes the caller's node once requireAttestedOrigin has confirmed the
	// token is bound to a scheduled pod, and it is counted as verified there.
	if b.noLocalNode() {
		return nil
	}

	if identity.NodeName != b.nodeName {
		b.recordViolation(reasonNodeClaimMismatch)
		slog.ErrorContext(ctx, "Rejecting caller whose token is bound to a different node",
			"user", identity.Username, "pod", identity.PodName,
			"tokenNode", identity.NodeName, "connectorNode", b.nodeName)

		return status.Errorf(codes.PermissionDenied,
			"token is bound to node %q but was presented to the connector on node %q",
			identity.NodeName, b.nodeName)
	}

	authNodeClaim.WithLabelValues(nodeClaimVerified).Inc()

	return nil
}

// requireAttestedOrigin refuses a credential whose provenance the API server
// has not fully attested. It applies to cross-node callers on every
// connector, and to every caller of a connector without a local node.
//
// Cross-node reach lets one caller have any node in the cluster cordoned,
// drained and rebooted, so it is granted only against a token the API server
// tied to a specific running pod on a specific node:
//
//   - No pod binding. `kubectl create token <sa>` without --bound-object-ref
//     authenticates as the ServiceAccount with the right audience but is tied
//     to nothing, so it is replayable from anywhere for its whole lifetime.
//   - Pod binding but no node claim. A token bound to a pod that has not been
//     scheduled carries a pod UID and no node, so verifyNodeClaim has nothing
//     to compare and skips. Anyone able to create a pod that never schedules —
//     an unsatisfiable nodeSelector is enough — could otherwise mint a
//     credential with cluster-wide authority and no node binding at all.
//
// An absent node claim is refused here rather than read as "this must be an old
// cluster": NVSentinel requires Kubernetes 1.34+ (see README), and pod-node
// info has been GA since 1.32, so every scheduled pod's token carries a node.
// Node-local callers of the node-local connector keep the permissive
// treatment, because their scope is the connector's own node — exactly what
// reaching the socket already grants — so a claimless token gains them
// nothing. Without a local node there is no such fallback: the claim is the
// only thing that says which node the caller may report on.
func (b *nodeBinder) requireAttestedOrigin(ctx context.Context, identity *grpcauth.Identity, crossNode bool) error {
	if identity.PodUID == "" {
		if crossNode {
			b.recordViolation(reasonUnboundCrossNodeToken)
			slog.ErrorContext(ctx, "Rejecting cross-node caller whose token is not bound to a pod",
				"user", identity.Username, "connectorNode", b.nodeName)

			return status.Errorf(codes.PermissionDenied,
				"service account %q may name other nodes only with a pod-bound token; "+
					"this credential has no pod binding (a token minted outside a pod cannot be traced to one)",
				identity.Username)
		}

		b.recordViolation(reasonUnboundToken)
		slog.ErrorContext(ctx, "Rejecting caller whose token is not bound to a pod", "user", identity.Username)

		return status.Errorf(codes.PermissionDenied,
			"service account %q must present a pod-bound token; this credential has no pod binding "+
				"(a token minted outside a pod cannot be traced to one)", identity.Username)
	}

	if identity.NodeName == "" {
		if crossNode {
			b.recordViolation(reasonCrossNodeClaimAbsent)
			slog.ErrorContext(ctx, "Rejecting cross-node caller whose token carries no node claim",
				"user", identity.Username, "pod", identity.PodName, "connectorNode", b.nodeName)

			return status.Errorf(codes.PermissionDenied,
				"service account %q may name other nodes only with a token bound to a scheduled pod; "+
					"this credential carries no node claim", identity.Username)
		}

		b.recordViolation(reasonNodeClaimAbsent)
		slog.ErrorContext(ctx, "Rejecting caller whose token carries no node claim",
			"user", identity.Username, "pod", identity.PodName)

		return status.Errorf(codes.PermissionDenied,
			"service account %q must present a token bound to a scheduled pod; this credential carries no "+
				"node claim, so there is no node its events could be pinned to", identity.Username)
	}

	return nil
}

// requireNodeNames rejects a cross-node batch containing a blank node name, in
// every Mode.
//
// A cross-node publisher is expected to name nodes explicitly, so a blank
// name from one is a bug in that publisher, not a scope decision — stamping
// our own node here would attribute another node's fault to this one, and
// forwarding the blank name produces an event nothing downstream can handle
// (the store keeps it as-is, fault-quarantine's node lookup on an empty name
// fails, and the k8s connector drops the whole batch without retrying).
// ModeAudit exists to preview what ModeEnforce would reject without losing
// events; an event ModeEnforce could never accept intact is not something
// audit mode can usefully forward, so this check is not routed through
// auditOrReject the way validateBatch is.
func (b *nodeBinder) requireNodeNames(ctx context.Context, events *pb.HealthEvents, callerScope scope) error {
	if callerScope != scopeCrossNode {
		return nil
	}

	for i, event := range events.GetEvents() {
		if event.GetNodeName() != "" {
			continue
		}

		b.recordViolation(reasonMissingNodeName)
		slog.ErrorContext(ctx, "Rejecting cross-node event with no node name",
			"agent", event.GetAgent(), "checkName", event.GetCheckName())

		return status.Errorf(codes.InvalidArgument,
			"event %d: nodeName is required for cross-node publishers (agent=%s)",
			i, event.GetAgent())
	}

	return nil
}

// validateBatch checks every node-local event against the caller's own node
// without mutating anything, and reports the first violation found. Cross-node
// callers have nothing left to check here: their only batch-level rule is
// requireNodeNames, enforced unconditionally before this runs.
func (b *nodeBinder) validateBatch(
	ctx context.Context, events *pb.HealthEvents, callerScope scope, scopeNode string,
) error {
	if callerScope != scopeNodeLocal {
		return nil
	}

	for i, event := range events.GetEvents() {
		nodeName := event.GetNodeName()

		if nodeName != "" && nodeName != scopeNode {
			b.recordViolation(reasonNodeMismatch)
			slog.ErrorContext(ctx, "Rejecting health event naming a different node",
				"claimedNodeName", nodeName,
				"scopeNodeName", scopeNode,
				"agent", event.GetAgent(),
				"checkName", event.GetCheckName(),
			)

			return status.Errorf(codes.PermissionDenied,
				"event %d: caller may only report health events for node %q, got %q "+
					"(cross-node reporting requires an allowlisted service account token)",
				i, scopeNode, nodeName)
		}
	}

	return nil
}

// validateDegradedBatch is the check used in place of validateBatch when the
// caller's scope is degraded — a fallback guess made because
// FailOpenOnUnavailable let the request continue without a verdict from the
// validator, not a verified identity.
//
// An event naming a node other than this connector's own might be a
// legitimate cross-node publisher the outage prevented from being verified,
// not an actual mismatch, so it is refused as retryable Unavailable — the
// same code the validator itself returned — rather than as node_mismatch /
// PermissionDenied. That distinction matters twice over: every publisher
// treats PermissionDenied as non-retryable, so labelling a guess that way
// would drop the batch for good instead of letting it retry once the
// validator recovers; and node_mismatch is one of the reasons METRICS.md
// tells operators to alert on as suspected credential abuse, so mislabelling
// an outage that way would make a routine control-plane blip look like an
// attack. A blank node name is not treated specially here: it gets the same
// stamp-to-this-node treatment a verified node-local caller's blank name
// would, which is the common case this fallback exists for.
func (b *nodeBinder) validateDegradedBatch(ctx context.Context, events *pb.HealthEvents) error {
	for i, event := range events.GetEvents() {
		nodeName := event.GetNodeName()
		if nodeName == "" || nodeName == b.nodeName {
			continue
		}

		slog.WarnContext(ctx, "Rejecting event naming another node while the token validator is "+
			"unavailable; scope cannot be verified until it recovers",
			"claimedNodeName", nodeName, "connectorNodeName", b.nodeName,
			"agent", event.GetAgent(), "checkName", event.GetCheckName())

		return status.Errorf(codes.Unavailable,
			"event %d: cannot verify whether this caller may name node %q while the token validator is "+
				"unavailable; retry once it recovers", i, nodeName)
	}

	return nil
}

// stampMissingNodeNames fills in the caller's scope node for events that left
// nodeName blank. Only reached for node-local callers, and only after the
// whole batch has been validated.
func (b *nodeBinder) stampMissingNodeNames(ctx context.Context, events *pb.HealthEvents, scopeNode string) {
	if scopeNode == "" {
		return
	}

	for _, event := range events.GetEvents() {
		// The nil check guards the assignment below, not the getter: unlike
		// every other read in this package, writing NodeName dereferences the
		// pointer. validateBatch needs no such check because the generated
		// getters already read a nil event as a blank node name.
		if event == nil || event.GetNodeName() != "" {
			continue
		}

		event.NodeName = scopeNode

		slog.DebugContext(ctx, "Stamped the caller's scope node onto event with blank nodeName",
			"nodeName", scopeNode, "agent", event.GetAgent())
	}
}

func (b *nodeBinder) recordViolation(reason string) {
	authViolations.WithLabelValues(reason).Inc()
}
