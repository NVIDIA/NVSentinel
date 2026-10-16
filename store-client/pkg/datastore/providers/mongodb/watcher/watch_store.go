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

package watcher

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/readconcern"
	"go.mongodb.org/mongo-driver/v2/mongo/readpref"
	"go.mongodb.org/mongo-driver/v2/mongo/writeconcern"
	"go.opentelemetry.io/contrib/instrumentation/go.mongodb.org/mongo-driver/v2/mongo/otelmongo"
	"sigs.k8s.io/controller-runtime/pkg/certwatcher"

	"github.com/nvidia/nvsentinel/commons/pkg/tracing"
	"github.com/nvidia/nvsentinel/store-client/pkg/lagstate"
)

// fieldClientName is the resume-token document field identifying the watcher.
const fieldClientName = "clientName"

// changeStreamMaxAwaitTime is how long the server holds a getMore with nothing to deliver.
// It bounds both the idle cost of the TryNext loop and the resolution of the caught-up
// timestamp behind change_stream_lag_seconds, so it wants to be well under any lag alert
// threshold without making the loop chatty.
const changeStreamMaxAwaitTime = 1 * time.Second

// changeStreamReopenInterval is the pause between reopen attempts after a runtime failure.
// Each attempt is itself bounded by openChangeStream's own retry deadline, so this only
// paces the outer loop.
const changeStreamReopenInterval = 5 * time.Second

// changeStreamReopenBudget bounds the whole recovery window, not a single attempt.
//
// Sized to absorb a replica-set roll: a 3-member rolling restart was measured at about two
// minutes end to end on a 287-node cluster (#2030), so five minutes covers that with margin
// while still being short enough that a persistent failure becomes a restart promptly.
//
// Deliberately a constant rather than a MongoDBConfig field. Making it tunable means a method
// on the TimeoutConfig interface, which every implementer would have to grow - including a
// mock in another module - plus the prefixed/legacy env pair its siblings carry. Nobody has
// asked to tune it, and a config field nothing can set is worse than no field. If a fleet
// with slower rolls needs it, follow ChangeStreamRetryDeadlineSeconds end to end.
const changeStreamReopenBudget = 5 * time.Minute

// changeStreamCloseTimeout bounds the best-effort close of a change stream being replaced.
//
// Short on purpose: that close runs while swapChangeStream holds the write lock, so it is also
// how long MarkProcessed can be made to wait. The cursor being closed belongs to a stream that
// just failed, so the server is usually unreachable and a longer wait buys nothing - the point
// is to release server-side resources when it is cheap, not to insist on it.
const changeStreamCloseTimeout = 3 * time.Second

// errChangeStreamClosed reports a server-closed cursor, which TryNext signals as "no event
// and no error". Treated as an error so the stream is reopened rather than spun on.
var errChangeStreamClosed = errors.New("change stream cursor closed by server")

var resumeTokenRecoveries = promauto.NewCounterVec(
	prometheus.CounterOpts{
		Name: "change_stream_resume_token_recoveries_total",
		Help: "Total number of times a stale or invalid resume token was detected and deleted, " +
			"causing the change stream to restart from the current position.",
	},
	[]string{"client", "phase"},
)

// changeStreamReopens counts attempts to reopen the change stream after a runtime
// failure. A steadily climbing failure count is a watcher that is retrying and not
// getting through; any success at all is a disconnect that used to end the stream
// permanently.
var changeStreamReopens = promauto.NewCounterVec(
	prometheus.CounterOpts{
		Name: "change_stream_reopens_total",
		Help: "Total number of attempts to reopen a change stream after a runtime failure.",
	},
	[]string{"client", "result"},
)

// Event represents a database-agnostic event that abstracts away provider-specific types
type Event map[string]any

type MongoDBClientTLSCertConfig struct {
	TlsCertPath string
	TlsKeyPath  string
	CaCertPath  string
}

// MongoDBConfig holds the MongoDB connection configuration.
type MongoDBConfig struct {
	URI                              string
	Database                         string
	Collection                       string
	ClientTLSCertConfig              MongoDBClientTLSCertConfig
	TotalPingTimeoutSeconds          int
	TotalPingIntervalSeconds         int
	TotalCACertTimeoutSeconds        int
	TotalCACertIntervalSeconds       int
	ChangeStreamRetryDeadlineSeconds int
	ChangeStreamRetryIntervalSeconds int
	// CertWatcher is an optional certificate watcher for automatic client certificate rotation.
	// When provided, GetCertificate is used for dynamic client certificate loading.
	// CA certificates are still loaded statically from ClientTLSCertConfig.CaCertPath.
	CertWatcher *certwatcher.CertWatcher
	// AppName is used to identify the client in MongoDB connection tracking
	AppName string
	// MaxPoolSize caps the driver connection pool when greater than zero;
	// zero keeps the driver default (100).
	MaxPoolSize uint64
}

// TokenConfig holds the token-specific configuration.
type TokenConfig struct {
	ClientName      string
	TokenDatabase   string
	TokenCollection string
}

// Struct for ResumeToken retrieval
type TokenDoc struct {
	ResumeToken bson.Raw `bson:"resumeToken"`
}

type ChangeStreamWatcher struct {
	client                    *mongo.Client
	changeStream              *mongo.ChangeStream
	eventChannel              chan Event
	resumeTokenCol            *mongo.Collection
	clientName                string
	mu                        sync.Mutex
	closeMu                   sync.RWMutex // Protects changeStream from concurrent Next/Close
	resumeTokenUpdateTimeout  time.Duration
	resumeTokenUpdateInterval time.Duration
	// Store database and collection for monitoring queries, and for reopening the stream
	database   string
	collection string
	// The rest of what reopening needs. NOT the whole MongoDBConfig: that carries the URI,
	// i.e. credentials, and retaining it on a long-lived watcher is what CodeQL flagged on
	// #2040. These two plus database/collection are everything openChangeStream reads.
	changeStreamRetryDeadlineSeconds int
	changeStreamRetryIntervalSeconds int
	pipeline                         mongo.Pipeline
	// reopen, reopenInterval and reopenBudget are the seam recoverStream is tested through.
	// All three are nil/zero on a watcher built as a bare struct literal, which tests do, so
	// recoveryPolicy resolves the defaults rather than requiring them of the constructor.
	reopen         func(context.Context, bool) error
	reopenInterval time.Duration
	reopenBudget   time.Duration
	// closeOnce ensures eventChannel is closed only once
	closeOnce sync.Once
	// done signals when the event loop goroutine has exited
	done chan struct{}
	// cancel cancels the internal context to stop the event loop
	cancel context.CancelFunc

	// lag carries the two timestamps change_stream_lag_seconds is derived from. Embedded so
	// this watcher satisfies the optional lag-state interface pkg/client asserts on.
	lag lagstate.Tracker
}

