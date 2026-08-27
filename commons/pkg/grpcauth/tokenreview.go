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

// Package grpcauth holds the shared server half of NVSentinel's gRPC
// ServiceAccount-token authentication: validation of a caller's projected SA
// token through the Kubernetes TokenReview API.
//
// It is deliberately stricter than a bare TokenReview call. Beyond
// status.authenticated it also verifies that the audience the resource server
// asked for is echoed back in status.audiences. Kubernetes requires the
// resource server to make this check: an authenticator that does not
// understand audiences may authenticate an API-server-audience token and
// return no audiences at all, which would otherwise let a token minted for
// another service be replayed here.
//
// A Validator answers "who is this?" and nothing more. Audience is not an
// authorization decision — any pod may request a token for an arbitrary
// audience through a projected volume in its own spec, so the audience says
// which service a token is for, not which workload holds it. Deciding what an
// authenticated identity may do is the caller's job; see
// platform-connectors/pkg/auth for the node-binding rules built on top of this.
//
// The client half lives in commons/pkg/grpcclient.
package grpcauth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"time"

	authv1 "k8s.io/api/authentication/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// authorizationHeader is the gRPC metadata key carrying the bearer credential.
const authorizationHeader = "authorization"

// bearerScheme is the required scheme on the authorization header. RFC 7235
// defines the scheme token as case-insensitive, so it is matched that way:
// rejecting "bearer foo" would refuse a well-formed credential.
const bearerScheme = "bearer"

// DefaultTokenReviewRetryWindow bounds the whole TokenReview operation,
// including the first attempt. Within it, retryable failures are retried with backoff so an
// API-server blip never surfaces to the caller; beyond it the call fails
// closed. It is sized to ride out a control-plane restart while staying well
// inside a publisher's own RPC deadline.
const DefaultTokenReviewRetryWindow = 8 * time.Second

// DefaultTokenReviewBackoff is the retry schedule inside that window.
var DefaultTokenReviewBackoff = wait.Backoff{Duration: 500 * time.Millisecond, Factor: 2, Jitter: 0.1, Steps: 8}

// Extra-info keys the Kubernetes authenticator attaches to a pod-bound token's
// TokenReview result. Pod name/uid have been reported for as long as bound
// tokens have existed; node name/uid arrive with the pod-node-info feature
// (on by default from 1.30, locked on from 1.32) and are simply absent on
// older clusters.
const (
	extraPodNameKey  = "authentication.kubernetes.io/pod-name"
	extraPodUIDKey   = "authentication.kubernetes.io/pod-uid"
	extraNodeNameKey = "authentication.kubernetes.io/node-name"
	extraNodeUIDKey  = "authentication.kubernetes.io/node-uid"
)

// Identity is the authenticated caller, as reported by TokenReview. Every
// field is a value type, so copying an Identity fully isolates it.
type Identity struct {
	// Username is the canonical Kubernetes username, e.g.
	// "system:serviceaccount:nvsentinel:csp-health-monitor".
	Username string
	// UID is the authenticated user's UID. For a ServiceAccount token this is
	// the ServiceAccount object's UID, which (unlike the name) is not reused
	// when a ServiceAccount is deleted and recreated.
	UID string
	// PodName and PodUID identify the exact pod instance the token is bound
	// to. TokenReview itself enforces that this pod still exists; these fields
	// are for audit logging, so a decision can be traced to one pod instance.
	PodName string
	PodUID  string
	// NodeName and NodeUID are the node the bound pod was scheduled on, written
	// into the token by the API server at issuance. Empty on clusters that do
	// not embed node info in tokens. When present, this is an attested claim of
	// where the caller runs — the caller cannot alter it.
	NodeName string
	NodeUID  string
}

// Validator authenticates bearer tokens against the Kubernetes TokenReview API.
// It is safe for concurrent use.
type Validator struct {
	client   kubernetes.Interface
	audience string
	// cache holds recent verdicts so steady-state callers do not cost one API
	// round trip per request.
	cache *verdictCache

	// retryWindow, backoff and now are fixed in production and only varied by
	// this package's own tests, which would otherwise have to spend the real
	// window to observe a retry.
	retryWindow time.Duration
	backoff     wait.Backoff
	now         func() time.Time
}

// NewValidator builds a Validator.
//
// audience must be non-empty: an audience-less TokenReview accepts the generic
// API-server token that every pod already has mounted, which is not an
// authentication decision worth making.
func NewValidator(client kubernetes.Interface, audience string) (*Validator, error) {
	if client == nil {
		return nil, fmt.Errorf("kubernetes client is required")
	}

	if audience == "" {
		return nil, fmt.Errorf("audience is required")
	}

	cache, err := newVerdictCache()
	if err != nil {
		return nil, err
	}

	return &Validator{
		client:      client,
		audience:    audience,
		cache:       cache,
		retryWindow: DefaultTokenReviewRetryWindow,
		backoff:     DefaultTokenReviewBackoff,
		now:         time.Now,
	}, nil
}

