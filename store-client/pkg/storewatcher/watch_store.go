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

package storewatcher

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/readconcern"
	"go.mongodb.org/mongo-driver/mongo/readpref"
	"go.mongodb.org/mongo-driver/mongo/writeconcern"
)

// Struct for ResumeToken retrieval
type TokenDoc struct {
	ResumeToken bson.Raw `bson:"resumeToken"`
}

type ChangeStreamWatcher struct {
	client                    *mongo.Client
	changeStream              *mongo.ChangeStream
	eventChannel              chan bson.M
	resumeTokenCol            *mongo.Collection
	clientName                string
	mu                        sync.Mutex
	resumeTokenUpdateTimeout  time.Duration
	resumeTokenUpdateInterval time.Duration
	// Store database and collection for monitoring queries
	database   string
	collection string
	// closeOnce ensures eventChannel is closed only once
	closeOnce sync.Once
}

// nolint: cyclop
func NewChangeStreamWatcher(
	ctx context.Context,
	mongoConfig MongoDBConfig,
	tokenConfig TokenConfig,
	pipeline mongo.Pipeline,
) (*ChangeStreamWatcher, error) {
	clientOpts, err := constructMongoClientOptions(mongoConfig)
	if err != nil {
		return nil, fmt.Errorf("error creating mongoDB clientOpts: %w", err)
	}

	client, err := mongo.Connect(ctx, clientOpts)
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

	// Change stream options
	opts := options.ChangeStream().SetFullDocument(options.UpdateLookup)

	var storedToken TokenDoc

	hasResumeToken := false

	// Check if the resume token exists
	err = tokenColl.FindOne(ctx, bson.M{"clientName": tokenConfig.ClientName}).Decode(&storedToken)
	if err == nil {
		if len(storedToken.ResumeToken) > 0 {
			slog.Info("ResumeToken found", "token", storedToken.ResumeToken)
			opts.SetResumeAfter(storedToken.ResumeToken)

			hasResumeToken = true
		} else {
			slog.Info("No valid resume token found, starting stream from the beginning..")
		}
	} else if !errors.Is(err, mongo.ErrNoDocuments) {
		// if no document was found, it is a normal case if it's the first time the client is connecting
		return nil, fmt.Errorf("error retrieving resume token from DB %s and collection %s: %w",
			tokenConfig.TokenDatabase, tokenConfig.TokenCollection, err)
	}

	// Open the change stream with appropriate read preference based on resume token presence
	cs, err := openChangeStream(ctx, client, mongoConfig, pipeline, opts, hasResumeToken)
	if err != nil {
		return nil, fmt.Errorf("failed to open change stream: %w", err)
	}

	watcher := &ChangeStreamWatcher{
		client:                    client,
		changeStream:              cs,
		eventChannel:              make(chan bson.M),
		resumeTokenCol:            tokenColl,
		clientName:                tokenConfig.ClientName,
		resumeTokenUpdateTimeout:  totalTimeout,
		resumeTokenUpdateInterval: interval,
		database:                  mongoConfig.Database,
		collection:                mongoConfig.Collection,
	}

	return watcher, nil
}