// LagState reports when the watcher last observed itself caught up and the server time of the
// last event it read. Either may be zero before the first read, which callers must treat as
// "unknown" rather than as caught up: see ADR-054.
func (w *ChangeStreamWatcher) LagState() (lastEmptyBatch, lastEventRead time.Time) {
	return w.lag.LagState()
}

func NewChangeStreamWatcher(
	ctx context.Context,
	mongoConfig MongoDBConfig,
	tokenConfig TokenConfig,
	pipeline mongo.Pipeline,
) (*ChangeStreamWatcher, error) {
	if err := validatePingConfig(mongoConfig); err != nil {
		return nil, err
	}

	clientOpts, err := constructMongoClientOptions(mongoConfig)
	if err != nil {
		return nil, fmt.Errorf("error creating mongoDB clientOpts: %w", err)
	}

	client, err := mongo.Connect(clientOpts)
	if err != nil {
		return nil, fmt.Errorf("error connecting to mongoDB: %w", err)
	}

	totalTimeout := time.Duration(mongoConfig.TotalPingTimeoutSeconds) * time.Second
	interval := time.Duration(mongoConfig.TotalPingIntervalSeconds) * time.Second

	// Confirm connectivity to the target database and collection
	err = confirmConnectivityWithDBAndCollection(ctx, client, mongoConfig.Database,
		mongoConfig.Collection, totalTimeout, interval)
	if err != nil {
		return nil, fmt.Errorf("error connecting to database: %w", err)
	}

	// Decide read preference for the change stream after determining whether a resume token exists.

	// Confirm connectivity to the token database and collection
	err = confirmConnectivityWithDBAndCollection(ctx, client, tokenConfig.TokenDatabase,
		tokenConfig.TokenCollection, totalTimeout, interval)
	if err != nil {
		return nil, fmt.Errorf("error connecting to database: %w", err)
	}

	// Use majority write concern for resume tokens to ensure consistency across replicas
	// This is critical when reading change streams from secondaries
	wc := writeconcern.Majority()
	rc := readconcern.Majority()
	// Use Primary read preference for resume tokens to ensure consistency
	// Even though change streams use SecondaryPreferred, resume tokens must be read from primary
	rp := readpref.Primary()
	tokenCollOpts := options.Collection().SetWriteConcern(wc).SetReadConcern(rc).SetReadPreference(rp)
	tokenColl := client.Database(tokenConfig.TokenDatabase).Collection(tokenConfig.TokenCollection, tokenCollOpts)

	opts, hasResumeToken, err := changeStreamOptions(ctx, tokenColl, tokenConfig.ClientName)
	if err != nil {
		return nil, err
	}

	// Open the change stream with appropriate read preference based on resume token presence.
	// Pass the token collection so stale tokens can be deleted and retried automatically.
	cs, err := openChangeStream(ctx, client, mongoConfig, pipeline, opts, hasResumeToken,
		tokenColl, tokenConfig.ClientName)
	if err != nil {
		return nil, fmt.Errorf("failed to open change stream: %w", err)
	}

	watcher := &ChangeStreamWatcher{
		client:                           client,
		changeStream:                     cs,
		eventChannel:                     make(chan Event),
		resumeTokenCol:                   tokenColl,
		clientName:                       tokenConfig.ClientName,
		resumeTokenUpdateTimeout:         totalTimeout,
		resumeTokenUpdateInterval:        interval,
		database:                         mongoConfig.Database,
		collection:                       mongoConfig.Collection,
		changeStreamRetryDeadlineSeconds: mongoConfig.ChangeStreamRetryDeadlineSeconds,
		changeStreamRetryIntervalSeconds: mongoConfig.ChangeStreamRetryIntervalSeconds,
		pipeline:                         pipeline,
		done:                             make(chan struct{}),
	}

	return watcher, nil
}

func validatePingConfig(config MongoDBConfig) error {
	if config.TotalPingTimeoutSeconds <= 0 {
		return fmt.Errorf("invalid ping timeout value, value must be a positive integer")
	}

	if config.TotalPingIntervalSeconds <= 0 {
		return fmt.Errorf("invalid ping interval value, value must be a positive integer")
	}

	if config.TotalPingIntervalSeconds >= config.TotalPingTimeoutSeconds {
		return fmt.Errorf("invalid ping interval value, value must be less than ping timeout")
	}

	return nil
}

// changeStreamOptions builds the change-stream options for this client, resuming from the
// stored token when one is present. Shared by the constructor and by the reopen path so a
// reopened stream resumes from the same checkpoint a fresh process would.
//
// MaxAwaitTime is set explicitly rather than left to the server default: with TryNext in the
// event loop it is what bounds how long an idle poll blocks, and therefore the resolution of
// the caught-up timestamp behind change_stream_lag_seconds.
func changeStreamOptions(
	ctx context.Context,
	tokenColl *mongo.Collection,
	clientName string,
) (*options.ChangeStreamOptionsBuilder, bool, error) {
	opts := options.ChangeStream().
		SetFullDocument(options.UpdateLookup).
		SetMaxAwaitTime(changeStreamMaxAwaitTime)

	var storedToken TokenDoc

	err := tokenColl.FindOne(ctx, bson.M{fieldClientName: clientName}).Decode(&storedToken)
	if err != nil {
		// No document is the normal case the first time a client connects.
		if errors.Is(err, mongo.ErrNoDocuments) {
			return opts, false, nil
		}

		return nil, false, fmt.Errorf("error retrieving resume token from DB %s and collection %s: %w",
			tokenColl.Database().Name(), tokenColl.Name(), err)
	}

	if len(storedToken.ResumeToken) == 0 {
		slog.Info("No valid resume token found, starting stream from the beginning..")

		return opts, false, nil
	}

	slog.Info("ResumeToken found", "token", storedToken.ResumeToken)
	opts.SetResumeAfter(storedToken.ResumeToken)

	return opts, true, nil
}