// Authenticate submits token to the TokenReview API and returns the caller's
// identity. It fails closed: any API error, unauthenticated verdict, or
// audience mismatch returns an error and never a partial Identity.
func (v *Validator) Authenticate(ctx context.Context, token string) (*Identity, error) {
	identity, cached := v.cache.get(token, v.now())
	if !cached {
		var err error

		identity, err = v.authenticateUncached(ctx, token)
		if err != nil {
			return nil, err
		}

		v.cache.put(token, identity, v.now())
	}

	// The full attested tuple, on every accepted call, at info: this is the
	// audit record that lets a decision be traced back to one pod instance on
	// one node. UIDs matter because names are reused — a deleted and recreated
	// ServiceAccount, pod or node keeps its name but never its UID.
	// The token itself is deliberately never logged.
	slog.InfoContext(ctx, "Request authenticated",
		"user", identity.Username, "userUID", identity.UID,
		"pod", identity.PodName, "podUID", identity.PodUID,
		"node", identity.NodeName, "nodeUID", identity.NodeUID,
		"cachedVerdict", cached)

	return identity, nil
}

// authenticateUncached performs the real TokenReview round trip.
func (v *Validator) authenticateUncached(ctx context.Context, token string) (*Identity, error) {
	review := &authv1.TokenReview{
		Spec: authv1.TokenReviewSpec{
			Token:     token,
			Audiences: []string{v.audience},
		},
	}

	result, err := CreateTokenReview(ctx, v.client, review, v.retryWindow, v.backoff)
	if err != nil {
		return nil, err
	}

	if !result.Status.Authenticated {
		slog.WarnContext(ctx, "Token authentication failed", "error", result.Status.Error)

		return nil, status.Errorf(codes.Unauthenticated, "token not authenticated: %s", result.Status.Error)
	}

	if !slices.Contains(result.Status.Audiences, v.audience) {
		slog.WarnContext(ctx, "Token audience mismatch",
			"user", result.Status.User.Username,
			"tokenAudiences", result.Status.Audiences,
			"requiredAudience", v.audience,
		)

		return nil, status.Errorf(codes.Unauthenticated,
			"token audiences %v do not include the required audience %q",
			result.Status.Audiences, v.audience)
	}

	identity := &Identity{
		Username: result.Status.User.Username,
		UID:      result.Status.User.UID,
	}

	for _, field := range []struct {
		key string
		dst *string
	}{
		{extraPodNameKey, &identity.PodName},
		{extraPodUIDKey, &identity.PodUID},
		{extraNodeNameKey, &identity.NodeName},
		{extraNodeUIDKey, &identity.NodeUID},
	} {
		value, err := exactlyOneExtraValue(result.Status.User.Extra, field.key)
		if err != nil {
			slog.ErrorContext(ctx, "Rejecting ambiguous TokenReview result",
				"user", result.Status.User.Username, "key", field.key, "error", err)

			return nil, status.Errorf(codes.Unauthenticated, "token review result is ambiguous: %v", err)
		}

		*field.dst = value
	}

	return identity, nil
}

// CreateTokenReview issues a TokenReview, retrying with exponential backoff
// while failures stay in the retryable class, and returns a gRPC-classified
// error. Permanent faults (RBAC, malformed request) and caller cancellation are
// surfaced immediately.
//
// window bounds the WHOLE operation, first attempt included. Starting the clock
// only after the first failure would let the REST client's own per-request
// timeout run to completion first, so a wedged API server could hold the
// caller's RPC for that timeout plus the retry window rather than the single
// bound configured here.
//
// The final error is always classified against the CALLER's context, never
// against the internal one, so our own window expiring reads as Unavailable
// (retry later) rather than as the caller's deadline.
//
// Exported so every resource server that authenticates with TokenReview shares
// one implementation rather than keeping its own copy of this logic.
func CreateTokenReview(
	callerCtx context.Context,
	client kubernetes.Interface,
	review *authv1.TokenReview,
	window time.Duration,
	backoff wait.Backoff,
) (*authv1.TokenReview, error) {
	opCtx, cancel := context.WithTimeout(callerCtx, window)
	defer cancel()

	var (
		result *authv1.TokenReview
		err    error
	)

	waitErr := wait.ExponentialBackoffWithContext(opCtx, backoff, func(attemptCtx context.Context) (bool, error) {
		result, err = client.AuthenticationV1().TokenReviews().Create(attemptCtx, review, metav1.CreateOptions{})

		switch {
		case err == nil:
			return true, nil
		case !isRetryableAPIError(err):
			return false, err
		default:
			return false, nil
		}
	})

	// The loop can finish without ever running the callback: if opCtx is
	// already expired on entry — which happens when the caller had already
	// given up — it returns immediately. err would then still be nil while
	// result is also nil, and returning that pair makes the caller treat a
	// total failure as a successful review and dereference a nil result.
	if err == nil && waitErr != nil {
		err = waitErr
	}

	// Belt and braces: a nil result with a nil error would panic in the caller,
	// and this code path runs inside a gRPC server with no panic recovery, so
	// one nil would take down health event ingestion for the whole node.
	if err == nil && result == nil {
		err = fmt.Errorf("token review completed without a result")
	}

	if err != nil {
		return nil, tokenReviewError(callerCtx, err)
	}

	return result, nil
}