// openChangeStream opens a change stream with the appropriate read preference based on whether
// a resume token is present. When resuming, it attempts SecondaryPreferred with bounded retries
// before falling back to Primary. When starting fresh, it uses SecondaryPreferred directly.
func openChangeStream(
	ctx context.Context,
	client *mongo.Client,
	mongoConfig MongoDBConfig,
	pipeline mongo.Pipeline,
	opts *options.ChangeStreamOptions,
	hasResumeToken bool,
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
			retryDeadlineSeconds, retryIntervalSeconds)
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
func openChangeStreamWithRetry(
	ctx context.Context,
	client *mongo.Client,
	mongoConfig MongoDBConfig,
	pipeline mongo.Pipeline,
	opts *options.ChangeStreamOptions,
	retryDeadlineSeconds int,
	retryIntervalSeconds int,
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

			cs, err := collP.Watch(ctx, pipeline, opts)
			if err != nil {
				return nil, fmt.Errorf("failed to start change stream on primary after retries: %w", err)
			}

			return cs, nil
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

func (w *ChangeStreamWatcher) Start(ctx context.Context) {
	go func(ctx context.Context) {
		defer w.closeOnce.Do(func() {
			close(w.eventChannel)
			slog.Info("ChangeStreamWatcher event channel closed", "client", w.clientName)
		})

		for {
			select {
			case <-ctx.Done():
				slog.Info("ChangeStreamWatcher context cancelled, stopping event processing", "client", w.clientName)
				return
			default:
				w.mu.Lock()
				hasNext := w.changeStream.Next(ctx)
				csErr := w.changeStream.Err()
				w.mu.Unlock()

				if hasNext {
					var event bson.M

					w.mu.Lock()
					err := w.changeStream.Decode(&event)
					w.mu.Unlock()

					if err != nil {
						slog.Info("Failed to decode change stream event", "error", err)
						continue
					}
					w.eventChannel <- event
				} else if csErr != nil {
					slog.Error("Failed to watch change stream", "error", csErr)
				}
			}
		}
	}(ctx)
}

func (w *ChangeStreamWatcher) MarkProcessed(ctx context.Context) error {
	token := w.changeStream.ResumeToken()

	timeout := time.Now().Add(w.resumeTokenUpdateTimeout)

	var err error

	slog.Info("Attempting to store resume token", "client", w.clientName)

	for {
		if time.Now().After(timeout) {
			return fmt.Errorf("retrying storing resume token for client %s timed out with error: %w", w.clientName, err)
		}

		_, err = w.resumeTokenCol.UpdateOne(
			ctx,
			bson.M{"clientName": w.clientName},
			bson.M{"$set": bson.M{"resumeToken": token}},
			options.Update().SetUpsert(true),
		)
		if err == nil {
			return nil
		}

		slog.Warn("Failed to store resume token for client, retrying",
			"client", w.clientName, "error", err)
		time.Sleep(w.resumeTokenUpdateInterval)
	}
}

func (w *ChangeStreamWatcher) Events() <-chan bson.M {
	return w.eventChannel
}

// GetUnprocessedEventCount returns the count of events inserted after the given ObjectID.
// This leverages MongoDB's default index on _id for efficient querying.
// Pass in the ObjectID of the event currently being processed.
// Optional additionalFilters can be provided to further filter the events.
func (w *ChangeStreamWatcher) GetUnprocessedEventCount(ctx context.Context, lastProcessedID primitive.ObjectID,
	additionalFilters ...bson.M) (int64, error) {
	filter := bson.M{"_id": bson.M{"$gt": lastProcessedID}}

	for _, additionalFilter := range additionalFilters {
		for key, value := range additionalFilter {
			filter[key] = value
		}
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
	w.mu.Lock()
	err := w.changeStream.Close(ctx)
	w.mu.Unlock()

	w.closeOnce.Do(func() {
		close(w.eventChannel)
		slog.Info("ChangeStreamWatcher event channel closed", "client", w.clientName)
	})

	if err != nil {
		return fmt.Errorf("failed to close change stream for client %s: %w", w.clientName, err)
	}

	return nil
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

	client, err := mongo.Connect(ctx, clientOpts)
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

	// load CA certificate
	caCert, err := pollTillCACertIsMountedSuccessfully(mongoConfig.ClientTLSCertConfig.CaCertPath,
		totalCertTimeout, intervalCert)
	if err != nil {
		return nil, fmt.Errorf("failed to read CA certificate with error: %w", err)
	}

	caCertPool := x509.NewCertPool()
	if !caCertPool.AppendCertsFromPEM(caCert) {
		return nil, fmt.Errorf("failed to append CA certificate to pool")
	}

	// load client certificate and key
	clientCert, err := tls.LoadX509KeyPair(mongoConfig.ClientTLSCertConfig.TlsCertPath,
		mongoConfig.ClientTLSCertConfig.TlsKeyPath)
	if err != nil {
		return nil, fmt.Errorf("failed to load client certificate and key: %w", err)
	}

	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{clientCert},
		RootCAs:      caCertPool,
		MinVersion:   tls.VersionTLS12,
	}

	credential := options.Credential{
		AuthMechanism: "MONGODB-X509",
		AuthSource:    "$external",
	}

	return options.Client().ApplyURI(mongoConfig.URI).SetTLSConfig(tlsConfig).SetAuth(credential), nil
}

func ConstructClientTLSConfig(
	totalCACertTimeoutSeconds int, intervalCACertSeconds int, clientCertMountPath string,
) (*tls.Config, error) {
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

	caCertPool := x509.NewCertPool()
	if !caCertPool.AppendCertsFromPEM(caCert) {
		return nil, fmt.Errorf("failed to append CA certificate to pool")
	}

	// Load client certificate and key
	clientCert, err := tls.LoadX509KeyPair(clientCertPath, clientKeyPath)
	if err != nil {
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