// openChangeStream opens a change stream with the appropriate read preference based on whether
// a resume token is present. When resuming, it attempts SecondaryPreferred with bounded retries
// before falling back to Primary. If the resume token is stale (oplog history lost), the token
// is deleted and the stream is reopened fresh. When starting fresh, it uses SecondaryPreferred directly.
func openChangeStream(
	ctx context.Context,
	client *mongo.Client,
	mongoConfig MongoDBConfig,
	pipeline mongo.Pipeline,
	opts *options.ChangeStreamOptionsBuilder,
	hasResumeToken bool,
	tokenColl *mongo.Collection,
	clientName string,
) (*mongo.ChangeStream, error) {
	// Set default values if not configured
	retryDeadlineSeconds := mongoConfig.ChangeStreamRetryDeadlineSeconds
	if retryDeadlineSeconds <= 0 {
		retryDeadlineSeconds = 60 // Default to 1 minute
	}

	retryIntervalSeconds := mongoConfig.ChangeStreamRetryIntervalSeconds
	if retryIntervalSeconds <= 0 {
		retryIntervalSeconds = 3 // Default to 3 seconds
	}

	if hasResumeToken {
		return openChangeStreamWithRetry(ctx, client, mongoConfig, pipeline, opts,
			retryDeadlineSeconds, retryIntervalSeconds, tokenColl, clientName)
	}

	// No resume token, open on SecondaryPreferred directly
	collSP := client.Database(mongoConfig.Database).Collection(
		mongoConfig.Collection, options.Collection().SetReadPreference(readpref.SecondaryPreferred()))

	cs, err := collSP.Watch(ctx, pipeline, opts)
	if err != nil {
		return nil, fmt.Errorf("failed to start change stream: %w", err)
	}

	return cs, nil
}

// openChangeStreamWithRetry attempts to open a change stream with retries on SecondaryPreferred
// before falling back to Primary. This is used when resuming from a stored token.
// If the error indicates the resume token is stale (oplog history lost), the stored token is
// deleted and the stream is reopened without a resume token so the watcher can recover
// automatically instead of entering an unrecoverable crash loop.
func openChangeStreamWithRetry(
	ctx context.Context,
	client *mongo.Client,
	mongoConfig MongoDBConfig,
	pipeline mongo.Pipeline,
	opts *options.ChangeStreamOptionsBuilder,
	retryDeadlineSeconds int,
	retryIntervalSeconds int,
	tokenColl *mongo.Collection,
	clientName string,
) (*mongo.ChangeStream, error) {
	// Try SecondaryPreferred first with bounded retries
	collSP := client.Database(mongoConfig.Database).Collection(
		mongoConfig.Collection, options.Collection().SetReadPreference(readpref.SecondaryPreferred()))

	deadline := time.Now().Add(time.Duration(retryDeadlineSeconds) * time.Second)

	for {
		cs, openErr := collSP.Watch(ctx, pipeline, opts)
		if openErr == nil {
			return cs, nil
		}

		if isUnrecoverableResumeTokenError(openErr) {
			return recoverFromStaleResumeToken(ctx, collSP, pipeline, opts, tokenColl, clientName)
		}

		// If context was cancelled, return immediately
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		if time.Now().After(deadline) {
			slog.Warn("Change stream open on SecondaryPreferred failed, falling back to Primary",
				"retryDeadlineSeconds", retryDeadlineSeconds,
				"error", openErr)

			collP := client.Database(mongoConfig.Database).Collection(
				mongoConfig.Collection, options.Collection().SetReadPreference(readpref.Primary()))

			cs, primaryErr := collP.Watch(ctx, pipeline, opts)
			if primaryErr == nil {
				return cs, nil
			}

			if isUnrecoverableResumeTokenError(primaryErr) {
				return recoverFromStaleResumeToken(ctx, collSP, pipeline, opts, tokenColl, clientName)
			}

			return nil, fmt.Errorf("failed to start change stream on primary after retries: %w", primaryErr)
		}

		slog.Warn("Failed to open change stream on SecondaryPreferred while resuming, retrying",
			"retryIntervalSeconds", retryIntervalSeconds,
			"error", openErr)

		// Use select with timer to make sleep interruptible by context cancellation
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Duration(retryIntervalSeconds) * time.Second):
		}
	}
}

// recoverFromStaleResumeToken deletes an unrecoverable resume token from the database
// and reopens the change stream without it. This handles cases where the token is stale
// (oplog rolled over), corrupt, or otherwise unrecognizable by MongoDB.
func recoverFromStaleResumeToken(
	ctx context.Context,
	coll *mongo.Collection,
	pipeline mongo.Pipeline,
	opts *options.ChangeStreamOptionsBuilder,
	tokenColl *mongo.Collection,
	clientName string,
) (*mongo.ChangeStream, error) {
	slog.Warn("Resume token is unrecoverable, deleting token and starting fresh",
		"client", clientName)
	resumeTokenRecoveries.WithLabelValues(clientName, "initialization").Inc()

	if _, err := tokenColl.DeleteOne(ctx, bson.M{fieldClientName: clientName}); err != nil {
		slog.Error("Failed to delete resume token", "client", clientName, "error", err)
	}

	opts.SetResumeAfter(nil)
	opts.SetStartAfter(nil)

	cs, err := coll.Watch(ctx, pipeline, opts)
	if err != nil {
		return nil, fmt.Errorf("failed to open change stream after stale token recovery: %w", err)
	}

	slog.Info("Successfully recovered from stale resume token, stream started fresh", "client", clientName)

	return cs, nil
}

// isUnrecoverableResumeTokenError returns true if the error indicates the stored resume
// token is invalid and the change stream cannot be opened with it. This covers:
//   - 9: FailedToParse (token data is not valid hex / malformed)
//   - 260: InvalidResumeToken (token is structurally corrupt or unrecognizable)
//   - 280: ChangeStreamFatalError (general fatal change stream error)
//   - 286: ChangeStreamHistoryLost (oplog rolled over past the token's position)
func isUnrecoverableResumeTokenError(err error) bool {
	if serverErr, ok := errors.AsType[mongo.ServerError](err); ok {
		return serverErr.HasErrorCode(9) || serverErr.HasErrorCode(260) ||
			serverErr.HasErrorCode(280) || serverErr.HasErrorCode(286)
	}

	return false
}

func (w *ChangeStreamWatcher) Start(ctx context.Context) {
	// Create a child context that we can cancel on Close()
	watchCtx, cancel := context.WithCancel(ctx) //nolint:gosec // G118 - cancel is stored in w.cancel and called in Close()
	w.cancel = cancel

	go func() {
		defer func() {
			w.closeOnce.Do(func() {
				close(w.eventChannel)
				slog.Info("ChangeStreamWatcher event channel closed", "client", w.clientName)
			})

			if w.done != nil {
				close(w.done)
			}
		}()

		w.eventLoop(watchCtx)
	}()
}

// streamStep is what a single TryNext told the read loop.
type streamStep int