// exactlyOneExtraValue returns the single value recorded for key in a TokenReview's extra
// map, or "" if the key is absent entirely. It is safe on a nil map.
//
// These fields identify who the caller is, so an ambiguous answer is refused
// rather than resolved: Kubernetes records exactly one value for each of them,
// and picking one of several would let whatever produced the extra list decide
// which identity this connector enforces against.
func exactlyOneExtraValue(extra map[string]authv1.ExtraValue, key string) (string, error) {
	values, present := extra[key]
	if !present {
		return "", nil
	}

	if len(values) != 1 {
		return "", fmt.Errorf("expected exactly one value for %q, got %d", key, len(values))
	}

	return values[0], nil
}

// BearerTokenFromContext extracts the bearer token from incoming gRPC
// metadata.
//
// The absent case is reported as ("", false, nil) rather than as an error so
// that a server accepting both authenticated and anonymous callers can branch
// on it. A header that is present but malformed is an error: it signals a
// broken client rather than a deliberate anonymous call, and silently
// downgrading it to anonymous would hide the misconfiguration.
func BearerTokenFromContext(ctx context.Context) (string, bool, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return "", false, nil
	}

	authHeaders := md.Get(authorizationHeader)
	if len(authHeaders) == 0 {
		return "", false, nil
	}

	// Picking one of several authorization headers would let the caller decide
	// which credential is checked and which is merely carried; there is no
	// correct choice to make here, so ambiguity is an error.
	if len(authHeaders) > 1 {
		return "", false, status.Error(codes.Unauthenticated,
			"multiple authorization headers")
	}

	scheme, credential, found := strings.Cut(authHeaders[0], " ")
	if !found || !strings.EqualFold(scheme, bearerScheme) {
		return "", false, status.Error(codes.Unauthenticated,
			"authorization header must use Bearer scheme")
	}

	token := credential
	if token == "" {
		return "", false, status.Error(codes.Unauthenticated, "empty bearer token")
	}

	return token, true, nil
}

// tokenReviewError maps a failed TokenReview call to a gRPC status.
//
// The only distinction that matters to a caller is retryable vs not.
// Unavailable is the one code every NVSentinel publisher treats as retryable
// (commons/pkg/healthpub isRetryable additionally retries DeadlineExceeded;
// health-events-analyzer retries Unavailable alone), so returning it for a
// transient outage keeps a health event alive across a control-plane blip, and
// returning Internal for a permanent fault stops the publisher from hiding a
// misconfiguration behind an endless retry loop.
// AuthBackendUnavailableReason marks a failure that happened while
// AUTHENTICATING the caller, before the RPC handler ran.
//
// It exists because Unavailable alone is ambiguous to a client: a connection
// that dropped after the server acted looks identical to one that never
// arrived. For an idempotent call that does not matter, but a client driving a
// destructive RPC — terminating a node — cannot safely retry an ambiguous
// failure, because the node may already be gone.
//
// A status carrying this reason is unambiguous: the server-side interceptor
// produced it before dispatch, so the handler never ran and no CSP action was
// taken. Retrying is then provably safe.
const AuthBackendUnavailableReason = "NVSENTINEL_AUTH_BACKEND_UNAVAILABLE"

// authErrorDomain scopes the reason above to this project.
const authErrorDomain = "nvsentinel.nvidia.com"

// withAuthUnavailableDetail tags st so a caller can tell a pre-handler
// authentication outage from an ambiguous transport failure. If the detail
// cannot be attached the bare status is returned: losing the hint costs a
// retry, never correctness.
func withAuthUnavailableDetail(st *status.Status) error {
	detailed, err := st.WithDetails(&errdetails.ErrorInfo{
		Reason: AuthBackendUnavailableReason,
		Domain: authErrorDomain,
	})
	if err != nil {
		return st.Err()
	}

	return detailed.Err()
}