const (
	// stepEvent means an event is waiting to be decoded.
	stepEvent streamStep = iota
	// stepCaughtUp means the await window closed with nothing to deliver, which is affirmative
	// evidence of being caught up with this consumer's own filtered stream.
	stepCaughtUp
	// stepFailed means the stream has to be reopened.
	stepFailed
)

// classifyStreamStep decides what one TryNext meant. Separate from the loop because the
// closed-cursor case is otherwise only reachable against a live server: TryNext reports it as
// "no event and no error", which is indistinguishable from an empty batch except by the cursor
// ID, and treating it as an empty batch spins forever against a dead stream.
func classifyStreamStep(hasNext bool, csErr error, cursorID int64) (streamStep, error) {
	switch {
	case hasNext:
		return stepEvent, nil
	case csErr != nil:
		return stepFailed, csErr
	case cursorID == 0:
		return stepFailed, errChangeStreamClosed
	default:
		return stepCaughtUp, nil
	}
}

func (w *ChangeStreamWatcher) eventLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			slog.Info("ChangeStreamWatcher context cancelled, stopping event processing", "client", w.clientName)
			return
		default:
			// TryNext rather than Next so an idle stream is distinguishable from a stalled
			// one: it returns once the server's await window closes with nothing to deliver,
			// which is the only moment "caught up" can be recorded. Idle cost is unchanged,
			// because both issue a getMore the server holds for changeStreamMaxAwaitTime.
			//
			// Read lock allows concurrent cursor reads but prevents Close() during one.
			w.closeMu.RLock()
			hasNext := w.changeStream.TryNext(ctx)
			csErr := w.changeStream.Err()
			cursorID := w.changeStream.ID()
			w.closeMu.RUnlock()

			step, stepErr := classifyStreamStep(hasNext, csErr, cursorID)

			switch step {
			case stepEvent:
				w.processNextEvent(ctx)
			case stepFailed:
				tokenDiscarded := w.handleChangeStreamError(stepErr)

				if !w.recoverStream(ctx, tokenDiscarded) {
					return
				}
			case stepCaughtUp:
				w.lag.RecordCaughtUp(time.Now())
			}
		}
	}
}

// processNextEvent decodes the event TryNext has already made current.
//
// It reads w.changeStream without closeMu on purpose: it runs only on the event-loop
// goroutine, which is also the only goroutine that replaces that pointer (via recoverStream)
// and the one Close waits for before touching it. Anything reading the cursor from another
// goroutine must go through resumeToken instead.
func (w *ChangeStreamWatcher) processNextEvent(ctx context.Context) {
	var event bson.M

	w.mu.Lock()
	err := w.changeStream.Decode(&event)
	w.mu.Unlock()

	if err != nil {
		slog.Error("Failed to decode change stream event", "client", w.clientName, "error", err)
		return
	}

	// Record the event's own server-side time, not time.Now(): lag is the age of what the
	// consumer is reading, so a backlog of old events must read as old.
	w.lag.RecordEventRead(clusterTimeOf(event))

	// Attach this event's resume token (read via mongoEvent.GetResumeToken) so
	// consumers can checkpoint exactly at the event they processed. The change
	// stream cursor keeps advancing while consumers work, so checkpointing the
	// cursor position instead could skip events that were never processed.
	// The driver-owned bson.Raw aliases the current batch's pooled response
	// buffer, which is recycled once the event loop calls Next() again — clone
	// it so the token stays valid after the channel handoff.
	if token := w.changeStream.ResumeToken(); len(token) > 0 {
		event["_resumeToken"] = bson.Raw(append([]byte(nil), token...))
	}

	genericEvent := Event(event)

	select {
	case <-ctx.Done():
		slog.Info("Context cancelled while sending event, stopping", "client", w.clientName)
	case w.eventChannel <- genericEvent:
	}
}

// clusterTimeOf extracts a change event's server-side timestamp. Returns the zero time when
// the field is absent or not a timestamp, which recordEventRead then ignores.
func clusterTimeOf(event bson.M) time.Time {
	ts, ok := event["clusterTime"].(bson.Timestamp)
	if !ok {
		return time.Time{}
	}

	return time.Unix(int64(ts.T), 0).UTC()
}

// handleChangeStreamError logs the failure and reports whether it discarded the stored
// resume token. Callers need that answer: a discarded token means the next open is meant to
// start fresh, so the recovery path must not quietly resume from the position the server just
// rejected.
func (w *ChangeStreamWatcher) handleChangeStreamError(csErr error) bool {
	if isUnrecoverableResumeTokenError(csErr) {
		slog.Warn("Change stream failed due to unrecoverable resume token error, "+
			"deleting token so next restart recovers automatically",
			"client", w.clientName, "error", csErr)

		w.deleteStaleResumeToken()

		return true
	}

	slog.Error("Failed to watch change stream", "client", w.clientName, "error", csErr)

	return false
}

func (w *ChangeStreamWatcher) deleteStaleResumeToken() {
	resumeTokenRecoveries.WithLabelValues(w.clientName, "runtime").Inc()

	//nolint:gosec // G118 - parent context is already cancelled when we reach this path
	deleteCtx, deleteCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer deleteCancel()

	if _, delErr := w.resumeTokenCol.DeleteOne(deleteCtx, bson.M{fieldClientName: w.clientName}); delErr != nil {
		slog.Error("Failed to delete stale resume token during event loop recovery",
			"client", w.clientName, "error", delErr)
	}
}

// recoveryPolicy resolves how recovery behaves for this watcher. The three seam fields are
// unset on a watcher built as a bare struct literal, which tests do, so the defaults are
// resolved here rather than required of the constructor.
func (w *ChangeStreamWatcher) recoveryPolicy() (func(context.Context, bool) error, time.Duration, time.Duration) {
	reopen := w.reopen
	if reopen == nil {
		reopen = w.reopenChangeStream
	}

	interval := w.reopenInterval
	if interval <= 0 {
		interval = changeStreamReopenInterval
	}

	budget := w.reopenBudget
	if budget <= 0 {
		budget = changeStreamReopenBudget
	}

	return reopen, interval, budget
}

// recoverStream reopens the change stream after a runtime failure, and reports whether the
// event loop may continue. It returns false when the context is done, or when the recovery
// budget is spent — at which point the loop exits, the event channel closes, and the process
// is expected to terminate so the pod restarts and resumes from the stored token.
//
// Bounded rather than indefinite, deliberately. Retrying forever would reinstate the property
// this whole change exists to remove: a process that stays Ready while it consumes nothing,
// only now spinning in a retry loop instead of a dead one, with nothing in restartCount to
// show for it. A bounded window absorbs the transient case it is meant to absorb — a
// replica-set roll, a brief partition — and converts a persistent failure into a restart that
// is visible. The budget is wall-clock rather than a number of attempts because what it has
// to cover is an outage of a certain duration, not a certain number of failures.
//
// What sizes a single attempt is server selection, not the retry deadline (#2040 review).
// serverSelectionTimeout is TotalPingTimeoutSeconds, 300s by default, so in a partition where
// no member is selectable the first SecondaryPreferred Watch blocks for 300s — already past
// ChangeStreamRetryDeadlineSeconds — and the Primary fallback blocks for another 300s. One
// pass is therefore about ten minutes, and recovery gives up after the first attempt rather
// than after several. Still bounded, which is what #2020 needed, but the bound is the budget
// plus one pass of that size, not the budget alone.
//
// For the case the budget was sized for — a replica-set roll — a secondary stays selectable,
// each Watch returns quickly, and the five minutes behaves as intended.
//
// DO NOT tighten that by giving the attempt a context deadline. It looks like the obvious fix
// and it breaks the fallback: the driver treats any context carrying a deadline as a CSOT
// timeout context and switches retries to an unlimited budget
// (x/mongo/driver/operation.go, "If context is a Timeout context, automatically set retries to
// unlimited"). A secondary returning a retryable aggregate error would then be retried inside
// the driver until that deadline, openChangeStreamWithRetry would never regain control, and
// the Primary fallback — the thing that exists for exactly that failure — would never run.
// Tried on #2040 and reverted.
func (w *ChangeStreamWatcher) recoverStream(ctx context.Context, tokenDiscarded bool) bool {
	reopen, interval, budget := w.recoveryPolicy()
	deadline := time.Now().Add(budget)

	for attempt := 1; ; attempt++ {
		if ctx.Err() != nil {
			return false
		}

		err := reopen(ctx, tokenDiscarded)
		if err == nil {
			changeStreamReopens.WithLabelValues(w.clientName, "success").Inc()
			slog.Info("Change stream reopened after failure, resuming",
				"client", w.clientName, "attempt", attempt)

			return true
		}

		changeStreamReopens.WithLabelValues(w.clientName, "failure").Inc()

		if ctx.Err() != nil {
			return false
		}

		if !time.Now().Add(interval).Before(deadline) {
			changeStreamReopens.WithLabelValues(w.clientName, "exhausted").Inc()
			slog.Error("Giving up on reopening the change stream, the recovery budget is spent",
				"client", w.clientName, "attempts", attempt,
				"budgetSeconds", budget.Seconds(), "error", err)

			return false
		}

		slog.Warn("Failed to reopen change stream, will retry",
			"client", w.clientName, "attempt", attempt,
			"retryIntervalSeconds", interval.Seconds(),
			"budgetRemainingSeconds", time.Until(deadline).Seconds(), "error", err)

		select {
		case <-ctx.Done():
			return false
		case <-time.After(interval):
		}
	}
}

// reopenChangeStream opens a replacement change stream and swaps it in, resuming from the
// furthest position it can establish so events written during the outage are not skipped.
//
// tokenDiscarded says the stored token was just deleted as unrecoverable, which disables the
// in-memory fallback below.
func (w *ChangeStreamWatcher) reopenChangeStream(ctx context.Context, tokenDiscarded bool) error {
	opts, hasResumeToken, err := changeStreamOptions(ctx, w.resumeTokenCol, w.clientName)
	if err != nil {
		return err
	}

	// The stored token tracks what the CONSUMER has checkpointed, so it can be absent while
	// the stream itself had read plenty - a watcher that failed before its first MarkProcessed
	// is the clear case. Reopening with no token at all starts at the current oplog position,
	// which silently drops everything written between the failure and the reopen: the same
	// class of loss this change exists to prevent, in a narrower window.
	//
	// The failed cursor still carries the position it reached, so prefer that over starting
	// fresh. Cloned because the driver's bson.Raw aliases a pooled buffer.
	//
	// Skipped when the stored token was just discarded as unrecoverable: that path starts
	// fresh deliberately, and the in-memory token is the position the server rejected.
	//
	// tokenDiscarded reflects the failure that entered recovery, not anything a retry did
	// since. If an attempt's own openChangeStream deletes a stale token internally and then
	// fails, the next attempt can offer the rejected in-memory token once more - at which
	// point openChangeStream's stale-token recovery catches it and starts fresh. The cost is
	// one wasted attempt out of the budget, not a skipped event, so this is left as the
	// simpler invariant rather than threading per-attempt state through.
	if !hasResumeToken && !tokenDiscarded {
		if token := w.resumeToken(); len(token) > 0 {
			opts.SetResumeAfter(bson.Raw(append([]byte(nil), token...)))

			hasResumeToken = true

			slog.Info("No stored checkpoint; resuming from the failed stream's own position",
				"client", w.clientName)
		}
	}

	cs, err := openChangeStream(ctx, w.client, w.reopenConfig(), w.pipeline, opts, hasResumeToken,
		w.resumeTokenCol, w.clientName)
	if err != nil {
		return fmt.Errorf("failed to reopen change stream: %w", err)
	}

	w.swapChangeStream(ctx, cs)

	return nil
}

// reopenConfig assembles the subset of MongoDBConfig that reopening reads. Keeping only these
// four fields, rather than the config as a whole, is what keeps the URI off a long-lived
// struct (#2040 review).
func (w *ChangeStreamWatcher) reopenConfig() MongoDBConfig {
	return MongoDBConfig{
		Database:                         w.database,
		Collection:                       w.collection,
		ChangeStreamRetryDeadlineSeconds: w.changeStreamRetryDeadlineSeconds,
		ChangeStreamRetryIntervalSeconds: w.changeStreamRetryIntervalSeconds,
	}
}

// swapChangeStream installs the replacement cursor and releases the one it replaces.
//
// The close happens inside the write lock, not after it. The driver does not permit
// concurrent use of a ChangeStream, and MarkProcessed reads the current cursor's resume token
// from the processor goroutine - with the close outside the lock, a reader could be inside
// ResumeToken() on the very cursor being closed. Holding the lock across the close is what
// makes "nobody is in a method on this cursor" true at that moment.
//
// Closing is best effort otherwise: the stream that just failed is usually unreachable, and
// failing to release it must not fail the recovery.
func (w *ChangeStreamWatcher) swapChangeStream(ctx context.Context, cs *mongo.ChangeStream) {
	w.closeMu.Lock()
	defer w.closeMu.Unlock()

	previous := w.changeStream
	w.changeStream = cs

	if previous == nil {
		return
	}

	closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), changeStreamCloseTimeout)
	defer cancel()

	if closeErr := previous.Close(closeCtx); closeErr != nil {
		slog.Debug("Failed to close the change stream being replaced",
			"client", w.clientName, "error", closeErr)
	}
}