// IsAuthBackendUnavailable reports whether err is an authentication-backend
// outage raised before the handler ran, and is therefore safe to retry even for
// a non-idempotent RPC.
func IsAuthBackendUnavailable(err error) bool {
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.Unavailable {
		return false
	}

	for _, d := range st.Details() {
		info, ok := d.(*errdetails.ErrorInfo)
		if !ok {
			continue
		}

		// Domain as well as reason: the reason string alone could be produced by
		// any service in the call path, and this decides whether a destructive
		// RPC is safe to repeat.
		if info.GetReason() == AuthBackendUnavailableReason && info.GetDomain() == authErrorDomain {
			return true
		}
	}

	return false
}

func tokenReviewError(ctx context.Context, err error) error {
	// The caller gave up, or its deadline passed. This is not a fault of the
	// API server and must not be reported as one.
	if ctxErr := ctx.Err(); ctxErr != nil {
		if errors.Is(ctxErr, context.DeadlineExceeded) {
			return status.Errorf(codes.DeadlineExceeded, "token validation deadline exceeded: %v", err)
		}

		return status.Errorf(codes.Canceled, "token validation canceled: %v", err)
	}

	if isRetryableAPIError(err) {
		// Timeouts, throttling, connection failures and 5xx: the token may well
		// be valid and the API server merely unreachable, so the caller should
		// back off and retry rather than treat its credential as rejected.
		slog.ErrorContext(ctx, "TokenReview API call failed; treating as retryable", "error", err)

		return withAuthUnavailableDetail(
			status.Newf(codes.Unavailable, "token validation unavailable: %v", err))
	}

	if k8serrors.IsUnauthorized(err) || k8serrors.IsForbidden(err) {
		// By far the most common permanent fault, and the least obvious from
		// the error alone, so it gets a named diagnostic.
		slog.ErrorContext(ctx, "TokenReview rejected: this service cannot create TokenReviews. "+
			"Check that its ClusterRole grants create on authentication.k8s.io/tokenreviews",
			"error", err)
	} else {
		slog.ErrorContext(ctx, "TokenReview request failed permanently", "error", err)
	}

	return status.Errorf(codes.Internal, "token validation failed: %v", err)
}

// isRetryableAPIError reports whether a TokenReview API failure might clear on
// its own. RBAC and malformed-request failures are configuration errors that
// no amount of retrying resolves; everything else (timeouts, throttling,
// connection failures, 5xx) is worth another attempt.
func isRetryableAPIError(err error) bool {
	switch {
	case k8serrors.IsUnauthorized(err), k8serrors.IsForbidden(err),
		k8serrors.IsBadRequest(err), k8serrors.IsInvalid(err), k8serrors.IsNotFound(err),
		k8serrors.IsMethodNotSupported(err), k8serrors.IsRequestEntityTooLargeError(err):
		return false
	default:
		return true
	}
}

// canonicalUsername matches exactly the username the API server reports for a
// ServiceAccount. Length limits are checked separately because Go's RE2 has no
// backreference-free way to bound each segment without duplicating the pattern.
var canonicalUsername = regexp.MustCompile(
	`^system:serviceaccount:([a-z0-9]([-a-z0-9]*[a-z0-9])?):` +
		`([a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*)$`)

const (
	// maxNamespaceLen is the DNS-1123 label limit Kubernetes applies to a
	// namespace name.
	maxNamespaceLen = 63
	// maxServiceAccountLen is the DNS-1123 subdomain limit applied to a
	// ServiceAccount name.
	maxServiceAccountLen = 253
)

// ValidateServiceAccountUsername checks that sa is a username Kubernetes could
// actually report, so an allowlist entry that can never match is rejected at
// startup rather than silently denying every request.
//
// Shared deliberately: platform-connector and janitor-provider both match
// allowlists against TokenReview's username, and two copies of "canonical"
// drifted apart once already.
func ValidateServiceAccountUsername(sa string) error {
	m := canonicalUsername.FindStringSubmatch(sa)
	if m == nil {
		return fmt.Errorf(
			"%q is not a canonical Kubernetes username: want "+
				"\"system:serviceaccount:<namespace>:<name>\" with a DNS-1123 label namespace "+
				"and a DNS-1123 subdomain name. Whitespace and capitals are refused rather "+
				"than trimmed, because an entry that does not match exactly can never equal "+
				"the username TokenReview reports", sa)
	}

	if namespace := m[1]; len(namespace) > maxNamespaceLen {
		return fmt.Errorf("%q has a %d-character namespace; Kubernetes limits it to %d",
			sa, len(namespace), maxNamespaceLen)
	}

	if name := m[3]; len(name) > maxServiceAccountLen {
		return fmt.Errorf("%q has a %d-character ServiceAccount name; Kubernetes limits it to %d",
			sa, len(name), maxServiceAccountLen)
	}

	return nil
}