// resumeToken reports the current cursor's resume token, read while holding the lock that
// guards the cursor.
//
// Both the field and the object need that lock, which is why this returns the token rather
// than the stream: the event loop replaces the pointer on reopen and closes the cursor it
// replaced, so handing out the pointer and calling ResumeToken() on it after the lock is
// released would race with that close. The driver does not permit concurrent use of a
// ChangeStream.
func (w *ChangeStreamWatcher) resumeToken() bson.Raw {
	w.closeMu.RLock()
	defer w.closeMu.RUnlock()

	if w.changeStream == nil {
		return nil
	}

	return w.changeStream.ResumeToken()
}

func (w *ChangeStreamWatcher) MarkProcessed(ctx context.Context, token []byte) error {
	// Use the change stream resume token if the passed token is empty
	// This handles the common case where callers pass empty byte slices
	var resumeTokenToStore any

	if len(token) == 0 {
		// Read under the cursor's own lock; see resumeToken. Shared with the event loop's
		// read lock around TryNext, so it cannot deadlock against a sender blocked in the
		// driver, and it waits at most changeStreamCloseTimeout if a reopen is swapping.
		currentResumeToken := w.resumeToken()

		if currentResumeToken == nil {
			slog.Warn("No resume token available from change stream", "client", w.clientName)
			return fmt.Errorf("no resume token available from change stream for client %s", w.clientName)
		}

		resumeTokenToStore = currentResumeToken

		slog.Debug("Using change stream resume token", "client", w.clientName)
	} else {
		// Use the provided per-event token. Tokens originate from the change
		// stream as raw BSON documents ({"_data": ...}), so store them as a
		// document — the same shape SetResumeAfter expects when the token is
		// read back on startup. Storing the plain byte slice would write a
		// BSON binary value that fails to decode into TokenDoc.ResumeToken.
		raw := bson.Raw(token)
		if err := raw.Validate(); err != nil {
			return fmt.Errorf("invalid resume token for client %s: %w", w.clientName, err)
		}

		resumeTokenToStore = raw

		slog.Debug("Using provided resume token", "client", w.clientName)
	}

	timeout := time.Now().Add(w.resumeTokenUpdateTimeout)

	var err error

	slog.Info("Attempting to store resume token", "client", w.clientName)

	for {
		if time.Now().After(timeout) {
			return fmt.Errorf("retrying storing resume token for client %s timed out with error: %w", w.clientName, err)
		}

		_, err = w.resumeTokenCol.UpdateOne(
			ctx,
			bson.M{fieldClientName: w.clientName},
			bson.M{"$set": bson.M{"resumeToken": resumeTokenToStore}},
			options.UpdateOne().SetUpsert(true),
		)
		if err == nil {
			return nil
		}

		slog.Warn("Failed to store resume token for client, retrying",
			"client", w.clientName, "error", err)
		time.Sleep(w.resumeTokenUpdateInterval)
	}
}

func (w *ChangeStreamWatcher) Events() <-chan Event {
	return w.eventChannel
}

// GetUnprocessedEventCount returns the count of events inserted after the given ObjectID.
// This leverages MongoDB's default index on _id for efficient querying.
// Pass in the ObjectID of the event currently being processed.
// Optional additionalFilters can be provided to further filter the events.
func (w *ChangeStreamWatcher) GetUnprocessedEventCount(ctx context.Context, lastProcessedID bson.ObjectID,
	additionalFilters ...bson.M) (int64, error) {
	filter := bson.M{"_id": bson.M{"$gt": lastProcessedID}}

	for _, additionalFilter := range additionalFilters {
		maps.Copy(filter, additionalFilter)
	}

	coll := w.client.Database(w.database).Collection(w.collection)

	count, err := coll.CountDocuments(ctx,
		filter,
		options.Count().SetLimit(1000000),
	)
	if err != nil {
		return 0, fmt.Errorf("failed to count unprocessed events with filter %v: %w", filter, err)
	}

	return count, nil
}

func (w *ChangeStreamWatcher) Close(ctx context.Context) error {
	// Cancel the watch context to stop the event processing goroutine
	if w.cancel != nil {
		w.cancel()
	}

	// Wait for the event loop goroutine to exit before closing the change stream
	// This prevents data races between Next() and Close()
	// Check if done channel exists (it might be nil in tests that don't use NewChangeStreamWatcher)
	if w.done != nil {
		select {
		case <-w.done:
			// Goroutine has exited, safe to close
			slog.Debug("Event loop goroutine exited successfully", "client", w.clientName)
		case <-time.After(5 * time.Second):
			// Timeout waiting for goroutine to exit
			slog.Warn("Timeout waiting for event loop to exit, forcing close", "client", w.clientName)
		}
	}

	// Now it's safe to close the changeStream since the event loop has exited
	// Use write lock to ensure no concurrent Next() calls
	w.closeMu.Lock()
	closeErr := w.changeStream.Close(ctx)
	w.closeMu.Unlock()

	// Ensure event channel is closed (may already be closed by defer in Start())
	w.closeOnce.Do(func() {
		close(w.eventChannel)
		slog.Info("ChangeStreamWatcher event channel closed", "client", w.clientName)
	})

	var disconnectErr error
	if w.client != nil {
		if disconnectErr = w.client.Disconnect(ctx); disconnectErr != nil {
			slog.Warn("Failed to disconnect MongoDB client",
				"client", w.clientName,
				"error", disconnectErr)
		} else {
			slog.Info("Successfully disconnected MongoDB client", "client", w.clientName)
		}
	}

	if closeErr != nil {
		closeErr = fmt.Errorf("failed to close change stream for client %s: %w", w.clientName, closeErr)
	}

	if disconnectErr != nil {
		disconnectErr = fmt.Errorf("failed to disconnect MongoDB client for %s: %w", w.clientName, disconnectErr)
	}

	return errors.Join(closeErr, disconnectErr)
}

func confirmConnectivityWithDBAndCollection(ctx context.Context, client *mongo.Client, mongoDbName string,
	mongoDbCollection string, timeoutInterval time.Duration, pingInterval time.Duration) error {
	// Try pinging till a timeout to confirm connectivity with MongoDB database
	timeout := time.Now().Add(timeoutInterval) // total timeout

	var err error

	slog.Info("Trying to ping database to confirm connectivity", "database", mongoDbName)

	for {
		if time.Now().After(timeout) {
			return fmt.Errorf("retrying ping to database %s timed out with error: %w", mongoDbName, err)
		}

		var result bson.M

		err = client.Database(mongoDbName).RunCommand(ctx, bson.D{{Key: "ping", Value: 1}}).Decode(&result)
		if err == nil {
			slog.Info("Successfully pinged database to confirm connectivity", "database", mongoDbName)
			break
		}

		time.Sleep(pingInterval)
	}

	coll, err := client.Database(mongoDbName).ListCollectionNames(ctx, bson.D{{Key: "name", Value: mongoDbCollection}})

	switch {
	case err != nil:
		return fmt.Errorf("unable to get list of collections for DB %s with error: %w", mongoDbName, err)
	case len(coll) == 0:
		return fmt.Errorf("no collection with name %s for DB %s was found", mongoDbCollection, mongoDbName)
	case len(coll) > 1:
		return fmt.Errorf("more than one collection with name %s for DB %s was found", mongoDbCollection, mongoDbName)
	}

	slog.Info("Confirmed that the collection exists in the database",
		"collection", mongoDbCollection,
		"database", mongoDbName)

	return nil
}

func GetCollectionClient(
	ctx context.Context,
	mongoConfig MongoDBConfig,
) (*mongo.Collection, error) {
	clientOpts, err := constructMongoClientOptions(mongoConfig)
	if err != nil {
		return nil, fmt.Errorf("error creating mongoDB clientOpts: %w", err)
	}

	client, err := mongo.Connect(clientOpts)
	if err != nil {
		return nil, fmt.Errorf("error connecting to mongoDB: %w", err)
	}

	if mongoConfig.TotalPingTimeoutSeconds <= 0 {
		return nil, fmt.Errorf("invalid ping timeout value, value must be a positive integer")
	}

	if mongoConfig.TotalPingIntervalSeconds <= 0 {
		return nil, fmt.Errorf("invalid ping interval value, value must be a positive integer")
	}

	if mongoConfig.TotalPingIntervalSeconds >= mongoConfig.TotalPingTimeoutSeconds {
		return nil, fmt.Errorf("invalid ping interval value, value must be less than ping timeout")
	}

	totalTimeout := time.Duration(mongoConfig.TotalPingTimeoutSeconds) * time.Second
	interval := time.Duration(mongoConfig.TotalPingIntervalSeconds) * time.Second

	// Confirm connectivity to the target database and collection
	err = confirmConnectivityWithDBAndCollection(ctx, client, mongoConfig.Database,
		mongoConfig.Collection, totalTimeout, interval)
	if err != nil {
		return nil, fmt.Errorf("error connecting to database: %w", err)
	}

	// For strong consistency, we need the majority of replicas to ack reads and writes
	wc := writeconcern.Majority()
	rc := readconcern.Majority()
	// Use Primary read preference for strong consistency guarantees
	rp := readpref.Primary()
	collOpts := options.Collection().SetWriteConcern(wc).SetReadConcern(rc).SetReadPreference(rp)

	return client.Database(mongoConfig.Database).Collection(mongoConfig.Collection, collOpts), nil
}

func constructMongoClientOptions(
	mongoConfig MongoDBConfig,
) (*options.ClientOptions, error) {
	var (
		tlsConfig *tls.Config
		err       error
	)
	// Use CertWatcher if provided for automatic client certificate rotation
	if mongoConfig.CertWatcher != nil {
		tlsConfig, err = constructDynamicTLSConfig(mongoConfig)
		if err != nil {
			return nil, err
		}
	} else {
		tlsConfig, err = constructStaticTLSConfig(mongoConfig)
		if err != nil {
			return nil, err
		}
	}

	// Set server selection timeout to allow MongoDB time to become ready
	// This uses the same timeout as the ping timeout for consistency
	serverSelectionTimeout := time.Duration(mongoConfig.TotalPingTimeoutSeconds) * time.Second

	clientOpts := options.Client().
		ApplyURI(mongoConfig.URI).
		SetServerSelectionTimeout(serverSelectionTimeout).
		// Driver v2 decodes nested documents into bson.D by default, whereas v1
		// mirrored the ancestor type and produced bson.M. Change stream events and
		// query results are read as bson.M and their nested fields are type-asserted
		// as such throughout this package, so keep the v1 shape.
		//
		// ObjectIDAsHexString likewise restores v1 behaviour: v2 refuses to decode
		// an ObjectID into a Go string ("decoding an object ID into a string is not
		// supported by default"), which breaks structs that bind `bson:"_id"` to a
		// string field.
		SetBSONOptions(&options.BSONOptions{
			DefaultDocumentM:    true,
			ObjectIDAsHexString: true,
		}).
		SetMonitor(otelmongo.NewMonitor(
			otelmongo.WithTracerProvider(tracing.GetChildOnlyTracerProvider()),
		))

	// Set AppName for MongoDB connection tracking if provided
	if mongoConfig.AppName != "" {
		clientOpts.SetAppName(mongoConfig.AppName)
	}

	// Cap the connection pool when configured; zero keeps the driver default (100)
	if mongoConfig.MaxPoolSize > 0 {
		clientOpts.SetMaxPoolSize(mongoConfig.MaxPoolSize)
	}

	// Only set TLS when TLS config was successfully built.
	// Only set X.509 auth when client certificate is available (CA-only
	// TLS should not attempt X.509 auth since there's no client cert).
	// When certificate rotation is enabled, the client certificate is
	// provided dynamically via GetClientCertificate rather than the
	// Certificates slice.
	if tlsConfig != nil {
		clientOpts.SetTLSConfig(tlsConfig)

		if len(tlsConfig.Certificates) > 0 || tlsConfig.GetClientCertificate != nil {
			credential := options.Credential{
				AuthMechanism: "MONGODB-X509",
				AuthSource:    "$external",
			}
			clientOpts.SetAuth(credential)
		}
	}

	return clientOpts, nil
}

func constructDynamicTLSConfig(mongoConfig MongoDBConfig) (*tls.Config, error) {
	slog.Info("Using certificate watcher for TLS configuration with automatic rotation support")

	// Load CA certificate statically (CA rotation is not supported)
	caCertPool, err := loadCACertPool(mongoConfig)
	if err != nil {
		return nil, err
	}

	// No CA cert available — TLS disabled
	if caCertPool == nil {
		return nil, nil
	}

	return &tls.Config{
		// Use GetCertificate for dynamic client certificate loading
		// Controller-runtime's certwatcher.GetCertificate returns *tls.Certificate
		GetClientCertificate: func(_ *tls.CertificateRequestInfo) (*tls.Certificate, error) {
			return mongoConfig.CertWatcher.GetCertificate(nil)
		},
		RootCAs:    caCertPool,
		MinVersion: tls.VersionTLS12,
	}, nil
}

// loadCACertPool loads the CA certificate pool from the config.
// Used when certificate rotation is enabled for the client cert but CA remains static.
func loadCACertPool(mongoConfig MongoDBConfig) (*x509.CertPool, error) {
	timeout := mongoConfig.TotalCACertTimeoutSeconds
	if timeout == 0 {
		timeout = 600 // 10 minutes by default
	}

	totalCertTimeout := time.Duration(timeout) * time.Second

	interval := mongoConfig.TotalCACertIntervalSeconds
	if interval == 0 {
		interval = 5 // 5 seconds by default
	}

	intervalCert := time.Duration(interval) * time.Second

	caCert, err := pollTillCACertIsMountedSuccessfully(mongoConfig.ClientTLSCertConfig.CaCertPath,
		totalCertTimeout, intervalCert)
	if err != nil {
		return nil, fmt.Errorf("failed to read CA certificate: %w", err)
	}

	// No certs available (TLS disabled)
	if caCert == nil {
		return nil, nil
	}

	caCertPool := x509.NewCertPool()
	if !caCertPool.AppendCertsFromPEM(caCert) {
		return nil, fmt.Errorf("failed to append CA certificate to pool")
	}

	return caCertPool, nil
}

// constructStaticTLSConfig creates a TLS config with statically loaded certificates.
// This is the backward-compatible path when certificate rotation is not enabled.
func constructStaticTLSConfig(mongoConfig MongoDBConfig) (*tls.Config, error) {
	// Load CA certificate
	caCertPool, err := loadCACertPool(mongoConfig)
	if err != nil {
		return nil, err
	}

	// No CA cert available — TLS disabled
	if caCertPool == nil {
		return nil, nil
	}

	// Load client certificate and key. If the files don't exist, fall back
	// to CA-only TLS (server verification without client cert) rather than
	// failing — the cert mount may not be present.
	clientCert, err := tls.LoadX509KeyPair(mongoConfig.ClientTLSCertConfig.TlsCertPath,
		mongoConfig.ClientTLSCertConfig.TlsKeyPath)
	if err != nil {
		if os.IsNotExist(err) {
			slog.Warn("Client certificate or key not found, using CA-only TLS (no mTLS)",
				"certPath", mongoConfig.ClientTLSCertConfig.TlsCertPath,
				"keyPath", mongoConfig.ClientTLSCertConfig.TlsKeyPath)

			return &tls.Config{
				RootCAs:    caCertPool,
				MinVersion: tls.VersionTLS12,
			}, nil
		}

		return nil, fmt.Errorf("failed to load client certificate and key: %w", err)
	}

	return &tls.Config{
		Certificates: []tls.Certificate{clientCert},
		RootCAs:      caCertPool,
		MinVersion:   tls.VersionTLS12,
	}, nil
}

// ConstructClientTLSConfig builds a TLS configuration from certificates at the
// given mount path. Returns (nil, nil) when clientCertMountPath is empty,
// indicating TLS is intentionally disabled. Returns a non-nil *tls.Config with
// RootCAs and client certificates when certs are found. Returns an error for
// invalid cert paths, unreadable files, or malformed certificates.
func ConstructClientTLSConfig(
	totalCACertTimeoutSeconds int, intervalCACertSeconds int, clientCertMountPath string,
) (*tls.Config, error) {
	if clientCertMountPath == "" {
		slog.Info("No client cert mount path configured, skipping TLS")
		return nil, nil
	}

	clientCertPath := filepath.Join(clientCertMountPath, "tls.crt")
	clientKeyPath := filepath.Join(clientCertMountPath, "tls.key")
	mongoCACertPath := filepath.Join(clientCertMountPath, "ca.crt")

	totalCertTimeout := time.Duration(totalCACertTimeoutSeconds) * time.Second
	intervalCert := time.Duration(intervalCACertSeconds) * time.Second

	// load CA certificate
	caCert, err := pollTillCACertIsMountedSuccessfully(mongoCACertPath, totalCertTimeout, intervalCert)
	if err != nil {
		return nil, fmt.Errorf("failed to read CA certificate: %w", err)
	}

	// No CA cert available — TLS disabled
	if caCert == nil {
		return nil, nil
	}

	caCertPool := x509.NewCertPool()
	if !caCertPool.AppendCertsFromPEM(caCert) {
		return nil, fmt.Errorf("failed to append CA certificate to pool")
	}

	// Load client certificate and key. If the files don't exist, fall back
	// to CA-only TLS (server verification without client cert).
	clientCert, err := tls.LoadX509KeyPair(clientCertPath, clientKeyPath)
	if err != nil {
		if os.IsNotExist(err) {
			slog.Warn("Client certificate or key not found, using CA-only TLS (no mTLS)",
				"certPath", clientCertPath, "keyPath", clientKeyPath)

			return &tls.Config{
				RootCAs:    caCertPool,
				MinVersion: tls.VersionTLS12,
			}, nil
		}

		return nil, fmt.Errorf("failed to load client certificate and key: %w", err)
	}

	return &tls.Config{
		Certificates: []tls.Certificate{clientCert},
		RootCAs:      caCertPool,
		MinVersion:   tls.VersionTLS12,
	}, nil
}

func pollTillCACertIsMountedSuccessfully(certPath string, timeoutInterval time.Duration,
	pingInterval time.Duration) ([]byte, error) {
	if certPath == "" {
		slog.Info("No CA cert path configured, TLS will be disabled")
		return nil, nil
	}

	if !filepath.IsAbs(certPath) {
		return nil, fmt.Errorf("CA cert path %q is not absolute — this is likely a misconfiguration. "+
			"Use --tls-enabled=false to explicitly disable TLS, or provide an absolute cert mount path", certPath)
	}

	timeout := time.Now().Add(timeoutInterval) // total timeout

	var err error

	slog.Info("Trying to read CA cert", "path", certPath)

	for {
		if time.Now().After(timeout) {
			return nil, fmt.Errorf("retrying reading CA cert from %s timed out with error: %w", certPath, err)
		}

		var caCert []byte
		// load CA certificate
		caCert, err = os.ReadFile(certPath)
		if err == nil {
			slog.Info("Successfully read CA cert")
			return caCert, nil
		} else {
			slog.Info("Failed to read CA certificate, retrying", "error", err)
		}

		time.Sleep(pingInterval)
	}
}
